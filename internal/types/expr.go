package types

import (
	"fmt"
	"go/constant"
	gotoken "go/token"
	"strconv"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

type operand struct {
	mode Mode
	typ  Type
	val  constant.Value
	expr ast.Expr
	obj  Object
	sel  *Selection
	recv *operand // receiver operand for method values
}

func (c *checker) invalid(e ast.Expr) *operand {
	return &operand{mode: Value, typ: Typ[Invalid], expr: e}
}

func isInvalid(x *operand) bool { return x.typ == Typ[Invalid] }

func (c *checker) record(x *operand) {
	if x.expr != nil {
		c.info.Types[x.expr] = TypeAndValue{Mode: x.mode, Type: x.typ, Value: x.val}
	}
}

// expr checks e and records its type.
func (c *checker) expr(e ast.Expr) *operand {
	x := c.exprInner(e)
	x.expr = e
	c.record(x)
	return x
}

// value checks e and requires a single value.
func (c *checker) value(e ast.Expr) *operand {
	x := c.expr(e)
	switch x.mode {
	case NoValue:
		c.errorf(e.Pos(), "%s (no value) used as value", exprString(e))
		return c.invalid(e)
	case TypeExpr:
		c.errorf(e.Pos(), "%s (type) is not an expression", exprString(e))
		return c.invalid(e)
	case FuncM:
		return c.funcValue(x, e)
	case BuiltinM:
		c.errorf(e.Pos(), "builtin %s must be called", exprString(e))
		return c.invalid(e)
	case PkgM:
		c.errorf(e.Pos(), "use of package %s without selector", exprString(e))
		return c.invalid(e)
	}
	if _, ok := x.typ.(*Tuple); ok {
		c.errorf(e.Pos(), "multiple-value %s in single-value context", exprString(e))
		return c.invalid(e)
	}
	return x
}

func (c *checker) exprInner(e ast.Expr) *operand {
	switch e := e.(type) {
	case *ast.Ident:
		return c.ident(e)
	case *ast.BasicLit:
		return c.basicLit(e)
	case *ast.FString:
		for _, p := range e.Parts {
			if p.Expr == nil {
				continue
			}
			x := c.value(p.Expr)
			if isInvalid(x) {
				continue
			}
			c.defaultType(x)
			c.record(x)
			if !c.printable(x.typ) {
				c.errorf(p.Expr.Pos(), "cannot format %s of type %s", exprString(p.Expr), x.typ)
			}
			if p.Spec != "" {
				sp, err := ParseSpec(p.Spec)
				if err != nil {
					c.errorf(e.Pos(), "f-string format spec %q: %v", p.Spec, err)
				} else if msg := sp.Check(x.typ); msg != "" {
					c.errorf(e.Pos(), "f-string format spec %q for %s: %s", p.Spec, x.typ, msg)
				}
			}
		}
		return &operand{mode: Value, typ: Typ[String]}
	case *ast.ParenExpr:
		x := c.expr(e.X)
		return &operand{mode: x.mode, typ: x.typ, val: x.val, obj: x.obj, sel: x.sel, recv: x.recv}
	case *ast.CompositeLit:
		return c.compositeLit(e, nil)
	case *ast.SelectorExpr:
		return c.selector(e)
	case *ast.IndexExpr:
		return c.index(e)
	case *ast.SliceExpr:
		return c.sliceExpr(e)
	case *ast.CallExpr:
		return c.call(e)
	case *ast.StarExpr:
		x := c.expr(e.X)
		if x.mode == TypeExpr {
			return &operand{mode: TypeExpr, typ: &Pointer{Elem: x.typ}}
		}
		x = c.checkValue(x)
		if isInvalid(x) {
			return x
		}
		p, ok := x.typ.Underlying().(*Pointer)
		if !ok {
			c.errorf(e.Pos(), "invalid indirect of %s (type %s)", exprString(e.X), x.typ)
			return c.invalid(e)
		}
		return &operand{mode: Variable, typ: p.Elem}
	case *ast.UnaryExpr:
		return c.unary(e)
	case *ast.BinaryExpr:
		return c.binary(e)
	case *ast.ComprehensionExpr:
		return c.comprehension(e)
	case *ast.FuncLit:
		return c.funcLit(e)
	case *ast.TypeAssertExpr:
		return c.typeAssert(e)
	case *ast.PropagateExpr:
		return c.propagate(e)
	case *ast.IntrinsicExpr:
		return c.intrinsic(e)
	case *ast.KeyValueExpr:
		c.errorf(e.Pos(), "unexpected key:value expression")
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.StructType, *ast.InterfaceType, *ast.Ellipsis:
		if st, ok := e.(*ast.StructType); ok {
			c.errorf(st.Pos(), "anonymous struct types are not supported; declare a named type")
			return c.invalid(e)
		}
		return &operand{mode: TypeExpr, typ: c.typExprInner(e)}
	default:
		c.errorf(e.Pos(), "unexpected expression %T", e)
	}
	return c.invalid(e)
}

// checkValue converts non-value operands into errors (used after c.expr).
func (c *checker) checkValue(x *operand) *operand {
	switch x.mode {
	case NoValue, TypeExpr, BuiltinM, FuncM, PkgM:
		return c.value(x.expr)
	}
	if _, ok := x.typ.(*Tuple); ok {
		return c.value(x.expr)
	}
	return x
}

func (c *checker) ident(e *ast.Ident) *operand {
	if e.Name == "_" {
		c.errorf(e.Pos(), "cannot use _ as value")
		return c.invalid(e)
	}
	obj := c.lookup(e.Name)
	if obj == nil {
		if _, ok := unsafeBuiltins[e.Name]; ok {
			c.errorf(e.Pos(), "%s is only available inside an unsafe block", e.Name)
		} else {
			c.errorf(e.Pos(), "undefined: %s", e.Name)
		}
		return c.invalid(e)
	}
	c.info.Uses[e] = obj
	switch o := obj.(type) {
	case *Var:
		o.used = true
		if !o.Global && !o.IsConst && c.ctx != nil && o.owner != c.ctx && o.owner != nil {
			c.capture(o)
		}
		if o.IsConst {
			return &operand{mode: Const, typ: o.typ, val: o.Value, obj: o}
		}
		return &operand{mode: Variable, typ: o.typ, obj: o}
	case *TypeName:
		if u, ok := o.typ.(*Unsupported); ok {
			c.errorf(e.Pos(), "type %s is not supported yet: %s", u.Name, u.Stage)
			return c.invalid(e)
		}
		return &operand{mode: TypeExpr, typ: o.typ, obj: o}
	case *Func, *FuncSet:
		return &operand{mode: FuncM, typ: obj.Type(), obj: obj}
	case *GenericFunc:
		return &operand{mode: FuncM, typ: Typ[Invalid], obj: obj}
	case *GenericType:
		c.errorf(e.Pos(), "generic type %s requires type arguments: %s[...]", e.Name, e.Name)
		return c.invalid(e)
	case *Builtin:
		if o.ID >= BMalloc && !c.checkUnsafeBuiltinName(e) {
			return c.invalid(e)
		}
		return &operand{mode: BuiltinM, obj: o}
	case *PkgName:
		o.used = true
		return &operand{mode: PkgM, obj: o}
	case *Nil:
		return &operand{mode: Value, typ: Typ[UntypedNil], obj: o}
	}
	c.errorf(e.Pos(), "unexpected object %s", e.Name)
	return c.invalid(e)
}

func (c *checker) basicLit(e *ast.BasicLit) *operand {
	var k gotoken.Token
	var t *Basic
	switch e.Kind {
	case token.INT:
		k, t = gotoken.INT, Typ[UntypedInt]
	case token.FLOAT:
		k, t = gotoken.FLOAT, Typ[UntypedFloat]
	case token.CHAR:
		k, t = gotoken.CHAR, Typ[UntypedRune]
	case token.STRING:
		k, t = gotoken.STRING, Typ[UntypedString]
	}
	lit := e.Value
	if e.Kind == token.INT || e.Kind == token.FLOAT {
		lit = strings.ReplaceAll(lit, "_", "") // digit separators
	}
	v := constant.MakeFromLiteral(lit, k, 0)
	if v.Kind() == constant.Unknown {
		c.errorf(e.Pos(), "malformed literal %s", e.Value)
		return c.invalid(e)
	}
	return &operand{mode: Const, typ: t, val: v}
}

// ---------------------------------------------------------------- untyped conversion

func (c *checker) defaultType(x *operand) {
	if !isUntyped(x.typ) {
		return
	}
	if x.typ == Typ[UntypedNil] {
		c.errorf(x.expr.Pos(), "use of untyped nil")
		x.typ = Typ[Invalid]
		return
	}
	c.convertUntyped(x, Default(x.typ))
}

// representable converts constant v to basic type b, or returns an error.
func representable(v constant.Value, b *Basic) (constant.Value, string) {
	switch {
	case b.IsBoolean():
		if v.Kind() == constant.Bool {
			return v, ""
		}
	case b.IsString():
		if v.Kind() == constant.String {
			return v, ""
		}
	case b.IsInteger():
		if v.Kind() != constant.Int && v.Kind() != constant.Float {
			break
		}
		iv := constant.ToInt(v)
		if iv.Kind() != constant.Int {
			return nil, "truncated"
		}
		if b.IsUntyped() {
			return iv, ""
		}
		bits := uint(b.Bits())
		var lo, hi constant.Value
		if b.IsUnsigned() {
			lo = constant.MakeInt64(0)
			hi = constant.Shift(constant.MakeInt64(1), gotoken.SHL, bits)
			hi = constant.BinaryOp(hi, gotoken.SUB, constant.MakeInt64(1))
		} else {
			hi = constant.Shift(constant.MakeInt64(1), gotoken.SHL, bits-1)
			lo = constant.UnaryOp(gotoken.SUB, hi, 0)
			hi = constant.BinaryOp(hi, gotoken.SUB, constant.MakeInt64(1))
		}
		if constant.Compare(iv, gotoken.LSS, lo) || constant.Compare(iv, gotoken.GTR, hi) {
			return nil, "overflows"
		}
		return iv, ""
	case b.IsFloat():
		if v.Kind() != constant.Int && v.Kind() != constant.Float {
			break
		}
		fv := constant.ToFloat(v)
		if b.Kind == Float32 {
			if f, _ := constant.Float32Val(fv); f > 3.4028234663852886e38 || f < -3.4028234663852886e38 {
				return nil, "overflows"
			}
		}
		return fv, ""
	}
	return nil, "mismatched"
}

func (c *checker) canConvertUntyped(x *operand, target Type) bool {
	if !isUntyped(x.typ) {
		return false
	}
	if x.typ == Typ[UntypedNil] {
		switch target.Underlying().(type) {
		case *Pointer, *Slice, *Chan, *Map, *Signature, *Interface:
			return true
		}
		return false
	}
	b := basic(target)
	if b == nil {
		return false
	}
	if x.val == nil {
		return x.typ == Typ[UntypedBool] && b.IsBoolean()
	}
	_, msg := representable(x.val, b)
	return msg == ""
}

// convertUntyped gives untyped operand x the type target.
func (c *checker) convertUntyped(x *operand, target Type) bool {
	if !isUntyped(x.typ) || isUntyped(target) && x.typ == target {
		return true
	}
	if x.typ == Typ[UntypedNil] {
		switch target.Underlying().(type) {
		case *Pointer, *Slice, *Chan, *Map, *Signature, *Interface:
			x.typ = target
			c.updateExprType(x.expr, target, nil)
			return true
		}
		c.errorf(x.expr.Pos(), "cannot use nil as %s value", target)
		return false
	}
	b := basic(target)
	if b == nil {
		c.errorf(x.expr.Pos(), "cannot use %s (untyped constant) as %s value", exprString(x.expr), target)
		return false
	}
	if x.val == nil {
		if x.typ == Typ[UntypedBool] && b.IsBoolean() {
			x.typ = target
			c.updateExprType(x.expr, target, nil)
			return true
		}
		c.errorf(x.expr.Pos(), "cannot use %s as %s value", exprString(x.expr), target)
		return false
	}
	v, msg := representable(x.val, b)
	if msg != "" {
		switch msg {
		case "mismatched":
			c.errorf(x.expr.Pos(), "cannot use %s (%s) as %s value", exprString(x.expr), x.typ, target)
		default:
			c.errorf(x.expr.Pos(), "constant %s %s %s", x.val, msg, target)
		}
		x.typ = Typ[Invalid]
		return false
	}
	x.typ, x.val = target, v
	c.updateExprType(x.expr, target, v)
	return true
}

func (c *checker) updateExprType(e ast.Expr, t Type, v constant.Value) {
	if e == nil {
		return
	}
	old, ok := c.info.Types[e]
	if !ok || !isUntyped(old.Type) {
		return
	}
	nv := old.Value
	if v != nil {
		nv = v
	} else if nv != nil {
		if b := basic(t); b != nil {
			if cv, msg := representable(nv, b); msg == "" {
				nv = cv
			}
		}
	}
	c.info.Types[e] = TypeAndValue{Mode: old.Mode, Type: t, Value: nv}
	switch x := e.(type) {
	case *ast.ParenExpr:
		c.updateExprType(x.X, t, nil)
	case *ast.UnaryExpr:
		if x.Op != token.NOT {
			c.updateExprType(x.X, t, nil)
		}
	case *ast.BinaryExpr:
		if !isComparison(x.Op) {
			c.updateExprType(x.X, t, nil)
			if x.Op != token.SHL && x.Op != token.SHR {
				c.updateExprType(x.Y, t, nil)
			}
		}
	}
}

// assignable checks that x can be assigned to type t, converting untyped values.
func (c *checker) assignable(x *operand, t Type, context string) bool {
	if isInvalid(x) || t == Typ[Invalid] {
		return false
	}
	if isUntyped(x.typ) {
		if x.typ != Typ[UntypedNil] && isInterface(t) {
			c.defaultType(x)
			c.record(x)
		} else {
			return c.convertUntyped(x, t)
		}
	}
	if assignableTo(x.typ, t) {
		if needsConv(x.typ, t) && x.expr != nil {
			c.info.Implicit[x.expr] = t
		}
		return true
	}
	if it, ok := t.Underlying().(*Interface); ok && x.expr != nil {
		_, why := Implements(x.typ, it)
		c.errorf(x.expr.Pos(), "cannot use %s (type %s) as %s value in %s: %s", exprString(x.expr), x.typ, t, context, why)
		return false
	}
	c.errorf(x.expr.Pos(), "cannot use %s (type %s) as %s value in %s", exprString(x.expr), x.typ, t, context)
	return false
}

func assignableTo(v, t Type) bool {
	if Identical(v, t) {
		return true
	}
	if it, ok := t.Underlying().(*Interface); ok {
		if ok, _ := Implements(v, it); ok {
			return true
		}
	}
	if vs, ok := v.Underlying().(*Signature); ok {
		if ts, ok := t.Underlying().(*Signature); ok && !vs.Throws && ts.Throws && sigSatisfies(vs, ts) {
			return true
		}
	}
	_, vn := v.(*Named)
	_, tn := t.(*Named)
	_, vb := v.(*Basic)
	_, tb := t.(*Basic)
	// identical underlying types and at least one is not a named type
	if (!vn || !tn) && !vb && !tb && Identical(v.Underlying(), t.Underlying()) {
		return true
	}
	return false
}

// ---------------------------------------------------------------- operators

func isComparison(k token.Kind) bool {
	switch k {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

var goOp = map[token.Kind]gotoken.Token{
	token.ADD: gotoken.ADD, token.SUB: gotoken.SUB, token.MUL: gotoken.MUL, token.QUO: gotoken.QUO,
	token.REM: gotoken.REM, token.AND: gotoken.AND, token.OR: gotoken.OR, token.XOR: gotoken.XOR,
	token.AND_NOT: gotoken.AND_NOT, token.LAND: gotoken.LAND, token.LOR: gotoken.LOR,
	token.EQL: gotoken.EQL, token.NEQ: gotoken.NEQ, token.LSS: gotoken.LSS, token.LEQ: gotoken.LEQ,
	token.GTR: gotoken.GTR, token.GEQ: gotoken.GEQ, token.NOT: gotoken.NOT,
	token.SHL: gotoken.SHL, token.SHR: gotoken.SHR,
}

// operator method name and transformation for a binary operator
func opMethod(k token.Kind) (name string, swap, negate bool) {
	switch k {
	case token.ADD:
		return "+", false, false
	case token.SUB:
		return "-", false, false
	case token.MUL:
		return "*", false, false
	case token.QUO:
		return "/", false, false
	case token.REM:
		return "%", false, false
	case token.SHL:
		return "<<", false, false
	case token.SHR:
		return ">>", false, false
	case token.AND:
		return "&", false, false
	case token.OR:
		return "|", false, false
	case token.XOR:
		return "^", false, false
	case token.EQL:
		return "==", false, false
	case token.NEQ:
		return "==", false, true
	case token.LSS:
		return "<", false, false
	case token.GTR:
		return "<", true, false
	case token.LEQ:
		return "<", true, true
	case token.GEQ:
		return "<", false, true
	}
	return "", false, false
}

func namedOf(t Type) *Named {
	n, _ := t.(*Named)
	return n
}

func (c *checker) binary(e *ast.BinaryExpr) *operand {
	x := c.value(e.X)
	y := c.value(e.Y)
	if isInvalid(x) || isInvalid(y) {
		return c.invalid(e)
	}
	r, info := c.binaryOp(e, e.Op, x, y)
	if info != nil {
		c.info.Ops[e] = info
	}
	return r
}

// binaryOp checks x op y. e is used for positions.
func (c *checker) binaryOp(e ast.Node, op token.Kind, x, y *operand) (*operand, *OpInfo) {
	if r, ok := c.ptrArith(e, op, x, y); ok {
		return r, nil
	}
	// operator overloading
	if name, swap, negate := opMethod(op); name != "" {
		recv, arg := x, y
		if swap {
			recv, arg = y, x
		}
		if n := namedOf(recv.typ); n != nil && len(n.Methods[name]) > 0 {
			var cands []*Func
			for _, m := range n.Methods[name] {
				if len(m.Sig().Params) == 1 {
					cands = append(cands, m)
				}
			}
			fn := c.resolve(cands, []*operand{arg}, e.Pos(), "operator "+name+" of "+n.String(), false)
			if fn == nil {
				return c.invalid(nil), nil
			}
			info := &OpInfo{Func: fn, Swap: swap, Negate: negate}
			if fn.PtrRecv {
				if recv.mode != Variable {
					c.errorf(e.Pos(), "operator %s has a pointer receiver; operand must be addressable", name)
				}
				info.RecvAddr = true
				c.markAddrTaken(recv.expr)
			}
			return &operand{mode: Value, typ: fn.Sig().Results[0].typ}, info
		}
	}

	if op == token.SHL || op == token.SHR {
		return c.shift(e, op, x, y), nil
	}
	if isComparison(op) {
		return c.comparison(e, op, x, y), nil
	}

	c.matchTypes(x, y)
	if isInvalid(x) || isInvalid(y) {
		return c.invalid(nil), nil
	}
	if !Identical(x.typ, y.typ) {
		c.errorf(e.Pos(), "invalid operation: mismatched types %s and %s", x.typ, y.typ)
		return c.invalid(nil), nil
	}
	b := basic(x.typ)
	ok := false
	if b != nil {
		switch op {
		case token.ADD:
			ok = b.IsNumeric() || b.IsString()
		case token.SUB, token.MUL, token.QUO:
			ok = b.IsNumeric()
		case token.REM, token.AND, token.OR, token.XOR, token.AND_NOT:
			ok = b.IsInteger()
		case token.LAND, token.LOR:
			ok = b.IsBoolean()
		}
	}
	if !ok {
		c.errorf(e.Pos(), "invalid operation: operator %s not defined on %s (type %s)", op, exprString(x.expr), x.typ)
		return c.invalid(nil), nil
	}
	if (op == token.QUO || op == token.REM) && y.val != nil && constant.Sign(y.val) == 0 && b.IsNumeric() {
		if b.IsInteger() || x.val != nil {
			c.errorf(e.Pos(), "invalid operation: division by zero")
			return c.invalid(nil), nil
		}
	}
	if x.val != nil && y.val != nil {
		gop := goOp[op]
		if op == token.QUO && b.IsInteger() {
			gop = gotoken.QUO_ASSIGN // integer division
		}
		v := constant.BinaryOp(x.val, gop, y.val)
		if !b.IsUntyped() {
			cv, msg := representable(v, b)
			if msg != "" {
				c.errorf(e.Pos(), "constant %s %s %s", v, msg, b)
				return c.invalid(nil), nil
			}
			v = cv
		}
		return &operand{mode: Const, typ: x.typ, val: v}, nil
	}
	return &operand{mode: Value, typ: x.typ}, nil
}

func untypedRank(t Type) int {
	switch t {
	case Typ[UntypedInt]:
		return 1
	case Typ[UntypedRune]:
		return 2
	case Typ[UntypedFloat]:
		return 3
	}
	return 0
}

func (c *checker) matchTypes(x, y *operand) {
	xu, yu := isUntyped(x.typ), isUntyped(y.typ)
	switch {
	case xu && !yu:
		c.convertUntyped(x, y.typ)
	case !xu && yu:
		c.convertUntyped(y, x.typ)
	case xu && yu:
		rx, ry := untypedRank(x.typ), untypedRank(y.typ)
		if rx > 0 && ry > 0 {
			if rx > ry {
				y.typ = x.typ
			} else {
				x.typ = y.typ
			}
		}
	}
}

func comparable(t Type) bool {
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.Kind != UntypedNil
	case *Pointer, *Chan, *Interface:
		return true
	case *Array:
		return comparable(u.Elem)
	}
	return false
}

func (c *checker) comparison(e ast.Node, op token.Kind, x, y *operand) *operand {
	isNil := func(o *operand) bool { return o.typ == Typ[UntypedNil] }
	if isNil(x) || isNil(y) {
		other := x
		if isNil(x) {
			other = y
		}
		if op != token.EQL && op != token.NEQ {
			c.errorf(e.Pos(), "invalid operation: operator %s not defined on nil", op)
			return c.invalid(nil)
		}
		if isNil(other) {
			c.errorf(e.Pos(), "invalid operation: nil %s nil", op)
			return c.invalid(nil)
		}
		switch other.typ.Underlying().(type) {
		case *Pointer, *Slice, *Chan, *Map, *Signature, *Interface:
			c.convertUntyped(x, other.typ)
			c.convertUntyped(y, other.typ)
			return &operand{mode: Value, typ: Typ[UntypedBool]}
		}
		c.errorf(e.Pos(), "invalid operation: cannot compare %s with nil", other.typ)
		return c.invalid(nil)
	}
	if _, xp := isPtr(x.typ); xp && (op == token.LSS || op == token.LEQ || op == token.GTR || op == token.GEQ) {
		if !c.requireUnsafe(e.Pos(), "ordered pointer comparison") {
			return c.invalid(nil)
		}
		if !Identical(x.typ, y.typ) {
			c.errorf(e.Pos(), "invalid operation: mismatched types %s and %s", x.typ, y.typ)
			return c.invalid(nil)
		}
		return &operand{mode: Value, typ: Typ[UntypedBool]}
	}
	c.matchTypes(x, y)
	if isInvalid(x) || isInvalid(y) {
		return c.invalid(nil)
	}
	if (op == token.EQL || op == token.NEQ) && !Identical(x.typ, y.typ) && (isInterface(x.typ) || isInterface(y.typ)) {
		// interface == concrete (or another interface): convert to the interface
		switch {
		case isInterface(x.typ) && assignableTo(y.typ, x.typ):
			c.info.Implicit[y.expr] = x.typ
		case isInterface(y.typ) && assignableTo(x.typ, y.typ):
			c.info.Implicit[x.expr] = y.typ
		default:
			c.errorf(e.Pos(), "invalid operation: mismatched types %s and %s", x.typ, y.typ)
			return c.invalid(nil)
		}
		c.record(x)
		c.record(y)
		return &operand{mode: Value, typ: Typ[UntypedBool]}
	}
	if !Identical(x.typ, y.typ) {
		c.errorf(e.Pos(), "invalid operation: mismatched types %s and %s", x.typ, y.typ)
		return c.invalid(nil)
	}
	if op == token.EQL || op == token.NEQ {
		if !comparable(x.typ) {
			if _, ok := x.typ.Underlying().(*Struct); ok {
				c.errorf(e.Pos(), "invalid operation: %s values cannot be compared with %s; declare @overload func (a %s) ==(b %s) bool", x.typ, op, x.typ, x.typ)
			} else {
				c.errorf(e.Pos(), "invalid operation: %s values cannot be compared with %s", x.typ, op)
			}
			return c.invalid(nil)
		}
	} else {
		b := basic(x.typ)
		if b == nil || !b.IsOrdered() {
			c.errorf(e.Pos(), "invalid operation: operator %s not defined on %s; declare @overload func (a %s) <(b %s) bool", op, x.typ, x.typ, x.typ)
			return c.invalid(nil)
		}
	}
	if x.val != nil && y.val != nil {
		return &operand{mode: Const, typ: Typ[UntypedBool], val: constant.MakeBool(constant.Compare(x.val, goOp[op], y.val))}
	}
	c.defaultType(x)
	c.defaultType(y)
	c.record(x)
	c.record(y)
	return &operand{mode: Value, typ: Typ[UntypedBool]}
}

func (c *checker) shift(e ast.Node, op token.Kind, x, y *operand) *operand {
	if !IsInteger(y.typ) {
		c.errorf(e.Pos(), "invalid shift count %s (type %s)", exprString(y.expr), y.typ)
		return c.invalid(nil)
	}
	if y.val != nil && constant.Sign(y.val) < 0 {
		c.errorf(e.Pos(), "invalid negative shift count")
		return c.invalid(nil)
	}
	if isUntyped(y.typ) {
		c.convertUntyped(y, Typ[Uint])
	}
	if !IsInteger(x.typ) {
		if x.val != nil && isUntyped(x.typ) {
			c.convertUntyped(x, Typ[Int])
		} else {
			c.errorf(e.Pos(), "invalid operation: shifted operand %s must be integer", exprString(x.expr))
			return c.invalid(nil)
		}
	}
	if x.val != nil && y.val != nil {
		s, _ := constant.Uint64Val(y.val)
		if s > 1023 {
			c.errorf(e.Pos(), "shift count too large")
			return c.invalid(nil)
		}
		v := constant.Shift(constant.ToInt(x.val), goOp[op], uint(s))
		if b := basic(x.typ); !b.IsUntyped() {
			cv, msg := representable(v, b)
			if msg != "" {
				c.errorf(e.Pos(), "constant %s %s %s", v, msg, b)
				return c.invalid(nil)
			}
			v = cv
		}
		return &operand{mode: Const, typ: x.typ, val: v}
	}
	if isUntyped(x.typ) {
		c.convertUntyped(x, Typ[Int])
	}
	return &operand{mode: Value, typ: x.typ}
}

func (c *checker) unary(e *ast.UnaryExpr) *operand {
	if e.Op == token.AND {
		if cl, ok := unparen(e.X).(*ast.CompositeLit); ok {
			x := c.expr(cl)
			if isInvalid(x) {
				return c.invalid(e)
			}
			return &operand{mode: Value, typ: &Pointer{Elem: x.typ}}
		}
		x := c.value(e.X)
		if isInvalid(x) {
			return c.invalid(e)
		}
		if x.mode != Variable {
			c.errorf(e.Pos(), "cannot take address of %s", exprString(e.X))
			return c.invalid(e)
		}
		c.markAddrTaken(e.X)
		return &operand{mode: Value, typ: &Pointer{Elem: x.typ}}
	}
	x := c.value(e.X)
	if isInvalid(x) {
		return c.invalid(e)
	}
	if n := namedOf(x.typ); n != nil && (e.Op == token.SUB || e.Op == token.NOT) {
		name := map[token.Kind]string{token.SUB: "-", token.NOT: "!"}[e.Op]
		for _, m := range n.Methods[name] {
			if len(m.Sig().Params) == 0 {
				info := &OpInfo{Func: m}
				if m.PtrRecv {
					if x.mode != Variable {
						c.errorf(e.Pos(), "operator %s has a pointer receiver; operand must be addressable", name)
					}
					info.RecvAddr = true
					c.markAddrTaken(e.X)
				}
				c.info.Ops[e] = info
				return &operand{mode: Value, typ: m.Sig().Results[0].typ}
			}
		}
	}
	b := basic(x.typ)
	ok := false
	switch e.Op {
	case token.ADD, token.SUB:
		ok = b != nil && b.IsNumeric()
	case token.XOR:
		ok = b != nil && b.IsInteger()
	case token.NOT:
		ok = b != nil && b.IsBoolean()
	case token.ARROW:
		ch, isChan := x.typ.Underlying().(*Chan)
		if !isChan {
			c.errorf(e.Pos(), "invalid operation: cannot receive from non-channel %s (type %s)", exprString(e.X), x.typ)
			return c.invalid(e)
		}
		c.record(x)
		return &operand{mode: Value, typ: ch.Elem}
	}
	if !ok {
		c.errorf(e.Pos(), "invalid operation: operator %s not defined on %s (type %s)", e.Op, exprString(e.X), x.typ)
		return c.invalid(e)
	}
	if x.val != nil {
		prec := uint(0)
		if b.IsUnsigned() {
			prec = uint(b.Bits())
		}
		return &operand{mode: Const, typ: x.typ, val: constant.UnaryOp(goOp[e.Op], x.val, prec)}
	}
	return &operand{mode: Value, typ: x.typ}
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// markAddrTaken boxes the root local variable of an addressable expression,
// so that its address stays valid after the function returns.
func (c *checker) markAddrTaken(e ast.Expr) {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
			continue
		case *ast.SelectorExpr:
			if s := c.info.Selections[x]; s != nil && s.Indirect {
				return
			}
			e = x.X
			continue
		case *ast.IndexExpr:
			if tv, ok := c.info.Types[x.X]; ok {
				if _, isArr := tv.Type.Underlying().(*Array); isArr {
					e = x.X
					continue
				}
			}
			return
		case *ast.Ident:
			if v, ok := c.info.Uses[x].(*Var); ok && !v.Global {
				v.Boxed = true
			}
		}
		return
	}
}

// ---------------------------------------------------------------- selectors

func (c *checker) selector(e *ast.SelectorExpr) *operand {
	if id, ok := e.X.(*ast.Ident); ok {
		if pn, ok := c.lookup(id.Name).(*PkgName); ok {
			pn.used = true
			c.info.Uses[id] = pn
			c.info.Types[id] = TypeAndValue{Mode: PkgM}
			bid, ok := StdPackages[pn.Path][e.Sel.Name]
			if m := StdMembers[pn.Path][e.Sel.Name]; m != nil && !ok {
				c.info.Selections[e] = &Selection{Kind: PkgSel}
				switch o := m.(type) {
				case *TypeName:
					return &operand{mode: TypeExpr, typ: o.typ, obj: o}
				case *Var:
					return &operand{mode: Const, typ: o.typ, val: o.Value, obj: o}
				case *Func:
					return &operand{mode: FuncM, typ: o.typ, obj: o}
				}
			}
			if !ok {
				c.errorf(e.Sel.Pos(), "undefined: %s.%s", id.Name, e.Sel.Name)
				return c.invalid(e)
			}
			b := &Builtin{object{name: id.Name + "." + e.Sel.Name}, bid}
			c.info.Selections[e] = &Selection{Kind: PkgSel, Builtin: b}
			return &operand{mode: BuiltinM, obj: b}
		}
	}
	x := c.expr(e.X)
	if x.mode == TypeExpr {
		c.errorf(e.Pos(), "method expressions are not supported yet")
		return c.invalid(e)
	}
	x = c.checkValue(x)
	if isInvalid(x) {
		return c.invalid(e)
	}
	name := e.Sel.Name
	t := x.typ
	indirect := false
	if p, ok := t.Underlying().(*Pointer); ok {
		if _, isNamedPtr := t.(*Named); !isNamedPtr {
			t = p.Elem
			indirect = true
		}
	}
	if it, ok := t.Underlying().(*Interface); ok && !indirect {
		if i, m := it.Method(name); m != nil {
			sel := &Selection{Kind: IfaceMethodSel, Index: i,
				Methods: []*Func{{object: object{name: name, typ: m.Sig}}}}
			c.info.Selections[e] = sel
			return &operand{mode: FuncM, sel: sel, recv: x, typ: m.Sig}
		}
	}
	if n := namedOf(t); n != nil {
		if ms := n.Methods[name]; len(ms) > 0 {
			sel := &Selection{Kind: MethodSel, Methods: ms, Indirect: indirect}
			c.info.Selections[e] = sel
			return &operand{mode: FuncM, sel: sel, recv: x}
		}
	}
	if st, ok := t.Underlying().(*Struct); ok {
		if i, f := st.Field(name); f != nil {
			c.info.Selections[e] = &Selection{Kind: FieldSel, Index: i, Indirect: indirect}
			mode := Value
			if indirect || x.mode == Variable {
				mode = Variable
			}
			return &operand{mode: mode, typ: f.Type}
		}
	}
	if p := c.property(e, t); p != nil {
		return p
	}
	if _, ok := t.Underlying().(*Slice); ok && !indirect && name == "append" {
		sel := &Selection{Kind: SliceMethodSel}
		c.info.Selections[e] = sel
		return &operand{mode: FuncM, sel: sel, recv: x}
	}
	c.errorf(e.Sel.Pos(), "%s.%s undefined (type %s has no field or method %s)", exprString(e.X), name, x.typ, name)
	return c.invalid(e)
}

// ---------------------------------------------------------------- indexing

func (c *checker) index(e *ast.IndexExpr) *operand {
	if x, ok := c.genericIndex(e); ok {
		return x
	}
	x := c.expr(e.X)
	if x.mode == TypeExpr || x.mode == FuncM {
		c.errorf(e.Pos(), "%s is not a generic type or function", exprString(e.X))
		return c.invalid(e)
	}
	x = c.checkValue(x)
	if isInvalid(x) {
		return c.invalid(e)
	}
	if len(e.Indices) != 1 {
		c.errorf(e.Pos(), "unexpected comma in index expression")
		return c.invalid(e)
	}
	// overloaded [] operator
	if n := namedOf(x.typ); n != nil && len(n.Methods["[]"]) > 0 {
		arg := c.value(e.Indices[0])
		fn := c.resolve(n.Methods["[]"], []*operand{arg}, e.Pos(), "operator [] of "+n.String(), false)
		if fn == nil {
			return c.invalid(e)
		}
		info := &OpInfo{Func: fn}
		if fn.PtrRecv {
			if x.mode != Variable {
				c.errorf(e.Pos(), "operator [] has a pointer receiver; operand must be addressable")
			}
			info.RecvAddr = true
			c.markAddrTaken(e.X)
		}
		c.info.Ops[e] = info
		return &operand{mode: Value, typ: fn.Sig().Results[0].typ}
	}

	if m, ok := x.typ.Underlying().(*Map); ok {
		k := c.value(e.Indices[0])
		if isInvalid(k) {
			return c.invalid(e)
		}
		if c.assignable(k, m.Key, "map index") {
			c.record(k)
		}
		return &operand{mode: MapIndexM, typ: m.Elem}
	}
	var elem Type
	mode := Variable
	length := int64(-1)
	switch u := x.typ.Underlying().(type) {
	case *Basic:
		if u.IsString() {
			elem, mode = Typ[Uint8], Value
			if x.val != nil {
				length = int64(len(constant.StringVal(x.val)))
			}
		}
	case *Slice:
		elem = u.Elem
	case *Array:
		elem, length = u.Elem, u.Len
		if x.mode != Variable {
			mode = Value
		}
	case *Pointer:
		if a, ok := u.Elem.Underlying().(*Array); ok {
			elem, length = a.Elem, a.Len
		} else if c.unsafeDepth > 0 {
			elem, length, mode = u.Elem, -1, Variable // raw p[i]
		} else {
			c.errorf(e.Pos(), "indexing a pointer (%s) requires an unsafe block", x.typ)
			return c.invalid(e)
		}
	}
	if elem == nil {
		c.errorf(e.Pos(), "invalid operation: cannot index %s (type %s)", exprString(e.X), x.typ)
		return c.invalid(e)
	}
	i := c.indexValue(e.Indices[0], length)
	if i == nil {
		return c.invalid(e)
	}
	if x.val != nil && i.val != nil && IsString(x.typ) {
		s := constant.StringVal(x.val)
		n, _ := constant.Int64Val(i.val)
		return &operand{mode: Const, typ: Typ[Uint8], val: constant.MakeInt64(int64(s[n]))}
	}
	return &operand{mode: mode, typ: elem}
}

func (c *checker) indexValue(e ast.Expr, length int64) *operand {
	i := c.value(e)
	if isInvalid(i) {
		return nil
	}
	if isUntyped(i.typ) && !c.convertUntyped(i, Typ[Int]) {
		return nil
	}
	if !IsInteger(i.typ) {
		c.errorf(e.Pos(), "index %s must be integer", exprString(e))
		return nil
	}
	if i.val != nil {
		n, _ := constant.Int64Val(constant.ToInt(i.val))
		if n < 0 {
			c.errorf(e.Pos(), "invalid index %s (must not be negative)", exprString(e))
			return nil
		}
		if length >= 0 && n >= length {
			c.errorf(e.Pos(), "index %d out of bounds [0:%d]", n, length)
			return nil
		}
	}
	return i
}

func (c *checker) sliceExpr(e *ast.SliceExpr) *operand {
	x := c.value(e.X)
	if isInvalid(x) {
		return c.invalid(e)
	}
	var res Type
	length := int64(-1)
	switch u := x.typ.Underlying().(type) {
	case *Basic:
		if u.IsString() {
			if e.Slice3 {
				c.errorf(e.Pos(), "3-index slice of string")
				return c.invalid(e)
			}
			res = x.typ
			if isUntyped(res) {
				res = Typ[String]
			}
		}
	case *Slice:
		res = x.typ
	case *Array:
		if x.mode != Variable {
			c.errorf(e.Pos(), "invalid operation: %s (slice of unaddressable value)", exprString(e))
			return c.invalid(e)
		}
		c.markAddrTaken(e.X)
		res, length = &Slice{Elem: u.Elem}, u.Len
	case *Pointer:
		if a, ok := u.Elem.Underlying().(*Array); ok {
			res, length = &Slice{Elem: a.Elem}, a.Len
		}
	}
	if res == nil {
		c.errorf(e.Pos(), "cannot slice %s (type %s)", exprString(e.X), x.typ)
		return c.invalid(e)
	}
	limit := int64(-1)
	if length >= 0 {
		limit = length + 1
	}
	for _, ix := range []ast.Expr{e.Low, e.High, e.Max} {
		if ix != nil && c.indexValue(ix, limit) == nil {
			return c.invalid(e)
		}
	}
	return &operand{mode: Value, typ: res}
}

// ---------------------------------------------------------------- composite literals

func (c *checker) compositeLit(e *ast.CompositeLit, hint Type) *operand {
	var typ Type
	switch {
	case e.Type != nil:
		if at, ok := e.Type.(*ast.ArrayType); ok {
			if _, ok := at.Len.(*ast.Ellipsis); ok {
				// [...]T{...}
				elem := c.typExpr(at.Elt)
				typ = &Array{Len: int64(len(e.Elts)), Elem: elem}
				c.info.Types[e.Type] = TypeAndValue{Mode: TypeExpr, Type: typ}
				break
			}
		}
		typ = c.typExpr(e.Type)
	case hint != nil:
		typ = hint
	default:
		c.errorf(e.Pos(), "missing type in composite literal")
		return c.invalid(e)
	}
	if typ == Typ[Invalid] {
		return c.invalid(e)
	}
	if n, ctor := c.goWrapper(typ); n != nil && ctor != "" && !gocpyRefLit(e) {
		if c.rewritten {
			c.errorf(e.Pos(), "a composite literal of the Go type %s needs field names (%s{Field: value})", typ, typ)
			return c.invalid(e)
		}
		c.rw().lits[e] = n
		return &operand{mode: Value, typ: typ}
	}
	switch u := typ.Underlying().(type) {
	case *Struct:
		if len(e.Elts) == 0 {
			break
		}
		_, keyed := e.Elts[0].(*ast.KeyValueExpr)
		if keyed {
			seen := map[string]bool{}
			for _, el := range e.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					c.errorf(el.Pos(), "mixture of field:value and value elements in struct literal")
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					c.errorf(kv.Key.Pos(), "invalid field name %s in struct literal", exprString(kv.Key))
					continue
				}
				_, f := u.Field(key.Name)
				if f == nil {
					c.errorf(key.Pos(), "unknown field %s in struct literal of type %s", key.Name, typ)
					continue
				}
				if seen[key.Name] {
					c.errorf(key.Pos(), "duplicate field name %s in struct literal", key.Name)
				}
				seen[key.Name] = true
				c.element(kv.Value, f.Type, "struct literal")
			}
		} else {
			for i, el := range e.Elts {
				if _, ok := el.(*ast.KeyValueExpr); ok {
					c.errorf(el.Pos(), "mixture of field:value and value elements in struct literal")
					continue
				}
				if i >= len(u.Fields) {
					c.errorf(el.Pos(), "too many values in struct literal of type %s", typ)
					break
				}
				c.element(el, u.Fields[i].Type, "struct literal")
			}
			if len(e.Elts) < len(u.Fields) {
				c.errorf(e.Pos(), "too few values in struct literal of type %s", typ)
			}
		}
	case *Map:
		c.mapLit(e, u)
	case *Slice, *Array:
		var elem Type
		limit := int64(-1)
		if s, ok := u.(*Slice); ok {
			elem = s.Elem
		} else {
			a := u.(*Array)
			elem, limit = a.Elem, a.Len
		}
		for i, el := range e.Elts {
			if _, ok := el.(*ast.KeyValueExpr); ok {
				c.errorf(el.Pos(), "indexed elements in slice/array literals are not supported yet")
				continue
			}
			if limit >= 0 && int64(i) >= limit {
				c.errorf(el.Pos(), "index %d out of bounds [0:%d] in array literal", i, limit)
				break
			}
			c.element(el, elem, "slice literal")
		}
	default:
		c.errorf(e.Pos(), "invalid composite literal type %s", typ)
		return c.invalid(e)
	}
	return &operand{mode: Value, typ: typ}
}

func (c *checker) element(el ast.Expr, t Type, context string) {
	if cl, ok := el.(*ast.CompositeLit); ok && cl.Type == nil {
		// elided type: {1, 2} inside []T{...}
		x := c.compositeLit(cl, t)
		x.expr = cl
		c.record(x)
		return
	}
	x := c.value(el)
	c.assignable(x, t, context)
	c.record(x)
}

// ---------------------------------------------------------------- comprehension

// iterTypes returns the key and value types of a for-in iteration over t.
func (c *checker) iterTypes(x *operand, pos token.Pos) (key, val Type, ok bool) {
	if isUntyped(x.typ) {
		c.defaultType(x)
		c.record(x)
	}
	switch u := x.typ.Underlying().(type) {
	case *Basic:
		if u.IsString() {
			return Typ[Int], Typ[Int32], true
		}
		if u.IsInteger() {
			return nil, x.typ, true // for i in n
		}
	case *Slice:
		return Typ[Int], u.Elem, true
	case *Chan:
		return nil, u.Elem, true // for v in ch: receive until closed
	case *Map:
		return u.Key, u.Elem, true // for k, v in m; for k in m (keys)
	case *Array:
		return Typ[Int], u.Elem, true
	case *Pointer:
		if a, ok := u.Elem.Underlying().(*Array); ok {
			return Typ[Int], a.Elem, true
		}
	}
	c.errorf(pos, "cannot iterate over %s (type %s)", exprString(x.expr), x.typ)
	return nil, nil, false
}

func (c *checker) declareIterVars(n ast.Node, keyId, valId *ast.Ident, key, val Type) {
	var kv, vv *Var
	if keyId != nil {
		if key == nil {
			c.errorf(keyId.Pos(), "this iteration yields only one value; write for v in ...")
		} else {
			kv = NewVar(keyId.Name, key, keyId.Pos())
			c.declare(c.scope, keyId, kv)
		}
	}
	if valId != nil {
		vv = NewVar(valId.Name, val, valId.Pos())
		c.declare(c.scope, valId, vv)
	}
	c.info.ForVars[n] = [2]*Var{kv, vv}
}

func (c *checker) comprehension(e *ast.ComprehensionExpr) *operand {
	it := c.value(e.Iter)
	if isInvalid(it) {
		return c.invalid(e)
	}
	key, val, ok := c.iterTypes(it, e.Iter.Pos())
	if !ok {
		return c.invalid(e)
	}
	if m, ok := it.typ.Underlying().(*Map); ok && e.Key == nil {
		val = m.Key
		c.info.MapKeys[e] = true
	}
	c.openScope("block")
	defer c.closeScope()
	c.declareIterVars(e, e.Key, e.Value, key, val)
	if e.Cond != nil {
		cond := c.value(e.Cond)
		if !isInvalid(cond) && !IsBool(cond.typ) {
			c.errorf(e.Cond.Pos(), "comprehension condition must be bool, found %s", cond.typ)
		}
		c.defaultType(cond)
		c.record(cond)
	}
	elt := c.value(e.Elt)
	if isInvalid(elt) {
		return c.invalid(e)
	}
	c.defaultType(elt)
	c.record(elt)
	return &operand{mode: Value, typ: &Slice{Elem: elt.typ}}
}

// ---------------------------------------------------------------- printing

func (c *checker) printable(t Type) bool {
	return printableT(t, map[Type]bool{})
}

func printableT(t Type, seen map[Type]bool) bool {
	if seen[t] {
		return true
	}
	seen[t] = true
	if n, ok := t.(*Named); ok {
		if HasStringMethod(n) {
			return true
		}
	}
	switch u := t.Underlying().(type) {
	case *Basic:
		return u.Kind != Invalid && u.Kind != UntypedNil
	case *Pointer, *Chan, *Interface:
		return true
	case *Map:
		return printableT(u.Key, seen) && printableT(u.Elem, seen)
	case *Slice:
		return printableT(u.Elem, seen)
	case *Array:
		return printableT(u.Elem, seen)
	case *Struct:
		for _, f := range u.Fields {
			if !printableT(f.Type, seen) {
				return false
			}
		}
		return true
	}
	return false
}

// HasStringMethod reports whether n has a value-receiver `String() string` method.
func HasStringMethod(n *Named) bool {
	for _, m := range n.Methods["String"] {
		s, ok := m.typ.(*Signature)
		if ok && !m.PtrRecv && len(s.Params) == 0 && len(s.Results) == 1 && Identical(s.Results[0].typ, Typ[String]) && !s.Throws {
			return true
		}
	}
	return false
}

// StringMethod returns the String() method used for printing.
func StringMethod(n *Named) *Func {
	for _, m := range n.Methods["String"] {
		s := m.Sig()
		if !m.PtrRecv && len(s.Params) == 0 && len(s.Results) == 1 && Identical(s.Results[0].typ, Typ[String]) {
			return m
		}
	}
	return nil
}

// Spec is a parsed f-string format spec: [align][width][.prec][verb]
type Spec struct {
	Align byte // 0, '<', '>', '^'
	Width int
	Prec  int // -1 if absent
	Verb  byte
}

// ParseSpec parses an f-string spec such as ">8", ".2f", "08x".
func ParseSpec(s string) (Spec, error) {
	sp := Spec{Prec: -1}
	i := 0
	if i < len(s) && (s[i] == '<' || s[i] == '>' || s[i] == '^') {
		sp.Align = s[i]
		i++
	}
	if i < len(s) && s[i] == '0' && sp.Align == 0 {
		sp.Align = '0' // zero padding
		i++
	}
	j := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > j {
		sp.Width, _ = strconv.Atoi(s[j:i])
	}
	if i < len(s) && s[i] == '.' {
		i++
		j = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == j {
			return sp, fmt.Errorf("missing precision after '.'")
		}
		sp.Prec, _ = strconv.Atoi(s[j:i])
	}
	if i < len(s) {
		sp.Verb = s[i]
		i++
		if !strings.ContainsRune("dxXfes", rune(sp.Verb)) {
			return sp, fmt.Errorf("unknown verb %q (use d, x, X, f, e, s)", sp.Verb)
		}
	}
	if i != len(s) {
		return sp, fmt.Errorf("unexpected %q", s[i:])
	}
	return sp, nil
}

// Check validates a spec against the formatted type; returns "" if ok.
func (sp Spec) Check(t Type) string {
	isInt, isFloat, isStr := IsInteger(t), IsFloat(t), IsString(t)
	switch sp.Verb {
	case 'd', 'x', 'X':
		if !isInt {
			return "verb requires an integer"
		}
	case 'f', 'e':
		if !isFloat {
			return "verb requires a float"
		}
	case 's':
		if !isStr {
			return "verb requires a string"
		}
	}
	if sp.Prec >= 0 && !isFloat {
		return "precision is only allowed for floats"
	}
	return ""
}

// gocpyRefLit: T{GocpyRef: r} (generated code)
func gocpyRefLit(e *ast.CompositeLit) bool {
	for _, el := range e.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "GocpyRef" {
				return true
			}
		}
	}
	return false
}
