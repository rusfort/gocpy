package c

import (
	"fmt"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/types"
)

// Exceptions are compiled to explicit propagation (see runtime gocpy_rt.h):
//
//	throw X        ->  gocpy_exc = gocpy_exc_new(&gocpy_ti_T, &x, sizeof x); goto <handler>;
//	f() @throws    ->  ({ R r = f(); if (GOCPY_PENDING()) goto <handler>; r; })
//
// <handler> is the dispatch label of the innermost try, the finally label
// when inside a catch clause, or gocpy_exc_out which returns to the caller.

// excHandler returns the label to jump to when an exception is pending.
func (g *gen) excHandler() string {
	if n := len(g.excStack); n > 0 {
		return g.excStack[n-1]
	}
	g.needExcOut = true
	return "gocpy_exc_out"
}

// tiRef returns a pointer expression to the runtime type descriptor of n.
func (g *gen) tiRef(n *types.Named) string { return g.tiRefT(n) }

// checked wraps a call to a @throws function with a pending-exception test.
func (g *gen) checked(call string, sig *types.Signature) string {
	h := g.excHandler()
	if len(sig.Results) == 0 {
		return fmt.Sprintf("({ %s; if (GOCPY_PENDING()) goto %s; (void)0; })", call, h)
	}
	t := g.newTmp()
	return fmt.Sprintf("({ %s %s = %s; if (GOCPY_PENDING()) goto %s; %s; })", g.resultType(sig), t, call, h, t)
}

func (g *gen) throwStmt(s *ast.ThrowStmt) {
	t := g.info.Throws[s].(*types.Named)
	x := g.expr(s.X)
	h := g.excHandler()
	if isIface(t) { // throw err: the dynamic type is the exception type
		g.line("gocpy_exc = gocpy_exc_from_iface(%s);", x)
		g.line("goto %s;", h)
		return
	}
	if t == types.ExceptionType { // rethrow
		g.line("gocpy_exc = (%s);", x)
		g.line("goto %s;", h)
		return
	}
	v := g.newTmp()
	ct := g.cType(t)
	g.line("{")
	g.ind++
	g.line("%s %s = %s;", ct, v, x)
	g.line("gocpy_exc = gocpy_exc_new(%s, &%s, sizeof(%s));", g.tiRef(t), v, ct)
	g.line("goto %s;", h)
	g.ind--
	g.line("}")
}

// leave emits a jump to a point outside of the innermost finStack entries,
// running finally blocks on the way. depth is the target len(g.finStack).
func (g *gen) leave(depth int, final func()) {
	n := len(g.finStack)
	if n <= depth {
		final()
		return
	}
	t := g.finStack[n-1]
	code := len(t.actions) + 1
	g.line("%s = %d; goto %s;", t.pend, code, t.fin)
	t.actions = append(t.actions, pendAction{code, func() { g.leave(depth, final) }})
}

func (g *gen) tryStmt(s *ast.TryStmt) {
	parent := g.excHandler()
	dispatch := g.newLabel("try_catch")
	end := g.newLabel("try_end")
	var ctx *tryCtx
	g.line("{")
	g.ind++
	if s.Finally != nil {
		ctx = &tryCtx{fin: g.newLabel("try_finally"), pend: g.newTmp()}
		g.line("int %s = 0;", ctx.pend)
		g.finStack = append(g.finStack, ctx)
	}
	after := end // where normal completion of body/catch goes
	if ctx != nil {
		after = ctx.fin
	}

	// body
	g.excStack = append(g.excStack, dispatch)
	g.line("{")
	g.ind++
	g.stmts(s.Body.List)
	g.ind--
	g.line("}")
	g.excStack = g.excStack[:len(g.excStack)-1]
	g.line("goto %s;", after)

	// dispatch on the exception type
	g.line("%s: ;", dispatch)
	g.line("{")
	g.ind++
	ex := g.newTmp()
	g.line("gocpy_exception *%s = gocpy_exc;", ex)
	inCatch := parent
	if ctx != nil {
		inCatch = ctx.fin
	}
	caughtAll := false
	for _, cc := range s.Catches {
		info := g.info.Catches[cc]
		var ifv string
		switch {
		case info.Type == nil:
			caughtAll = true
			g.line("{")
		case isIface(info.Type):
			ifv = g.newTmp()
			g.line("gocpy_iface %s;", ifv)
			g.line("if (gocpy_exc_as_iface(%s, %s, &%s)) {", ex, g.idescRef(info.Type), ifv)
		default:
			g.line("if (%s->type == %s) {", ex, g.tiRef(info.Type.(*types.Named)))
		}
		g.ind++
		g.line("gocpy_exc = NULL;")
		if info.Var != nil {
			if info.Type == nil {
				g.declLocal(info.Var, ex)
			} else if ifv != "" {
				g.declLocal(info.Var, ifv)
			} else {
				ct := g.cType(info.Type)
				g.declLocal(info.Var, fmt.Sprintf("*(%s *)%s->data", ct, ex))
			}
		}
		g.excStack = append(g.excStack, inCatch)
		g.stmts(cc.Body.List)
		g.excStack = g.excStack[:len(g.excStack)-1]
		g.line("goto %s;", after)
		g.ind--
		g.line("}")
	}
	if !caughtAll {
		// not handled here: keep it pending and propagate (through finally)
		if ctx != nil {
			g.line("goto %s;", ctx.fin)
		} else {
			g.line("(void)%s;", ex)
			g.line("goto %s;", parent)
		}
	}
	g.ind--
	g.line("}")

	if ctx != nil {
		g.finStack = g.finStack[:len(g.finStack)-1]
		g.line("%s: ;", ctx.fin)
		pe := g.newTmp()
		g.line("{")
		g.ind++
		g.line("gocpy_exception *%s = gocpy_exc;", pe)
		g.line("gocpy_exc = NULL;")
		g.line("{")
		g.ind++
		if s.Finally == g.deferFin && g.deferFin != nil {
			g.line("gocpy_run_defers(&gocpy_defers);")
		}
		g.stmts(s.Finally.List)
		g.ind--
		g.line("}")
		g.line("if (%s) { gocpy_exc = %s; goto %s; }", pe, pe, g.excHandler())
		g.ind--
		g.line("}")
		for _, a := range ctx.actions {
			g.line("if (%s == %d) {", ctx.pend, a.code)
			g.ind++
			a.emit()
			g.ind--
			g.line("}")
		}
	}
	g.ind--
	g.line("}")
	g.line("%s: ;", end)
}
