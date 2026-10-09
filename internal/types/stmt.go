package types

import (
	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

func (c *checker) openScope(kind string) {
	parent := c.scope
	if parent == nil {
		parent = c.fileScope()
	}
	c.scope = NewScope(parent, kind)
}

func (c *checker) closeScope() {
	c.scope = c.scope.Parent
	if c.scope == c.fileScope() {
		c.scope = nil
	}
}

func (c *checker) funcBody(fn *Func) {
	sig := fn.Sig()
	c.scope = fn.TScope // nil for non-instances (file scope)
	c.openScope("func")
	defer func() { c.scope = nil }()
	c.sig = sig
	c.fn = fn
	c.ctx = &funcCtx{}
	defer func() { c.ctx = nil }()
	c.loopDepth, c.breakOK = 0, 0
	c.protected, c.inFinally, c.throwSites, c.locals = 0, 0, 0, nil
	defer func() { c.fn, c.sig = nil, nil }()

	declareVars := func(fl *ast.FieldList, vars []*Var) {
		if fl == nil {
			return
		}
		c.inParams = true
		defer func() { c.inParams = false }()
		i := 0
		for _, f := range fl.List {
			if len(f.Names) == 0 {
				i++
				continue
			}
			for _, n := range f.Names {
				if n.Name != "_" {
					c.declare(c.scope, n, vars[i])
				}
				i++
			}
		}
	}
	if fn.Decl.Recv != nil && sig.Recv != nil {
		declareVars(fn.Decl.Recv, []*Var{sig.Recv})
	}
	declareVars(fn.Decl.Type.Params, sig.Params)
	declareVars(fn.Decl.Type.Results, sig.Results)

	c.stmtList(fn.Decl.Body.List)
	for _, v := range c.locals {
		if !v.used && !c.hasDirective(v.pos, "ignore:unused") {
			c.errorf(v.pos, "%s declared and not used (use _ or add //gocpy:ignore:unused)", v.name)
		}
	}
	if sig.Throws && c.throwSites == 0 && fn.Decl.Body != nil {
		c.errorf(fn.pos, "%s is marked @throws but never throws; remove @throws", fn.name)
	}
	if len(sig.Results) > 0 && !isTerminatingList(fn.Decl.Body.List) {
		c.errorf(fn.Decl.Body.Pos(), "missing return at end of %s", fn.name)
	}
}

func (c *checker) stmtList(list []ast.Stmt) {
	for i, s := range list {
		if b, ok := s.(*ast.BranchStmt); ok && b.Tok == token.FALLTHROUGH {
			c.errorf(b.Pos(), "fallthrough statement out of place")
			continue
		}
		_ = i
		c.stmt(s)
	}
}

func (c *checker) block(b *ast.BlockStmt) {
	c.openScope("block")
	c.stmtList(b.List)
	c.closeScope()
}

func (c *checker) stmt(s ast.Stmt) {
	switch s := s.(type) {
	case *ast.EmptyStmt, *ast.BadStmt:
	case *ast.ExprStmt:
		inner := unparen(s.X)
		if p, isProp := inner.(*ast.PropagateExpr); isProp {
			inner = unparen(p.X)
		}
		call, ok := inner.(*ast.CallExpr)
		x := c.expr(s.X)
		if !ok {
			if !isInvalid(x) {
				c.errorf(s.Pos(), "%s is not used", exprString(s.X))
			}
			return
		}
		_ = call
	case *ast.DeclStmt:
		d := s.Decl.(*ast.GenDecl)
		if d.Tok == token.TYPE {
			c.errorf(d.Pos(), "local type declarations are not supported yet; declare the type at package level")
			return
		}
		for _, sp := range d.Specs {
			c.valueSpec(c.scope, sp.(*ast.ValueSpec), d.Tok == token.CONST, false)
		}
	case *ast.AssignStmt:
		switch s.Tok {
		case token.DEFINE:
			c.define(s)
		case token.ASSIGN:
			c.assign(s)
		default:
			c.opAssign(s)
		}
	case *ast.IncDecStmt:
		x := c.lhsValue(s.X)
		if isInvalid(x) {
			return
		}
		if x.mode != Variable && x.mode != MapIndexM {
			c.errorf(s.Pos(), "cannot assign to %s", exprString(s.X))
		} else if _, p := isPtr(x.typ); p {
			if c.requireUnsafe(s.Pos(), "pointer arithmetic") {
				c.info.PtrOps[s] = PtrAdd
			}
		} else if b := basic(x.typ); b == nil || !b.IsNumeric() {
			c.errorf(s.Pos(), "invalid operation: %s%s (non-numeric type %s)", exprString(s.X), s.Tok, x.typ)
		}
	case *ast.BlockStmt:
		c.block(s)
	case *ast.IfStmt:
		c.openScope("block")
		if s.Init != nil {
			c.stmt(s.Init)
		}
		c.cond(s.Cond, "if")
		c.block(s.Body)
		if s.Else != nil {
			c.stmt(s.Else)
		}
		c.closeScope()
	case *ast.ForStmt:
		c.openScope("block")
		if s.Init != nil {
			c.stmt(s.Init)
		}
		if s.Cond != nil {
			c.cond(s.Cond, "for")
		}
		if s.Post != nil {
			if a, ok := s.Post.(*ast.AssignStmt); ok && a.Tok == token.DEFINE {
				c.errorf(a.Pos(), "cannot declare in post statement of for loop")
			} else {
				c.stmt(s.Post)
			}
		}
		c.loopDepth++
		c.block(s.Body)
		c.loopDepth--
		c.closeScope()
	case *ast.ForInStmt:
		it := c.value(s.Iter)
		if isInvalid(it) {
			return
		}
		key, val, ok := c.iterTypes(it, s.Iter.Pos())
		if !ok {
			return
		}
		if m, ok := it.typ.Underlying().(*Map); ok && s.Key == nil {
			val = m.Key
			c.info.MapKeys[s] = true
		}
		c.openScope("block")
		c.declareIterVars(s, s.Key, s.Value, key, val)
		c.loopDepth++
		c.block(s.Body)
		c.loopDepth--
		c.closeScope()
	case *ast.SwitchStmt:
		c.switchStmt(s)
	case *ast.ReturnStmt:
		c.returnStmt(s)
	case *ast.BranchStmt:
		if c.inFinally > 0 && ((s.Tok == token.BREAK && c.loopDepth == c.finallyLoopDepth() && c.breakOK == c.finallyBreakOK()) ||
			(s.Tok == token.CONTINUE && c.loopDepth == c.finallyLoopDepth())) {
			c.errorf(s.Pos(), "%s cannot leave a finally block", s.Tok)
		}
		if s.Label != nil {
			c.errorf(s.Pos(), "labels are not supported yet")
			return
		}
		switch s.Tok {
		case token.BREAK:
			if c.loopDepth == 0 && c.breakOK == 0 {
				c.errorf(s.Pos(), "break is not in a loop or switch")
			}
		case token.CONTINUE:
			if c.loopDepth == 0 {
				c.errorf(s.Pos(), "continue is not in a loop")
			}
		}
	case *ast.GoStmt:
		c.goStmt(s)
	case *ast.DeferStmt:
		c.deferStmt(s)
	case *ast.SelectStmt:
		c.selectStmt(s)
	case *ast.SendStmt:
		c.sendStmt(s)
	case *ast.TryStmt:
		c.tryStmt(s)
	case *ast.ThrowStmt:
		c.throwStmt(s)
	case *ast.UnsafeStmt:
		c.unsafeBlock(s)
	default:
		c.errorf(s.Pos(), "unexpected statement %T", s)
	}
}

func (c *checker) cond(e ast.Expr, what string) {
	x := c.value(e)
	if isInvalid(x) {
		return
	}
	if !IsBool(x.typ) {
		c.errorf(e.Pos(), "non-boolean condition in %s statement", what)
		return
	}
	c.defaultType(x)
	c.record(x)
}

// rhsValues evaluates the right-hand side, expanding a single multi-value call.
func (c *checker) rhsValues(lhsN int, rhs []ast.Expr, pos token.Pos) []*operand {
	if u, ok := unparen(rhsOne(rhs)).(*ast.UnaryExpr); ok && lhsN == 2 && u.Op == token.ARROW {
		x := c.value(rhs[0])
		if isInvalid(x) {
			return nil
		}
		c.record(x)
		c.info.CommaOk[u] = true
		return []*operand{{mode: Value, typ: x.typ}, {mode: Value, typ: Typ[Bool]}}
	}
	if len(rhs) == 1 && lhsN == 2 {
		switch r := unparen(rhs[0]).(type) {
		case *ast.IndexExpr, *ast.TypeAssertExpr:
			x := c.value(rhs[0])
			if isInvalid(x) {
				return nil
			}
			_, isTA := r.(*ast.TypeAssertExpr)
			if x.mode != MapIndexM && !isTA {
				c.errorf(pos, "assignment mismatch: 2 variables but 1 value")
				return nil
			}
			c.record(x)
			c.info.CommaOk[r] = true
			return []*operand{{mode: Value, typ: x.typ}, {mode: Value, typ: Typ[Bool]}}
		}
	}
	if len(rhs) == 1 && lhsN > 1 {
		x := c.expr(rhs[0])
		if isInvalid(x) {
			return nil
		}
		tup, ok := x.typ.(*Tuple)
		if !ok || len(tup.Vars) != lhsN {
			n := 1
			if ok {
				n = len(tup.Vars)
			}
			c.errorf(pos, "assignment mismatch: %d variables but %s returns %d values", lhsN, exprString(rhs[0]), n)
			return nil
		}
		var out []*operand
		for _, v := range tup.Vars {
			out = append(out, &operand{mode: Value, typ: v.typ, expr: nil})
		}
		return out
	}
	if len(rhs) != lhsN {
		c.errorf(pos, "assignment mismatch: %d variables but %d values", lhsN, len(rhs))
		return nil
	}
	var out []*operand
	for _, r := range rhs {
		out = append(out, c.value(r))
	}
	return out
}

// define handles `a, b := ...`.
// Gocpy rule: every name on the left must be new in the current scope.
func (c *checker) define(s *ast.AssignStmt) {
	vals := c.rhsValues(len(s.Lhs), s.Rhs, s.Pos())
	newVars := 0
	for i, l := range s.Lhs {
		id, ok := l.(*ast.Ident)
		if !ok {
			c.errorf(l.Pos(), "non-name %s on left side of :=", exprString(l))
			continue
		}
		var t Type = Typ[Invalid]
		if vals != nil && i < len(vals) && !isInvalid(vals[i]) {
			x := vals[i]
			if x.expr != nil {
				c.defaultType(x)
				c.record(x)
			}
			t = x.typ
		}
		if id.Name == "_" {
			c.info.Defs[id] = nil
			continue
		}
		newVars++
		v := NewVar(id.Name, t, id.Pos())
		c.declare(c.scope, id, v)
	}
	if newVars == 0 {
		c.errorf(s.Pos(), "no new variables on left side of :=")
	}
}

func (c *checker) assign(s *ast.AssignStmt) {
	var lhs []*operand
	for _, l := range s.Lhs {
		if id, ok := l.(*ast.Ident); ok && id.Name == "_" {
			lhs = append(lhs, nil)
			continue
		}
		x := c.lhsValue(l)
		if !isInvalid(x) && x.mode != Variable && x.mode != MapIndexM {
			c.errorf(l.Pos(), "cannot assign to %s (neither addressable nor a map index)", exprString(l))
		}
		lhs = append(lhs, x)
	}
	vals := c.rhsValues(len(s.Lhs), s.Rhs, s.Pos())
	if vals == nil {
		return
	}
	for i, x := range vals {
		if isInvalid(x) {
			continue
		}
		if lhs[i] == nil {
			if x.expr != nil {
				c.defaultType(x)
				c.record(x)
			}
			continue
		}
		if isInvalid(lhs[i]) {
			continue
		}
		if x.expr == nil {
			if needsConv(x.typ, lhs[i].typ) {
				c.errorf(s.Pos(), "cannot assign %s to %s (type %s) from a multi-value expression: implicit interface conversion; use a temporary variable", x.typ, exprString(s.Lhs[i]), lhs[i].typ)
				continue
			}
			if !assignableTo(x.typ, lhs[i].typ) {
				c.errorf(s.Pos(), "cannot assign %s to %s (type %s)", x.typ, exprString(s.Lhs[i]), lhs[i].typ)
			}
			continue
		}
		c.assignable(x, lhs[i].typ, "assignment")
		c.record(x)
	}
}

var assignOpBase = map[token.Kind]token.Kind{
	token.ADD_ASSIGN: token.ADD, token.SUB_ASSIGN: token.SUB, token.MUL_ASSIGN: token.MUL,
	token.QUO_ASSIGN: token.QUO, token.REM_ASSIGN: token.REM, token.AND_ASSIGN: token.AND,
	token.OR_ASSIGN: token.OR, token.XOR_ASSIGN: token.XOR, token.SHL_ASSIGN: token.SHL,
	token.SHR_ASSIGN: token.SHR, token.AND_NOT_ASSIGN: token.AND_NOT,
}

// AssignOp returns the binary operator of an op-assignment token.
func AssignOp(k token.Kind) token.Kind { return assignOpBase[k] }

func (c *checker) opAssign(s *ast.AssignStmt) {
	if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
		c.errorf(s.Pos(), "assignment operator %s requires single-valued expressions", s.Tok)
		return
	}
	x := c.lhsValue(s.Lhs[0])
	y := c.value(s.Rhs[0])
	if isInvalid(x) || isInvalid(y) {
		return
	}
	if x.mode != Variable && x.mode != MapIndexM {
		c.errorf(s.Pos(), "cannot assign to %s", exprString(s.Lhs[0]))
		return
	}
	lt := x.typ
	r, info := c.binaryOp(s, assignOpBase[s.Tok], x, y)
	c.record(y)
	if isInvalid(r) {
		return
	}
	if info != nil {
		if c.info.AssignOps == nil {
			c.info.AssignOps = map[*ast.AssignStmt]*OpInfo{}
		}
		c.info.AssignOps[s] = info
	}
	if !assignableTo(r.typ, lt) && !(isUntyped(r.typ) && c.canConvertUntyped(r, lt)) {
		c.errorf(s.Pos(), "cannot assign %s to %s (type %s)", r.typ, exprString(s.Lhs[0]), lt)
	}
}

func (c *checker) returnStmt(s *ast.ReturnStmt) {
	if c.inFinally > 0 {
		c.errorf(s.Pos(), "return cannot leave a finally block")
	}
	results := c.sig.Results
	if len(s.Results) == 0 {
		if len(results) > 0 && results[0].name == "" {
			c.errorf(s.Pos(), "not enough return values (want %d)", len(results))
		}
		return
	}
	if len(results) == 0 {
		c.errorf(s.Pos(), "too many return values")
		for _, r := range s.Results {
			c.expr(r)
		}
		return
	}
	vals := c.rhsValues(len(results), s.Results, s.Pos())
	for i, x := range vals {
		if isInvalid(x) {
			continue
		}
		if x.expr == nil {
			if needsConv(x.typ, results[i].typ) {
				c.errorf(s.Pos(), "cannot return %s as %s from a multi-value expression: implicit interface conversion; use temporary variables", x.typ, results[i].typ)
				continue
			}
			if !assignableTo(x.typ, results[i].typ) {
				c.errorf(s.Pos(), "cannot use %s as %s in return statement", x.typ, results[i].typ)
			}
			continue
		}
		c.assignable(x, results[i].typ, "return statement")
		c.record(x)
	}
}

func (c *checker) switchStmt(s *ast.SwitchStmt) {
	c.openScope("block")
	defer c.closeScope()
	if s.Init != nil {
		c.stmt(s.Init)
	}
	var tag *operand
	if s.Tag != nil {
		if as, ok := s.Tag.(*ast.AssignStmt); ok {
			ta, isTA := unparen(rhsOne(as.Rhs)).(*ast.TypeAssertExpr)
			id, isId := as.Lhs[0].(*ast.Ident)
			if !isTA || ta.Type != nil || !isId || len(as.Lhs) != 1 || as.Tok != token.DEFINE {
				c.errorf(s.Tag.Pos(), "invalid switch header; expected v := x.(type)")
				return
			}
			c.typeSwitch(s, id, ta)
			return
		}
		es, ok := s.Tag.(*ast.ExprStmt)
		if !ok {
			c.errorf(s.Tag.Pos(), "invalid switch header")
			return
		}
		if ta, ok := unparen(es.X).(*ast.TypeAssertExpr); ok && ta.Type == nil {
			c.typeSwitch(s, nil, ta)
			return
		}
		tag = c.value(es.X)
		if isInvalid(tag) {
			return
		}
		c.defaultType(tag)
		c.record(tag)
	}
	hasDefault := false
	for ci, cl := range s.Body {
		if cl.List == nil {
			if hasDefault {
				c.errorf(cl.Pos(), "multiple defaults in switch")
			}
			hasDefault = true
		}
		for _, e := range cl.List {
			x := c.value(e)
			if isInvalid(x) {
				continue
			}
			if tag == nil {
				if !IsBool(x.typ) {
					c.errorf(e.Pos(), "invalid case %s in switch (mismatched types %s and bool)", exprString(e), x.typ)
				}
				c.defaultType(x)
				c.record(x)
				continue
			}
			t := &operand{mode: tag.mode, typ: tag.typ, val: tag.val, expr: tag.expr}
			r, info := c.binaryOp(e, token.EQL, t, x)
			c.record(x)
			if info != nil {
				c.info.Ops[e] = info
			}
			_ = r
		}
		c.openScope("block")
		c.breakOK++
		body := cl.Body
		if n := len(body); n > 0 {
			if b, ok := body[n-1].(*ast.BranchStmt); ok && b.Tok == token.FALLTHROUGH {
				if ci == len(s.Body)-1 {
					c.errorf(b.Pos(), "cannot fallthrough final case in switch")
				}
				body = body[:n-1]
			}
		}
		c.stmtList(body)
		c.breakOK--
		c.closeScope()
	}
}

// ---------------------------------------------------------------- termination

func isTerminatingList(list []ast.Stmt) bool {
	for i := len(list) - 1; i >= 0; i-- {
		if _, ok := list[i].(*ast.EmptyStmt); ok {
			continue
		}
		return isTerminating(list[i])
	}
	return false
}

func isTerminating(s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.ReturnStmt, *ast.ThrowStmt:
		return true
	case *ast.UnsafeStmt:
		return isTerminatingList(s.Body.List)
	case *ast.BlockStmt:
		return isTerminatingList(s.List)
	case *ast.TryStmt:
		// unmatched exceptions leave the function, so only handled paths matter
		if !isTerminatingList(s.Body.List) {
			return false
		}
		for _, cc := range s.Catches {
			if !isTerminatingList(cc.Body.List) {
				return false
			}
		}
		return true
	case *ast.IfStmt:
		return s.Else != nil && isTerminatingList(s.Body.List) && isTerminating(s.Else)
	case *ast.ForStmt:
		return s.Cond == nil && !hasBreak(s.Body)
	case *ast.SwitchStmt:
		hasDefault := false
		for _, cl := range s.Body {
			if cl.List == nil {
				hasDefault = true
			}
			n := len(cl.Body)
			if n > 0 {
				if b, ok := cl.Body[n-1].(*ast.BranchStmt); ok && b.Tok == token.FALLTHROUGH {
					continue
				}
			}
			if !isTerminatingList(cl.Body) || hasBreakList(cl.Body) {
				return false
			}
		}
		return hasDefault
	}
	return false
}

func hasBreakList(list []ast.Stmt) bool {
	for _, s := range list {
		if hasBreakStmt(s) {
			return true
		}
	}
	return false
}

func hasBreak(b *ast.BlockStmt) bool { return hasBreakList(b.List) }

// hasBreakStmt reports an unlabeled break that targets the enclosing statement.
func hasBreakStmt(s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.BranchStmt:
		return s.Tok == token.BREAK
	case *ast.BlockStmt:
		return hasBreakList(s.List)
	case *ast.IfStmt:
		if hasBreak(s.Body) {
			return true
		}
		if s.Else != nil {
			return hasBreakStmt(s.Else)
		}
	}
	return false
}

func rhsOne(rhs []ast.Expr) ast.Expr {
	if len(rhs) == 1 {
		return rhs[0]
	}
	return nil
}
