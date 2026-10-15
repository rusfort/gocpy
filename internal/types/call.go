package types

import (
	"go/constant"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

func (c *checker) call(e *ast.CallExpr) *operand {
	f := c.expr(e.Fun)
	if isInvalid(f) && f.mode == Value {
		for _, a := range e.Args {
			c.expr(a)
		}
		return c.invalid(e)
	}
	switch f.mode {
	case TypeExpr:
		return c.conversionCall(e, f.typ)
	case BuiltinM:
		return c.builtin(e, f.obj.(*Builtin))
	case FuncM:
		if f.sel != nil && f.sel.Kind == SliceMethodSel {
			return c.sliceAppendMethod(e, f)
		}
		var cands []*Func
		var what string
		var gen *GenericFunc
		switch {
		case f.sel != nil:
			cands = f.sel.Methods
			what = "method " + e.Fun.(*ast.SelectorExpr).Sel.Name
		default:
			switch o := f.obj.(type) {
			case *Func:
				cands = []*Func{o}
			case *FuncSet:
				cands = o.Funcs
			case *GenericFunc:
				gen = o
			}
			what = f.obj.Name()
		}
		args := c.args(e)
		if args == nil {
			return c.invalid(e)
		}
		if gen != nil {
			targs := c.infer(gen, args, e)
			if targs == nil {
				return c.invalid(e)
			}
			inst := c.instFunc(gen, targs, e.Pos())
			if inst == nil {
				return c.invalid(e)
			}
			cands = []*Func{inst}
		}
		fn := c.resolve(cands, args, e.Pos(), what, e.Ellipsis)
		if fn == nil {
			return c.invalid(e)
		}
		if f.sel != nil && f.sel.Kind == IfaceMethodSel {
			sig := fn.Sig()
			c.info.Calls[e] = &CallInfo{Kind: CallIface, Sig: sig, Index: f.sel.Index, Throws: sig.Throws}
			if sig.Throws {
				c.throwSite(e.Pos(), "exception from "+fn.name+"()")
			}
			return resultOperand(sig)
		}
		ci := &CallInfo{Kind: CallFunc, Func: fn, Throws: fn.Sig().Throws}
		if ci.Throws {
			c.throwSite(e.Pos(), "exception from "+fn.name+"()")
		}
		if f.sel != nil {
			ci.Kind = CallMethod
			recv := f.recv
			switch {
			case fn.PtrRecv && !f.sel.Indirect:
				if recv.mode != Variable {
					c.errorf(e.Pos(), "cannot call pointer method %s on %s (not addressable)", fn.name, exprString(recv.expr))
				}
				ci.RecvAddr = true
				c.markAddrTaken(recv.expr)
			case !fn.PtrRecv && f.sel.Indirect:
				ci.RecvDeref = true
			}
		}
		c.info.Calls[e] = ci
		sig := fn.Sig()
		switch len(sig.Results) {
		case 0:
			return &operand{mode: NoValue}
		case 1:
			return &operand{mode: Value, typ: sig.Results[0].typ}
		}
		return &operand{mode: Value, typ: &Tuple{Vars: sig.Results}}
	}
	if f.typ != nil {
		if sig, ok := f.typ.Underlying().(*Signature); ok && (f.mode == Value || f.mode == Variable || f.mode == MapIndexM) {
			return c.callValue(e, f, sig)
		}
	}
	c.errorf(e.Pos(), "invalid operation: cannot call non-function %s", exprString(e.Fun))
	return c.invalid(e)
}

func (c *checker) args(e *ast.CallExpr) []*operand {
	out := make([]*operand, 0, len(e.Args))
	ok := true
	for _, a := range e.Args {
		x := c.value(a)
		if isInvalid(x) {
			ok = false
		}
		out = append(out, x)
	}
	if !ok {
		return nil
	}
	return out
}

// argScore: -1 not assignable, 1 untyped convertible, 2 exact (or untyped with matching default).
func (c *checker) argScore(x *operand, t Type) int {
	if isUntyped(x.typ) {
		if !c.canConvertUntyped(x, t) {
			if _, ok := t.Underlying().(*Interface); ok && x.typ != Typ[UntypedNil] && assignableTo(Default(x.typ), t) {
				return 0 // the default type, boxed (any)
			}
			return -1
		}
		if Identical(Default(x.typ), t) {
			return 2
		}
		return 1
	}
	if Identical(x.typ, t) {
		return 2
	}
	if assignableTo(x.typ, t) {
		return 1
	}
	return -1
}

func paramTypes(sig *Signature, nargs int, spread bool) []Type {
	n := len(sig.Params)
	if !sig.Variadic || spread {
		if nargs != n {
			return nil
		}
		ts := make([]Type, n)
		for i, p := range sig.Params {
			ts[i] = p.typ
		}
		return ts
	}
	if nargs < n-1 {
		return nil
	}
	ts := make([]Type, 0, nargs)
	for i := 0; i < n-1; i++ {
		ts = append(ts, sig.Params[i].typ)
	}
	elem := sig.Params[n-1].typ.(*Slice).Elem
	for i := n - 1; i < nargs; i++ {
		ts = append(ts, elem)
	}
	return ts
}

// resolve selects the function among candidates (overload resolution).
// No implicit conversions: only exact types or untyped constants convert.
func (c *checker) resolve(cands []*Func, args []*operand, pos token.Pos, what string, spread bool) *Func {
	if len(cands) == 0 {
		c.errorf(pos, "no candidates for %s", what)
		return nil
	}
	if spread {
		for _, f := range cands {
			if !f.Sig().Variadic {
				continue
			}
		}
	}
	if len(cands) == 1 && !cands[0].Overloaded {
		fn := cands[0]
		sig := fn.Sig()
		if spread && !sig.Variadic {
			c.errorf(pos, "cannot use ... in call to non-variadic %s", what)
			return nil
		}
		ts := paramTypes(sig, len(args), spread)
		if ts == nil {
			want := len(sig.Params)
			if sig.Variadic && !spread {
				c.errorf(pos, "not enough arguments in call to %s: have %d, want at least %d", what, len(args), want-1)
			} else if len(args) < want {
				c.errorf(pos, "not enough arguments in call to %s: have %d, want %d", what, len(args), want)
			} else {
				c.errorf(pos, "too many arguments in call to %s: have %d, want %d", what, len(args), want)
			}
			return nil
		}
		ok := true
		for i, a := range args {
			if !c.assignable(a, ts[i], "argument to "+what) {
				ok = false
			}
			c.record(a)
		}
		if !ok {
			return nil
		}
		return fn
	}

	best, bestScore, tie := (*Func)(nil), -1, false
	for _, f := range cands {
		sig := f.Sig()
		if spread && !sig.Variadic {
			continue
		}
		ts := paramTypes(sig, len(args), spread)
		if ts == nil {
			continue
		}
		score := 0
		for i, a := range args {
			s := c.argScore(a, ts[i])
			if s < 0 {
				score = -1
				break
			}
			score += s
		}
		if score < 0 {
			continue
		}
		if !sig.Variadic {
			score++ // prefer fixed arity over variadic
		}
		if score > bestScore {
			best, bestScore, tie = f, score, false
		} else if score == bestScore {
			tie = true
		}
	}
	var argTypes []string
	for _, a := range args {
		argTypes = append(argTypes, a.typ.String())
	}
	if best == nil {
		var sigs []string
		for _, f := range cands {
			sigs = append(sigs, "\n\t"+f.Sig().String()+" at "+f.pos.String())
		}
		c.errorf(pos, "no overload of %s matches arguments (%s); candidates:%s", what, strings.Join(argTypes, ", "), strings.Join(sigs, ""))
		return nil
	}
	if tie {
		c.errorf(pos, "ambiguous call to overloaded %s with arguments (%s); add explicit conversions", what, strings.Join(argTypes, ", "))
		return nil
	}
	ts := paramTypes(best.Sig(), len(args), spread)
	for i, a := range args {
		c.assignable(a, ts[i], "argument to "+what)
		c.record(a)
	}
	return best
}

func (c *checker) sliceAppendMethod(e *ast.CallExpr, f *operand) *operand {
	recv := f.recv
	if recv.mode != Variable {
		c.errorf(e.Pos(), "cannot append to %s (not addressable)", exprString(recv.expr))
		return c.invalid(e)
	}
	elem := recv.typ.Underlying().(*Slice).Elem
	if e.Ellipsis {
		if len(e.Args) != 1 {
			c.errorf(e.Pos(), "append with ... takes exactly one argument")
			return c.invalid(e)
		}
		x := c.value(e.Args[0])
		c.assignable(x, &Slice{Elem: elem}, "append")
		c.record(x)
	} else {
		if len(e.Args) == 0 {
			c.errorf(e.Pos(), "append requires at least one value")
		}
		for _, a := range e.Args {
			x := c.value(a)
			c.assignable(x, elem, "append")
			c.record(x)
		}
	}
	c.info.Calls[e] = &CallInfo{Kind: CallSliceAppend, TypeArg: recv.typ}
	return &operand{mode: NoValue}
}

// ---------------------------------------------------------------- builtins

func (c *checker) typeArg(e ast.Expr) Type {
	x := c.expr(e)
	if x.mode != TypeExpr {
		c.errorf(e.Pos(), "%s is not a type", exprString(e))
		return Typ[Invalid]
	}
	return x.typ
}

func (c *checker) builtin(e *ast.CallExpr, b *Builtin) *operand {
	ci := &CallInfo{Kind: CallBuiltin, Builtin: b}
	c.info.Calls[e] = ci
	nargs := len(e.Args)
	need := func(min, max int) bool {
		if nargs < min || (max >= 0 && nargs > max) {
			c.errorf(e.Pos(), "wrong number of arguments to %s: %d", b.name, nargs)
			return false
		}
		return true
	}
	if e.Ellipsis && b.ID != BAppend {
		c.errorf(e.Pos(), "invalid use of ... with %s", b.name)
	}
	switch b.ID {
	case BLen, BCap:
		if !need(1, 1) {
			return c.invalid(e)
		}
		x := c.value(e.Args[0])
		if isInvalid(x) {
			return c.invalid(e)
		}
		switch u := x.typ.Underlying().(type) {
		case *Basic:
			if u.IsString() && b.ID == BLen {
				if x.val != nil {
					return &operand{mode: Const, typ: Typ[Int], val: constant.MakeInt64(int64(len(constant.StringVal(x.val))))}
				}
				c.defaultType(x)
				c.record(x)
				return &operand{mode: Value, typ: Typ[Int]}
			}
		case *Slice, *Chan:
			return &operand{mode: Value, typ: Typ[Int]}
		case *Map:
			if b.ID == BLen {
				return &operand{mode: Value, typ: Typ[Int]}
			}
		case *Array:
			return &operand{mode: Const, typ: Typ[Int], val: constant.MakeInt64(u.Len)}
		case *Pointer:
			if a, ok := u.Elem.Underlying().(*Array); ok {
				return &operand{mode: Const, typ: Typ[Int], val: constant.MakeInt64(a.Len)}
			}
		}
		c.errorf(e.Args[0].Pos(), "invalid argument %s (type %s) for %s", exprString(e.Args[0]), x.typ, b.name)
		return c.invalid(e)

	case BAppend:
		if !need(1, -1) {
			return c.invalid(e)
		}
		s := c.value(e.Args[0])
		if isInvalid(s) {
			return c.invalid(e)
		}
		st, ok := s.typ.Underlying().(*Slice)
		if !ok {
			c.errorf(e.Args[0].Pos(), "first argument to append must be a slice, have %s", s.typ)
			return c.invalid(e)
		}
		if e.Ellipsis {
			if nargs != 2 {
				c.errorf(e.Pos(), "append with ... takes exactly two arguments")
				return c.invalid(e)
			}
			x := c.value(e.Args[1])
			c.assignable(x, &Slice{Elem: st.Elem}, "append")
			c.record(x)
		} else {
			for _, a := range e.Args[1:] {
				x := c.value(a)
				c.assignable(x, st.Elem, "append")
				c.record(x)
			}
		}
		ci.TypeArg = s.typ
		return &operand{mode: Value, typ: s.typ}

	case BMake:
		if !need(1, 3) {
			return c.invalid(e)
		}
		t := c.typeArg(e.Args[0])
		switch t.Underlying().(type) {
		case *Slice:
			if nargs < 2 {
				c.errorf(e.Pos(), "make(%s) requires a length", t)
				return c.invalid(e)
			}
		case *Chan, *Map:
			if nargs > 2 {
				c.errorf(e.Pos(), "make(%s) accepts at most one size argument", t)
				return c.invalid(e)
			}
		default:
			if t != Typ[Invalid] {
				c.errorf(e.Args[0].Pos(), "make: cannot make %s; type must be a slice, map or channel", t)
			}
			return c.invalid(e)
		}
		for _, a := range e.Args[1:] {
			c.indexValue(a, -1)
		}
		ci.TypeArg = t
		return &operand{mode: Value, typ: t}

	case BClose:
		if !need(1, 1) {
			return c.invalid(e)
		}
		x := c.value(e.Args[0])
		if isInvalid(x) {
			return c.invalid(e)
		}
		if _, ok := x.typ.Underlying().(*Chan); !ok {
			c.errorf(e.Args[0].Pos(), "invalid operation: close of non-channel %s", x.typ)
			return c.invalid(e)
		}
		c.record(x)
		return &operand{mode: NoValue}

	case BDelete:
		if !need(2, 2) {
			return c.invalid(e)
		}
		m := c.value(e.Args[0])
		if isInvalid(m) {
			return c.invalid(e)
		}
		mt, ok := m.typ.Underlying().(*Map)
		if !ok {
			c.errorf(e.Args[0].Pos(), "invalid argument: %s (type %s) is not a map", exprString(e.Args[0]), m.typ)
			return c.invalid(e)
		}
		c.record(m)
		k := c.value(e.Args[1])
		if !isInvalid(k) && c.assignable(k, mt.Key, "argument to delete") {
			c.record(k)
		}
		return &operand{mode: NoValue}

	case BNew:
		if !need(1, 1) {
			return c.invalid(e)
		}
		t := c.typeArg(e.Args[0])
		ci.TypeArg = t
		if n, ctor := c.goWrapper(t); n != nil && ctor != "" && !c.rewritten {
			c.rw().news[e] = n
		}
		return &operand{mode: Value, typ: &Pointer{Elem: t}}

	case BSizeof:
		if !need(1, 1) {
			return c.invalid(e)
		}
		ci.TypeArg = c.typeArg(e.Args[0])
		return &operand{mode: Value, typ: Typ[Int]}

	case FmtPrintln, FmtPrint, FmtSprint:
		for _, a := range e.Args {
			x := c.value(a)
			if isInvalid(x) {
				continue
			}
			c.defaultType(x)
			c.record(x)
			if !c.printable(x.typ) {
				c.errorf(a.Pos(), "cannot print %s of type %s", exprString(a), x.typ)
			}
		}
		if b.ID == FmtSprint {
			return &operand{mode: Value, typ: Typ[String]}
		}
		return &operand{mode: NoValue}
	}
	if b.ID >= BMalloc {
		return c.unsafeBuiltin(e, b, ci)
	}
	c.errorf(e.Pos(), "unknown builtin %s", b.name)
	return c.invalid(e)
}

// ---------------------------------------------------------------- conversions

func (c *checker) conversionCall(e *ast.CallExpr, t Type) *operand {
	if len(e.Args) != 1 {
		c.errorf(e.Pos(), "conversion to %s needs exactly one argument", t)
		return c.invalid(e)
	}
	x := c.value(e.Args[0])
	if isInvalid(x) || t == Typ[Invalid] {
		return c.invalid(e)
	}
	c.info.Calls[e] = &CallInfo{Kind: CallConversion, TypeArg: t}
	if t == ExceptionType {
		// Exception("message"): a ready-made exception, like java.lang.Exception
		if !c.assignable(x, Typ[String], "argument to Exception") {
			return c.invalid(e)
		}
		c.record(x)
		return &operand{mode: Value, typ: ExceptionType}
	}
	tb := basic(t)

	if x.val != nil && tb != nil && isUntyped(x.typ) {
		// constant conversion: string(65) -> "A", float64(1), int(2.0)
		if tb.IsString() && IsInteger(x.typ) {
			r, _ := constant.Int64Val(constant.ToInt(x.val))
			c.convertUntyped(x, Typ[Int32])
			c.record(x)
			return &operand{mode: Const, typ: t, val: constant.MakeString(string(rune(r)))}
		}
		if c.convertUntyped(x, t) {
			c.record(x)
			return &operand{mode: Const, typ: t, val: x.val}
		}
		return c.invalid(e)
	}
	c.defaultType(x)
	c.record(x)
	if r, ok := c.ptrConversion(e, x, t); ok {
		return r
	}
	if convertible(x.typ, t) {
		if x.val != nil && tb != nil {
			if v, msg := representable(x.val, tb); msg == "" {
				if !(IsFloat(x.typ) && IsInteger(t)) && !(IsString(t) && !IsString(x.typ)) {
					return &operand{mode: Const, typ: t, val: v}
				}
			}
		}
		return &operand{mode: Value, typ: t}
	}
	c.errorf(e.Pos(), "cannot convert %s (type %s) to type %s", exprString(e.Args[0]), x.typ, t)
	return c.invalid(e)
}

func convertible(v, t Type) bool {
	if assignableTo(v, t) || Identical(v.Underlying(), t.Underlying()) {
		return true
	}
	vb, tb := basic(v), basic(t)
	if vb != nil && tb != nil {
		if vb.IsNumeric() && tb.IsNumeric() {
			return true
		}
		if tb.IsString() && vb.IsInteger() {
			return true // string(rune)
		}
	}
	isBytes := func(x Type) bool {
		s, ok := x.Underlying().(*Slice)
		return ok && Identical(s.Elem, Typ[Uint8])
	}
	if (tb != nil && tb.IsString() && isBytes(v)) || (vb != nil && vb.IsString() && isBytes(t)) {
		return true
	}
	vp, ok1 := v.Underlying().(*Pointer)
	tp, ok2 := t.Underlying().(*Pointer)
	if ok1 && ok2 && Identical(vp.Elem.Underlying(), tp.Elem.Underlying()) {
		return true
	}
	return false
}
