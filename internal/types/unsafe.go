package types

// unsafe { ... } blocks: manual memory (malloc/free), pointer arithmetic and
// pointer conversions. Memory from malloc is not scanned by the GC, so only
// pointer-free ("plain") types may be accessed through converted pointers.

import (
	"go/constant"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// unsafe builtins (visible only inside unsafe blocks)
const (
	BMalloc BuiltinID = 100 + iota
	BCalloc
	BRealloc
	BFree
	BMemcpy
	BMemset
	BSliceOf
)

var unsafeBuiltins = map[string]BuiltinID{
	"malloc": BMalloc, "calloc": BCalloc, "realloc": BRealloc, "free": BFree,
	"memcpy": BMemcpy, "memset": BMemset, "slice": BSliceOf,
}

// PtrOp classifies pointer arithmetic.
type PtrOp int

const (
	PtrAdd  PtrOp = iota + 1 // p + n, n + p, p - n
	PtrDiff                  // p - q (elements)
)

// Plain reports whether values of t contain no GC pointers (numbers, bools,
// arrays and structs of them): only such values may live in malloc memory.
func Plain(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.IsNumeric() || u.IsBoolean()
	case *Array:
		return Plain(u.Elem)
	case *Struct:
		for _, f := range u.Fields {
			if !Plain(f.Type) {
				return false
			}
		}
		return true
	}
	return false
}

func isPtr(t Type) (*Pointer, bool) {
	p, ok := t.Underlying().(*Pointer)
	return p, ok
}

func isUintptr(t Type) bool {
	b, ok := t.Underlying().(*Basic)
	return ok && b.Kind == Uintptr
}

// unsafeBlock checks unsafe { ... }.
func (c *checker) unsafeBlock(s *ast.UnsafeStmt) {
	c.openScope("unsafe")
	for name, id := range unsafeBuiltins {
		c.scope.elems[name] = &Builtin{object: object{name: name}, ID: id}
	}
	c.unsafeDepth++
	c.openScope("block")
	c.stmtList(s.Body.List)
	c.closeScope()
	c.unsafeDepth--
	c.closeScope()
}

// checkUnsafeBuiltinName reports a user declaration hidden by an unsafe builtin.
func (c *checker) checkUnsafeBuiltinName(e *ast.Ident) bool {
	for s := c.scope; s != nil; s = s.Parent {
		if s.Kind == "unsafe" {
			if prev, _ := s.Parent.LookupParent(e.Name); prev != nil && prev.Pos().Line != 0 && !c.otherPackage(prev, e.Pos()) {
				c.errorf(e.Pos(), "%s inside unsafe is the builtin, but %s is also declared at %s; rename it", e.Name, e.Name, prev.Pos())
				return false
			}
			return true
		}
	}
	return true
}

func (c *checker) requireUnsafe(pos token.Pos, what string) bool {
	if c.unsafeDepth == 0 {
		c.errorf(pos, "%s requires an unsafe block", what)
		return false
	}
	return true
}

// ptrArith checks p + n, n + p, p - n and p - q.
func (c *checker) ptrArith(e ast.Node, op token.Kind, x, y *operand) (*operand, bool) {
	px, xp := isPtr(x.typ)
	_, yp := isPtr(y.typ)
	if !xp && !yp || (op != token.ADD && op != token.SUB) {
		return nil, false
	}
	if !c.requireUnsafe(e.Pos(), "pointer arithmetic") {
		return c.invalid(nil), true
	}
	if xp && yp {
		if op != token.SUB || !Identical(x.typ, y.typ) {
			c.errorf(e.Pos(), "invalid operation: %s %s %s (only p - q of the same pointer type)", x.typ, op, y.typ)
			return c.invalid(nil), true
		}
		c.info.PtrOps[e] = PtrDiff
		return &operand{mode: Value, typ: Typ[Int]}, true
	}
	p, n := x, y
	if yp {
		if op == token.SUB {
			c.errorf(e.Pos(), "invalid operation: integer - pointer")
			return c.invalid(nil), true
		}
		p, n = y, x
	} else {
		_ = px
	}
	if isUntyped(n.typ) {
		if !c.convertUntyped(n, Typ[Int]) {
			return c.invalid(nil), true
		}
	}
	if !IsInteger(n.typ) {
		c.errorf(e.Pos(), "invalid operation: pointer offset must be an integer, not %s", n.typ)
		return c.invalid(nil), true
	}
	c.record(n)
	c.info.PtrOps[e] = PtrAdd
	return &operand{mode: Value, typ: p.typ}, true
}

// ptrConversion checks *T <-> *U and pointer <-> uintptr conversions.
func (c *checker) ptrConversion(e *ast.CallExpr, x *operand, t Type) (*operand, bool) {
	_, sp := isPtr(x.typ)
	tp, dp := isPtr(t)
	if !(sp && dp && !Identical(x.typ.Underlying(), t.Underlying())) && !(sp && isUintptr(t)) && !(isUintptr(x.typ) && dp) {
		return nil, false
	}
	if !c.requireUnsafe(e.Pos(), "pointer conversion") {
		return c.invalid(e), true
	}
	if dp && !Plain(tp.Elem) {
		c.errorf(e.Pos(), "cannot convert to %s: %s contains GC pointers (only numbers, bools and arrays/structs of them may be accessed through raw pointers)", t, tp.Elem)
		return c.invalid(e), true
	}
	return &operand{mode: Value, typ: t}, true
}

func (c *checker) unsafeBuiltin(e *ast.CallExpr, b *Builtin, ci *CallInfo) *operand {
	nargs := len(e.Args)
	need := func(n int) bool {
		if nargs != n {
			c.errorf(e.Pos(), "wrong number of arguments to %s: have %d, want %d", b.name, nargs, n)
			return false
		}
		return true
	}
	ptrArg := func(i int, plainOnly bool) *operand {
		x := c.value(e.Args[i])
		if isInvalid(x) {
			return nil
		}
		p, ok := isPtr(x.typ)
		if !ok {
			c.errorf(e.Args[i].Pos(), "argument to %s must be a pointer, not %s", b.name, x.typ)
			return nil
		}
		if plainOnly && !Plain(p.Elem) {
			c.errorf(e.Args[i].Pos(), "%s: %s contains GC pointers", b.name, p.Elem)
			return nil
		}
		return x
	}
	intArg := func(i int, t Type) bool {
		x := c.value(e.Args[i])
		if isInvalid(x) {
			return false
		}
		if isUntyped(x.typ) {
			if !c.convertUntyped(x, t) {
				return false
			}
		} else if !IsInteger(x.typ) {
			c.errorf(e.Args[i].Pos(), "argument to %s must be an integer, not %s", b.name, x.typ)
			return false
		}
		if x.val != nil && constant.Sign(x.val) < 0 {
			c.errorf(e.Args[i].Pos(), "negative size in %s", b.name)
			return false
		}
		c.record(x)
		return true
	}
	bytePtr := &operand{mode: Value, typ: &Pointer{Elem: Typ[Uint8]}}
	switch b.ID {
	case BMalloc, BCalloc:
		if need(1) && intArg(0, Typ[Int]) {
			return bytePtr
		}
	case BRealloc:
		if need(2) && ptrArg(0, true) != nil && intArg(1, Typ[Int]) {
			return bytePtr
		}
	case BFree:
		if need(1) {
			x := c.value(e.Args[0])
			if isInvalid(x) {
				break
			}
			if _, ok := isPtr(x.typ); !ok && x.typ != Typ[UntypedNil] {
				c.errorf(e.Args[0].Pos(), "argument to free must be a pointer, not %s", x.typ)
				break
			}
			if x.typ == Typ[UntypedNil] {
				c.convertUntyped(x, &Pointer{Elem: Typ[Uint8]})
			}
			c.record(x)
			return &operand{mode: NoValue}
		}
	case BMemcpy:
		if need(3) && ptrArg(0, true) != nil && ptrArg(1, true) != nil && intArg(2, Typ[Int]) {
			return &operand{mode: NoValue}
		}
	case BMemset:
		if need(3) && ptrArg(0, true) != nil && intArg(1, Typ[Uint8]) && intArg(2, Typ[Int]) {
			return &operand{mode: NoValue}
		}
	case BSliceOf:
		if need(2) {
			x := ptrArg(0, false)
			if x != nil && intArg(1, Typ[Int]) {
				p, _ := isPtr(x.typ)
				ci.TypeArg = p.Elem
				return &operand{mode: Value, typ: &Slice{Elem: p.Elem}}
			}
		}
	}
	return c.invalid(e)
}

// ---------------------------------------------------------------- intrinsics

// Swappable reports whether @be_to_le / @le_to_be apply to t.
func Swappable(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.IsNumeric() && !isUntyped(u)
	case *Array:
		return Swappable(u.Elem)
	case *Struct:
		for _, f := range u.Fields {
			if !Swappable(f.Type) {
				return false
			}
		}
		return len(u.Fields) > 0
	}
	return false
}

func (c *checker) intrinsic(e *ast.IntrinsicExpr) *operand {
	switch e.Name {
	case "be_to_le", "le_to_be":
		if len(e.Args) != 1 {
			c.errorf(e.Pos(), "@%s takes exactly one argument", e.Name)
			return c.invalid(e)
		}
		x := c.value(e.Args[0])
		if isInvalid(x) {
			return c.invalid(e)
		}
		c.defaultType(x)
		c.record(x)
		if !Swappable(x.typ) {
			c.errorf(e.Pos(), "@%s requires an integer/float type or an array/struct of them, not %s", e.Name, x.typ)
			return c.invalid(e)
		}
		return &operand{mode: Value, typ: x.typ}
	}
	c.errorf(e.Pos(), "unknown intrinsic @%s (available: @be_to_le, @le_to_be)", e.Name)
	return c.invalid(e)
}
