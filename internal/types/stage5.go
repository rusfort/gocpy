package types

import (
	"sort"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// Stage 5: closures, function values, defer, maps, interfaces, error and `?`.

// funcCtx identifies the function (declaration or literal) being checked.
type funcCtx struct {
	parent   *funcCtx
	lit      *ast.FuncLit
	captures []*Var
	capSet   map[*Var]bool
}

// TypeSwitch describes `switch v := x.(type)`.
type TypeSwitch struct {
	X    ast.Expr                 // the interface operand
	Vars map[*ast.CaseClause]*Var // per-clause binding (nil without binding)
}

// Propagate describes x? : N is the number of values of x (the last is error).
type Propagate struct{ N int }

// capture records that v (declared in an enclosing function) is used by the
// current function literal and by every literal in between.
func (c *checker) capture(v *Var) {
	for ctx := c.ctx; ctx != nil && ctx != v.owner; ctx = ctx.parent {
		if ctx.lit == nil {
			break
		}
		if !ctx.capSet[v] {
			ctx.capSet[v] = true
			ctx.captures = append(ctx.captures, v)
		}
	}
	v.Boxed = true // shared between the frame and the closure
}

func (c *checker) declareFields(fl *ast.FieldList, vars []*Var) {
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

func (c *checker) funcLit(e *ast.FuncLit) *operand {
	sig := c.signature(e.Type, nil)
	sig.Throws = e.Type.Annots.Has("throws")
	for _, a := range e.Type.Annots {
		if a.Name != "throws" {
			c.errorf(a.Pos(), "annotation @%s is not allowed on a function literal", a.Name)
		}
	}
	// save the enclosing function context
	sSig, sFn, sLoop, sBreak := c.sig, c.fn, c.loopDepth, c.breakOK
	sProt, sFin, sSites, sStack, sCtx := c.protected, c.inFinally, c.throwSites, c.finStack, c.ctx
	ctx := &funcCtx{parent: c.ctx, lit: e, capSet: map[*Var]bool{}}
	c.ctx = ctx
	c.sig = sig
	c.fn = &Func{object: object{name: "function literal", pos: e.Pos(), typ: sig}}
	c.loopDepth, c.breakOK, c.protected, c.inFinally, c.throwSites, c.finStack = 0, 0, 0, 0, 0, nil
	start := len(c.locals)

	c.openScope("func")
	c.declareFields(e.Type.Params, sig.Params)
	c.declareFields(e.Type.Results, sig.Results)
	c.stmtList(e.Body.List)
	c.closeScope()

	for _, v := range c.locals[start:] {
		if !v.used && !c.hasDirective(v.pos, "ignore:unused") {
			c.errorf(v.pos, "%s declared and not used (use _ or add //gocpy:ignore:unused)", v.name)
		}
	}
	c.locals = c.locals[:start]
	if sig.Throws && c.throwSites == 0 {
		c.errorf(e.Pos(), "function literal is marked @throws but never throws; remove @throws")
	}
	if len(sig.Results) > 0 && !isTerminatingList(e.Body.List) {
		c.errorf(e.Body.Pos(), "missing return at end of function literal")
	}
	c.sig, c.fn, c.loopDepth, c.breakOK = sSig, sFn, sLoop, sBreak
	c.protected, c.inFinally, c.throwSites, c.finStack, c.ctx = sProt, sFin, sSites, sStack, sCtx
	c.info.Captures[e] = ctx.captures
	return &operand{mode: Value, typ: sig}
}

// funcValue turns a function name used as a value into a function value.
func (c *checker) funcValue(x *operand, e ast.Expr) *operand {
	if x.sel != nil {
		return c.methodValue(x, e)
	}
	if _, ok := x.obj.(*GenericFunc); ok {
		c.errorf(e.Pos(), "cannot use generic function %s without instantiation", exprString(e))
		return c.invalid(e)
	}
	fn, ok := x.obj.(*Func)
	if !ok {
		c.errorf(e.Pos(), "overloaded function %s cannot be used as a value", exprString(e))
		return c.invalid(e)
	}
	if fn.Decl == nil && fn.CName != "" {
		c.errorf(e.Pos(), "std function %s cannot be used as a value; wrap it in a closure", exprString(e))
		return c.invalid(e)
	}
	c.info.FuncRefs[e] = fn
	r := &operand{mode: Value, typ: fn.typ, expr: e}
	c.record(r)
	return r
}

// ---------------------------------------------------------------- interfaces

func (c *checker) interfaceType(e *ast.InterfaceType) Type {
	it := &Interface{}
	var termSets []bool
	seen := map[string]*IMethod{}
	add := func(m *IMethod, pos token.Pos) {
		if prev := seen[m.Name]; prev != nil {
			if !Identical(prev.Sig, m.Sig) {
				c.errorf(pos, "duplicate method %s with a different signature", m.Name)
			}
			return
		}
		seen[m.Name] = m
		it.Methods = append(it.Methods, m)
	}
	if e.Methods != nil {
		for _, f := range e.Methods.List {
			if len(f.Names) == 0 {
				switch x := f.Type.(type) {
				case *ast.BinaryExpr, *ast.UnaryExpr:
					if (isBin(x) && x.(*ast.BinaryExpr).Op == token.OR) || (!isBin(x) && x.(*ast.UnaryExpr).Op == token.TILDE) {
						it.Terms = intersectTerms(it.Terms, c.terms(f.Type), len(termSets) > 0)
						termSets = append(termSets, true)
						continue
					}
				}
				old := c.inConstraint
				c.inConstraint = true
				t := c.typExpr(f.Type)
				c.inConstraint = old
				if n, ok := t.(*Named); ok && n.underlying == nil {
					c.resolveNamed(n)
				}
				ei, ok := t.Underlying().(*Interface)
				if !ok {
					if t != Typ[Invalid] {
						it.Terms = intersectTerms(it.Terms, []Term{{Type: t}}, len(termSets) > 0)
						termSets = append(termSets, true)
					}
					continue
				}
				for _, m := range ei.Methods {
					add(m, f.Type.Pos())
				}
				it.Comparable = it.Comparable || ei.Comparable
				if len(ei.Terms) > 0 {
					it.Terms = intersectTerms(it.Terms, ei.Terms, len(termSets) > 0)
					termSets = append(termSets, true)
				}
				continue
			}
			ft, ok := f.Type.(*ast.FuncType)
			if !ok {
				c.errorf(f.Type.Pos(), "invalid interface method")
				continue
			}
			sig := c.signature(ft, nil)
			sig.Throws = f.Annots.Has("throws") || ft.Annots.Has("throws")
			for _, a := range f.Annots {
				if a.Name != "throws" {
					c.errorf(a.Pos(), "annotation @%s is not allowed on an interface method", a.Name)
				}
			}
			add(&IMethod{Name: f.Names[0].Name, Sig: sig}, f.Names[0].Pos())
		}
	}
	sort.Slice(it.Methods, func(i, j int) bool { return it.Methods[i].Name < it.Methods[j].Name })
	if len(it.Methods) == 0 && !it.IsConstraint() {
		return AnyType
	}
	return it
}

// resolveNamed resolves a type declaration on demand (embedded interfaces
// declared later in the file).
func (c *checker) resolveNamed(n *Named) {
	s := c.pendingTypes[n]
	if s == nil || n.underlying != nil {
		return
	}
	delete(c.pendingTypes, n)
	t := c.typExpr(s.Type)
	if tn, ok := t.(*Named); ok && tn.underlying == nil {
		c.resolveNamed(tn)
	}
	n.underlying = t.Underlying()
	if n.underlying == nil {
		c.errorf(s.Pos(), "invalid recursive type %s", n)
		n.underlying = Typ[Invalid]
	}
}

// LookupMethod finds the method `name` in the method set of t.
// It returns the method signature and, for concrete types, the method.
func LookupMethod(t Type, name string) (*Signature, *Func, string) {
	if it, ok := t.Underlying().(*Interface); ok {
		if _, m := it.Method(name); m != nil {
			return m.Sig, nil, ""
		}
		return nil, nil, "missing method " + name
	}
	ptr := false
	n := namedOf(t)
	if p, ok := t.(*Pointer); ok {
		n, ptr = namedOf(p.Elem), true
	}
	if n == nil {
		return nil, nil, "missing method " + name
	}
	ms := n.Methods[name]
	if len(ms) != 1 || ms[0].IsOperator {
		if len(ms) > 1 {
			return nil, nil, "method " + name + " is overloaded"
		}
		return nil, nil, "missing method " + name
	}
	if ms[0].PtrRecv && !ptr {
		return nil, nil, "method " + name + " has pointer receiver (use *" + n.String() + ")"
	}
	return ms[0].Sig(), ms[0], ""
}

// sigSatisfies: a method satisfies an interface method if the signatures are
// identical, except that a non-throwing method may implement a @throws one.
func sigSatisfies(have, want *Signature) bool {
	if have.Throws && !want.Throws {
		return false
	}
	h, w := *have, *want
	h.Recv, w.Recv = nil, nil
	h.Throws, w.Throws = false, false
	return Identical(&h, &w)
}

// Implements reports whether t implements interface it.
func Implements(t Type, it *Interface) (bool, string) {
	for _, m := range it.Methods {
		sig, _, why := LookupMethod(t, m.Name)
		if sig == nil {
			return false, why
		}
		if !sigSatisfies(sig, m.Sig) {
			return false, "wrong type for method " + m.Name + " (have " + sig.String() + ", want " + m.Sig.String() + ")"
		}
	}
	return true, ""
}

func isInterface(t Type) bool {
	_, ok := t.Underlying().(*Interface)
	return ok
}

// needsConv reports whether assigning a v value to t requires a conversion
// to an interface value.
func needsConv(v, t Type) bool {
	return isInterface(t) && !Identical(v, t)
}

func (c *checker) typeAssert(e *ast.TypeAssertExpr) *operand {
	x := c.value(e.X)
	if isInvalid(x) {
		return c.invalid(e)
	}
	it, ok := x.typ.Underlying().(*Interface)
	if !ok {
		c.errorf(e.X.Pos(), "invalid operation: %s (type %s) is not an interface", exprString(e.X), x.typ)
		return c.invalid(e)
	}
	c.record(x)
	if e.Type == nil {
		c.errorf(e.Pos(), "use of .(type) outside type switch")
		return c.invalid(e)
	}
	t := c.typExpr(e.Type)
	if t == Typ[Invalid] {
		return c.invalid(e)
	}
	if !isInterface(t) {
		if ok, why := Implements(t, it); !ok {
			c.errorf(e.Type.Pos(), "impossible type assertion: %s does not implement %s (%s)", t, x.typ, why)
			return c.invalid(e)
		}
	}
	return &operand{mode: Value, typ: t}
}

func (c *checker) typeSwitch(s *ast.SwitchStmt, bind *ast.Ident, ta *ast.TypeAssertExpr) {
	x := c.value(ta.X)
	if isInvalid(x) {
		return
	}
	if !isInterface(x.typ) {
		c.errorf(ta.X.Pos(), "%s (type %s) is not an interface", exprString(ta.X), x.typ)
		return
	}
	c.record(x)
	it := x.typ.Underlying().(*Interface)
	info := &TypeSwitch{X: ta.X, Vars: map[*ast.CaseClause]*Var{}}
	c.info.TypeSwitches[s] = info
	used := false
	hasDefault := false
	seen := map[Type]bool{}
	for _, cl := range s.Body {
		if cl.List == nil {
			if hasDefault {
				c.errorf(cl.Pos(), "multiple defaults in switch")
			}
			hasDefault = true
		}
		var single Type
		for _, e := range cl.List {
			if id, ok := e.(*ast.Ident); ok && id.Name == "nil" {
				if _, isNil := c.lookup("nil").(*Nil); isNil {
					c.info.Types[e] = TypeAndValue{Mode: Value, Type: Typ[UntypedNil]}
					single = nil
					if len(cl.List) == 1 {
						single = x.typ
					}
					continue
				}
			}
			t := c.typExpr(e)
			if t == Typ[Invalid] {
				continue
			}
			c.info.Types[e] = TypeAndValue{Mode: TypeExpr, Type: t}
			if seen[t] {
				c.errorf(e.Pos(), "duplicate case %s in type switch", t)
			}
			seen[t] = true
			if !isInterface(t) {
				if ok, why := Implements(t, it); !ok {
					c.errorf(e.Pos(), "impossible type switch case: %s cannot have dynamic type %s (%s)", exprString(ta.X), t, why)
				}
			}
			if len(cl.List) == 1 {
				single = t
			}
		}
		c.openScope("block")
		if bind != nil {
			vt := x.typ
			if single != nil {
				vt = single
			}
			v := NewVar(bind.Name, vt, bind.Pos())
			c.inParams = true // checked below: must be used in at least one clause
			c.declare(c.scope, bind, v)
			c.inParams = false
			info.Vars[cl] = v
		}
		for _, st := range cl.Body {
			if b, ok := st.(*ast.BranchStmt); ok && b.Tok == token.FALLTHROUGH {
				c.errorf(b.Pos(), "cannot fallthrough in type switch")
			}
		}
		c.breakOK++
		c.stmtList(cl.Body)
		c.breakOK--
		if v := info.Vars[cl]; v != nil && v.used {
			used = true
		}
		c.closeScope()
	}
	if bind != nil && !used && !c.hasDirective(bind.Pos(), "ignore:unused") {
		c.errorf(bind.Pos(), "%s declared and not used", bind.Name)
	}
}

// ---------------------------------------------------------------- maps

// Keyable reports whether t can be a map key (hash/equality generated by codegen).
func Keyable(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.Kind != Invalid && u.Kind != UntypedNil
	case *Pointer, *Chan:
		return true
	case *Array:
		return Keyable(u.Elem)
	case *Struct:
		for _, f := range u.Fields {
			if !Keyable(f.Type) {
				return false
			}
		}
		return true
	}
	return false
}

func (c *checker) mapType(e *ast.MapType) Type {
	k := c.typExpr(e.Key)
	v := c.typExpr(e.Value)
	if k == Typ[Invalid] || v == Typ[Invalid] {
		return Typ[Invalid]
	}
	if n, ok := k.(*Named); ok && n.underlying == nil {
		c.resolveNamed(n)
	}
	if !Keyable(k) {
		if isInterface(k) {
			c.errorf(e.Key.Pos(), "invalid map key type %s: interface keys are not supported yet", k)
		} else {
			c.errorf(e.Key.Pos(), "invalid map key type %s", k)
		}
		return Typ[Invalid]
	}
	return &Map{Key: k, Elem: v}
}

func (c *checker) mapLit(e *ast.CompositeLit, m *Map) {
	for _, el := range e.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			c.errorf(el.Pos(), "missing key in map literal")
			continue
		}
		c.element(kv.Key, m.Key, "map literal")
		c.element(kv.Value, m.Elem, "map literal")
	}
}

// ---------------------------------------------------------------- error handling

func (c *checker) propagate(e *ast.PropagateExpr) *operand {
	x := c.expr(e.X)
	if isInvalid(x) {
		return c.invalid(e)
	}
	var vals []Type
	switch {
	case x.mode == NoValue:
	case x.mode == TypeExpr || x.mode == FuncM || x.mode == BuiltinM || x.mode == PkgM:
		c.checkValue(x)
		return c.invalid(e)
	default:
		if tup, ok := x.typ.(*Tuple); ok {
			for _, v := range tup.Vars {
				vals = append(vals, v.typ)
			}
		} else {
			vals = []Type{x.typ}
		}
	}
	if len(vals) == 0 || !Identical(vals[len(vals)-1], ErrorType) {
		c.errorf(e.Question, "the ? operator requires a value of type error or (..., error), found %s", x.typ)
		return c.invalid(e)
	}
	if c.sig == nil {
		c.errorf(e.Question, "the ? operator is not allowed in package initializers")
		return c.invalid(e)
	}
	rs := c.sig.Results
	if len(rs) == 0 || !Identical(rs[len(rs)-1].typ, ErrorType) {
		c.errorf(e.Question, "the ? operator can only be used in a function whose last result is error")
		return c.invalid(e)
	}
	if c.inFinally > 0 {
		c.errorf(e.Question, "the ? operator cannot leave a finally block")
	}
	c.info.Propagates[e] = &Propagate{N: len(vals)}
	tup, _ := x.typ.(*Tuple)
	switch len(vals) {
	case 1:
		return &operand{mode: NoValue}
	case 2:
		return &operand{mode: Value, typ: vals[0]}
	}
	return &operand{mode: Value, typ: &Tuple{Vars: tup.Vars[:len(vals)-1]}}
}

func (c *checker) deferStmt(s *ast.DeferStmt) {
	if c.sig == nil {
		c.errorf(s.Pos(), "defer outside a function")
		return
	}
	c.expr(s.Call)
	ci := c.info.Calls[s.Call]
	if ci == nil {
		return
	}
	switch ci.Kind {
	case CallFunc, CallMethod, CallValue, CallIface:
	case CallBuiltin:
		if ci.Builtin.ID != BClose && ci.Builtin.ID != BFree {
			c.errorf(s.Pos(), "defer of builtin %s is not supported; wrap it in func() { ... }()", ci.Builtin.name)
		}
		return
	default:
		c.errorf(s.Pos(), "defer requires a function or method call")
		return
	}
	if ci.Throws {
		c.errorf(s.Pos(), "a deferred call cannot be @throws; handle the exception inside: defer func() { try { ... } catch e { ... } }()")
	}
}

func (c *checker) callValue(e *ast.CallExpr, f *operand, sig *Signature) *operand {
	args := c.args(e)
	if args == nil {
		return c.invalid(e)
	}
	tmp := &Func{object: object{name: exprString(e.Fun), typ: sig}}
	if c.resolve([]*Func{tmp}, args, e.Pos(), exprString(e.Fun), e.Ellipsis) == nil {
		return c.invalid(e)
	}
	c.record(f)
	c.info.Calls[e] = &CallInfo{Kind: CallValue, Sig: sig, Throws: sig.Throws}
	if sig.Throws {
		c.throwSite(e.Pos(), "exception from "+exprString(e.Fun)+"()")
	}
	return resultOperand(sig)
}

func resultOperand(sig *Signature) *operand {
	switch len(sig.Results) {
	case 0:
		return &operand{mode: NoValue}
	case 1:
		return &operand{mode: Value, typ: sig.Results[0].typ}
	}
	return &operand{mode: Value, typ: &Tuple{Vars: sig.Results}}
}

func initErrors() {
	newErr := stdFunc("New", "gocpy_errors_new", param(Typ[String]), []*Var{NewVar("", ErrorType, token.Pos{})})
	StdPackages["std/errors"] = map[string]BuiltinID{}
	StdMembers["std/errors"] = map[string]Object{"New": newErr}
}

func isBin(e ast.Expr) bool { _, ok := e.(*ast.BinaryExpr); return ok }

// intersectTerms intersects two type sets (when have is meaningful).
func intersectTerms(a, b []Term, have bool) []Term {
	if !have {
		return b
	}
	var out []Term
	for _, x := range a {
		for _, y := range b {
			if x.Tilde == y.Tilde && Identical(x.Type, y.Type) {
				out = append(out, x)
			} else if x.Tilde && !y.Tilde && Identical(x.Type.Underlying(), y.Type.Underlying()) {
				out = append(out, y)
			} else if y.Tilde && !x.Tilde && Identical(x.Type.Underlying(), y.Type.Underlying()) {
				out = append(out, x)
			}
		}
	}
	if len(out) == 0 {
		out = []Term{{Type: Typ[Invalid]}} // empty type set
	}
	return out
}
