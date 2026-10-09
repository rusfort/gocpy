package c

import (
	"fmt"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

// commaOk lowers `v, ok := <-ch`: it emits the receive and returns C
// expressions for the value and the ok flag.
func (g *gen) commaOk(rhs []ast.Expr) (val, ok string, isRecv bool) {
	if len(rhs) != 1 {
		return "", "", false
	}
	if g.info.CommaOk[unparen(rhs[0])] {
		if v, k, ok := g.commaOkExt(rhs[0]); ok {
			return v, k, true
		}
	}
	u, isU := unparen(rhs[0]).(*ast.UnaryExpr)
	if !isU || !g.info.CommaOk[u] {
		return "", "", false
	}
	v, k := g.newTmp(), g.newTmp()
	g.line("%s %s;", g.cType(g.typeOf(u)), v)
	g.line("bool %s = gocpy_chan_recv(%s, &%s);", k, g.expr(u.X), v)
	return v, k, true
}

func (g *gen) sendStmt(s *ast.SendStmt) {
	ch := g.typeOf(s.Chan).Underlying().(*types.Chan)
	v := g.newTmp()
	g.line("{ %s %s = %s; gocpy_chan_send(%s, &%s); }", g.cType(ch.Elem), v, g.expr(s.Value), g.expr(s.Chan), v)
}

// goStmt: arguments are evaluated in the current goroutine, stored in a
// heap block and passed to a generated thunk that performs the call.
func (g *gen) goStmt(s *ast.GoStmt) {
	thunk, p := g.thunkCall(s.Call)
	g.line("gocpy_go(%s, %s);", thunk, p)
	g.line("}")
}

func (g *gen) selectStmt(s *ast.SelectStmt) {
	brk := g.newLabel("brk")
	g.line("{")
	g.ind++
	n := 0
	hasDefault := false
	for _, cl := range s.Body {
		if cl.Comm == nil {
			hasDefault = true
		} else {
			n++
		}
	}
	cs := g.newTmp()
	if n > 0 {
		g.line("gocpy_selcase %s[%d];", cs, n)
	}
	// evaluate channels and send values in source order
	type recvInfo struct {
		buf string
		u   *ast.UnaryExpr
	}
	idx := map[*ast.CommClause]int{}
	recv := map[*ast.CommClause]recvInfo{}
	i := 0
	for _, cl := range s.Body {
		if cl.Comm == nil {
			continue
		}
		idx[cl] = i
		switch comm := cl.Comm.(type) {
		case *ast.SendStmt:
			et := g.typeOf(comm.Chan).Underlying().(*types.Chan).Elem
			v := g.newTmp()
			g.line("%s %s = %s;", g.cType(et), v, g.expr(comm.Value))
			g.line("%s[%d] = (gocpy_selcase){ %s, true, &%s, false };", cs, i, g.expr(comm.Chan), v)
		default:
			u := selRecv(cl.Comm)
			et := g.typeOf(u)
			v := g.newTmp()
			g.line("%s %s;", g.cType(et), v)
			g.line("%s[%d] = (gocpy_selcase){ %s, false, &%s, false };", cs, i, g.expr(u.X), v)
			recv[cl] = recvInfo{v, u}
		}
		i++
	}
	arr := "NULL"
	if n > 0 {
		arr = cs
	}
	chosen := g.newTmp()
	g.line("int %s = gocpy_select(%s, %d, %v);", chosen, arr, n, hasDefault)
	g.loops = append(g.loops, loopLabels{brk: brk, finDepth: len(g.finStack)})
	for _, cl := range s.Body {
		if cl.Comm == nil {
			g.line("if (%s == -1) {", chosen)
		} else {
			g.line("if (%s == %d) {", chosen, idx[cl])
		}
		g.ind++
		if r, ok := recv[cl]; ok {
			if as, isAssign := cl.Comm.(*ast.AssignStmt); isAssign {
				srcs := []string{r.buf, fmt.Sprintf("%s[%d].ok", cs, idx[cl])}
				for k, l := range as.Lhs {
					id, _ := l.(*ast.Ident)
					if as.Tok == token.DEFINE {
						if v, _ := g.info.Defs[id].(*types.Var); v != nil {
							g.declLocal(v, srcs[k])
						}
					} else if id == nil || id.Name != "_" {
						g.line("%s = %s;", g.expr(l), srcs[k])
					}
				}
			}
		}
		g.stmts(cl.Body)
		g.line("goto %s;", brk)
		g.ind--
		g.line("}")
	}
	g.loops = g.loops[:len(g.loops)-1]
	g.ind--
	g.line("}")
	g.line("%s: ;", brk)
}

func selRecv(s ast.Stmt) *ast.UnaryExpr {
	switch s := s.(type) {
	case *ast.ExprStmt:
		return unparen(s.X).(*ast.UnaryExpr)
	case *ast.AssignStmt:
		return unparen(s.Rhs[0]).(*ast.UnaryExpr)
	}
	return nil
}
