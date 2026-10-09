package types

import (
	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// lhsValue checks an assignment target; a plain variable being assigned
// does not count as a use (Go's "declared and not used" rule).
func (c *checker) lhsValue(e ast.Expr) *operand {
	if id, ok := unparen(e).(*ast.Ident); ok {
		if v, ok := c.lookup(id.Name).(*Var); ok {
			was := v.used
			x := c.value(e)
			v.used = was
			return x
		}
	}
	return c.value(e)
}

// throwSite registers a place where an exception may leave the current statement.
func (c *checker) throwSite(pos token.Pos, what string) {
	if c.fn == nil {
		c.errorf(pos, "%s in a package-level initializer; exceptions cannot escape initialization", what)
		return
	}
	if c.protected > 0 {
		return
	}
	c.throwSites++
	if !c.sig.Throws {
		c.errorf(pos, "%s may escape %s; add @throws or wrap in try/catch with a catch-all", what, c.fn.name)
	}
}

// Finally blocks may contain loops; break/continue are only rejected when
// they would jump out of the finally block itself.
func (c *checker) finallyLoopDepth() int { return c.finStack[len(c.finStack)-1][0] }
func (c *checker) finallyBreakOK() int   { return c.finStack[len(c.finStack)-1][1] }

func (c *checker) throwStmt(s *ast.ThrowStmt) {
	x := c.value(s.X)
	if isInvalid(x) {
		return
	}
	n, ok := x.typ.(*Named)
	if !ok {
		c.errorf(s.X.Pos(), "throw requires a value of a named type (e.g. type ParseError struct{...}), found %s", x.typ)
		return
	}
	if _, isPtr := n.Underlying().(*Pointer); isPtr && n != ExceptionType {
		c.errorf(s.X.Pos(), "cannot throw %s: named pointer types are not allowed", n)
		return
	}
	c.record(x)
	c.info.Throws[s] = n
	c.throwSite(s.Pos(), "exception")
}

func (c *checker) tryStmt(s *ast.TryStmt) {
	if len(s.Catches) == 0 && s.Finally == nil {
		c.errorf(s.Pos(), "try requires at least one catch or a finally block")
	}
	catchAll := false
	seen := map[Type]bool{}
	var infos []*CatchInfo
	for i, cc := range s.Catches {
		info := &CatchInfo{}
		if cc.Type != nil {
			if id, ok := cc.Type.(*ast.Ident); ok && id.Name == "Exception" && c.lookup("Exception") == ExceptionType.Obj {
				cc.Type = nil // catch e: Exception is the same as catch e
			}
		}
		if cc.Type == nil {
			if i != len(s.Catches)-1 {
				c.errorf(cc.Pos(), "catch-all must be the last catch clause")
			}
			catchAll = true
		} else {
			t := c.typExpr(cc.Type)
			if _, ok := t.(*Named); !ok && t != Typ[Invalid] {
				c.errorf(cc.Type.Pos(), "catch type must be a named type, found %s", t)
			} else if seen[t] {
				c.errorf(cc.Type.Pos(), "duplicate catch of %s", t)
			}
			seen[t] = true
			info.Type = t
		}
		infos = append(infos, info)
		c.info.Catches[cc] = info
	}
	if catchAll {
		c.protected++
	}
	c.block(s.Body)
	if catchAll {
		c.protected--
	}
	for i, cc := range s.Catches {
		c.openScope("block")
		if cc.Name != nil && cc.Name.Name != "_" {
			t := infos[i].Type
			if t == nil {
				t = ExceptionType
			}
			v := NewVar(cc.Name.Name, t, cc.Name.Pos())
			c.declare(c.scope, cc.Name, v)
			infos[i].Var = v
		}
		c.stmtList(cc.Body.List)
		c.closeScope()
	}
	if s.Finally != nil {
		c.inFinally++
		c.finStack = append(c.finStack, [2]int{c.loopDepth, c.breakOK})
		c.block(s.Finally)
		c.finStack = c.finStack[:len(c.finStack)-1]
		c.inFinally--
	}
}
