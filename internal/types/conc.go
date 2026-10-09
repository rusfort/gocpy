package types

import (
	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

func (c *checker) goStmt(s *ast.GoStmt) {
	x := c.expr(s.Call)
	_ = x
	ci := c.info.Calls[s.Call]
	if ci == nil {
		return
	}
	switch ci.Kind {
	case CallFunc, CallMethod, CallValue, CallIface:
	default:
		c.errorf(s.Pos(), "go requires a function or method call")
		return
	}
	if ci.Throws {
		c.errorf(s.Pos(), "go %s: a goroutine entry cannot be @throws; catch exceptions inside the goroutine", exprString(s.Call.Fun))
	}
}

func (c *checker) sendStmt(s *ast.SendStmt) {
	ch := c.value(s.Chan)
	v := c.value(s.Value)
	if isInvalid(ch) || isInvalid(v) {
		return
	}
	ct, ok := ch.typ.Underlying().(*Chan)
	if !ok {
		c.errorf(s.Pos(), "invalid operation: cannot send to non-channel %s (type %s)", exprString(s.Chan), ch.typ)
		return
	}
	c.record(ch)
	if c.assignable(v, ct.Elem, "send") {
		c.record(v)
	}
}

// recvExpr returns the channel operand of a receive expression or nil.
func recvOf(e ast.Expr) *ast.UnaryExpr {
	if u, ok := unparen(e).(*ast.UnaryExpr); ok && u.Op == token.ARROW {
		return u
	}
	return nil
}

func (c *checker) selectStmt(s *ast.SelectStmt) {
	hasDefault := false
	for _, cl := range s.Body {
		c.openScope("block")
		switch comm := cl.Comm.(type) {
		case nil:
			if hasDefault {
				c.errorf(cl.Pos(), "multiple defaults in select")
			}
			hasDefault = true
		case *ast.SendStmt:
			c.sendStmt(comm)
		case *ast.ExprStmt:
			if recvOf(comm.X) == nil {
				c.errorf(comm.Pos(), "select case must be a send or receive")
				break
			}
			x := c.value(comm.X)
			if !isInvalid(x) {
				c.record(x)
			}
		case *ast.AssignStmt:
			if len(comm.Rhs) != 1 || recvOf(comm.Rhs[0]) == nil || len(comm.Lhs) > 2 || (comm.Tok != token.DEFINE && comm.Tok != token.ASSIGN) {
				c.errorf(comm.Pos(), "select case must be a send or receive")
				break
			}
			if comm.Tok == token.DEFINE {
				c.define(comm)
			} else {
				c.assign(comm)
			}
		default:
			c.errorf(cl.Pos(), "select case must be a send or receive")
		}
		c.breakOK++
		c.stmtList(cl.Body)
		c.breakOK--
		c.closeScope()
	}
}
