package types

import (
	"fmt"
	"go/constant"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// Mode classifies an expression.
type Mode int

const (
	NoValue   Mode = iota // call without results
	Value                 // rvalue
	Variable              // addressable
	Const                 // constant
	TypeExpr              // type
	BuiltinM              // builtin function
	PkgM                  // package name
	FuncM                 // function / function set (before call)
	MapIndexM             // m[k]: assignable, not addressable
)

// TypeAndValue records the result of checking an expression.
type TypeAndValue struct {
	Mode  Mode
	Type  Type
	Value constant.Value
}

// SelKind is the kind of a selector.
type SelKind int

const (
	FieldSel SelKind = iota
	MethodSel
	PkgSel
	SliceMethodSel // xs.append(...)
	IfaceMethodSel // method of an interface value (Index = itab slot)
)

// Selection describes x.f.
type Selection struct {
	Kind     SelKind
	Index    int     // field index
	Methods  []*Func // candidates for MethodSel
	Indirect bool    // x is a pointer (auto-deref)
	Builtin  *Builtin
}

// CallKind is the resolved kind of a call.
type CallKind int

const (
	CallFunc CallKind = iota
	CallMethod
	CallConversion
	CallBuiltin
	CallSliceAppend
	CallValue // call of a function value
	CallIface // dynamic call through an interface (Index = itab slot)
)

// CallInfo is the resolved target of a call.
type CallInfo struct {
	Kind      CallKind
	Func      *Func      // CallFunc / CallMethod (after overload resolution)
	Builtin   *Builtin   // CallBuiltin
	TypeArg   Type       // make/new/sizeof type argument, conversion target
	RecvAddr  bool       // pass &recv
	RecvDeref bool       // pass *recv
	Throws    bool       // callee is @throws: codegen checks for a pending exception
	Sig       *Signature // CallValue / CallIface
	Index     int        // CallIface
}

// CatchInfo describes one catch clause.
type CatchInfo struct {
	Type Type // nil for catch-all
	Var  *Var // nil if unnamed
}

// OpInfo describes an overloaded operator use.
type OpInfo struct {
	Func     *Func
	Swap     bool // a > b  -> b < a
	Negate   bool // a != b -> !(a == b)
	RecvAddr bool
}

// Info holds results of type checking.
type Info struct {
	Types      map[ast.Expr]TypeAndValue
	Defs       map[*ast.Ident]Object
	Uses       map[*ast.Ident]Object
	Selections map[*ast.SelectorExpr]*Selection
	Calls      map[*ast.CallExpr]*CallInfo
	Ops        map[ast.Expr]*OpInfo
	// Implicit objects declared by statements without identifiers on the left.
	ForVars map[ast.Node][2]*Var // ForInStmt / ComprehensionExpr: key, value
	// AssignOps records overloaded operators used by op-assignments (a += b).
	AssignOps map[*ast.AssignStmt]*OpInfo
	Catches   map[*ast.CatchClause]*CatchInfo
	CommaOk   map[ast.Expr]bool       // v, ok := <-ch / m[k] / x.(T)
	Throws    map[*ast.ThrowStmt]Type // type of the thrown value (ExceptionType for rethrow)
	// Captures lists the variables captured by each function literal.
	Captures map[*ast.FuncLit][]*Var
	// FuncRefs: function names used as values.
	FuncRefs map[ast.Expr]*Func
	// Implicit: expressions implicitly converted to an interface type.
	Implicit map[ast.Expr]Type
	// MapKeys: for k in m (single variable iterates keys).
	MapKeys      map[ast.Node]bool
	TypeSwitches map[*ast.SwitchStmt]*TypeSwitch
	Propagates   map[*ast.PropagateExpr]*Propagate
	// MethodVals: x.M used as a function value.
	MethodVals map[ast.Expr]*MethodVal
	// PtrOps: pointer arithmetic (BinaryExpr or op-assignment).
	PtrOps map[ast.Node]PtrOp
}

// Package is a checked package.
type Package struct {
	Name  string
	Scope *Scope
	Files []*ast.File
	// declaration order
	Types   []*Named
	Funcs   []*Func
	Globals []*Var
	// file each object was declared in
	FileOf map[Object]*ast.File
}

// Error is a type error.
type Error struct {
	Pos token.Pos
	Msg string
}

func (e Error) Error() string { return fmt.Sprintf("%s: %s", e.Pos, e.Msg) }

// ErrorList implements error.
type ErrorList []Error

func (l ErrorList) Error() string {
	s := ""
	for i, e := range l {
		if i > 0 {
			s += "\n"
		}
		s += e.Error()
	}
	return s
}

type checker struct {
	pkg  *Package
	info *Info
	errs ErrorList

	fileScopes map[*ast.File]*Scope
	imports    map[*ast.File]map[string]*PkgName
	curFile    *ast.File
	filePrefix map[string]string // file name -> ast.File.Prefix

	scope *Scope // current local scope (nil at package level)

	// function context
	sig       *Signature
	fn        *Func
	loopDepth int
	breakOK   int

	protected  int      // >0 inside a try body that has a catch-all
	inFinally  int      // >0 inside a finally block
	throwSites int      // unprotected throw sites in the current function
	inParams   bool     // declaring parameters/results (exempt from the unused check)
	finStack   [][2]int // loopDepth/breakOK at entry of each enclosing finally
	locals     []*Var
	ctx        *funcCtx

	pendingTypes map[*Named]*ast.TypeSpec

	unsafeDepth  int      // >0 inside unsafe { ... }
	inConstraint bool     // evaluating a type parameter constraint
	phase        int      // checking phase (methods of instances are attached from phase 4)
	pendingInst  []*Named // instances created before phase 4
	rewrites     *goRewrites
	rewritten    bool // second pass after rewriting Go field accesses
}

// Check type-checks a package made of files.
func Check(files []*ast.File) (*Package, *Info, error) {
	return check(files, false)
}

func check(files []*ast.File, rewritten bool) (*Package, *Info, error) {
	c := &checker{
		rewritten: rewritten,
		pkg:       &Package{Scope: NewScope(Universe, "package"), Files: files, FileOf: map[Object]*ast.File{}},
		info: &Info{
			Types:        map[ast.Expr]TypeAndValue{},
			Defs:         map[*ast.Ident]Object{},
			Uses:         map[*ast.Ident]Object{},
			Selections:   map[*ast.SelectorExpr]*Selection{},
			Calls:        map[*ast.CallExpr]*CallInfo{},
			Ops:          map[ast.Expr]*OpInfo{},
			ForVars:      map[ast.Node][2]*Var{},
			Catches:      map[*ast.CatchClause]*CatchInfo{},
			CommaOk:      map[ast.Expr]bool{},
			Captures:     map[*ast.FuncLit][]*Var{},
			FuncRefs:     map[ast.Expr]*Func{},
			Implicit:     map[ast.Expr]Type{},
			MapKeys:      map[ast.Node]bool{},
			MethodVals:   map[ast.Expr]*MethodVal{},
			PtrOps:       map[ast.Node]PtrOp{},
			TypeSwitches: map[*ast.SwitchStmt]*TypeSwitch{},
			Propagates:   map[*ast.PropagateExpr]*Propagate{},
			Throws:       map[*ast.ThrowStmt]Type{},
		},
		fileScopes: map[*ast.File]*Scope{},
		imports:    map[*ast.File]map[string]*PkgName{},
	}
	c.run()
	if !rewritten && !c.rewrites.empty() {
		for _, f := range files {
			c.rewrites.rewrite(c, reflect.ValueOf(f))
		}
		return check(files, true)
	}
	if len(c.errs) > 0 {
		sort.SliceStable(c.errs, func(i, j int) bool {
			a, b := c.errs[i].Pos, c.errs[j].Pos
			if a.File != b.File {
				return a.File < b.File
			}
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.Column < b.Column
		})
		// the same error in a generic body is reported once, for the first instance
		seen := map[string]bool{}
		out := c.errs[:0]
		for _, e := range c.errs {
			base := e.Msg
			if i := strings.Index(base, " (in instance "); i >= 0 {
				base = base[:i]
			}
			k := e.Pos.String() + base
			if !seen[k] {
				seen[k] = true
				out = append(out, e)
			}
		}
		c.errs = out
		return c.pkg, c.info, c.errs
	}
	return c.pkg, c.info, nil
}

func (c *checker) errorf(p token.Pos, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.fn != nil && c.fn.Inst != "" {
		msg += " (in instance " + c.fn.Inst + ")"
	}
	c.errs = append(c.errs, Error{p, msg})
}

func (c *checker) run() {
	if len(c.pkg.Files) == 0 {
		return
	}
	c.pkg.Name = c.pkg.Files[0].Name.Name

	// 1. imports into file scopes
	for _, f := range c.pkg.Files {
		fs := NewScope(c.pkg.Scope, "file")
		c.fileScopes[f] = fs
		c.imports[f] = map[string]*PkgName{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				c.errorf(imp.Pos(), "bad import path")
				continue
			}
			if _, ok := StdPackages[p]; !ok {
				c.errorf(imp.Pos(), "package %q not found (available: %s)", p, StdList())
				continue
			}
			name := path.Base(p)
			if imp.Alias != nil {
				name = imp.Alias.Name
				if name == "." || name == "_" {
					c.errorf(imp.Alias.Pos(), "dot and blank imports are not allowed")
					continue
				}
			}
			pn := &PkgName{object: object{name: name, pos: imp.Pos()}, Path: p}
			if prev := fs.Insert(pn); prev != nil {
				c.errorf(imp.Pos(), "%s imported twice", name)
				continue
			}
			c.imports[f][name] = pn
		}
	}

	// 2. collect package-level objects
	var typeSpecs, aliasSpecs []*ast.TypeSpec
	var typeAnnots []ast.Annotations
	type valueDecl struct {
		spec    *ast.ValueSpec
		isConst bool
		file    *ast.File
	}
	var values []valueDecl
	funcSets := map[string]*FuncSet{}
	var genMethods []*ast.FuncDecl

	for _, f := range c.pkg.Files {
		c.curFile = f
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.TypeParams != nil {
							if s.Alias {
								c.errorf(s.Pos(), "generic type aliases are not supported")
								continue
							}
							gt := &GenericType{object: object{name: s.Name.Name, pos: s.Name.Pos()}, Spec: s, file: f}
							c.declare(c.pkg.Scope, s.Name, gt)
							c.pkg.FileOf[gt] = f
							continue
						}
						if s.Alias {
							tn := &TypeName{object{name: s.Name.Name, pos: s.Name.Pos()}}
							c.declare(c.pkg.Scope, s.Name, tn)
							c.pkg.FileOf[tn] = f
							aliasSpecs = append(aliasSpecs, s)
							continue
						}
						tn := &TypeName{object{name: s.Name.Name, pos: s.Name.Pos()}}
						n := &Named{Obj: tn, Methods: map[string][]*Func{}}
						tn.typ = n
						c.declare(c.pkg.Scope, s.Name, tn)
						c.pkg.FileOf[tn] = f
						c.pkg.Types = append(c.pkg.Types, n)
						typeSpecs = append(typeSpecs, s)
						typeAnnots = append(typeAnnots, d.Annots)
					case *ast.ValueSpec:
						values = append(values, valueDecl{s, d.Tok == token.CONST, f})
					}
				}
			case *ast.FuncDecl:
				if d.Type.TypeParams != nil {
					switch {
					case d.Recv != nil:
						c.errorf(d.Pos(), "methods cannot have type parameters")
					case d.Annots.Has("overload"):
						c.errorf(d.Pos(), "generic function %s cannot be @overload", d.Name.Name)
					case d.Annots.Has("extern") || d.Annots.Has("export"):
						c.errorf(d.Pos(), "generic function %s cannot be @extern/@export", d.Name.Name)
					case d.Body == nil:
						c.errorf(d.Pos(), "generic function %s has no body", d.Name.Name)
					default:
						gf := &GenericFunc{object: object{name: d.Name.Name, pos: d.Name.Pos()}, Decl: d, file: f}
						c.checkImportConflict(d.Name)
						c.declare(c.pkg.Scope, d.Name, gf)
						c.pkg.FileOf[gf] = f
					}
					continue
				}
				if _, _, generic := recvBase(d.Recv); generic {
					genMethods = append(genMethods, d)
					continue
				}
				fn := &Func{object: object{name: d.Name.Name, pos: d.Name.Pos()}, Decl: d,
					Overloaded: d.Annots.Has("overload"), IsOperator: d.IsOperator}
				c.pkg.FileOf[fn] = f
				if a := d.Annots.Get("extern"); a != nil {
					fn.Extern = c.annotString(a, d.Name.Name)
				}
				if a := d.Annots.Get("export"); a != nil {
					fn.Export = c.annotString(a, d.Name.Name)
				}
				if d.Body == nil && fn.Extern == "" {
					c.errorf(d.Name.Pos(), "function %s has no body; use @extern(\"c_name\") for C functions", d.Name.Name)
				}
				if d.Body != nil && fn.Extern != "" {
					c.errorf(d.Name.Pos(), "@extern function %s must not have a body", d.Name.Name)
				}
				c.pkg.Funcs = append(c.pkg.Funcs, fn)
				if d.Recv != nil {
					continue // attached after types are resolved
				}
				c.checkImportConflict(d.Name)
				if fs := funcSets[fn.name]; fs != nil {
					fs.Funcs = append(fs.Funcs, fn)
					c.info.Defs[d.Name] = fn
					continue
				}
				if prev := c.pkg.Scope.Lookup(fn.name); prev != nil {
					if pf, ok := prev.(*Func); ok {
						fs := &FuncSet{object: object{name: fn.name, pos: pf.pos}, Funcs: []*Func{pf, fn}}
						funcSets[fn.name] = fs
						c.pkg.Scope.elems[fn.name] = fs
						c.info.Defs[d.Name] = fn
						continue
					}
				}
				c.declare(c.pkg.Scope, d.Name, fn)
			}
		}
	}
	// methods of generic types are instantiated with their type
	for _, d := range genMethods {
		base, _, _ := recvBase(d.Recv)
		gt, _ := c.pkg.Scope.Lookup(base.Name).(*GenericType)
		if gt == nil {
			c.errorf(d.Recv.Pos(), "%s is not a generic type", base.Name)
			continue
		}
		gt.methods = append(gt.methods, d)
	}
	// overload sets must be fully annotated (also checked by package check)
	for name, fs := range funcSets {
		for _, f := range fs.Funcs {
			if !f.Overloaded {
				c.errorf(f.pos, "%s is declared %d times; every variant must be marked @overload", name, len(fs.Funcs))
			}
		}
	}

	// 3. resolve type declarations
	c.pendingTypes = map[*Named]*ast.TypeSpec{}
	for _, s := range typeSpecs {
		if tn, ok := c.pkg.Scope.Lookup(s.Name.Name).(*TypeName); ok {
			if n, ok := tn.typ.(*Named); ok {
				c.pendingTypes[n] = s
			}
		}
	}
	// aliases (type A = T) denote T itself
	for _, s := range aliasSpecs {
		tn, ok := c.pkg.Scope.Lookup(s.Name.Name).(*TypeName)
		if !ok || tn.typ != nil {
			continue
		}
		c.curFile = c.pkg.FileOf[tn]
		tn.typ = Typ[Invalid]
		tn.typ = c.typExpr(s.Type)
	}
	for i, s := range typeSpecs {
		tn := c.pkg.Scope.Lookup(s.Name.Name).(*TypeName)
		n := tn.typ.(*Named)
		c.curFile = c.pkg.FileOf[tn]
		c.resolveNamed(n)
		if n.underlying == nil {
			continue
		}
		if st, ok := n.underlying.(*Struct); ok {
			if typeAnnots[i].Has("packed") {
				// packed/aligned layout is a property of the declared type
				cp := *st
				cp.Packed = true
				st = &cp
				n.underlying = st
			}
			if a := typeAnnots[i].Get("align"); a != nil {
				cp := *st
				cp.Align = c.annotInt(a)
				n.underlying = &cp
			}
		}
	}

	// 4. function signatures and methods
	c.phase = 4
	for _, n := range c.pendingInst {
		c.instMethods(n)
	}
	c.pendingInst = nil
	for _, fn := range c.pkg.Funcs {
		if fn.TScope != nil {
			continue // instance: signature already resolved
		}
		c.curFile = c.pkg.FileOf[fn]
		d := fn.Decl
		sig := c.signature(d.Type, d.Recv)
		sig.Throws = d.Annots.Has("throws")
		fn.typ = sig
		if d.Recv != nil {
			c.attachMethod(fn, sig)
		}
	}
	for _, fn := range c.pkg.Funcs {
		if fn.Decl.Recv == nil && fn.name == "main" && c.pkg.Name == "main" {
			s := fn.Sig()
			if len(s.Params) > 0 || len(s.Results) > 0 {
				c.errorf(fn.pos, "func main must have no parameters and no results")
			}
		}
	}
	c.checkOverloadAmbiguity()

	// 5. package-level vars and consts
	for _, v := range values {
		c.curFile = v.file
		vars := c.valueSpec(c.pkg.Scope, v.spec, v.isConst, true)
		for _, x := range vars {
			c.pkg.FileOf[x] = v.file
		}
	}

	// directives
	for _, f := range c.pkg.Files {
		for line, ds := range f.Directives {
			for _, d := range ds {
				if !knownDirectives[d] {
					c.errorf(token.Pos{File: f.Package.File, Line: line, Column: 1}, "unknown directive //gocpy:%s", d)
				}
			}
		}
	}

	// 6. bodies
	c.phase = 6
	for i := 0; i < len(c.pkg.Funcs); i++ { // instances are appended while checking
		fn := c.pkg.Funcs[i]
		if fn.Decl.Body != nil {
			c.curFile = c.pkg.FileOf[fn]
			c.funcBody(fn)
		}
	}

	if c.pkg.Name == "main" {
		if _, ok := c.pkg.Scope.Lookup("main").(*Func); !ok {
			c.errorf(c.pkg.Files[0].Name.Pos(), "package main has no func main")
		}
	}
	// unused imports are errors (as in Go)
	for _, f := range c.pkg.Files {
		c.markGenericImportUses(f)
		for _, pn := range c.imports[f] {
			if !pn.used {
				c.errorf(pn.pos, "%q imported and not used", pn.Path)
			}
		}
	}
}

var knownDirectives = map[string]bool{"ignore:unused": true}

// markGenericImportUses marks imports referenced from generic declarations as
// used. Generic types, functions and their methods are type-checked only when
// instantiated, so a package that declares a generic without using it (or a
// library package checked on its own) would otherwise report such imports as
// unused. Shadowing is forbidden, so a selector on an import name always
// refers to the import.
func (c *checker) markGenericImportUses(f *ast.File) {
	imps := c.imports[f]
	if len(imps) == 0 {
		return
	}
	mark := func(n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			if se, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok {
					if pn := imps[id.Name]; pn != nil {
						pn.used = true
					}
				}
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok && ts.TypeParams != nil {
					mark(ts)
				}
			}
		case *ast.FuncDecl:
			if _, _, generic := recvBase(d.Recv); generic || d.Type.TypeParams != nil {
				mark(d)
			}
		}
	}
}

// hasDirective reports whether the line of pos carries //gocpy:<d>.
func (c *checker) hasDirective(pos token.Pos, d string) bool {
	for _, f := range c.pkg.Files {
		if f.Package.File == pos.File {
			for _, x := range f.Directives[pos.Line] {
				if x == d {
					return true
				}
			}
		}
	}
	return false
}

func (c *checker) annotString(a *ast.Annotation, def string) string {
	if len(a.Args) == 0 {
		return def
	}
	if lit, ok := a.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, _ := strconv.Unquote(lit.Value)
		return s
	}
	c.errorf(a.At, "@%s expects a string argument", a.Name)
	return def
}

func (c *checker) annotInt(a *ast.Annotation) int64 {
	if len(a.Args) == 1 {
		if lit, ok := a.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT {
			n, err := strconv.ParseInt(lit.Value, 0, 64)
			if err == nil && n > 0 && n&(n-1) == 0 {
				return n
			}
		}
	}
	c.errorf(a.At, "@%s expects a power-of-two integer", a.Name)
	return 0
}

// otherPackage reports whether the package-level object o belongs to another
// package than the code at pos. All packages share one scope after the build
// merged them, so a local name in a dependency may equal a top-level name of
// the target package; that is not shadowing.
func (c *checker) otherPackage(o Object, pos token.Pos) bool {
	if c.pkg.Scope.Lookup(o.Name()) != o {
		return false
	}
	if c.filePrefix == nil {
		c.filePrefix = map[string]string{}
		for _, f := range c.pkg.Files {
			c.filePrefix[f.Package.File] = f.Prefix
		}
	}
	a, ok1 := c.filePrefix[o.Pos().File]
	b, ok2 := c.filePrefix[pos.File]
	return ok1 && ok2 && a != b
}

// scopeOf returns the scope where lookups start for package-level code.
func (c *checker) fileScope() *Scope { return c.fileScopes[c.curFile] }

// declare inserts obj into scope, reporting redeclarations and import conflicts.
func (c *checker) declare(s *Scope, id *ast.Ident, obj Object) {
	if id.Name == "_" {
		c.info.Defs[id] = obj
		return
	}
	c.checkImportConflict(id)
	if s.Lookup(id.Name) == nil && s.Parent != nil {
		if prev, _ := s.Parent.LookupParent(id.Name); prev != nil && !c.otherPackage(prev, id.Pos()) {
			if _, isPkg := prev.(*PkgName); !isPkg { // import conflicts are reported above
				if prev.Pos().Line == 0 {
					c.errorf(id.Pos(), "%s shadows a predeclared identifier; shadowing is not allowed", id.Name)
				} else {
					c.errorf(id.Pos(), "%s shadows the declaration at %s; shadowing is not allowed, choose another name", id.Name, prev.Pos())
				}
			}
		}
	}
	if v, ok := obj.(*Var); ok && s.Kind != "package" {
		v.owner = c.ctx
	}
	if v, ok := obj.(*Var); ok && s.Kind != "package" && !c.inParams && !v.IsConst {
		c.locals = append(c.locals, v)
	}
	if prev := s.Insert(obj); prev != nil {
		if s.Kind == "package" {
			c.errorf(id.Pos(), "%s redeclared in this package (previous declaration at %s)", id.Name, prev.Pos())
		} else {
			c.errorf(id.Pos(), "%s already declared in this scope at %s; redeclaration is not allowed, use '=' to assign", id.Name, prev.Pos())
		}
	}
	c.info.Defs[id] = obj
}

// Gocpy rule: no identifier may have the same name as an imported package.
func (c *checker) checkImportConflict(id *ast.Ident) {
	if c.curFile == nil {
		return
	}
	for _, f := range c.pkg.Files {
		if pn := c.imports[f][id.Name]; pn != nil {
			c.errorf(id.Pos(), "%s conflicts with imported package %q; choose another name", id.Name, pn.Path)
			return
		}
	}
}

func (c *checker) attachMethod(fn *Func, sig *Signature) {
	d := fn.Decl
	rt := sig.Recv.typ
	if p, ok := rt.(*Pointer); ok {
		rt = p.Elem
		fn.PtrRecv = true
	}
	n, ok := rt.(*Named)
	if !ok || (n.Origin == nil && c.pkg.Scope.Lookup(n.Obj.name) != n.Obj) {
		c.errorf(d.Recv.Pos(), "invalid receiver type %s (must be a type declared in this package)", sig.Recv.typ)
		return
	}
	if isInterface(n) {
		c.errorf(d.Recv.Pos(), "invalid receiver type %s (an interface type cannot have methods)", n)
		return
	}
	if _, isPtr := n.underlying.(*Pointer); isPtr {
		c.errorf(d.Recv.Pos(), "invalid receiver type %s (pointer base type)", n)
		return
	}
	fn.RecvNamed = n
	name := fn.name
	if st, ok := n.underlying.(*Struct); ok {
		if _, f := st.Field(name); f != nil {
			c.errorf(fn.pos, "type %s has both field and method named %s", n, name)
		}
	}
	if prev := n.Methods[name]; len(prev) > 0 && (!fn.Overloaded || !prev[0].Overloaded) {
		if !fn.Overloaded {
			c.errorf(fn.pos, "method %s.%s redeclared; mark every variant @overload", n, name)
		}
	}
	n.Methods[name] = append(n.Methods[name], fn)
	c.info.Defs[d.Name] = fn

	if fn.IsOperator {
		np := len(sig.Params)
		switch name {
		case "!":
			if np != 0 {
				c.errorf(fn.pos, "operator ! takes no parameters")
			}
		case "-":
			if np > 1 {
				c.errorf(fn.pos, "operator - takes 0 (unary) or 1 (binary) parameters")
			}
		default:
			if np != 1 {
				c.errorf(fn.pos, "operator %s takes exactly 1 parameter", name)
			}
		}
		if (name == "==" || name == "<") && (len(sig.Results) != 1 || !Identical(sig.Results[0].typ, Typ[Bool])) {
			c.errorf(fn.pos, "operator %s must return bool", name)
		}
		if len(sig.Results) != 1 {
			c.errorf(fn.pos, "operator %s must return exactly one value", name)
		}
	}
}

func (c *checker) checkOverloadAmbiguity() {
	check := func(fs []*Func, what string) {
		for i := 0; i < len(fs); i++ {
			for j := i + 1; j < len(fs); j++ {
				a, b := fs[i].Sig(), fs[j].Sig()
				if len(a.Params) != len(b.Params) || a.Variadic != b.Variadic {
					continue
				}
				same := true
				for k := range a.Params {
					if !Identical(a.Params[k].typ, b.Params[k].typ) {
						same = false
						break
					}
				}
				if same {
					c.errorf(fs[j].pos, "ambiguous overload of %s: same parameter types as declaration at %s", what, fs[i].pos)
				}
			}
		}
	}
	if fsAll := c.pkg.Scope.elems; fsAll != nil {
		for name, o := range fsAll {
			if set, ok := o.(*FuncSet); ok {
				check(set.Funcs, name)
			}
		}
	}
	for _, n := range c.pkg.Types {
		for name, ms := range n.Methods {
			if len(ms) > 1 {
				check(ms, n.Obj.name+"."+name)
			}
		}
	}
}

// ---------------------------------------------------------------- type expressions

func (c *checker) typExpr(e ast.Expr) Type {
	t := c.typExprInner(e)
	c.info.Types[e] = TypeAndValue{Mode: TypeExpr, Type: t}
	return t
}

func (c *checker) typExprInner(e ast.Expr) Type {
	switch e := e.(type) {
	case *ast.Ident:
		obj := c.lookup(e.Name)
		if obj == nil {
			c.errorf(e.Pos(), "undefined: %s", e.Name)
			return Typ[Invalid]
		}
		c.info.Uses[e] = obj
		if _, ok := obj.(*GenericType); ok {
			c.errorf(e.Pos(), "generic type %s requires type arguments: %s[...]", e.Name, e.Name)
			return Typ[Invalid]
		}
		tn, ok := obj.(*TypeName)
		if !ok {
			c.errorf(e.Pos(), "%s is not a type", e.Name)
			return Typ[Invalid]
		}
		if !c.inConstraint {
			if it, ok := tn.typ.Underlying().(*Interface); ok && it.IsConstraint() {
				c.errorf(e.Pos(), "cannot use constraint %s outside a type parameter list", e.Name)
				return Typ[Invalid]
			}
		}
		if u, ok := tn.typ.(*Unsupported); ok {
			c.errorf(e.Pos(), "type %s is not supported yet: %s", u.Name, u.Stage)
			return Typ[Invalid]
		}
		return tn.typ
	case *ast.ParenExpr:
		return c.typExpr(e.X)
	case *ast.StarExpr:
		return &Pointer{Elem: c.typExpr(e.X)}
	case *ast.ArrayType:
		elem := c.typExpr(e.Elt)
		if e.Len == nil {
			return &Slice{Elem: elem}
		}
		if _, ok := e.Len.(*ast.Ellipsis); ok {
			c.errorf(e.Pos(), "[...]T is only allowed in composite literals (not supported yet)")
			return Typ[Invalid]
		}
		x := c.expr(e.Len)
		if x.mode != Const || !IsInteger(x.typ) {
			c.errorf(e.Len.Pos(), "array length must be a constant integer")
			return Typ[Invalid]
		}
		n, ok := constant.Int64Val(constant.ToInt(x.val))
		if !ok || n < 0 {
			c.errorf(e.Len.Pos(), "invalid array length")
			return Typ[Invalid]
		}
		return &Array{Len: n, Elem: elem}
	case *ast.StructType:
		st := &Struct{}
		seen := map[string]bool{}
		for _, f := range e.Fields.List {
			ft := c.typExpr(f.Type)
			if len(f.Names) == 0 {
				c.errorf(f.Pos(), "embedded fields are not supported yet")
				continue
			}
			for _, n := range f.Names {
				if seen[n.Name] && n.Name != "_" {
					c.errorf(n.Pos(), "duplicate field %s", n.Name)
				}
				seen[n.Name] = true
				st.Fields = append(st.Fields, &Field{Name: n.Name, Type: ft, Pos: n.Pos()})
			}
		}
		return st
	case *ast.FuncType:
		return c.signature(e, nil)
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if pn, ok := c.lookup(id.Name).(*PkgName); ok {
				pn.used = true
				c.info.Uses[id] = pn
				if tn, ok := StdMembers[pn.Path][e.Sel.Name].(*TypeName); ok {
					return tn.typ
				}
				c.errorf(e.Sel.Pos(), "undefined type %s.%s", id.Name, e.Sel.Name)
				return Typ[Invalid]
			}
		}
		c.errorf(e.Pos(), "%s is not a type", exprString(e))
		return Typ[Invalid]
	case *ast.MapType:
		return c.mapType(e)
	case *ast.ChanType:
		return &Chan{Elem: c.typExpr(e.Value)}
	case *ast.InterfaceType:
		return c.interfaceType(e)
	case *ast.IndexExpr:
		if x, ok := c.genericIndex(e); ok {
			if x.mode == TypeExpr {
				return x.typ
			}
			if !isInvalid(x) {
				c.errorf(e.Pos(), "%s is not a type", exprString(e))
			}
			return Typ[Invalid]
		}
	}
	c.errorf(e.Pos(), "%s is not a type", exprString(e))
	return Typ[Invalid]
}

func (c *checker) lookup(name string) Object {
	s := c.scope
	if s == nil {
		s = c.fileScope()
	}
	o, _ := s.LookupParent(name)
	return o
}

func (c *checker) fieldVars(fl *ast.FieldList, variadicOK bool) ([]*Var, bool) {
	if fl == nil {
		return nil, false
	}
	var vars []*Var
	variadic := false
	for i, f := range fl.List {
		te := f.Type
		if el, ok := te.(*ast.Ellipsis); ok {
			if !variadicOK || i != len(fl.List)-1 || len(f.Names) > 1 {
				c.errorf(el.Pos(), "can only use ... with final parameter")
			}
			variadic = true
			t := &Slice{Elem: c.typExpr(el.Elt)}
			c.info.Types[te] = TypeAndValue{Mode: TypeExpr, Type: t}
			name := "_"
			var pos token.Pos
			if len(f.Names) > 0 {
				name, pos = f.Names[0].Name, f.Names[0].Pos()
			}
			vars = append(vars, NewVar(name, t, pos))
			continue
		}
		t := c.typExpr(te)
		if len(f.Names) == 0 {
			vars = append(vars, NewVar("", t, te.Pos()))
			continue
		}
		for _, n := range f.Names {
			vars = append(vars, NewVar(n.Name, t, n.Pos()))
		}
	}
	return vars, variadic
}

func (c *checker) signature(ft *ast.FuncType, recv *ast.FieldList) *Signature {
	sig := &Signature{Throws: ft.Annots.Has("throws")}
	if recv != nil {
		rv, _ := c.fieldVars(recv, false)
		if len(rv) != 1 {
			c.errorf(recv.Pos(), "method must have exactly one receiver")
		} else {
			sig.Recv = rv[0]
		}
	}
	sig.Params, sig.Variadic = c.fieldVars(ft.Params, true)
	sig.Results, _ = c.fieldVars(ft.Results, false)
	return sig
}

// ---------------------------------------------------------------- values

func (c *checker) valueSpec(scope *Scope, s *ast.ValueSpec, isConst, global bool) []*Var {
	var typ Type
	if s.Type != nil {
		typ = c.typExpr(s.Type)
	}
	var vals []*operand
	if len(s.Values) > 0 {
		if len(s.Values) == 1 && len(s.Names) > 1 {
			x := c.expr(s.Values[0])
			if tup, ok := x.typ.(*Tuple); ok && len(tup.Vars) == len(s.Names) {
				for _, v := range tup.Vars {
					vals = append(vals, &operand{mode: Value, typ: v.typ})
				}
			} else {
				c.errorf(s.Pos(), "assignment mismatch: %d variables but 1 value", len(s.Names))
			}
		} else {
			if len(s.Values) != len(s.Names) {
				c.errorf(s.Pos(), "assignment mismatch: %d variables but %d values", len(s.Names), len(s.Values))
			}
			for _, v := range s.Values {
				if isConst {
					vals = append(vals, c.expr(v))
				} else {
					vals = append(vals, c.value(v))
				}
			}
		}
	} else if isConst {
		c.errorf(s.Pos(), "missing constant value")
	}

	var out []*Var
	for i, n := range s.Names {
		v := NewVar(n.Name, typ, n.Pos())
		v.Global = global
		v.IsConst = isConst
		if i < len(vals) {
			x := vals[i]
			if isConst && x.mode != Const {
				c.errorf(s.Values[i].Pos(), "%s is not constant", exprString(s.Values[i]))
			}
			if typ != nil {
				c.assignable(x, typ, "variable declaration")
			} else if isConst {
				v.typ = x.typ // untyped constants stay untyped
			} else {
				c.defaultType(x)
				v.typ = x.typ
			}
			if isConst && x.val != nil {
				v.Value = x.val
			}
			if x.expr != nil {
				c.record(x)
			}
		}
		if v.typ == nil {
			v.typ = Typ[Invalid]
		}
		c.declare(scope, n, v)
		if global && !isConst {
			c.pkg.Globals = append(c.pkg.Globals, v)
		}
		out = append(out, v)
	}
	return out
}

// exprString renders an expression for messages.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprString(x.Fun) + "(...)"
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.UnaryExpr:
		return x.Op.String() + exprString(x.X)
	case *ast.BinaryExpr:
		return exprString(x.X) + " " + x.Op.String() + " " + exprString(x.Y)
	case *ast.IndexExpr:
		return exprString(x.X) + "[...]"
	case *ast.ParenExpr:
		return "(" + exprString(x.X) + ")"
	case *ast.ArrayType:
		if x.Len == nil {
			return "[]" + exprString(x.Elt)
		}
		return "[" + exprString(x.Len) + "]" + exprString(x.Elt)
	case *ast.CompositeLit:
		if x.Type != nil {
			return exprString(x.Type) + "{...}"
		}
		return "{...}"
	}
	return fmt.Sprintf("%T", e)
}
