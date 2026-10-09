package types

// Generics are implemented by instantiation (monomorphization): for every
// distinct list of type arguments the generic declaration is cloned and
// checked with the type parameters bound to the arguments. Instances are
// ordinary *Func / *Named objects appended to Package.Funcs / Package.Types,
// so the backend needs no knowledge of generics.

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// maxInstances bounds the instances of one generic (catches F[T] -> F[[]T] loops).
const maxInstances = 64

// GenericFunc is a function with type parameters.
type GenericFunc struct {
	object
	Decl  *ast.FuncDecl
	file  *ast.File
	insts []*Func
	targs [][]Type
}

// GenericType is a type declaration with type parameters.
type GenericType struct {
	object
	Spec    *ast.TypeSpec
	file    *ast.File
	methods []*ast.FuncDecl
	insts   []*Named
}

// Term is an element of a constraint type set: T or ~T.
type Term struct {
	Tilde bool
	Type  Type
}

// ComparableType is the predeclared constraint comparable.
var ComparableType *Named

func initGenerics() {
	obj := &TypeName{object{name: "comparable"}}
	ComparableType = &Named{Obj: obj, underlying: &Interface{Comparable: true}, Methods: map[string][]*Func{}}
	obj.typ = ComparableType
	Universe.Insert(obj)
}

// IsConstraint reports whether it can only be used as a constraint.
func (t *Interface) IsConstraint() bool { return t.Comparable || len(t.Terms) > 0 }

// ---------------------------------------------------------------- AST cloning

// cloneAST deep-copies an AST subtree.
func cloneAST[T any](x T) T {
	memo := map[uintptr]reflect.Value{}
	return deepCopy(reflect.ValueOf(x), memo).Interface().(T)
}

func deepCopy(v reflect.Value, memo map[uintptr]reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		if c, ok := memo[v.Pointer()]; ok && c.Type() == v.Type() {
			return c
		}
		n := reflect.New(v.Elem().Type())
		memo[v.Pointer()] = n
		n.Elem().Set(deepCopy(v.Elem(), memo))
		return n
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		r := reflect.New(v.Type()).Elem()
		r.Set(deepCopy(v.Elem(), memo))
		return r
	case reflect.Struct:
		n := reflect.New(v.Type()).Elem()
		n.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if f := n.Field(i); f.CanSet() {
				f.Set(deepCopy(v.Field(i), memo))
			}
		}
		return n
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			n.Index(i).Set(deepCopy(v.Index(i), memo))
		}
		return n
	}
	return v
}

// ---------------------------------------------------------------- helpers

// tparamList flattens a type parameter list.
func tparamList(fl *ast.FieldList) (names []*ast.Ident, cons []ast.Expr) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, n := range f.Names {
			names = append(names, n)
			cons = append(cons, f.Type)
		}
	}
	return
}

// bindScope binds type parameter names to type arguments.
func (c *checker) bindScope(file *ast.File, names []*ast.Ident, targs []Type) *Scope {
	s := NewScope(c.fileScopes[file], "generic")
	for i, n := range names {
		if n.Name == "_" || i >= len(targs) {
			continue
		}
		s.Insert(&TypeName{object{name: n.Name, typ: targs[i], pos: n.Pos()}})
	}
	return s
}

// withScope runs f with the given lookup scope and file.
func (c *checker) withScope(s *Scope, file *ast.File, f func()) {
	oldScope, oldFile := c.scope, c.curFile
	c.scope, c.curFile = s, file
	defer func() { c.scope, c.curFile = oldScope, oldFile }()
	f()
}

func identicalList(a, b []Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Identical(a[i], b[i]) {
			return false
		}
	}
	return true
}

func typeListString(ts []Type) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.String())
	}
	return strings.Join(s, ", ")
}

// mangle returns a C-identifier-safe encoding of t.
func mangle(t Type) string {
	switch t := t.(type) {
	case *Basic:
		return Default(t).String()
	case *Named:
		if t.Pkg != "" {
			return t.Pkg + "_" + t.Obj.name
		}
		return t.Obj.name
	case *Pointer:
		return "P" + mangle(t.Elem)
	case *Slice:
		return "S" + mangle(t.Elem)
	case *Array:
		return fmt.Sprintf("A%d%s", t.Len, mangle(t.Elem))
	case *Map:
		return "M" + mangle(t.Key) + "_" + mangle(t.Elem)
	case *Chan:
		return "C" + mangle(t.Elem)
	case *Interface:
		if len(t.Methods) == 0 {
			return "any"
		}
	}
	h := fnv.New32a()
	h.Write([]byte(t.String()))
	return fmt.Sprintf("X%08x", h.Sum32())
}

func mangleList(ts []Type) string {
	var s []string
	for _, t := range ts {
		s = append(s, mangle(t))
	}
	return strings.Join(s, "__")
}

func (c *checker) typeList(es []ast.Expr) ([]Type, bool) {
	var ts []Type
	ok := true
	for _, e := range es {
		t := c.typExpr(e)
		if t == Typ[Invalid] {
			ok = false
		}
		ts = append(ts, t)
	}
	return ts, ok
}

// ---------------------------------------------------------------- constraints

// constraint evaluates a type parameter constraint.
func (c *checker) constraint(e ast.Expr) *Interface {
	old := c.inConstraint
	c.inConstraint = true
	defer func() { c.inConstraint = old }()
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.OR {
			return &Interface{Terms: c.terms(e)}
		}
	case *ast.UnaryExpr:
		if x.Op == token.TILDE {
			return &Interface{Terms: c.terms(e)}
		}
	}
	t := c.typExpr(e)
	if n, ok := t.(*Named); ok && n.underlying == nil {
		c.resolveNamed(n)
	}
	if it, ok := t.Underlying().(*Interface); ok {
		return it
	}
	if t == Typ[Invalid] {
		return AnyType
	}
	return &Interface{Terms: []Term{{Type: t}}}
}

// terms evaluates a union T | ~U | ...
func (c *checker) terms(e ast.Expr) []Term {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.OR {
			return append(c.terms(x.X), c.terms(x.Y)...)
		}
	case *ast.ParenExpr:
		return c.terms(x.X)
	case *ast.UnaryExpr:
		if x.Op == token.TILDE {
			t := c.typExpr(x.X)
			if t != Typ[Invalid] && t.Underlying() != t {
				c.errorf(x.Pos(), "invalid use of ~ (underlying type of %s is %s)", t, t.Underlying())
			}
			return []Term{{Tilde: true, Type: t}}
		}
	}
	t := c.typExpr(e)
	if it, ok := t.Underlying().(*Interface); ok {
		if len(it.Methods) > 0 {
			c.errorf(e.Pos(), "cannot use %s in a union (interface contains methods)", t)
		}
		return it.Terms
	}
	return []Term{{Type: t}}
}

// satisfies reports why t does not satisfy constraint it ("" if it does).
func satisfies(t Type, it *Interface) string {
	if it.Comparable && !comparable(t) {
		return fmt.Sprintf("%s does not satisfy comparable", t)
	}
	if len(it.Terms) > 0 {
		ok := false
		for _, term := range it.Terms {
			if Identical(t, term.Type) || (term.Tilde && Identical(t.Underlying(), term.Type.Underlying())) {
				ok = true
				break
			}
		}
		if !ok {
			var ss []string
			for _, term := range it.Terms {
				s := term.Type.String()
				if term.Tilde {
					s = "~" + s
				}
				ss = append(ss, s)
			}
			return fmt.Sprintf("%s does not satisfy %s", t, strings.Join(ss, " | "))
		}
	}
	if len(it.Methods) > 0 {
		if ok, why := Implements(t, &Interface{Methods: it.Methods}); !ok {
			return fmt.Sprintf("%s does not satisfy %s (%s)", t, it, why)
		}
	}
	return ""
}

func (c *checker) checkConstraints(scope *Scope, file *ast.File, names []*ast.Ident, cons []ast.Expr, targs []Type, pos token.Pos) bool {
	ok := true
	c.withScope(scope, file, func() {
		for i := range names {
			// constraints are evaluated on a clone: they may be evaluated many times
			it := c.constraint(cloneAST(cons[i]))
			if why := satisfies(targs[i], it); why != "" {
				c.errorf(pos, "%s (type parameter %s)", why, names[i].Name)
				ok = false
			}
		}
	})
	return ok
}

// ---------------------------------------------------------------- instantiation

// instFunc returns the instance of g for targs.
func (c *checker) instFunc(g *GenericFunc, targs []Type, pos token.Pos) *Func {
	names, cons := tparamList(g.Decl.Type.TypeParams)
	if len(targs) != len(names) {
		c.errorf(pos, "wrong number of type arguments for %s: have %d, want %d", g.name, len(targs), len(names))
		return nil
	}
	for i, ta := range g.targs {
		if identicalList(ta, targs) {
			return g.insts[i]
		}
	}
	scope := c.bindScope(g.file, names, targs)
	if !c.checkConstraints(scope, g.file, names, cons, targs, pos) {
		return nil
	}
	if len(g.insts) >= maxInstances {
		c.errs = append(c.errs, Error{pos, fmt.Sprintf("too many instances of %s (%d): infinite recursive instantiation", g.name, maxInstances)})
		return nil
	}
	d := cloneAST(g.Decl)
	d.Type.TypeParams = nil
	fn := &Func{object: object{name: g.name, pos: g.pos}, Decl: d, TScope: scope,
		Suffix: "__" + mangleList(targs), Inst: g.name + "[" + typeListString(targs) + "]"}
	g.insts = append(g.insts, fn)
	g.targs = append(g.targs, targs)
	c.withScope(scope, g.file, func() {
		sig := c.signature(d.Type, nil)
		sig.Throws = d.Annots.Has("throws")
		fn.typ = sig
	})
	c.pkg.FileOf[fn] = g.file
	c.pkg.Funcs = append(c.pkg.Funcs, fn)
	return fn
}

// instType returns the instance of g for targs.
func (c *checker) instType(g *GenericType, targs []Type, pos token.Pos) Type {
	names, cons := tparamList(g.Spec.TypeParams)
	if len(targs) != len(names) {
		c.errorf(pos, "wrong number of type arguments for %s: have %d, want %d", g.name, len(targs), len(names))
		return Typ[Invalid]
	}
	for _, n := range g.insts {
		if identicalList(n.TArgs, targs) {
			return n
		}
	}
	scope := c.bindScope(g.file, names, targs)
	if !c.checkConstraints(scope, g.file, names, cons, targs, pos) {
		return Typ[Invalid]
	}
	if len(g.insts) >= maxInstances {
		c.errs = append(c.errs, Error{pos, fmt.Sprintf("too many instances of %s (%d): infinite recursive instantiation", g.name, maxInstances)})
		return Typ[Invalid]
	}
	tn := &TypeName{object{name: g.name + "__" + mangleList(targs), pos: g.pos}}
	n := &Named{Obj: tn, Methods: map[string][]*Func{}, Origin: g, TArgs: targs,
		display: g.name + "[" + typeListString(targs) + "]"}
	tn.typ = n
	g.insts = append(g.insts, n)
	c.pkg.FileOf[tn] = g.file
	c.pkg.Types = append(c.pkg.Types, n)
	c.withScope(scope, g.file, func() {
		t := c.typExpr(cloneAST(g.Spec.Type))
		if tn2, ok := t.(*Named); ok && tn2.underlying == nil {
			c.resolveNamed(tn2)
		}
		n.underlying = t.Underlying()
		if n.underlying == nil {
			c.errorf(g.Spec.Pos(), "invalid recursive type %s", n)
			n.underlying = Typ[Invalid]
		}
		if it, ok := n.underlying.(*Interface); ok && it.IsConstraint() {
			c.errorf(g.Spec.Pos(), "generic constraint interfaces are not supported")
		}
	})
	if c.phase >= 4 {
		c.instMethods(n)
	} else {
		c.pendingInst = append(c.pendingInst, n)
	}
	return n
}

// instMethods instantiates the methods of a generic type instance.
func (c *checker) instMethods(n *Named) {
	g := n.Origin
	for _, md := range g.methods {
		d := cloneAST(md)
		rnames := recvTParams(d.Recv)
		if len(rnames) != len(n.TArgs) {
			c.errorf(d.Recv.Pos(), "receiver of %s.%s must list %d type parameters", g.name, d.Name.Name, len(n.TArgs))
			continue
		}
		scope := c.bindScope(g.file, rnames, n.TArgs)
		fn := &Func{object: object{name: d.Name.Name, pos: d.Name.Pos()}, Decl: d,
			Overloaded: d.Annots.Has("overload"), IsOperator: d.IsOperator, TScope: scope,
			Inst: n.String() + "." + d.Name.Name}
		c.withScope(scope, g.file, func() {
			sig := c.signature(d.Type, d.Recv)
			sig.Throws = d.Annots.Has("throws")
			fn.typ = sig
			if sig.Recv != nil {
				c.attachMethod(fn, sig)
			}
		})
		c.pkg.FileOf[fn] = g.file
		c.pkg.Funcs = append(c.pkg.Funcs, fn)
	}
}

// recvBase returns the receiver base type expression and its type parameter names.
func recvBase(recv *ast.FieldList) (base *ast.Ident, tparams []*ast.Ident, generic bool) {
	if recv == nil || len(recv.List) != 1 {
		return nil, nil, false
	}
	t := recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	switch x := t.(type) {
	case *ast.Ident:
		return x, nil, false
	case *ast.IndexExpr:
		id, _ := x.X.(*ast.Ident)
		for _, ix := range x.Indices {
			if p, ok := ix.(*ast.Ident); ok {
				tparams = append(tparams, p)
			} else {
				tparams = append(tparams, &ast.Ident{NamePos: ix.Pos(), Name: "_"})
			}
		}
		return id, tparams, true
	}
	return nil, nil, false
}

func recvTParams(recv *ast.FieldList) []*ast.Ident {
	_, ps, _ := recvBase(recv)
	return ps
}

// genericIndex handles F[T...] / List[T...] where X names a generic.
func (c *checker) genericIndex(e *ast.IndexExpr) (*operand, bool) {
	id, ok := unparen(e.X).(*ast.Ident)
	if !ok {
		return nil, false
	}
	switch o := c.lookup(id.Name).(type) {
	case *GenericFunc:
		c.info.Uses[id] = o
		targs, ok := c.typeList(e.Indices)
		if !ok {
			return c.invalid(e), true
		}
		fn := c.instFunc(o, targs, e.Pos())
		if fn == nil {
			return c.invalid(e), true
		}
		return &operand{mode: FuncM, typ: fn.typ, obj: fn}, true
	case *GenericType:
		c.info.Uses[id] = o
		targs, ok := c.typeList(e.Indices)
		if !ok {
			return c.invalid(e), true
		}
		t := c.instType(o, targs, e.Pos())
		if t == Typ[Invalid] {
			return c.invalid(e), true
		}
		return &operand{mode: TypeExpr, typ: t}, true
	}
	return nil, false
}

// ---------------------------------------------------------------- inference

// infer deduces the type arguments of a call to g from its arguments.
func (c *checker) infer(g *GenericFunc, args []*operand, e *ast.CallExpr) []Type {
	names, _ := tparamList(g.Decl.Type.TypeParams)
	idx := map[string]int{}
	for i, n := range names {
		idx[n.Name] = i
	}
	binds := make([]Type, len(names))
	// parameter type expressions, one per argument
	var pts []ast.Expr
	for _, f := range g.Decl.Type.Params.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			pts = append(pts, f.Type)
		}
	}
	paramAt := func(i int) ast.Expr {
		if len(pts) == 0 {
			return nil
		}
		last := pts[len(pts)-1]
		if el, ok := last.(*ast.Ellipsis); ok && i >= len(pts)-1 {
			if e.Ellipsis {
				return &ast.ArrayType{Lbrack: el.Pos(), Elt: el.Elt}
			}
			return el.Elt
		}
		if i < len(pts) {
			return pts[i]
		}
		return nil
	}
	failed := false
	var unify func(te ast.Expr, t Type)
	unify = func(te ast.Expr, t Type) {
		if failed || te == nil || t == nil {
			return
		}
		switch x := te.(type) {
		case *ast.ParenExpr:
			unify(x.X, t)
		case *ast.Ident:
			i, ok := idx[x.Name]
			if !ok {
				return
			}
			if binds[i] == nil {
				binds[i] = t
			} else if !Identical(binds[i], t) {
				c.errorf(e.Pos(), "type %s does not match inferred type %s for %s in call to %s", t, binds[i], x.Name, g.name)
				failed = true
			}
		case *ast.StarExpr:
			if p, ok := t.Underlying().(*Pointer); ok {
				unify(x.X, p.Elem)
			}
		case *ast.ArrayType:
			switch u := t.Underlying().(type) {
			case *Slice:
				if x.Len == nil {
					unify(x.Elt, u.Elem)
				}
			case *Array:
				if x.Len != nil {
					unify(x.Elt, u.Elem)
				}
			}
		case *ast.MapType:
			if m, ok := t.Underlying().(*Map); ok {
				unify(x.Key, m.Key)
				unify(x.Value, m.Elem)
			}
		case *ast.ChanType:
			if ch, ok := t.Underlying().(*Chan); ok {
				unify(x.Value, ch.Elem)
			}
		case *ast.FuncType:
			sig, ok := t.Underlying().(*Signature)
			if !ok {
				return
			}
			flat := func(fl *ast.FieldList) []ast.Expr {
				var out []ast.Expr
				if fl == nil {
					return nil
				}
				for _, f := range fl.List {
					n := len(f.Names)
					if n == 0 {
						n = 1
					}
					for i := 0; i < n; i++ {
						out = append(out, f.Type)
					}
				}
				return out
			}
			ps, rs := flat(x.Params), flat(x.Results)
			for i := 0; i < len(ps) && i < len(sig.Params); i++ {
				unify(ps[i], sig.Params[i].typ)
			}
			for i := 0; i < len(rs) && i < len(sig.Results); i++ {
				unify(rs[i], sig.Results[i].typ)
			}
		case *ast.IndexExpr:
			n, ok := t.(*Named)
			id, isId := x.X.(*ast.Ident)
			if !ok || !isId || n.Origin == nil || n.Origin.name != id.Name {
				return
			}
			for i := 0; i < len(x.Indices) && i < len(n.TArgs); i++ {
				unify(x.Indices[i], n.TArgs[i])
			}
		}
	}
	for i, a := range args {
		if !isUntyped(a.typ) {
			unify(paramAt(i), a.typ)
		}
	}
	if failed {
		return nil
	}
	// untyped constants bound directly to a type parameter: default type
	// (the widest kind wins: 1 and 2.5 give float64)
	for i, a := range args {
		if !isUntyped(a.typ) {
			continue
		}
		id, ok := paramAt(i).(*ast.Ident)
		if !ok {
			continue
		}
		j, ok := idx[id.Name]
		if !ok {
			continue
		}
		if b, ok := a.typ.(*Basic); ok && b.Kind == UntypedNil {
			continue
		}
		if binds[j] == nil {
			binds[j] = a.typ
		} else if isUntyped(binds[j]) && untypedRank(a.typ) > untypedRank(binds[j]) {
			binds[j] = a.typ
		}
	}
	for i, b := range binds {
		if b == nil {
			c.errorf(e.Pos(), "cannot infer %s in call to %s; specify type arguments: %s[...]", names[i].Name, g.name, g.name)
			return nil
		}
		binds[i] = Default(b)
	}
	return binds
}

// ---------------------------------------------------------------- method values

// MethodVal describes x.M used as a function value.
type MethodVal struct {
	Func      *Func // concrete method (nil for interface methods)
	Iface     bool
	Index     int // itab slot for interface methods
	Sig       *Signature
	Recv      ast.Expr
	RecvAddr  bool
	RecvDeref bool
}

func (c *checker) methodValue(x *operand, e ast.Expr) *operand {
	sel := x.sel
	mv := &MethodVal{Recv: x.recv.expr}
	var sig *Signature
	switch sel.Kind {
	case IfaceMethodSel:
		sig = x.typ.(*Signature)
		mv.Iface, mv.Index = true, sel.Index
	case MethodSel:
		if len(sel.Methods) != 1 || sel.Methods[0].Overloaded {
			c.errorf(e.Pos(), "overloaded method %s cannot be used as a value", exprString(e))
			return c.invalid(e)
		}
		fn := sel.Methods[0]
		if fn.Decl == nil {
			c.errorf(e.Pos(), "method %s cannot be used as a value; wrap it in a closure", exprString(e))
			return c.invalid(e)
		}
		if fn.IsOperator {
			c.errorf(e.Pos(), "operator method cannot be used as a value")
			return c.invalid(e)
		}
		mv.Func = fn
		sig = fn.Sig()
		switch {
		case fn.PtrRecv && !sel.Indirect:
			if x.recv.mode != Variable {
				c.errorf(e.Pos(), "cannot take pointer method value %s (receiver not addressable)", exprString(e))
				return c.invalid(e)
			}
			mv.RecvAddr = true
			c.markAddrTaken(x.recv.expr)
		case !fn.PtrRecv && sel.Indirect:
			mv.RecvDeref = true
		}
	default:
		c.errorf(e.Pos(), "%s cannot be used as a value", exprString(e))
		return c.invalid(e)
	}
	mv.Sig = &Signature{Params: sig.Params, Results: sig.Results, Variadic: sig.Variadic, Throws: sig.Throws}
	c.info.MethodVals[e] = mv
	r := &operand{mode: Value, typ: mv.Sig, expr: e}
	c.record(r)
	return r
}
