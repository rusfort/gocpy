// Package types implements the Gocpy type checker.
package types

import (
	"fmt"
	"go/constant"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// ---------------------------------------------------------------- types

// Type is a Gocpy type.
type Type interface {
	String() string
	Underlying() Type
}

type BasicKind int

const (
	Invalid BasicKind = iota
	Bool
	Int
	Int8
	Int16
	Int32
	Int64
	Uint
	Uint8
	Uint16
	Uint32
	Uint64
	Uintptr
	Float32
	Float64
	String

	UntypedBool
	UntypedInt
	UntypedRune
	UntypedFloat
	UntypedString
	UntypedNil
)

type Basic struct {
	Kind BasicKind
	Name string
}

func (b *Basic) String() string   { return b.Name }
func (b *Basic) Underlying() Type { return b }
func (b *Basic) IsUntyped() bool  { return b.Kind >= UntypedBool }
func (b *Basic) IsInteger() bool {
	return (b.Kind >= Int && b.Kind <= Uintptr) || b.Kind == UntypedInt || b.Kind == UntypedRune
}
func (b *Basic) IsUnsigned() bool { return b.Kind >= Uint && b.Kind <= Uintptr }
func (b *Basic) IsFloat() bool {
	return b.Kind == Float32 || b.Kind == Float64 || b.Kind == UntypedFloat
}
func (b *Basic) IsNumeric() bool { return b.IsInteger() || b.IsFloat() }
func (b *Basic) IsString() bool  { return b.Kind == String || b.Kind == UntypedString }
func (b *Basic) IsBoolean() bool { return b.Kind == Bool || b.Kind == UntypedBool }
func (b *Basic) IsOrdered() bool { return b.IsNumeric() || b.IsString() }

// Size in bits for integer and float kinds (int/uint are 64-bit).
func (b *Basic) Bits() int {
	switch b.Kind {
	case Int8, Uint8:
		return 8
	case Int16, Uint16:
		return 16
	case Int32, Uint32, Float32:
		return 32
	}
	return 64
}

// Typ holds the predeclared basic types.
var Typ = map[BasicKind]*Basic{}

func init() {
	for _, b := range []*Basic{
		{Invalid, "invalid"}, {Bool, "bool"}, {Int, "int"}, {Int8, "int8"}, {Int16, "int16"},
		{Int32, "int32"}, {Int64, "int64"}, {Uint, "uint"}, {Uint8, "uint8"}, {Uint16, "uint16"},
		{Uint32, "uint32"}, {Uint64, "uint64"}, {Uintptr, "uintptr"}, {Float32, "float32"},
		{Float64, "float64"}, {String, "string"},
		{UntypedBool, "untyped bool"}, {UntypedInt, "untyped int"}, {UntypedRune, "untyped rune"},
		{UntypedFloat, "untyped float"}, {UntypedString, "untyped string"}, {UntypedNil, "untyped nil"},
	} {
		Typ[b.Kind] = b
	}
}

type Pointer struct{ Elem Type }
type Slice struct{ Elem Type }
type Array struct {
	Len  int64
	Elem Type
}

type Field struct {
	Name string
	Type Type
	Pos  token.Pos
}

type Struct struct {
	Fields []*Field
	Packed bool
	Align  int64
}

func (s *Struct) Field(name string) (int, *Field) {
	for i, f := range s.Fields {
		if f.Name == name {
			return i, f
		}
	}
	return -1, nil
}

// Named is a declared type.
type Named struct {
	Obj        *TypeName
	underlying Type
	Methods    map[string][]*Func // >1 entries only for @overload / operators
	CName      string             // C type for std library types (runtime-provided)
	Pkg        string             // package name for std types ("sync"), "" for the current package
	Origin     *GenericType       // instance of a generic type
	TArgs      []Type             // type arguments of an instance
	display    string             // List[int] for instances
}

// Signature is a function type.
type Signature struct {
	Recv     *Var
	Params   []*Var
	Results  []*Var
	Variadic bool
	Throws   bool
}

// Map is map[K]V. Iteration order is insertion order.
type Map struct{ Key, Elem Type }

func (t *Map) Underlying() Type { return t }
func (t *Map) String() string   { return "map[" + t.Key.String() + "]" + t.Elem.String() }

// IMethod is a method of an interface.
type IMethod struct {
	Name string
	Sig  *Signature // Recv is nil
}

// Interface is an interface type; Methods are sorted by name (itab order).
type Interface struct {
	Methods    []*IMethod
	Terms      []Term // type set of a constraint interface (~int | float64)
	Comparable bool   // constraint embeds comparable
}

func (t *Interface) Underlying() Type { return t }
func (t *Interface) String() string {
	if len(t.Methods) == 0 {
		return "any"
	}
	var ms []string
	for _, m := range t.Methods {
		ms = append(ms, m.Name+strings.TrimPrefix(m.Sig.String(), "func"))
	}
	return "interface{ " + strings.Join(ms, "; ") + " }"
}

// Method returns the index and method with the given name.
func (t *Interface) Method(name string) (int, *IMethod) {
	for i, m := range t.Methods {
		if m.Name == name {
			return i, m
		}
	}
	return -1, nil
}

// Chan is a channel type: chan T.
type Chan struct{ Elem Type }

func (t *Chan) Underlying() Type { return t }
func (t *Chan) String() string   { return "chan " + t.Elem.String() }

// Tuple is the type of a multi-value call.
type Tuple struct{ Vars []*Var }

// Unsupported marks predeclared types not implemented yet (any, error).
type Unsupported struct{ Name, Stage string }

func (t *Pointer) Underlying() Type     { return t }
func (t *Slice) Underlying() Type       { return t }
func (t *Array) Underlying() Type       { return t }
func (t *Struct) Underlying() Type      { return t }
func (t *Named) Underlying() Type       { return t.underlying }
func (t *Signature) Underlying() Type   { return t }
func (t *Tuple) Underlying() Type       { return t }
func (t *Unsupported) Underlying() Type { return t }

func (t *Pointer) String() string { return "*" + t.Elem.String() }
func (t *Slice) String() string   { return "[]" + t.Elem.String() }
func (t *Array) String() string   { return fmt.Sprintf("[%d]%s", t.Len, t.Elem) }
func (t *Named) String() string {
	if t.display != "" {
		return Demangle(t.display)
	}
	if t.Pkg != "" {
		return t.Pkg + "." + t.Obj.name
	}
	return Demangle(t.Obj.name)
}

// Demangle maps package-prefixed names of imported Gocpy packages
// ("geo__Point") back to their source spelling ("geo.Point"). The build
// driver installs it when a program consists of several packages.
var Demangle = func(s string) string { return s }

func (t *Unsupported) String() string { return t.Name }
func (t *Struct) String() string {
	var parts []string
	for _, f := range t.Fields {
		parts = append(parts, f.Name+" "+f.Type.String())
	}
	return "struct{" + strings.Join(parts, "; ") + "}"
}
func (t *Signature) String() string {
	var ps []string
	for i, p := range t.Params {
		s := p.typ.String()
		if t.Variadic && i == len(t.Params)-1 {
			s = "..." + t.Params[i].typ.(*Slice).Elem.String()
		}
		ps = append(ps, s)
	}
	s := "func(" + strings.Join(ps, ", ") + ")"
	switch len(t.Results) {
	case 0:
	case 1:
		s += " " + t.Results[0].typ.String()
	default:
		var rs []string
		for _, r := range t.Results {
			rs = append(rs, r.typ.String())
		}
		s += " (" + strings.Join(rs, ", ") + ")"
	}
	if t.Throws {
		s = "@throws " + s
	}
	return s
}
func (t *Tuple) String() string {
	var rs []string
	for _, r := range t.Vars {
		rs = append(rs, r.typ.String())
	}
	return "(" + strings.Join(rs, ", ") + ")"
}

// Identical reports whether two types are identical.
func Identical(a, b Type) bool {
	if a == b {
		return true
	}
	switch x := a.(type) {
	case *Pointer:
		y, ok := b.(*Pointer)
		return ok && Identical(x.Elem, y.Elem)
	case *Slice:
		y, ok := b.(*Slice)
		return ok && Identical(x.Elem, y.Elem)
	case *Chan:
		y, ok := b.(*Chan)
		return ok && Identical(x.Elem, y.Elem)
	case *Map:
		y, ok := b.(*Map)
		return ok && Identical(x.Key, y.Key) && Identical(x.Elem, y.Elem)
	case *Interface:
		y, ok := b.(*Interface)
		if !ok || len(x.Methods) != len(y.Methods) {
			return false
		}
		for i, m := range x.Methods {
			if m.Name != y.Methods[i].Name || !Identical(m.Sig, y.Methods[i].Sig) {
				return false
			}
		}
		return true
	case *Array:
		y, ok := b.(*Array)
		return ok && x.Len == y.Len && Identical(x.Elem, y.Elem)
	case *Struct:
		y, ok := b.(*Struct)
		if !ok || len(x.Fields) != len(y.Fields) {
			return false
		}
		for i := range x.Fields {
			if x.Fields[i].Name != y.Fields[i].Name || !Identical(x.Fields[i].Type, y.Fields[i].Type) {
				return false
			}
		}
		return true
	case *Signature:
		y, ok := b.(*Signature)
		if !ok || x.Variadic != y.Variadic || x.Throws != y.Throws ||
			len(x.Params) != len(y.Params) || len(x.Results) != len(y.Results) {
			return false
		}
		for i := range x.Params {
			if !Identical(x.Params[i].typ, y.Params[i].typ) {
				return false
			}
		}
		for i := range x.Results {
			if !Identical(x.Results[i].typ, y.Results[i].typ) {
				return false
			}
		}
		return true
	}
	return false
}

func basic(t Type) *Basic {
	if t == nil {
		return nil
	}
	b, _ := t.Underlying().(*Basic)
	return b
}

func isUntyped(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.IsUntyped()
}

// IsString etc. are exported helpers for codegen.
func IsString(t Type) bool  { b := basic(t); return b != nil && b.IsString() }
func IsInteger(t Type) bool { b := basic(t); return b != nil && b.IsInteger() }
func IsFloat(t Type) bool   { b := basic(t); return b != nil && b.IsFloat() }
func IsUnsigned(t Type) bool {
	b := basic(t)
	return b != nil && b.IsUnsigned()
}
func IsBool(t Type) bool { b := basic(t); return b != nil && b.IsBoolean() }

// Default returns the default type for untyped constants.
func Default(t Type) Type {
	if b, ok := t.(*Basic); ok {
		switch b.Kind {
		case UntypedBool:
			return Typ[Bool]
		case UntypedInt:
			return Typ[Int]
		case UntypedRune:
			return Typ[Int32]
		case UntypedFloat:
			return Typ[Float64]
		case UntypedString:
			return Typ[String]
		}
	}
	return t
}

// ---------------------------------------------------------------- objects

// Object is a named language entity.
type Object interface {
	Name() string
	Type() Type
	Pos() token.Pos
}

type object struct {
	name string
	typ  Type
	pos  token.Pos
}

func (o *object) Name() string   { return o.name }
func (o *object) Type() Type     { return o.typ }
func (o *object) Pos() token.Pos { return o.pos }

// Var is a variable, parameter, result or constant.
type Var struct {
	object
	Global  bool
	IsConst bool
	Value   constant.Value // for constants
	CName   string         // C identifier, assigned by codegen
	Boxed   bool           // address taken or captured: allocated on the GC heap
	used    bool
	owner   *funcCtx // function (or literal) that declares the variable
}

func NewVar(name string, t Type, pos token.Pos) *Var {
	return &Var{object: object{name: name, typ: t, pos: pos}}
}

// TypeName is a declared or predeclared type name.
type TypeName struct{ object }

// Func is a declared function, method or operator method.
type Func struct {
	object
	Decl       *ast.FuncDecl
	RecvNamed  *Named
	PtrRecv    bool
	Overloaded bool
	IsOperator bool
	Extern     string // C name for @extern functions without body
	Export     string // C name from @export
	CName      string // assigned by codegen
	TScope     *Scope // instances: scope binding the type parameters
	Suffix     string // instances: C name suffix (__int)
	Inst       string // instances: Max[int] (for diagnostics)
}

func (f *Func) Sig() *Signature { return f.typ.(*Signature) }

// FuncSet is a group of @overload functions sharing a name.
type FuncSet struct {
	object
	Funcs []*Func
}

// PkgName is an imported package.
type PkgName struct {
	object
	Path string
	used bool
}

// BuiltinID identifies builtin functions.
type BuiltinID int

const (
	BLen BuiltinID = iota
	BCap
	BAppend
	BMake
	BNew
	BSizeof
	BClose
	BDelete
	// std/fmt
	FmtPrintln
	FmtPrint
	FmtSprint
)

// Builtin is a builtin function (len, append...) or std function (fmt.Println).
type Builtin struct {
	object
	ID BuiltinID
}

// Nil is the predeclared nil.
type Nil struct{ object }

// ---------------------------------------------------------------- scopes

// Scope is a lexical scope.
type Scope struct {
	Parent *Scope
	elems  map[string]Object
	Kind   string // "universe", "package", "file", "func", "block"
}

func NewScope(parent *Scope, kind string) *Scope {
	return &Scope{Parent: parent, elems: map[string]Object{}, Kind: kind}
}

func (s *Scope) Lookup(name string) Object { return s.elems[name] }

// LookupParent walks up the scope chain.
func (s *Scope) LookupParent(name string) (Object, *Scope) {
	for ; s != nil; s = s.Parent {
		if o := s.elems[name]; o != nil {
			return o, s
		}
	}
	return nil, nil
}

func (s *Scope) Insert(o Object) Object {
	if prev := s.elems[o.Name()]; prev != nil {
		return prev
	}
	s.elems[o.Name()] = o
	return nil
}

// ---------------------------------------------------------------- universe

// Universe is the outermost scope.
var Universe *Scope

// AnyType is the empty interface (predeclared any).
var AnyType = &Interface{}

// ErrorType is the predeclared interface error { Error() string }.
var ErrorType *Named

// ExceptionType is the predeclared type Exception (a caught exception of any type).
var ExceptionType *Named

func init() {
	Universe = NewScope(nil, "universe")
	def := func(name string, t Type) {
		Universe.Insert(&TypeName{object{name: name, typ: t}})
	}
	for _, k := range []BasicKind{Bool, Int, Int8, Int16, Int32, Int64, Uint, Uint8, Uint16,
		Uint32, Uint64, Uintptr, Float32, Float64, String} {
		def(Typ[k].Name, Typ[k])
	}
	def("byte", Typ[Uint8])
	def("rune", Typ[Int32])
	def("any", AnyType)
	errObj := &TypeName{object{name: "error"}}
	ErrorType = &Named{Obj: errObj, Methods: map[string][]*Func{}, CName: "gocpy_iface"}
	ErrorType.underlying = &Interface{Methods: []*IMethod{{Name: "Error",
		Sig: &Signature{Results: []*Var{NewVar("", Typ[String], token.Pos{})}}}}}
	errObj.typ = ErrorType
	Universe.Insert(errObj)

	// Exception: the type of a catch-all variable (`catch e { ... }`).
	excObj := &TypeName{object{name: "Exception"}}
	ExceptionType = &Named{Obj: excObj, underlying: &Pointer{Elem: Typ[Uint8]}, Methods: map[string][]*Func{}}
	excObj.typ = ExceptionType
	Universe.Insert(excObj)
	for name, cname := range map[string]string{
		"Message": "gocpy_exc_message", "TypeName": "gocpy_exc_typename", "String": "gocpy_exc_string",
	} {
		sig := &Signature{Recv: NewVar("e", ExceptionType, token.Pos{}), Results: []*Var{NewVar("", Typ[String], token.Pos{})}}
		ExceptionType.Methods[name] = []*Func{{object: object{name: name, typ: sig}, RecvNamed: ExceptionType, CName: cname}}
	}

	Universe.Insert(&Var{object: object{name: "true", typ: Typ[UntypedBool]}, IsConst: true, Value: constant.MakeBool(true)})
	Universe.Insert(&Var{object: object{name: "false", typ: Typ[UntypedBool]}, IsConst: true, Value: constant.MakeBool(false)})
	Universe.Insert(&Nil{object{name: "nil", typ: Typ[UntypedNil]}})

	for name, id := range map[string]BuiltinID{
		"len": BLen, "cap": BCap, "append": BAppend, "make": BMake, "new": BNew, "sizeof": BSizeof,
		"close":  BClose,
		"delete": BDelete,
	} {
		Universe.Insert(&Builtin{object{name: name}, id})
	}
	initStd()
	initErrors()
	initGenerics()
}

// StdPackages lists std packages and their exported builtins (stage 2).
var StdPackages = map[string]map[string]BuiltinID{
	"std/fmt": {"Println": FmtPrintln, "Print": FmtPrint, "Sprint": FmtSprint},
}
