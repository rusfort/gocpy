package c

import (
	"fmt"
	"go/constant"
	"strconv"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

// expr returns a C expression for e. It never writes statements to g.w;
// constructs that need temporaries use GNU statement expressions ({ ... }).
func (g *gen) expr(e ast.Expr) string {
	if t, ok := g.info.Implicit[e]; ok {
		return g.boxIface(g.exprRaw(e), g.typeOf(e), t)
	}
	return g.exprRaw(e)
}

func (g *gen) exprRaw(e ast.Expr) string {
	if fn, ok := g.info.FuncRefs[e]; ok {
		return g.funcValue(fn)
	}
	if mv, ok := g.info.MethodVals[e]; ok {
		return g.methodValue(mv)
	}
	if tv, ok := g.info.Types[e]; ok && tv.Value != nil && tv.Type != nil {
		if _, isBasic := tv.Type.Underlying().(*types.Basic); isBasic {
			return g.constLit(tv.Value, tv.Type)
		}
	}
	switch e := e.(type) {
	case *ast.Ident:
		switch o := g.info.Uses[e].(type) {
		case *types.Var:
			return g.varRef(o)
		case *types.Nil:
			return g.zero(g.typeOf(e))
		}
		g.errorf(e.Pos(), "codegen: unexpected identifier %s", e.Name)
		return "0"
	case *ast.ParenExpr:
		return "(" + g.expr(e.X) + ")"
	case *ast.FString:
		return g.fstring(e)
	case *ast.CompositeLit:
		return g.composite(e, g.typeOf(e))
	case *ast.SelectorExpr:
		sel := g.info.Selections[e]
		if sel == nil || sel.Kind != types.FieldSel {
			g.errorf(e.Pos(), "codegen: unexpected selector")
			return "0"
		}
		xt := g.typeOf(e.X)
		name := cIdent(e.Sel.Name)
		if sel.Indirect {
			return fmt.Sprintf("((%s)gocpy_nonnil(%s))->%s", g.cType(xt), g.expr(e.X), name)
		}
		return fmt.Sprintf("(%s).%s", g.expr(e.X), name)
	case *ast.IndexExpr:
		return g.index(e)
	case *ast.SliceExpr:
		return g.slice(e)
	case *ast.CallExpr:
		return g.call(e)
	case *ast.StarExpr:
		pt := g.typeOf(e.X)
		return fmt.Sprintf("(*(%s)gocpy_nonnil(%s))", g.cType(pt), g.expr(e.X))
	case *ast.UnaryExpr:
		return g.unary(e)
	case *ast.BinaryExpr:
		return g.binary(e)
	case *ast.ComprehensionExpr:
		return g.comprehension(e)
	case *ast.FuncLit:
		return g.funcLit(e)
	case *ast.TypeAssertExpr:
		return g.typeAssert(e)
	case *ast.PropagateExpr:
		return g.propagate(e)
	case *ast.IntrinsicExpr:
		return fmt.Sprintf("%s(%s)", g.bswapFn(g.typeOf(e)), g.expr(e.Args[0]))
	}
	g.errorf(e.Pos(), "codegen: unsupported expression %T", e)
	return "0"
}

// pure reports whether e can be evaluated twice without side effects.
func pure(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident, *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return pure(x.X)
	case *ast.SelectorExpr:
		return pure(x.X)
	}
	return false
}

func (g *gen) index(e *ast.IndexExpr) string {
	if info := g.info.Ops[e]; info != nil {
		recv := g.expr(e.X)
		if info.RecvAddr {
			recv = "&(" + recv + ")"
		}
		return fmt.Sprintf("%s(%s, %s)", info.Func.CName, recv, g.expr(e.Indices[0]))
	}
	xt := g.typeOf(e.X)
	if m, ok := xt.Underlying().(*types.Map); ok {
		return g.mapRead(e, m)
	}
	idx := e.Indices[0]
	is := g.expr(idx)
	constIdx := false
	if tv := g.info.Types[idx]; tv.Value != nil {
		constIdx = true
	}
	switch u := xt.Underlying().(type) {
	case *types.Basic:
		return fmt.Sprintf("gocpy_string_at(%s, %s)", g.expr(e.X), is)
	case *types.Slice:
		et := g.cType(u.Elem)
		if pure(e.X) {
			x := g.expr(e.X)
			return fmt.Sprintf("((%s *)(%s).data)[gocpy_bounds(%s, (%s).len)]", et, x, is, x)
		}
		s, i := g.newTmp(), g.newTmp()
		return fmt.Sprintf("(*({ gocpy_slice %s = %s; int64_t %s = %s; gocpy_bounds(%s, %s.len); &((%s *)%s.data)[%s]; }))",
			s, g.expr(e.X), i, is, i, s, et, s, i)
	case *types.Array:
		if constIdx {
			return fmt.Sprintf("(%s).a[%s]", g.expr(e.X), is)
		}
		return fmt.Sprintf("(%s).a[gocpy_bounds(%s, %d)]", g.expr(e.X), is, u.Len)
	case *types.Pointer:
		a, ok := u.Elem.Underlying().(*types.Array)
		if !ok { // raw p[i] (unsafe): no bounds checks
			return fmt.Sprintf("(%s)[(int64_t)(%s)]", g.expr(e.X), is)
		}
		return fmt.Sprintf("((%s)gocpy_nonnil(%s))->a[gocpy_bounds(%s, %d)]", g.cType(xt), g.expr(e.X), is, a.Len)
	}
	g.errorf(e.Pos(), "codegen: cannot index %s", xt)
	return "0"
}

func (g *gen) optIndex(e ast.Expr) string {
	if e == nil {
		return "GOCPY_ABSENT"
	}
	return "(int64_t)(" + g.expr(e) + ")"
}

func (g *gen) slice(e *ast.SliceExpr) string {
	xt := g.typeOf(e.X)
	lo, hi, max := g.optIndex(e.Low), g.optIndex(e.High), g.optIndex(e.Max)
	switch u := xt.Underlying().(type) {
	case *types.Basic:
		return fmt.Sprintf("gocpy_string_slice(%s, %s, %s)", g.expr(e.X), lo, hi)
	case *types.Slice:
		return fmt.Sprintf("gocpy_slice_slice(%s, %s, %s, %s, sizeof(%s))", g.expr(e.X), lo, hi, max, g.cType(u.Elem))
	case *types.Array:
		return fmt.Sprintf("gocpy_slice_slice(gocpy_slice_from_array((%s).a, %d), %s, %s, %s, sizeof(%s))",
			g.expr(e.X), u.Len, lo, hi, max, g.cType(u.Elem))
	case *types.Pointer:
		a := u.Elem.Underlying().(*types.Array)
		return fmt.Sprintf("gocpy_slice_slice(gocpy_slice_from_array(((%s)gocpy_nonnil(%s))->a, %d), %s, %s, %s, sizeof(%s))",
			g.cType(xt), g.expr(e.X), a.Len, lo, hi, max, g.cType(a.Elem))
	}
	g.errorf(e.Pos(), "codegen: cannot slice %s", xt)
	return "0"
}

// ---------------------------------------------------------------- operators

func (g *gen) unary(e *ast.UnaryExpr) string {
	if info := g.info.Ops[e]; info != nil {
		recv := g.expr(e.X)
		if info.RecvAddr {
			recv = "&(" + recv + ")"
		}
		return fmt.Sprintf("%s(%s)", info.Func.CName, recv)
	}
	t := g.typeOf(e)
	switch e.Op {
	case token.ARROW:
		v := g.newTmp()
		return fmt.Sprintf("({ %s %s; gocpy_chan_recv(%s, &%s); %s; })", g.cType(t), v, g.expr(e.X), v, v)
	case token.AND:
		if cl, ok := unparen(e.X).(*ast.CompositeLit); ok {
			lt := g.typeOf(cl)
			ct := g.cType(lt)
			p := g.newTmp()
			return fmt.Sprintf("({ %s *%s = (%s *)gocpy_alloc(sizeof(%s)); *%s = %s; %s; })", ct, p, ct, ct, p, g.composite(cl, lt), p)
		}
		return "(&" + g.expr(e.X) + ")"
	case token.SUB:
		return g.wrap(t, "-"+g.expr(e.X))
	case token.ADD:
		return "(" + g.expr(e.X) + ")"
	case token.NOT:
		return "(!" + g.expr(e.X) + ")"
	case token.XOR:
		return g.wrap(t, "~"+g.expr(e.X))
	}
	g.errorf(e.Pos(), "codegen: unsupported unary %s", e.Op)
	return "0"
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

// wrap casts arithmetic on narrow integers back to their type (C promotes to int).
func (g *gen) wrap(t types.Type, s string) string {
	if b, ok := t.Underlying().(*types.Basic); ok && b.IsInteger() && types.Default(b).(*types.Basic).Bits() < 64 {
		return "((" + g.cType(t) + ")(" + s + "))"
	}
	return "(" + s + ")"
}

func (g *gen) binary(e *ast.BinaryExpr) string {
	if info := g.info.Ops[e]; info != nil {
		x, y := e.X, e.Y
		if info.Swap {
			x, y = y, x
		}
		recv := g.expr(x)
		if info.RecvAddr {
			recv = "&(" + recv + ")"
		}
		s := fmt.Sprintf("%s(%s, %s)", info.Func.CName, recv, g.expr(y))
		if info.Negate {
			return "(!" + s + ")"
		}
		return s
	}
	switch e.Op {
	case token.LAND:
		return "(" + g.expr(e.X) + " && " + g.expr(e.Y) + ")"
	case token.LOR:
		return "(" + g.expr(e.X) + " || " + g.expr(e.Y) + ")"
	}
	switch g.info.PtrOps[e] {
	case types.PtrAdd:
		if _, xp := g.typeOf(e.X).Underlying().(*types.Pointer); xp {
			return fmt.Sprintf("(%s %s (int64_t)(%s))", g.expr(e.X), cOps[e.Op], g.expr(e.Y))
		}
		return fmt.Sprintf("((int64_t)(%s) + %s)", g.expr(e.X), g.expr(e.Y))
	case types.PtrDiff:
		return fmt.Sprintf("((int64_t)(%s - %s))", g.expr(e.X), g.expr(e.Y))
	}
	xt := g.typeOf(e.X)
	if isCmp(e.Op) {
		yt := g.typeOf(e.Y)
		if _, isNil := g.info.Uses[identOf(e.X)].(*types.Nil); isNil {
			xt = yt
		}
		return g.compare(e.Op, xt, g.expr(e.X), g.expr(e.Y))
	}
	return g.binCode(e.Op, xt, g.expr(e.X), e.Y)
}

func identOf(e ast.Expr) *ast.Ident {
	id, _ := unparen(e).(*ast.Ident)
	return id
}

func isCmp(k token.Kind) bool {
	switch k {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

var cOps = map[token.Kind]string{
	token.ADD: "+", token.SUB: "-", token.MUL: "*", token.QUO: "/", token.REM: "%",
	token.AND: "&", token.OR: "|", token.XOR: "^", token.SHL: "<<", token.SHR: ">>",
	token.EQL: "==", token.NEQ: "!=", token.LSS: "<", token.LEQ: "<=", token.GTR: ">", token.GEQ: ">=",
}

func (g *gen) compare(op token.Kind, t types.Type, x, y string) string {
	switch u := t.Underlying().(type) {
	case *types.Basic:
		if u.IsString() {
			switch op {
			case token.EQL:
				return fmt.Sprintf("gocpy_string_eq(%s, %s)", x, y)
			case token.NEQ:
				return fmt.Sprintf("(!gocpy_string_eq(%s, %s))", x, y)
			}
			return fmt.Sprintf("(gocpy_string_cmp(%s, %s) %s 0)", x, y, cOps[op])
		}
	case *types.Slice:
		// only comparison with nil is allowed
		if strings.Contains(y, "{0}") {
			return fmt.Sprintf("((%s).data %s NULL)", x, cOps[op])
		}
		return fmt.Sprintf("((%s).data %s NULL)", y, cOps[op])
	case *types.Interface:
		if op == token.NEQ {
			return fmt.Sprintf("(!gocpy_iface_eq(%s, %s))", x, y)
		}
		return fmt.Sprintf("gocpy_iface_eq(%s, %s)", x, y)
	case *types.Signature:
		// only comparison with nil is allowed
		if strings.Contains(y, "{0}") {
			return fmt.Sprintf("((%s).fn %s NULL)", x, cOps[op])
		}
		return fmt.Sprintf("((%s).fn %s NULL)", y, cOps[op])
	case *types.Array:
		a, b := g.newTmp(), g.newTmp()
		ct := g.cType(t)
		eq := "=="
		if op == token.NEQ {
			eq = "!="
		}
		return fmt.Sprintf("({ %s %s = %s; %s %s = %s; memcmp(&%s, &%s, sizeof(%s)) %s 0; })", ct, a, x, ct, b, y, a, b, ct, eq)
	}
	return fmt.Sprintf("(%s %s %s)", x, cOps[op], y)
}

// binCode renders x op y for arithmetic operators.
func (g *gen) binCode(op token.Kind, t types.Type, x string, ye ast.Expr) string {
	return g.binCodeStr(op, t, x, g.expr(ye), ye)
}

// binCodeStr is binCode with an already lowered right operand y.
func (g *gen) binCodeStr(op token.Kind, t types.Type, x, y string, ye ast.Expr) string {
	if _, isPtr := t.Underlying().(*types.Pointer); isPtr { // p += n (unsafe)
		return fmt.Sprintf("(%s %s (int64_t)(%s))", x, cOps[op], y)
	}
	yconst := g.info.Types[ye].Value
	b, _ := t.Underlying().(*types.Basic)
	if b == nil {
		g.errorf(ye.Pos(), "codegen: operator %s on %s", op, t)
		return "0"
	}
	ct := g.cType(t)
	switch {
	case b.IsString() && op == token.ADD:
		return fmt.Sprintf("gocpy_string_concat(%s, %s)", x, y)
	case op == token.AND_NOT:
		return g.wrap(t, x+" & ~"+y)
	case (op == token.QUO || op == token.REM) && b.IsInteger():
		if yconst != nil && constant.Sign(yconst) != 0 {
			return g.wrap(t, x+" "+cOps[op]+" "+y)
		}
		a, d := g.newTmp(), g.newTmp()
		return fmt.Sprintf("({ %s %s = %s; %s %s = %s; GOCPY_DIV_CHECK(%s); (%s)(%s %s %s); })",
			ct, a, x, ct, d, y, d, ct, a, cOps[op], d)
	case op == token.SHL || op == token.SHR:
		bits := types.Default(b).(*types.Basic).Bits()
		ut := map[int]string{8: "uint8_t", 16: "uint16_t", 32: "uint32_t", 64: "uint64_t"}[bits]
		if yconst != nil {
			n, _ := constant.Uint64Val(constant.ToInt(yconst))
			if n >= uint64(bits) {
				if op == token.SHR && !b.IsUnsigned() {
					return fmt.Sprintf("((%s)((%s) < 0 ? -1 : 0))", ct, x)
				}
				return "((" + ct + ")0)"
			}
			if op == token.SHL {
				return fmt.Sprintf("((%s)((%s)(%s) << %s))", ct, ut, x, y)
			}
			return g.wrap(t, x+" >> "+y)
		}
		a, s := g.newTmp(), g.newTmp()
		if op == token.SHL {
			return fmt.Sprintf("({ %s %s = %s; uint64_t %s = (uint64_t)(%s); (%s)(%s >= %d ? 0 : (%s)%s << %s); })",
				ct, a, x, s, y, ct, s, bits, ut, a, s)
		}
		over := "0"
		if !b.IsUnsigned() {
			over = fmt.Sprintf("(%s < 0 ? -1 : 0)", a)
		}
		return fmt.Sprintf("({ %s %s = %s; uint64_t %s = (uint64_t)(%s); (%s)(%s >= %d ? %s : %s >> %s); })",
			ct, a, x, s, y, ct, s, bits, over, a, s)
	}
	if o, ok := cOps[op]; ok {
		return g.wrap(t, x+" "+o+" "+y)
	}
	g.errorf(ye.Pos(), "codegen: unsupported operator %s", op)
	return "0"
}

// ---------------------------------------------------------------- calls

func (g *gen) call(e *ast.CallExpr) string {
	ci := g.info.Calls[e]
	if ci == nil {
		g.errorf(e.Pos(), "codegen: unresolved call")
		return "0"
	}
	switch ci.Kind {
	case types.CallConversion:
		return g.conversion(ci.TypeArg, e.Args[0])
	case types.CallBuiltin:
		return g.builtin(e, ci)
	case types.CallValue:
		return g.callValue(e, ci)
	case types.CallIface:
		return g.callIface(e, ci)
	case types.CallSliceAppend:
		recv := e.Fun.(*ast.SelectorExpr).X
		et := g.cType(elemType(ci.TypeArg))
		p := g.newTmp()
		var b strings.Builder
		fmt.Fprintf(&b, "({ gocpy_slice *%s = &(%s); ", p, g.expr(recv))
		if e.Ellipsis {
			fmt.Fprintf(&b, "gocpy_slice_append_slice(%s, %s, sizeof(%s)); ", p, g.expr(e.Args[0]), et)
		} else {
			for _, a := range e.Args {
				v := g.newTmp()
				fmt.Fprintf(&b, "%s %s = %s; gocpy_slice_append(%s, &%s, sizeof(%s)); ", et, v, g.expr(a), p, v, et)
			}
		}
		b.WriteString("(void)0; })")
		return b.String()
	}
	fn := ci.Func
	sig := fn.Sig()
	args := g.callArgs(e, ci)
	call := fmt.Sprintf("%s(%s)", fn.CName, strings.Join(args, ", "))
	if ci.Throws {
		return g.checked(call, sig)
	}
	return call
}

// callArgs lowers the receiver and arguments of a function or method call.
func (g *gen) callArgs(e *ast.CallExpr, ci *types.CallInfo) []string {
	sig := ci.Sig
	if ci.Func != nil {
		sig = ci.Func.Sig()
	}
	var args []string
	if ci.Kind == types.CallMethod {
		recv := e.Fun.(*ast.SelectorExpr).X
		r := g.expr(recv)
		switch {
		case ci.RecvAddr:
			r = "&(" + r + ")"
		case ci.RecvDeref:
			r = fmt.Sprintf("(*(%s)gocpy_nonnil(%s))", g.cType(g.typeOf(recv)), r)
		}
		args = append(args, r)
	}
	n := len(sig.Params)
	if sig.Variadic && !e.Ellipsis {
		for i := 0; i < n-1; i++ {
			args = append(args, g.expr(e.Args[i]))
		}
		args = append(args, g.sliceOf(sig.Params[n-1].Type().(*types.Slice).Elem, e.Args[n-1:]))
	} else {
		for _, a := range e.Args {
			args = append(args, g.expr(a))
		}
	}
	return args
}

// sliceOf builds a new slice from element expressions.
func (g *gen) sliceOf(elem types.Type, elts []ast.Expr) string {
	et := g.cType(elem)
	if len(elts) == 0 {
		return fmt.Sprintf("gocpy_slice_lit(0, sizeof(%s), NULL)", et)
	}
	a := g.newTmp()
	var b strings.Builder
	fmt.Fprintf(&b, "({ %s %s[%d]; ", et, a, len(elts))
	for i, el := range elts {
		fmt.Fprintf(&b, "%s[%d] = %s; ", a, i, g.element(el, elem))
	}
	fmt.Fprintf(&b, "gocpy_slice_lit(%d, sizeof(%s), %s); })", len(elts), et, a)
	return b.String()
}

func (g *gen) element(el ast.Expr, t types.Type) string {
	if cl, ok := el.(*ast.CompositeLit); ok && cl.Type == nil {
		return g.composite(cl, t)
	}
	return g.expr(el)
}

func (g *gen) conversion(t types.Type, arg ast.Expr) string {
	st := g.typeOf(arg)
	x := g.expr(arg)
	if t == types.ExceptionType {
		return "gocpy_exc_basic(" + x + ")"
	}
	if isIface(t) {
		return g.boxIface(x, st, t)
	}
	tb, _ := t.Underlying().(*types.Basic)
	sb, _ := st.Underlying().(*types.Basic)
	ct := g.cType(t)
	switch {
	case tb != nil && sb != nil && tb.IsString() && sb.IsInteger():
		return fmt.Sprintf("gocpy_string_from_rune((int64_t)(%s))", x)
	case tb != nil && tb.IsString() && sb == nil:
		return fmt.Sprintf("gocpy_string_from_bytes(%s)", x)
	case sb != nil && sb.IsString() && tb == nil:
		return fmt.Sprintf("gocpy_bytes_from_string(%s)", x)
	case tb != nil && sb != nil:
		return "((" + ct + ")(" + x + "))"
	}
	if g.cType(st) == ct {
		return "(" + x + ")"
	}
	if _, sp := st.Underlying().(*types.Pointer); sp && tb != nil { // uintptr(p)
		return "((" + ct + ")(" + x + "))"
	}
	if _, ok := t.Underlying().(*types.Pointer); ok {
		return "((" + ct + ")(" + x + "))"
	}
	tmp := g.newTmp()
	return fmt.Sprintf("({ %s %s = %s; *(%s *)&%s; })", g.cType(st), tmp, x, ct, tmp)
}

func (g *gen) builtin(e *ast.CallExpr, ci *types.CallInfo) string {
	switch ci.Builtin.ID {
	case types.BMalloc, types.BCalloc:
		return fmt.Sprintf("gocpy_raw_malloc((int64_t)(%s), %v)", g.expr(e.Args[0]), ci.Builtin.ID == types.BCalloc)
	case types.BRealloc:
		return fmt.Sprintf("gocpy_raw_realloc((void *)(%s), (int64_t)(%s))", g.expr(e.Args[0]), g.expr(e.Args[1]))
	case types.BFree:
		return fmt.Sprintf("gocpy_raw_free((void *)(%s))", g.expr(e.Args[0]))
	case types.BMemcpy:
		return fmt.Sprintf("gocpy_raw_memcpy((void *)(%s), (const void *)(%s), (int64_t)(%s))", g.expr(e.Args[0]), g.expr(e.Args[1]), g.expr(e.Args[2]))
	case types.BMemset:
		return fmt.Sprintf("gocpy_raw_memset((void *)(%s), (uint8_t)(%s), (int64_t)(%s))", g.expr(e.Args[0]), g.expr(e.Args[1]), g.expr(e.Args[2]))
	case types.BSliceOf:
		n := g.newTmp()
		return fmt.Sprintf("({ int64_t %s = (int64_t)(%s); if (%s < 0) gocpy_panic(\"slice: negative length\"); (gocpy_slice){ (void *)(%s), %s, %s }; })",
			n, g.expr(e.Args[1]), n, g.expr(e.Args[0]), n, n)
	case types.BClose:
		return fmt.Sprintf("gocpy_chan_close(%s)", g.expr(e.Args[0]))
	case types.BDelete:
		m := g.typeOf(e.Args[0]).Underlying().(*types.Map)
		k := g.newTmp()
		return fmt.Sprintf("({ %s %s = %s; gocpy_map_delete(%s, &%s); })", g.cType(m.Key), k, g.expr(e.Args[1]), g.expr(e.Args[0]), k)
	case types.BLen, types.BCap:
		x := g.expr(e.Args[0])
		if _, isMap := g.typeOf(e.Args[0]).Underlying().(*types.Map); isMap {
			return "gocpy_map_len(" + x + ")"
		}
		if _, isChan := g.typeOf(e.Args[0]).Underlying().(*types.Chan); isChan {
			if ci.Builtin.ID == types.BCap {
				return "gocpy_chan_cap(" + x + ")"
			}
			return "gocpy_chan_len(" + x + ")"
		}
		if ci.Builtin.ID == types.BCap {
			return "(" + x + ").cap"
		}
		return "(" + x + ").len"
	case types.BAppend:
		et := g.cType(elemType(ci.TypeArg))
		r := g.newTmp()
		var b strings.Builder
		fmt.Fprintf(&b, "({ gocpy_slice %s = %s; ", r, g.expr(e.Args[0]))
		if e.Ellipsis {
			fmt.Fprintf(&b, "gocpy_slice_append_slice(&%s, %s, sizeof(%s)); ", r, g.expr(e.Args[1]), et)
		} else {
			for _, a := range e.Args[1:] {
				v := g.newTmp()
				fmt.Fprintf(&b, "%s %s = %s; gocpy_slice_append(&%s, &%s, sizeof(%s)); ", et, v, g.expr(a), r, v, et)
			}
		}
		fmt.Fprintf(&b, "%s; })", r)
		return b.String()
	case types.BMake:
		if m, isMap := ci.TypeArg.Underlying().(*types.Map); isMap {
			hint := "0"
			if len(e.Args) == 2 {
				hint = "(int64_t)(" + g.expr(e.Args[1]) + ")"
			}
			return g.mapMake(m, hint)
		}
		if ch, isChan := ci.TypeArg.Underlying().(*types.Chan); isChan {
			size := "0"
			if len(e.Args) == 2 {
				size = "(int64_t)(" + g.expr(e.Args[1]) + ")"
			}
			return fmt.Sprintf("gocpy_chan_make(sizeof(%s), %s)", g.cType(ch.Elem), size)
		}
		et := g.cType(elemType(ci.TypeArg))
		capS := "GOCPY_ABSENT"
		if len(e.Args) == 3 {
			capS = "(int64_t)(" + g.expr(e.Args[2]) + ")"
		}
		return fmt.Sprintf("gocpy_make_slice((int64_t)(%s), %s, sizeof(%s))", g.expr(e.Args[1]), capS, et)
	case types.BNew:
		ct := g.cType(ci.TypeArg)
		return fmt.Sprintf("((%s *)gocpy_alloc(sizeof(%s)))", ct, ct)
	case types.BSizeof:
		return fmt.Sprintf("((int64_t)sizeof(%s))", g.cType(ci.TypeArg))
	case types.FmtPrintln, types.FmtPrint, types.FmtSprint:
		sb := g.newTmp()
		var b strings.Builder
		fmt.Fprintf(&b, "({ gocpy_sb %s = gocpy_sb_new(); ", sb)
		prevString := true
		for i, a := range e.Args {
			t := g.typeOf(a)
			isStr := types.IsString(t)
			if i > 0 {
				// Println always separates; Print/Sprint only between non-strings (Go rules)
				if ci.Builtin.ID == types.FmtPrintln || (!isStr && !prevString) {
					fmt.Fprintf(&b, "gocpy_sb_byte(&%s, ' '); ", sb)
				}
			}
			prevString = isStr
			b.WriteString(g.fmtValue(t, g.expr(a), "&"+sb))
			b.WriteString("; ")
		}
		switch ci.Builtin.ID {
		case types.FmtPrintln:
			fmt.Fprintf(&b, "gocpy_sb_byte(&%s, '\\n'); gocpy_sb_write_stdout(&%s); (void)0; })", sb, sb)
		case types.FmtPrint:
			fmt.Fprintf(&b, "gocpy_sb_write_stdout(&%s); (void)0; })", sb)
		default:
			fmt.Fprintf(&b, "gocpy_sb_string(&%s); })", sb)
		}
		return b.String()
	}
	g.errorf(e.Pos(), "codegen: unsupported builtin %s", ci.Builtin.Name())
	return "0"
}

// ---------------------------------------------------------------- composite literals

func (g *gen) composite(e *ast.CompositeLit, t types.Type) string {
	ct := g.cType(t)
	switch u := t.Underlying().(type) {
	case *types.Map:
		return g.mapLit(e, u)
	case *types.Struct:
		if len(e.Elts) == 0 {
			return "((" + ct + "){0})"
		}
		var parts []string
		for i, el := range e.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				name := kv.Key.(*ast.Ident).Name
				_, f := u.Field(name)
				parts = append(parts, "."+cIdent(name)+" = "+g.element(kv.Value, f.Type))
				continue
			}
			parts = append(parts, "."+cIdent(u.Fields[i].Name)+" = "+g.element(el, u.Fields[i].Type))
		}
		return "((" + ct + "){ " + strings.Join(parts, ", ") + " })"
	case *types.Slice:
		return g.sliceOf(u.Elem, e.Elts)
	case *types.Array:
		tmp := g.newTmp()
		var b strings.Builder
		fmt.Fprintf(&b, "({ %s %s = {0}; ", ct, tmp)
		for i, el := range e.Elts {
			fmt.Fprintf(&b, "%s.a[%d] = %s; ", tmp, i, g.element(el, u.Elem))
		}
		fmt.Fprintf(&b, "%s; })", tmp)
		return b.String()
	}
	g.errorf(e.Pos(), "codegen: unsupported composite literal of type %s", t)
	return "0"
}

// ---------------------------------------------------------------- comprehension

func (g *gen) comprehension(e *ast.ComprehensionExpr) string {
	rt := g.typeOf(e).(*types.Slice)
	et := g.cType(rt.Elem)
	res := g.newTmp()
	old, oldInd := g.w, g.ind
	var b strings.Builder
	g.w, g.ind = &b, 0
	vars := g.info.ForVars[e]
	g.mapKeys = g.info.MapKeys[e]
	g.iterate(e.Iter, vars[0], vars[1], func() {
		v := g.newTmp()
		if e.Cond != nil {
			g.line("if (%s) {", g.expr(e.Cond))
			g.ind++
		}
		g.line("%s %s = %s; gocpy_slice_append(&%s, &%s, sizeof(%s));", et, v, g.expr(e.Elt), res, v, et)
		if e.Cond != nil {
			g.ind--
			g.line("}")
		}
	})
	g.w, g.ind = old, oldInd
	body := strings.ReplaceAll(strings.TrimSpace(b.String()), "\n", " ")
	return fmt.Sprintf("({ gocpy_slice %s = gocpy_slice_lit(0, sizeof(%s), NULL); %s %s; })", res, et, body, res)
}

// ---------------------------------------------------------------- formatting

func (g *gen) fstring(e *ast.FString) string {
	sb := g.newTmp()
	var b strings.Builder
	fmt.Fprintf(&b, "({ gocpy_sb %s = gocpy_sb_new(); ", sb)
	for _, p := range e.Parts {
		if p.Expr == nil {
			text, err := strconv.Unquote(`"` + strings.ReplaceAll(p.Text, `"`, `\"`) + `"`)
			if err != nil {
				text = p.Text
			}
			fmt.Fprintf(&b, "gocpy_sb_bytes(&%s, %s, %d); ", sb, cString(text), len(text))
			continue
		}
		t := g.typeOf(p.Expr)
		x := g.expr(p.Expr)
		spec, _ := types.ParseSpec(p.Spec)
		mark := ""
		if spec.Width > 0 {
			mark = g.newTmp()
			fmt.Fprintf(&b, "int64_t %s = %s.len; ", mark, sb)
		}
		switch {
		case spec.Verb == 'x' || spec.Verb == 'X':
			upper := "false"
			if spec.Verb == 'X' {
				upper = "true"
			}
			if types.IsUnsigned(t) {
				fmt.Fprintf(&b, "gocpy_sb_hex(&%s, (uint64_t)(%s), %s); ", sb, x, upper)
			} else {
				v := g.newTmp()
				fmt.Fprintf(&b, "{ int64_t %s = (int64_t)(%s); if (%s < 0) { gocpy_sb_byte(&%s, '-'); %s = -%s; } gocpy_sb_hex(&%s, (uint64_t)%s, %s); } ",
					v, x, v, sb, v, v, sb, v, upper)
			}
		case spec.Verb == 'f' || spec.Verb == 'e' || spec.Prec >= 0:
			verb := byte('f')
			if spec.Verb == 'e' {
				verb = 'e'
			}
			fmt.Fprintf(&b, "gocpy_sb_fprec(&%s, (double)(%s), %d, '%c'); ", sb, x, spec.Prec, verb)
		default:
			b.WriteString(g.fmtValue(t, x, "&"+sb))
			b.WriteString("; ")
		}
		if spec.Width > 0 {
			align := spec.Align
			if align == 0 {
				align = '>'
				if types.IsString(t) {
					align = '<'
				}
			}
			fmt.Fprintf(&b, "gocpy_sb_pad(&%s, %s, %d, '%c'); ", sb, mark, spec.Width, align)
		}
	}
	fmt.Fprintf(&b, "gocpy_sb_string(&%s); })", sb)
	return b.String()
}

// fmtValue returns a C statement-expression that appends x (of type t) to sb in Go %v style.
func (g *gen) fmtValue(t types.Type, x, sb string) string {
	if sig, fn, _ := types.LookupMethod(t, "Error"); fn != nil && !fn.PtrRecv && len(sig.Params) == 0 &&
		len(sig.Results) == 1 && types.IsString(sig.Results[0].Type()) && !sig.Throws && !ptrShaped(t) {
		return fmt.Sprintf("gocpy_sb_str(%s, %s(%s))", sb, fn.CName, x)
	}
	if n, ok := t.(*types.Named); ok {
		if m := types.StringMethod(n); m != nil {
			return fmt.Sprintf("gocpy_sb_str(%s, %s(%s))", sb, m.CName, x)
		}
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch {
		case u.IsBoolean():
			return fmt.Sprintf("gocpy_sb_bool(%s, %s)", sb, x)
		case u.IsString():
			return fmt.Sprintf("gocpy_sb_str(%s, %s)", sb, x)
		case u.IsUnsigned():
			return fmt.Sprintf("gocpy_sb_u64(%s, (uint64_t)(%s))", sb, x)
		case u.IsInteger():
			return fmt.Sprintf("gocpy_sb_i64(%s, (int64_t)(%s))", sb, x)
		case u.IsFloat():
			bits := 64
			if types.Default(u).(*types.Basic).Kind == types.Float32 {
				bits = 32
			}
			return fmt.Sprintf("gocpy_sb_f64(%s, (double)(%s), %d)", sb, x, bits)
		}
	case *types.Pointer, *types.Chan:
		return fmt.Sprintf("gocpy_sb_ptr(%s, (const void *)(%s))", sb, x)
	case *types.Interface:
		return fmt.Sprintf("gocpy_sb_iface(%s, %s)", sb, x)
	case *types.Signature:
		return fmt.Sprintf("gocpy_sb_ptr(%s, (%s).fn)", sb, x)
	case *types.Slice, *types.Array, *types.Struct, *types.Map:
		id := g.typeID(t)
		if _, ok := g.fmtHelpers[id]; !ok {
			g.fmtHelpers[id] = t
			g.fmtOrder = append(g.fmtOrder, id)
		}
		return fmt.Sprintf("gocpy_fmt_%s(%s, %s)", id, sb, x)
	}
	return fmt.Sprintf("gocpy_sb_cstr(%s, \"?\")", sb)
}

// fmtHelperDefs emits helper functions (may discover more helpers while emitting).
func (g *gen) fmtHelperDefs(b *strings.Builder) {
	for ; g.fmtDone < len(g.fmtOrder); g.fmtDone++ {
		id := g.fmtOrder[g.fmtDone]
		t := g.fmtHelpers[id]
		fmt.Fprintf(b, "void gocpy_fmt_%s(gocpy_sb *sb, %s v) {\n", id, g.cType(t))
		switch u := t.Underlying().(type) {
		case *types.Slice:
			fmt.Fprintf(b, "    gocpy_sb_byte(sb, '[');\n    for (int64_t i = 0; i < v.len; i++) {\n        if (i) gocpy_sb_byte(sb, ' ');\n        %s;\n    }\n    gocpy_sb_byte(sb, ']');\n",
				g.fmtValue(u.Elem, fmt.Sprintf("((%s *)v.data)[i]", g.cType(u.Elem)), "sb"))
		case *types.Array:
			fmt.Fprintf(b, "    gocpy_sb_byte(sb, '[');\n    for (int64_t i = 0; i < %d; i++) {\n        if (i) gocpy_sb_byte(sb, ' ');\n        %s;\n    }\n    gocpy_sb_byte(sb, ']');\n",
				u.Len, g.fmtValue(u.Elem, "v.a[i]", "sb"))
		case *types.Map:
			fmt.Fprintf(b, "    gocpy_sb_cstr(sb, \"map[\");\n    int64_t it = 0; void *k, *e; bool first = true;\n    while (gocpy_map_next(v, &it, &k, &e)) {\n        if (!first) gocpy_sb_byte(sb, ' ');\n        first = false;\n        %s;\n        gocpy_sb_byte(sb, ':');\n        %s;\n    }\n    gocpy_sb_byte(sb, ']');\n",
				g.fmtValue(u.Key, fmt.Sprintf("(*(%s *)k)", g.cType(u.Key)), "sb"), g.fmtValue(u.Elem, fmt.Sprintf("(*(%s *)e)", g.cType(u.Elem)), "sb"))
		case *types.Struct:
			b.WriteString("    gocpy_sb_byte(sb, '{');\n")
			for j, f := range u.Fields {
				if j > 0 {
					b.WriteString("    gocpy_sb_byte(sb, ' ');\n")
				}
				fmt.Fprintf(b, "    %s;\n", g.fmtValue(f.Type, "v."+cIdent(f.Name), "sb"))
			}
			b.WriteString("    gocpy_sb_byte(sb, '}');\n")
		}
		b.WriteString("}\n\n")
	}
}
