package c

import (
	"fmt"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

func (g *gen) stmts(list []ast.Stmt) {
	for _, s := range list {
		g.stmt(s)
	}
}

func (g *gen) stmt(s ast.Stmt) {
	mark := len(g.props)
	g.stmtInner(s)
	g.flushProps(mark)
}

func (g *gen) stmtInner(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.UnsafeStmt:
		g.stmt(s.Body)
	case *ast.EmptyStmt:
	case *ast.ExprStmt:
		g.line("%s;", g.expr(s.X))
	case *ast.DeclStmt:
		d := s.Decl.(*ast.GenDecl)
		if d.Tok != token.VAR {
			return // constants are inlined
		}
		for _, sp := range d.Specs {
			g.assignSpec(sp.(*ast.ValueSpec), false)
		}
	case *ast.AssignStmt:
		g.assignStmt(s)
	case *ast.IncDecStmt:
		op := "++"
		if s.Tok == token.DEC {
			op = "--"
		}
		g.line("%s%s;", g.lvalue(s.X), op)
	case *ast.BlockStmt:
		g.line("{")
		g.ind++
		g.stmts(s.List)
		g.ind--
		g.line("}")
	case *ast.IfStmt:
		g.ifStmt(s)
	case *ast.ForStmt:
		g.forStmt(s)
	case *ast.ForInStmt:
		lbl := loopLabels{brk: g.newLabel("brk"), cont: g.newLabel("cont"), finDepth: len(g.finStack)}
		g.line("{")
		g.ind++
		vars := g.info.ForVars[s]
		g.mapKeys = g.info.MapKeys[s]
		g.iterate(s.Iter, vars[0], vars[1], func() {
			g.loops = append(g.loops, lbl)
			g.stmts(s.Body.List)
			g.loops = g.loops[:len(g.loops)-1]
			g.line("%s: ;", lbl.cont)
		})
		g.ind--
		g.line("}")
		g.line("%s: ;", lbl.brk)
	case *ast.SwitchStmt:
		g.switchStmt(s)
	case *ast.ReturnStmt:
		g.returnStmt(s)
	case *ast.SendStmt:
		g.sendStmt(s)
	case *ast.GoStmt:
		g.goStmt(s)
	case *ast.DeferStmt:
		g.deferStmt(s)
	case *ast.SelectStmt:
		g.selectStmt(s)
	case *ast.TryStmt:
		g.tryStmt(s)
	case *ast.ThrowStmt:
		g.throwStmt(s)
	case *ast.BranchStmt:
		for i := len(g.loops) - 1; i >= 0; i-- {
			l := g.loops[i]
			target := ""
			switch {
			case s.Tok == token.BREAK:
				target = l.brk
			case s.Tok == token.CONTINUE && l.cont != "":
				target = l.cont
			default:
				continue
			}
			g.leave(l.finDepth, func() { g.line("goto %s;", target) })
			return
		}
		g.errorf(s.Pos(), "internal: branch without target")
	default:
		g.errorf(s.Pos(), "codegen: unsupported statement %T", s)
	}
}

func (g *gen) assignStmt(s *ast.AssignStmt) {
	switch s.Tok {
	case token.DEFINE:
		var vars []*types.Var
		for _, l := range s.Lhs {
			v, _ := g.info.Defs[l.(*ast.Ident)].(*types.Var)
			vars = append(vars, v)
		}
		if val, ok, isRecv := g.commaOk(s.Rhs); isRecv {
			for i, src := range []string{val, ok} {
				if vars[i] != nil {
					g.declLocal(vars[i], src)
				}
			}
			return
		}
		if len(s.Rhs) == 1 && len(vars) > 1 {
			t := g.newTmp()
			g.line("%s %s = %s;", g.cType(g.typeOf(s.Rhs[0])), t, g.expr(s.Rhs[0]))
			for i, v := range vars {
				if v != nil {
					g.declLocal(v, fmt.Sprintf("%s.r%d", t, i))
				}
			}
			return
		}
		// evaluate all values before declaring (a, b := b, a refers to outer vars)
		vals := make([]string, len(vars))
		for i, r := range s.Rhs {
			vals[i] = g.expr(r)
		}
		if len(vars) > 1 {
			for i, r := range s.Rhs {
				t := g.newTmp()
				g.line("%s %s = %s;", g.cType(g.typeOf(r)), t, vals[i])
				vals[i] = t
			}
		}
		for i, v := range vars {
			if v == nil {
				g.line("(void)(%s);", vals[i])
				continue
			}
			g.declLocal(v, vals[i])
		}
	case token.ASSIGN:
		blank := func(e ast.Expr) bool {
			id, ok := e.(*ast.Ident)
			return ok && id.Name == "_"
		}
		if val, ok, isRecv := g.commaOk(s.Rhs); isRecv && len(s.Lhs) == 2 {
			for i, src := range []string{val, ok} {
				if !blank(s.Lhs[i]) {
					g.line("%s = %s;", g.lvalue(s.Lhs[i]), src)
				}
			}
			return
		}
		if len(s.Rhs) == 1 && len(s.Lhs) > 1 {
			t := g.newTmp()
			g.line("%s %s = %s;", g.cType(g.typeOf(s.Rhs[0])), t, g.expr(s.Rhs[0]))
			for i, l := range s.Lhs {
				if !blank(l) {
					g.line("%s = %s.r%d;", g.lvalue(l), t, i)
				}
			}
			return
		}
		if len(s.Lhs) == 1 {
			if blank(s.Lhs[0]) {
				g.line("(void)(%s);", g.expr(s.Rhs[0]))
			} else if ie, m := mapIndex(g, s.Lhs[0]); ie != nil {
				// evaluate the value first: it may grow the same map
				v := g.newTmp()
				g.line("{ %s %s = %s; %s = %s; }", g.cType(m.Elem), v, g.expr(s.Rhs[0]), g.mapSlot(ie, m), v)
			} else {
				g.line("%s = %s;", g.expr(s.Lhs[0]), g.expr(s.Rhs[0]))
			}
			return
		}
		// parallel assignment: a, b = b, a
		tmps := make([]string, len(s.Rhs))
		for i, r := range s.Rhs {
			tmps[i] = g.newTmp()
			g.line("%s %s = %s;", g.cType(g.typeOf(r)), tmps[i], g.expr(r))
		}
		for i, l := range s.Lhs {
			if !blank(l) {
				g.line("%s = %s;", g.lvalue(l), tmps[i])
			}
		}
	default:
		lt := g.typeOf(s.Lhs[0])
		p := g.newTmp()
		g.line("{")
		g.ind++
		lhs := "(*" + p + ")"
		var val string
		if ie, m := mapIndex(g, s.Lhs[0]); ie != nil {
			// m[k] op= v: evaluate v before taking the slot (v may grow the map)
			rv := g.newTmp()
			g.line("%s %s = %s;", g.cType(g.typeOf(s.Rhs[0])), rv, g.expr(s.Rhs[0]))
			g.line("%s *%s = &%s;", g.cType(lt), p, g.mapSlot(ie, m))
			if info := g.info.AssignOps[s]; info != nil {
				recv := lhs
				if info.RecvAddr {
					recv = p
				}
				val = fmt.Sprintf("%s(%s, %s)", info.Func.CName, recv, rv)
			} else {
				val = g.binCodeStr(types.AssignOp(s.Tok), lt, lhs, rv, s.Rhs[0])
			}
			g.line("*%s = %s;", p, val)
			g.ind--
			g.line("}")
			return
		}
		g.line("%s *%s = &(%s);", g.cType(lt), p, g.expr(s.Lhs[0]))
		if info := g.info.AssignOps[s]; info != nil {
			recv := lhs
			if info.RecvAddr {
				recv = p
			}
			val = fmt.Sprintf("%s(%s, %s)", info.Func.CName, recv, g.expr(s.Rhs[0]))
		} else {
			val = g.binCode(types.AssignOp(s.Tok), lt, lhs, s.Rhs[0])
		}
		g.line("*%s = %s;", p, val)
		g.ind--
		g.line("}")
	}
}

func (g *gen) ifStmt(s *ast.IfStmt) {
	g.line("{")
	g.ind++
	if s.Init != nil {
		g.stmt(s.Init)
	}
	g.line("if (%s) {", g.expr(s.Cond))
	g.ind++
	g.stmts(s.Body.List)
	g.ind--
	if s.Else != nil {
		g.line("} else {")
		g.ind++
		if b, ok := s.Else.(*ast.BlockStmt); ok {
			g.stmts(b.List)
		} else {
			g.stmt(s.Else)
		}
		g.ind--
	}
	g.line("}")
	g.ind--
	g.line("}")
}

func (g *gen) forStmt(s *ast.ForStmt) {
	lbl := loopLabels{brk: g.newLabel("brk"), cont: g.newLabel("cont"), finDepth: len(g.finStack)}
	g.line("{")
	g.ind++
	if s.Init != nil {
		g.stmt(s.Init)
	}
	g.line("for (;;) {")
	g.ind++
	if s.Cond != nil {
		g.line("if (!(%s)) break;", g.expr(s.Cond))
	}
	g.loops = append(g.loops, lbl)
	g.stmts(s.Body.List)
	g.loops = g.loops[:len(g.loops)-1]
	g.line("%s: ;", lbl.cont)
	if s.Post != nil {
		g.stmt(s.Post)
	}
	g.ind--
	g.line("}")
	g.ind--
	g.line("}")
	g.line("%s: ;", lbl.brk)
}

// iterate emits a loop over iter, declaring key/val, and calls body inside.
func (g *gen) iterate(iter ast.Expr, key, val *types.Var, body func()) {
	it := g.newTmp()
	i := g.newTmp()
	t := g.typeOf(iter)
	src := g.expr(iter)
	switch u := t.Underlying().(type) {
	case *types.Basic:
		if u.IsString() {
			g.line("gocpy_string %s = %s;", it, src)
			g.line("for (int64_t %s = 0; %s < %s.len; ) {", i, i, it)
			g.ind++
			r, w := g.newTmp(), g.newTmp()
			g.line("int32_t %s; int64_t %s = gocpy_decode_rune(%s, %s, &%s);", r, w, it, i, r)
			if key != nil {
				g.declLocal(key, i)
			}
			if val != nil {
				g.declLocal(val, r)
			}
			body()
			g.line("%s += %s;", i, w)
			g.ind--
			g.line("}")
			return
		}
		// for i in n
		g.line("%s %s = %s;", g.cType(t), it, src)
		g.line("for (%s %s = 0; %s < %s; %s++) {", g.cType(t), i, i, it, i)
		g.ind++
		if val != nil {
			g.declLocal(val, i)
		}
		body()
		g.ind--
		g.line("}")
		return
	case *types.Map:
		keysOnly := g.mapKeys
		g.mapKeys = false
		kp, vp := g.newTmp(), g.newTmp()
		g.line("gocpy_map *%s = %s;", it, src)
		g.line("int64_t %s = 0; void *%s, *%s;", i, kp, vp)
		g.line("while (gocpy_map_next(%s, &%s, &%s, &%s)) {", it, i, kp, vp)
		g.ind++
		if key != nil {
			g.declLocal(key, fmt.Sprintf("*(%s *)%s", g.cType(u.Key), kp))
		}
		if val != nil {
			if keysOnly {
				g.declLocal(val, fmt.Sprintf("*(%s *)%s", g.cType(u.Key), kp))
			} else {
				g.declLocal(val, fmt.Sprintf("*(%s *)%s", g.cType(u.Elem), vp))
			}
		}
	case *types.Chan:
		g.line("gocpy_chan *%s = %s;", it, src)
		g.line("for (;;) {")
		g.ind++
		v := g.newTmp()
		g.line("%s %s;", g.cType(u.Elem), v)
		g.line("if (!gocpy_chan_recv(%s, &%s)) break;", it, v)
		if val != nil {
			g.declLocal(val, v)
		}
		_ = i
	case *types.Slice:
		g.line("gocpy_slice %s = %s;", it, src)
		g.line("for (int64_t %s = 0; %s < %s.len; %s++) {", i, i, it, i)
		g.ind++
		if key != nil {
			g.declLocal(key, i)
		}
		if val != nil {
			g.declLocal(val, fmt.Sprintf("((%s *)%s.data)[%s]", g.cType(u.Elem), it, i))
		}
	case *types.Array:
		g.line("%s %s = %s;", g.cType(t), it, src) // range over a copy, as in Go
		g.line("for (int64_t %s = 0; %s < %d; %s++) {", i, i, u.Len, i)
		g.ind++
		if key != nil {
			g.declLocal(key, i)
		}
		if val != nil {
			g.declLocal(val, fmt.Sprintf("%s.a[%s]", it, i))
		}
	case *types.Pointer:
		a := u.Elem.Underlying().(*types.Array)
		g.line("%s %s = (%s)gocpy_nonnil(%s);", g.cType(t), it, g.cType(t), src)
		g.line("for (int64_t %s = 0; %s < %d; %s++) {", i, i, a.Len, i)
		g.ind++
		if key != nil {
			g.declLocal(key, i)
		}
		if val != nil {
			g.declLocal(val, fmt.Sprintf("%s->a[%s]", it, i))
		}
	default:
		g.errorf(iter.Pos(), "codegen: cannot iterate over %s", t)
		return
	}
	body()
	g.ind--
	g.line("}")
}

func (g *gen) switchStmt(s *ast.SwitchStmt) {
	if ts := g.info.TypeSwitches[s]; ts != nil {
		g.typeSwitch(s, ts)
		return
	}
	brk := g.newLabel("brk")
	g.line("{")
	g.ind++
	if s.Init != nil {
		g.stmt(s.Init)
	}
	var tag string
	var tagType types.Type
	if s.Tag != nil {
		e := s.Tag.(*ast.ExprStmt).X
		tagType = g.typeOf(e)
		tag = g.newTmp()
		g.line("%s %s = %s;", g.cType(tagType), tag, g.expr(e))
	}
	labels := make([]string, len(s.Body))
	defLabel := brk
	for i, cl := range s.Body {
		labels[i] = g.newLabel("case")
		if cl.List == nil {
			defLabel = labels[i]
			continue
		}
		var conds []string
		for _, e := range cl.List {
			if tag == "" {
				conds = append(conds, g.expr(e))
				continue
			}
			if info := g.info.Ops[e]; info != nil {
				conds = append(conds, fmt.Sprintf("%s(%s, %s)", info.Func.CName, tag, g.expr(e)))
				continue
			}
			conds = append(conds, g.compare(token.EQL, tagType, tag, g.expr(e)))
		}
		g.line("if (%s) goto %s;", join(conds, " || "), labels[i])
	}
	g.line("goto %s;", defLabel)
	g.loops = append(g.loops, loopLabels{brk: brk, finDepth: len(g.finStack)})
	for i, cl := range s.Body {
		g.line("%s: ;", labels[i])
		g.line("{")
		g.ind++
		body := cl.Body
		fall := false
		if n := len(body); n > 0 {
			if b, ok := body[n-1].(*ast.BranchStmt); ok && b.Tok == token.FALLTHROUGH {
				fall = true
				body = body[:n-1]
			}
		}
		g.stmts(body)
		if fall {
			g.line("goto %s;", labels[i+1])
		} else {
			g.line("goto %s;", brk)
		}
		g.ind--
		g.line("}")
	}
	g.loops = g.loops[:len(g.loops)-1]
	g.ind--
	g.line("}")
	g.line("%s: ;", brk)
}

func (g *gen) returnStmt(s *ast.ReturnStmt) {
	sig := g.sig
	var vals []string
	switch {
	case len(sig.Results) == 0:
	case len(s.Results) == 0: // bare return with named results
		for _, r := range sig.Results {
			vals = append(vals, g.varRef(r))
		}
	case len(s.Results) == 1 && len(sig.Results) > 1: // return f() with a tuple result
		t := g.newTmp()
		g.line("%s %s = %s;", g.resultType(sig), t, g.expr(s.Results[0]))
		for i := range sig.Results {
			vals = append(vals, fmt.Sprintf("%s.r%d", t, i))
		}
	default:
		for _, r := range s.Results {
			vals = append(vals, g.expr(r))
		}
	}
	g.emitReturn(vals)
}

func join(xs []string, sep string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += sep
		}
		s += x
	}
	return s
}
