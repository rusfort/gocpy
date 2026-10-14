package c

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

// Stage 5 lowering: function values and closures, interfaces (itabs),
// maps, defer and the ? operator.
//
//	func value      gocpy_func{fn, env}; fn(void *env, params...)
//	closure         lifted to gocpy_litN(void *gocpy_env, ...); env[i] = pointer to a captured heap cell
//	interface       gocpy_iface{itab, data}; data = boxed copy, or the pointer itself for pointer types
//	map             gocpy_map* (runtime hash table, insertion order)
//	defer           body wrapped in try/finally; finally runs gocpy_run_defers
//	x?              error stored in gocpy_perr, jump to a return path emitted after the statement

// fnState is the per-function generator state, saved while a function
// literal is lifted.
type fnState struct {
	w          *strings.Builder
	ind        int
	sig        *types.Signature
	names      map[string]int
	taken      map[string]bool
	loops      []loopLabels
	excStack   []string
	finStack   []*tryCtx
	needExcOut bool
	needRetv   bool
	needPerr   bool
	deferFin   *ast.BlockStmt
	deferNamed bool
	props      []propPending
	mapKeys    bool
}

type propPending struct {
	label string
}

func (g *gen) saveState() fnState {
	return fnState{g.w, g.ind, g.sig, g.names, g.taken, g.loops, g.excStack, g.finStack,
		g.needExcOut, g.needRetv, g.needPerr, g.deferFin, g.deferNamed, g.props, g.mapKeys}
}

func (g *gen) restoreState(s fnState) {
	g.w, g.ind, g.sig, g.names, g.taken, g.loops, g.excStack, g.finStack = s.w, s.ind, s.sig, s.names, s.taken, s.loops, s.excStack, s.finStack
	g.needExcOut, g.needRetv, g.needPerr, g.deferFin, g.deferNamed, g.props, g.mapKeys = s.needExcOut, s.needRetv, s.needPerr, s.deferFin, s.deferNamed, s.props, s.mapKeys
}

// emitBody emits the body of a function whose opening line has been written.
// prologue (optional) runs first (closure environment loads).
func (g *gen) emitBody(sig *types.Signature, params []*types.Var, body *ast.BlockStmt, prologue func()) {
	out := g.w
	g.ind++
	if prologue != nil {
		prologue()
	}
	for _, p := range params {
		if p.Name() == "" || p.Name() == "_" {
			continue
		}
		pn := g.paramName(p)
		name := g.localName(p)
		if p.Boxed {
			ct := g.cType(p.Type())
			g.line("%s *%s = (%s *)gocpy_alloc(sizeof(%s));", ct, name, ct, ct)
			g.line("*%s = %s;", name, pn)
		}
	}
	named := false
	for _, r := range sig.Results {
		if r.Name() != "" && r.Name() != "_" {
			g.declLocal(r, g.zero(r.Type()))
			named = true
		}
	}
	g.excStack, g.finStack, g.needExcOut, g.needRetv, g.needPerr = nil, nil, false, false, false
	g.deferFin, g.deferNamed, g.props = nil, false, nil
	b := &strings.Builder{}
	g.w = b
	if containsDefer(body.List) {
		g.line("gocpy_defer *gocpy_defers = NULL;")
		g.deferFin = &ast.BlockStmt{}
		g.deferNamed = named
		g.tryStmt(&ast.TryStmt{Body: body, Finally: g.deferFin})
		if len(sig.Results) > 0 {
			g.line("__builtin_unreachable();")
		}
	} else {
		g.stmts(body.List)
	}
	g.w = out
	if g.needRetv {
		g.line("%s _retv;", g.resultType(sig))
	}
	if g.needPerr {
		g.line("gocpy_iface gocpy_perr;")
	}
	out.WriteString(b.String())
	if g.needExcOut {
		if len(sig.Results) == 0 {
			g.line("gocpy_exc_out: return;")
		} else {
			g.line("gocpy_exc_out: return (%s){0};", g.resultType(sig))
		}
	}
	g.ind--
	g.line("}")
	g.line("")
}

// containsDefer reports whether a statement list (not nested literals) has defer.
func containsDefer(list []ast.Stmt) bool {
	for _, s := range list {
		if stmtHasDefer(s) {
			return true
		}
	}
	return false
}

func stmtHasDefer(s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.DeferStmt:
		return true
	case *ast.BlockStmt:
		return containsDefer(s.List)
	case *ast.UnsafeStmt:
		return containsDefer(s.Body.List)
	case *ast.IfStmt:
		return containsDefer(s.Body.List) || (s.Else != nil && stmtHasDefer(s.Else))
	case *ast.ForStmt:
		return containsDefer(s.Body.List)
	case *ast.ForInStmt:
		return containsDefer(s.Body.List)
	case *ast.SwitchStmt:
		for _, cl := range s.Body {
			if containsDefer(cl.Body) {
				return true
			}
		}
	case *ast.SelectStmt:
		for _, cl := range s.Body {
			if containsDefer(cl.Body) {
				return true
			}
		}
	case *ast.TryStmt:
		if containsDefer(s.Body.List) || (s.Finally != nil && containsDefer(s.Finally.List)) {
			return true
		}
		for _, cc := range s.Catches {
			if containsDefer(cc.Body.List) {
				return true
			}
		}
	}
	return false
}

// emitReturn returns vals (one C expression per result) from the current
// function, running finally blocks and deferred calls on the way.
func (g *gen) emitReturn(vals []string) {
	sig := g.sig
	if len(sig.Results) == 0 {
		g.leave(0, func() { g.line("return;") })
		return
	}
	if g.deferNamed {
		// deferred functions may modify named results: assign, then return them
		tmps := make([]string, len(vals))
		for i, v := range vals {
			tmps[i] = g.newTmp()
			g.line("%s %s = %s;", g.cType(sig.Results[i].Type()), tmps[i], v)
		}
		var refs []string
		for i, r := range sig.Results {
			g.line("%s = %s;", g.varRef(r), tmps[i])
			refs = append(refs, g.varRef(r))
		}
		ret := refs[0]
		if len(refs) > 1 {
			ret = fmt.Sprintf("(%s){ %s }", g.resultType(sig), join(refs, ", "))
		}
		g.leave(0, func() { g.line("return %s;", ret) })
		return
	}
	val := vals[0]
	if len(vals) > 1 {
		val = fmt.Sprintf("(%s){ %s }", g.resultType(sig), join(vals, ", "))
	}
	if len(g.finStack) == 0 {
		g.line("return %s;", val)
		return
	}
	g.needRetv = true
	g.line("_retv = %s;", val)
	g.leave(0, func() { g.line("return _retv;") })
}

// ---------------------------------------------------------------- function values

func (g *gen) fnPtrType(sig *types.Signature, first string) string {
	ps := []string{first}
	for _, p := range sig.Params {
		ps = append(ps, g.cType(p.Type()))
	}
	return fmt.Sprintf("%s (*)(%s)", g.resultType(sig), strings.Join(ps, ", "))
}

// funcValue returns a gocpy_func for a declared function.
func (g *gen) funcValue(fn *types.Func) string {
	name := "gocpy_fv_" + fn.CName
	if !g.done[name] {
		g.done[name] = true
		sig := fn.Sig()
		ps := []string{"void *gocpy_env"}
		var as []string
		for i, p := range sig.Params {
			ps = append(ps, fmt.Sprintf("%s a%d", g.cType(p.Type()), i))
			as = append(as, fmt.Sprintf("a%d", i))
		}
		proto := fmt.Sprintf("%s %s(%s)", g.resultType(sig), name, strings.Join(ps, ", "))
		ret := "return "
		if len(sig.Results) == 0 {
			ret = ""
		}
		fmt.Fprintf(&g.thunks, "%s { (void)gocpy_env; %s%s(%s); }\n\n", proto, ret, fn.CName, strings.Join(as, ", "))
		fmt.Fprintf(&g.thunkProtos, "%s;\n", proto)
	}
	return fmt.Sprintf("((gocpy_func){ (void *)%s, NULL })", name)
}

// funcLit lifts a function literal and returns the closure value.
func (g *gen) funcLit(e *ast.FuncLit) string {
	sig := g.typeOf(e).(*types.Signature)
	caps := g.info.Captures[e]
	name := "gocpy_lit" + g.newTmp()
	env := "NULL"
	if len(caps) > 0 {
		ev := g.newTmp()
		var b strings.Builder
		fmt.Fprintf(&b, "({ void **%s = (void **)gocpy_alloc(%d * sizeof(void *)); ", ev, len(caps))
		for i, v := range caps {
			fmt.Fprintf(&b, "%s[%d] = (void *)%s; ", ev, i, v.CName)
		}
		fmt.Fprintf(&b, "(void *)%s; })", ev)
		env = b.String()
	}
	saved := g.saveState()
	var out strings.Builder
	g.w, g.ind, g.sig, g.loops = &out, 0, sig, nil
	g.names, g.taken = map[string]int{}, map[string]bool{}
	for _, v := range caps {
		g.taken[v.CName] = true
	}
	ps := []string{"void *gocpy_env"}
	for _, p := range sig.Params {
		ps = append(ps, g.cType(p.Type())+" "+g.paramName(p))
	}
	proto := fmt.Sprintf("%s %s(%s)", g.resultType(sig), name, strings.Join(ps, ", "))
	g.line("%s {", proto)
	g.emitBody(sig, sig.Params, e.Body, func() {
		g.line("(void)gocpy_env;")
		for i, v := range caps {
			ct := g.cType(v.Type())
			g.line("%s *%s = (%s *)((void **)gocpy_env)[%d];", ct, v.CName, ct, i)
		}
	})
	g.restoreState(saved)
	g.thunks.WriteString(out.String())
	fmt.Fprintf(&g.thunkProtos, "%s;\n", proto)
	return fmt.Sprintf("((gocpy_func){ (void *)%s, %s })", name, env)
}

func (g *gen) callValue(e *ast.CallExpr, ci *types.CallInfo) string {
	f := g.newTmp()
	fv := g.expr(e.Fun)
	args := g.callArgs(e, ci)
	call := fmt.Sprintf("({ gocpy_func %s = %s; ((%s)gocpy_func_fn(%s))(%s); })",
		f, fv, g.fnPtrType(ci.Sig, "void *"), f, strings.Join(append([]string{f + ".env"}, args...), ", "))
	if ci.Throws {
		return g.checked(call, ci.Sig)
	}
	return call
}

func (g *gen) callIface(e *ast.CallExpr, ci *types.CallInfo) string {
	r := g.newTmp()
	recv := g.expr(e.Fun.(*ast.SelectorExpr).X)
	args := g.callArgs(e, ci)
	call := fmt.Sprintf("({ gocpy_iface %s = %s; ((%s)gocpy_iface_fn(%s, %d))(%s); })",
		r, recv, g.fnPtrType(ci.Sig, "void *"), r, ci.Index, strings.Join(append([]string{r + ".data"}, args...), ", "))
	if ci.Throws {
		return g.checked(call, ci.Sig)
	}
	return call
}

// ---------------------------------------------------------------- type identity helpers

func shortHash(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// typeName is the Go-style name used in runtime messages and %T.
func (g *gen) typeName(t types.Type) string {
	switch t := t.(type) {
	case *types.Named:
		if t == types.ErrorType {
			return "error"
		}
		if t == types.ExceptionType {
			return "Exception"
		}
		if t.Pkg != "" {
			return t.Pkg + "." + t.Obj.Name()
		}
		s := t.String()
		if i := strings.IndexByte(s, '['); i >= 0 && strings.Contains(s[:i], ".") || i < 0 && strings.Contains(s, ".") {
			return s // declared in an imported package
		}
		return g.pkg.Name + "." + s
	case *types.Pointer:
		return "*" + g.typeName(t.Elem)
	case *types.Slice:
		return "[]" + g.typeName(t.Elem)
	case *types.Array:
		return fmt.Sprintf("[%d]%s", t.Len, g.typeName(t.Elem))
	case *types.Map:
		return "map[" + g.typeName(t.Key) + "]" + g.typeName(t.Elem)
	case *types.Chan:
		return "chan " + g.typeName(t.Elem)
	case *types.Interface:
		if len(t.Methods) == 0 {
			return "interface {}"
		}
	}
	return types.Default(t).String()
}

// sigKey is the canonical signature string shared with the runtime.
func (g *gen) sigKey(sig *types.Signature) string {
	var ps, rs []string
	for _, p := range sig.Params {
		ps = append(ps, g.typeID(p.Type()))
	}
	if sig.Variadic {
		ps = append(ps, "...")
	}
	for _, r := range sig.Results {
		rs = append(rs, g.typeID(r.Type()))
	}
	k := "0"
	if sig.Throws {
		k = "1"
	}
	return k + "(" + strings.Join(ps, ",") + ")(" + strings.Join(rs, ",") + ")"
}

func ptrShaped(t types.Type) bool {
	_, ok := t.Underlying().(*types.Pointer)
	return ok
}

// tiRefT returns a pointer expression to the runtime typeinfo of any type.
func (g *gen) tiRefT(t types.Type) string {
	t = types.Default(t)
	id := g.typeID(t)
	if _, ok := g.typeinfos[id]; !ok {
		g.typeinfos[id] = t
		g.tiOrder = append(g.tiOrder, id)
	}
	return "&gocpy_ti_" + id
}

// methodSet lists the methods usable through interfaces for t (sorted by name).
func methodSet(t types.Type) []*types.Func {
	ptr := false
	n, _ := t.(*types.Named)
	if p, ok := t.(*types.Pointer); ok {
		n, _ = p.Elem.(*types.Named)
		ptr = true
	}
	if n == nil || n == types.ExceptionType {
		return nil
	}
	var out []*types.Func
	for _, name := range sortedKeys(n.Methods) {
		ms := n.Methods[name]
		if len(ms) != 1 || ms[0].IsOperator || (ms[0].PtrRecv && !ptr) {
			continue
		}
		out = append(out, ms[0])
	}
	return out
}

// methodWrapper adapts a method to the interface calling convention.
func (g *gen) methodWrapper(fn *types.Func) string {
	name := "gocpy_mw_" + fn.CName
	if g.done[name] {
		return name
	}
	g.done[name] = true
	sig := fn.Sig()
	rt := g.cType(fn.RecvNamed)
	recv := "*(" + rt + " *)self"
	if fn.PtrRecv {
		recv = "(" + rt + " *)self"
	}
	ps := []string{"void *self"}
	as := []string{recv}
	for i, p := range sig.Params {
		ps = append(ps, fmt.Sprintf("%s a%d", g.cType(p.Type()), i))
		as = append(as, fmt.Sprintf("a%d", i))
	}
	proto := fmt.Sprintf("%s %s(%s)", g.resultType(sig), name, strings.Join(ps, ", "))
	ret := "return "
	if len(sig.Results) == 0 {
		ret = ""
	}
	fmt.Fprintf(&g.thunks, "%s { %s%s(%s); }\n\n", proto, ret, fn.CName, strings.Join(as, ", "))
	fmt.Fprintf(&g.thunkProtos, "%s;\n", proto)
	return name
}

// idescRef returns a pointer to the runtime descriptor of interface type it.
func (g *gen) idescRef(t types.Type) string {
	id := g.typeID(t)
	name := "gocpy_idesc_" + id
	if !g.done[name] {
		g.done[name] = true
		it := t.Underlying().(*types.Interface)
		var names, sigs []string
		for _, m := range it.Methods {
			names = append(names, cString(m.Name))
			sigs = append(sigs, cString(g.sigKey(m.Sig)))
		}
		if len(names) == 0 {
			names, sigs = []string{"NULL"}, []string{"NULL"}
		}
		fmt.Fprintf(&g.thunks, "static const char *const %s_n[] = { %s };\nstatic const char *const %s_s[] = { %s };\n",
			name, strings.Join(names, ", "), name, strings.Join(sigs, ", "))
		fmt.Fprintf(&g.thunks, "const gocpy_idesc %s = { %s, %d, %s_n, %s_s };\n\n",
			name, cString(g.typeName(t)), len(it.Methods), name, name)
		fmt.Fprintf(&g.thunkProtos, "extern const gocpy_idesc %s;\n", name)
	}
	return "&" + name
}

// itabRef returns a static itab for concrete type src implementing interface iface.
func (g *gen) itabRef(iface, src types.Type) string {
	id := g.typeID(iface) + "__" + g.typeID(src)
	name := "gocpy_itab_" + id
	if !g.done[name] {
		g.done[name] = true
		it := iface.Underlying().(*types.Interface)
		var fns []string
		for _, m := range it.Methods {
			_, fn, _ := types.LookupMethod(src, m.Name)
			if fn == nil {
				g.errorf(token.Pos{}, "internal: %s does not implement %s", src, iface)
				fns = append(fns, "NULL")
				continue
			}
			fns = append(fns, "(void *)"+g.methodWrapper(fn))
		}
		n := len(fns)
		if n == 0 {
			fns, n = []string{"NULL"}, 1
		}
		fmt.Fprintf(&g.thunkProtos, "typedef struct { const gocpy_typeinfo *type; void *fns[%d]; } gocpy_itabT_%s;\nextern const gocpy_itabT_%s %s;\n", n, id, id, name)
		fmt.Fprintf(&g.thunks, "const gocpy_itabT_%s %s = { %s, { %s } };\n\n", id, name, g.tiRefT(src), strings.Join(fns, ", "))
	}
	return "((const gocpy_itab *)&" + name + ")"
}

// boxIface converts C value x of type src to interface type target.
func (g *gen) boxIface(x string, src, target types.Type) string {
	if b, ok := src.(*types.Basic); ok && b.Kind == types.UntypedNil {
		return "((gocpy_iface){0})"
	}
	src = types.Default(src)
	ti := target.Underlying().(*types.Interface)
	if si, ok := src.Underlying().(*types.Interface); ok {
		if len(ti.Methods) == 0 || types.Identical(si, ti) {
			return x // same method table (or none needed)
		}
		return fmt.Sprintf("gocpy_iface_conv(%s, %s)", x, g.idescRef(target))
	}
	itab := g.itabRef(target, src)
	if ptrShaped(src) {
		return fmt.Sprintf("((gocpy_iface){ %s, (void *)(%s) })", itab, x)
	}
	v := g.newTmp()
	ct := g.cType(src)
	return fmt.Sprintf("({ %s %s = %s; (gocpy_iface){ %s, gocpy_box(&%s, sizeof(%s)) }; })", ct, v, x, itab, v, ct)
}

// extract returns the concrete value of type t stored in interface value x (C lvalue name).
func (g *gen) extract(x string, t types.Type) string {
	ct := g.cType(t)
	if ptrShaped(t) {
		return fmt.Sprintf("((%s)%s.data)", ct, x)
	}
	return fmt.Sprintf("(*(%s *)%s.data)", ct, x)
}

func (g *gen) typeAssert(e *ast.TypeAssertExpr) string {
	t := g.typeOf(e)
	x := g.newTmp()
	src := g.expr(e.X)
	iname := cString(g.typeName(g.typeOf(e.X)))
	if isIface(t) {
		r := g.newTmp()
		return fmt.Sprintf("({ gocpy_iface %s = %s, %s; if (!gocpy_iface_to_iface(%s, %s, &%s)) gocpy_panic_assert(%s, %s, %s); %s; })",
			x, src, r, x, g.idescRef(t), r, x, iname, cString(g.typeName(t)), r)
	}
	return fmt.Sprintf("({ gocpy_iface %s = %s; if (gocpy_iface_type(%s) != %s) gocpy_panic_assert(%s, %s, %s); %s; })",
		x, src, x, g.tiRefT(t), x, iname, cString(g.typeName(t)), g.extract(x, t))
}

func isIface(t types.Type) bool {
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

// ---------------------------------------------------------------- equality & hashing

// eqFn returns a C function bool f(const void *a, const void *b) for t.
func (g *gen) eqFn(t types.Type) string {
	t = types.Default(t)
	id := g.typeID(t)
	name := "gocpy_eq_" + id
	if g.done[name] {
		return name
	}
	g.done[name] = true
	ct := g.cType(t)
	a, b := "(*(const "+ct+" *)a)", "(*(const "+ct+" *)b)"
	var body string
	switch u := t.Underlying().(type) {
	case *types.Struct:
		var parts []string
		for _, f := range u.Fields {
			fa, fb := a+"."+cIdent(f.Name), b+"."+cIdent(f.Name)
			parts = append(parts, fmt.Sprintf("%s(&%s, &%s)", g.eqFn(f.Type), fa, fb))
		}
		if len(parts) == 0 {
			parts = []string{"true"}
		}
		body = "return " + strings.Join(parts, " && ") + ";"
	case *types.Array:
		body = fmt.Sprintf("for (int64_t i = 0; i < %d; i++) if (!%s(&%s.a[i], &%s.a[i])) return false; return true;", u.Len, g.eqFn(u.Elem), a, b)
	case *types.Interface:
		body = fmt.Sprintf("return gocpy_iface_eq(%s, %s);", a, b)
	default:
		body = "return " + g.compare(token.EQL, t, a, b) + ";"
	}
	fmt.Fprintf(&g.thunkProtos, "bool %s(const void *a, const void *b);\n", name)
	fmt.Fprintf(&g.thunks, "bool %s(const void *a, const void *b) { %s }\n\n", name, body)
	return name
}

// hashFn returns a C function uint64_t f(const void *p) for map key type t.
func (g *gen) hashFn(t types.Type) string {
	t = types.Default(t)
	if b, ok := t.Underlying().(*types.Basic); ok {
		switch {
		case b.IsString():
			return "gocpy_hash_string"
		case b.IsFloat():
			if types.Default(b).(*types.Basic).Kind == types.Float32 {
				return "gocpy_hash_f32"
			}
			return "gocpy_hash_f64"
		}
	}
	id := g.typeID(t)
	name := "gocpy_hash_" + id
	if g.done[name] {
		return name
	}
	g.done[name] = true
	ct := g.cType(t)
	p := "(*(const " + ct + " *)p)"
	var body string
	switch u := t.Underlying().(type) {
	case *types.Struct:
		var b strings.Builder
		b.WriteString("uint64_t h = 0; ")
		for _, f := range u.Fields {
			fmt.Fprintf(&b, "h = gocpy_hash_mix(h, %s(&%s.%s)); ", g.hashFn(f.Type), p, cIdent(f.Name))
		}
		b.WriteString("return h;")
		body = b.String()
	case *types.Array:
		body = fmt.Sprintf("uint64_t h = 0; for (int64_t i = 0; i < %d; i++) h = gocpy_hash_mix(h, %s(&%s.a[i])); return h;", u.Len, g.hashFn(u.Elem), p)
	default:
		body = fmt.Sprintf("return gocpy_hash_bytes(p, sizeof(%s));", ct)
	}
	fmt.Fprintf(&g.thunkProtos, "uint64_t %s(const void *p);\n", name)
	fmt.Fprintf(&g.thunks, "uint64_t %s(const void *p) { %s }\n\n", name, body)
	return name
}

// ---------------------------------------------------------------- typeinfo

func (g *gen) typeinfoDefs(b *strings.Builder) {
	for ; g.tiDone < len(g.tiOrder); g.tiDone++ {
		id := g.tiOrder[g.tiDone]
		t := g.typeinfos[id]
		ct := g.cType(t)
		fmt.Fprintf(b, "static void gocpy_tifmt_%s(gocpy_sb *sb, const void *p) { %s; }\n",
			id, g.dynFmt(t, "(*(const "+ct+" *)p)", "sb"))
		eq := "NULL"
		if types.Keyable(t) || isIface(t) {
			eq = g.eqFn(t)
		}
		ms := methodSet(t)
		mt := "NULL"
		if len(ms) > 0 {
			var es []string
			for _, m := range ms {
				es = append(es, fmt.Sprintf("{ %s, %s, (void *)%s }", cString(m.Name()), cString(g.sigKey(m.Sig())), g.methodWrapper(m)))
			}
			fmt.Fprintf(b, "static const gocpy_method gocpy_tim_%s[] = { %s };\n", id, strings.Join(es, ", "))
			mt = "gocpy_tim_" + id
		}
		kind, elem, idesc := g.tiKind(t)
		nf, fields, key, alen := "0", "NULL", "NULL", "0"
		switch u := t.Underlying().(type) {
		case *types.Struct:
			if len(u.Fields) > 0 {
				var fs []string
				for _, f := range u.Fields {
					fs = append(fs, fmt.Sprintf("{ %s, offsetof(%s, %s), %s }", cString(f.Name), ct, cIdent(f.Name), g.tiRefT(f.Type)))
				}
				fmt.Fprintf(b, "static const gocpy_field gocpy_tif_%s[] = { %s };\n", id, strings.Join(fs, ", "))
				nf, fields = fmt.Sprint(len(u.Fields)), "gocpy_tif_"+id
			}
		case *types.Map:
			key, elem = g.tiRefT(u.Key), g.tiRefT(u.Elem)
		case *types.Array:
			alen = fmt.Sprint(u.Len)
		}
		fmt.Fprintf(b, "const gocpy_typeinfo gocpy_ti_%s = { %s, gocpy_tifmt_%s, sizeof(%s), %v, %s, %d, %s, %s, %s, %s, %s, %s, %s, %s };\n\n",
			id, cString(g.typeName(t)), id, ct, ptrShaped(t), eq, len(ms), mt, kind, elem, idesc, nf, fields, key, alen)
	}
}

// tiKind returns the kind, element typeinfo and interface descriptor fields
// of the typeinfo of t (used by fmt verbs, Scan and errors.As).
func (g *gen) tiKind(t types.Type) (kind, elem, idesc string) {
	kind, elem, idesc = "GOCPY_K_OTHER", "NULL", "NULL"
	switch u := t.Underlying().(type) {
	case *types.Basic:
		b := types.Default(u).(*types.Basic)
		switch {
		case b.Kind == types.Bool:
			kind = "GOCPY_K_BOOL"
		case b.IsString():
			kind = "GOCPY_K_STRING"
		case b.IsFloat():
			kind = "GOCPY_K_FLOAT"
		case b.IsUnsigned():
			kind = "GOCPY_K_UINT"
		case b.IsInteger():
			kind = "GOCPY_K_INT"
		}
	case *types.Pointer:
		kind, elem = "GOCPY_K_PTR", g.tiRefT(u.Elem)
	case *types.Slice:
		kind, elem = "GOCPY_K_SLICE", g.tiRefT(u.Elem)
	case *types.Array:
		kind, elem = "GOCPY_K_ARRAY", g.tiRefT(u.Elem)
	case *types.Interface:
		kind = "GOCPY_K_IFACE"
		if !u.IsConstraint() {
			idesc = g.idescRef(t)
		}
	case *types.Struct:
		kind = "GOCPY_K_STRUCT"
	case *types.Map:
		kind = "GOCPY_K_MAP"
	}
	return
}

// dynFmt formats a dynamic value like Go's fmt: Error() first, then String().
func (g *gen) dynFmt(t types.Type, x, sb string) string {
	for _, name := range []string{"Error", "String"} {
		sig, fn, _ := types.LookupMethod(t, name)
		if fn != nil && len(sig.Params) == 0 && len(sig.Results) == 1 && types.IsString(sig.Results[0].Type()) && !sig.Throws {
			recv := x
			if fn.PtrRecv && !ptrShaped(t) {
				continue
			}
			if !fn.PtrRecv && ptrShaped(t) {
				recv = "(*" + x + ")"
			}
			return fmt.Sprintf("gocpy_sb_str(%s, %s(%s))", sb, fn.CName, recv)
		}
	}
	return g.fmtValue(t, x, sb)
}

// ---------------------------------------------------------------- maps

func (g *gen) mapMake(m *types.Map, hint string) string {
	return fmt.Sprintf("gocpy_map_make(sizeof(%s), sizeof(%s), %s, %s, %s)",
		g.cType(m.Key), g.cType(m.Elem), g.hashFn(m.Key), g.eqFn(m.Key), hint)
}

func (g *gen) mapRead(e *ast.IndexExpr, m *types.Map) string {
	mv, k, v := g.newTmp(), g.newTmp(), g.newTmp()
	kt, vt := g.cType(m.Key), g.cType(m.Elem)
	return fmt.Sprintf("({ gocpy_map *%s = %s; %s %s = %s; %s %s; gocpy_map_get(%s, &%s, &%s, sizeof(%s)); %s; })",
		mv, g.expr(e.X), kt, k, g.expr(e.Indices[0]), vt, v, mv, k, v, vt, v)
}

// mapSlot returns an lvalue for m[k] (inserting the key if missing).
func (g *gen) mapSlot(e *ast.IndexExpr, m *types.Map) string {
	mv, k := g.newTmp(), g.newTmp()
	kt, vt := g.cType(m.Key), g.cType(m.Elem)
	return fmt.Sprintf("(*({ gocpy_map *%s = %s; %s %s = %s; (%s *)gocpy_map_assign(%s, &%s); }))",
		mv, g.expr(e.X), kt, k, g.expr(e.Indices[0]), vt, mv, k)
}

func mapIndex(g *gen, e ast.Expr) (*ast.IndexExpr, *types.Map) {
	ie, ok := unparen(e).(*ast.IndexExpr)
	if !ok {
		return nil, nil
	}
	m, ok := g.typeOf(ie.X).Underlying().(*types.Map)
	if !ok {
		return nil, nil
	}
	return ie, m
}

// lvalue renders an assignable expression (map elements become insert slots).
func (g *gen) lvalue(e ast.Expr) string {
	if ie, m := mapIndex(g, e); ie != nil {
		return g.mapSlot(ie, m)
	}
	return g.expr(e)
}

func (g *gen) mapLit(e *ast.CompositeLit, m *types.Map) string {
	mv := g.newTmp()
	var b strings.Builder
	fmt.Fprintf(&b, "({ gocpy_map *%s = %s; ", mv, g.mapMake(m, fmt.Sprint(len(e.Elts))))
	kt, vt := g.cType(m.Key), g.cType(m.Elem)
	for _, el := range e.Elts {
		kv := el.(*ast.KeyValueExpr)
		k, v := g.newTmp(), g.newTmp()
		fmt.Fprintf(&b, "{ %s %s = %s; %s %s = %s; *(%s *)gocpy_map_assign(%s, &%s) = %s; } ",
			kt, k, g.element(kv.Key, m.Key), vt, v, g.element(kv.Value, m.Elem), vt, mv, k, v)
	}
	fmt.Fprintf(&b, "%s; })", mv)
	return b.String()
}

// ---------------------------------------------------------------- statements

// commaOkExt lowers v, ok := m[k] and v, ok := x.(T).
func (g *gen) commaOkExt(r ast.Expr) (val, ok string, handled bool) {
	switch x := unparen(r).(type) {
	case *ast.IndexExpr:
		m, isMap := g.typeOf(x.X).Underlying().(*types.Map)
		if !isMap {
			return "", "", false
		}
		v, k, okv := g.newTmp(), g.newTmp(), g.newTmp()
		g.line("%s %s;", g.cType(m.Elem), v)
		g.line("%s %s = %s;", g.cType(m.Key), k, g.expr(x.Indices[0]))
		g.line("bool %s = gocpy_map_get(%s, &%s, &%s, sizeof(%s));", okv, g.expr(x.X), k, v, v)
		return v, okv, true
	case *ast.TypeAssertExpr:
		t := g.typeOf(x)
		src, v, okv := g.newTmp(), g.newTmp(), g.newTmp()
		g.line("gocpy_iface %s = %s;", src, g.expr(x.X))
		if isIface(t) {
			g.line("gocpy_iface %s;", v)
			g.line("bool %s = gocpy_iface_to_iface(%s, %s, &%s);", okv, src, g.idescRef(t), v)
			return v, okv, true
		}
		g.line("bool %s = gocpy_iface_type(%s) == %s;", okv, src, g.tiRefT(t))
		g.line("%s %s = %s ? %s : %s;", g.cType(t), v, okv, g.extract(src, t), g.zero(t))
		return v, okv, true
	}
	return "", "", false
}

// deferStmt: arguments are evaluated now; the call runs when the function exits.
func (g *gen) deferStmt(s *ast.DeferStmt) {
	thunk, arg := g.thunkCall(s.Call)
	g.line("gocpy_defer_push(&gocpy_defers, %s, %s);", thunk, arg)
	g.line("}")
}

// thunkCall evaluates the callee and arguments of call into a heap block and
// returns a thunk void f(void*) performing the call. It opens a C block that
// the caller must close.
func (g *gen) thunkCall(call *ast.CallExpr) (thunk, arg string) {
	ci := g.info.Calls[call]
	var ptypes, vals []string
	var target string // call expression in the thunk, %s = argument list
	switch ci.Kind {
	case types.CallBuiltin: // close(ch) / free(p)
		if ci.Builtin.ID == types.BFree {
			ptypes = []string{"void *"}
			target = "gocpy_raw_free(%s)"
		} else {
			ptypes = []string{"gocpy_chan*"}
			target = "gocpy_chan_close(%s)"
		}
		vals = []string{g.expr(call.Args[0])}
	case types.CallValue:
		ptypes = []string{"gocpy_func"}
		vals = []string{g.expr(call.Fun)}
		target = "((" + g.fnPtrType(ci.Sig, "void *") + ")gocpy_func_fn(a->a0))(a->a0.env%s)"
	case types.CallIface:
		ptypes = []string{"gocpy_iface"}
		vals = []string{g.expr(call.Fun.(*ast.SelectorExpr).X)}
		target = fmt.Sprintf("((%s)gocpy_iface_fn(a->a0, %d))(a->a0.data%%s)", g.fnPtrType(ci.Sig, "void *"), ci.Index)
	default:
		sig := ci.Func.Sig()
		if ci.Kind == types.CallMethod {
			ptypes = append(ptypes, g.cType(sig.Recv.Type()))
		}
		target = ci.Func.CName + "(%s)"
	}
	if ci.Kind != types.CallBuiltin {
		sig := ci.Sig
		if ci.Func != nil {
			sig = ci.Func.Sig()
		}
		for _, p := range sig.Params {
			ptypes = append(ptypes, g.cType(p.Type()))
		}
		vals = append(vals, g.callArgs(call, ci)...)
	}
	id := g.newTmp()
	st := "gocpy_thunkargs" + id
	var def strings.Builder
	def.WriteString("typedef struct { ")
	for i, t := range ptypes {
		fmt.Fprintf(&def, "%s a%d; ", t, i)
	}
	if len(ptypes) == 0 {
		def.WriteString("char unused; ")
	}
	fmt.Fprintf(&def, "} %s;\n", st)
	g.thunkProtos.WriteString(def.String())
	first := 0
	if ci.Kind == types.CallValue || ci.Kind == types.CallIface {
		first = 1
	}
	var as []string
	for i := first; i < len(ptypes); i++ {
		as = append(as, fmt.Sprintf("a->a%d", i))
	}
	argList := strings.Join(as, ", ")
	if first == 1 && argList != "" {
		argList = ", " + argList
	}
	thunk = "gocpy_thunk" + id
	fmt.Fprintf(&g.thunks, "void %s(void *p) { %s *a = (%s *)p; (void)a; %s; }\n\n", thunk, st, st, fmt.Sprintf(target, argList))
	fmt.Fprintf(&g.thunkProtos, "void %s(void *p);\n", thunk)
	p := g.newTmp()
	g.line("{")
	g.line("%s *%s = (%s *)gocpy_alloc(sizeof(%s));", st, p, st, st)
	for i, v := range vals {
		g.line("%s->a%d = %s;", p, i, v)
	}
	return thunk, p
}

// propagate lowers x?: on a non-nil error jump to a return path emitted
// after the current statement.
func (g *gen) propagate(e *ast.PropagateExpr) string {
	info := g.info.Propagates[e]
	xt := g.typeOf(e.X)
	t := g.newTmp()
	label := g.newLabel("gocpy_prop")
	g.needPerr = true
	g.props = append(g.props, propPending{label: label})
	errv := t
	if info.N > 1 {
		errv = fmt.Sprintf("%s.r%d", t, info.N-1)
	}
	head := fmt.Sprintf("({ %s %s = %s; if (%s.itab != NULL) { gocpy_perr = %s; goto %s; } ", g.cType(xt), t, g.expr(e.X), errv, errv, label)
	switch info.N {
	case 1:
		return head + "(void)0; })"
	case 2:
		return head + t + ".r0; })"
	}
	rt := g.typeOf(e)
	var parts []string
	for i := 0; i < info.N-1; i++ {
		parts = append(parts, fmt.Sprintf("%s.r%d", t, i))
	}
	return head + fmt.Sprintf("(%s){ %s }; })", g.cType(rt), strings.Join(parts, ", "))
}

// flushProps emits the return paths of ? operators used since mark.
func (g *gen) flushProps(mark int) {
	if len(g.props) <= mark {
		return
	}
	pending := g.props[mark:]
	g.props = g.props[:mark]
	skip := g.newLabel("gocpy_noprop")
	g.line("goto %s;", skip)
	for _, p := range pending {
		g.line("%s: {", p.label)
		g.ind++
		var vals []string
		rs := g.sig.Results
		for i, r := range rs {
			if i == len(rs)-1 {
				vals = append(vals, "gocpy_perr")
			} else {
				vals = append(vals, g.zero(r.Type()))
			}
		}
		g.emitReturn(vals)
		g.ind--
		g.line("}")
	}
	g.line("%s: ;", skip)
}

// typeSwitch lowers switch v := x.(type).
func (g *gen) typeSwitch(s *ast.SwitchStmt, ts *types.TypeSwitch) {
	brk := g.newLabel("brk")
	g.line("{")
	g.ind++
	if s.Init != nil {
		g.stmt(s.Init)
	}
	xt := g.typeOf(ts.X)
	x := g.newTmp()
	g.line("gocpy_iface %s = %s;", x, g.expr(ts.X))
	labels := make([]string, len(s.Body))
	defLabel := brk
	for i, cl := range s.Body {
		labels[i] = g.newLabel("case")
		if cl.List == nil {
			defLabel = labels[i]
			continue
		}
		var conds []string
		for _, e := range cl.List {
			tv := g.info.Types[e]
			switch {
			case tv.Mode != types.TypeExpr:
				conds = append(conds, x+".itab == NULL")
			case isIface(tv.Type):
				conds = append(conds, fmt.Sprintf("(%s.itab != NULL && gocpy_itab_get(%s, %s.itab->type) != NULL)", x, g.idescRef(tv.Type), x))
			default:
				conds = append(conds, fmt.Sprintf("gocpy_iface_type(%s) == %s", x, g.tiRefT(tv.Type)))
			}
		}
		g.line("if (%s) goto %s;", join(conds, " || "), labels[i])
	}
	g.line("goto %s;", defLabel)
	g.loops = append(g.loops, loopLabels{brk: brk, finDepth: len(g.finStack)})
	for i, cl := range s.Body {
		g.line("%s: ;", labels[i])
		g.line("{")
		g.ind++
		if v := ts.Vars[cl]; v != nil {
			vt := v.Type()
			var init string
			switch {
			case types.Identical(vt, xt):
				init = x
			case isIface(vt):
				init = fmt.Sprintf("gocpy_iface_conv(%s, %s)", x, g.idescRef(vt))
			default:
				init = g.extract(x, vt)
			}
			g.declLocal(v, init)
			g.line("(void)%s;", v.CName)
		}
		g.stmts(cl.Body)
		g.line("goto %s;", brk)
		g.ind--
		g.line("}")
	}
	g.loops = g.loops[:len(g.loops)-1]
	g.ind--
	g.line("}")
	g.line("%s: ;", brk)
}

// methodValue lowers x.M used as a value: the receiver is evaluated (and
// copied) now and stored in the closure environment.
func (g *gen) methodValue(mv *types.MethodVal) string {
	var rt, recv, name string
	switch {
	case mv.Iface:
		rt = "gocpy_iface"
		recv = g.expr(mv.Recv)
		name = fmt.Sprintf("gocpy_mvi_%s_%d", g.typeID(g.typeOf(mv.Recv)), mv.Index)
	default:
		fn := mv.Func
		rt = g.cType(fn.RecvNamed)
		if fn.PtrRecv {
			rt += "*"
		}
		recv = g.expr(mv.Recv)
		switch {
		case mv.RecvAddr:
			recv = "&(" + recv + ")"
		case mv.RecvDeref:
			recv = "(*" + recv + ")"
		}
		name = "gocpy_mv_" + fn.CName
	}
	if !g.done[name] {
		g.done[name] = true
		sig := mv.Sig
		ps := []string{"void *gocpy_env"}
		var as []string
		for i, p := range sig.Params {
			ps = append(ps, fmt.Sprintf("%s a%d", g.cType(p.Type()), i))
			as = append(as, fmt.Sprintf("a%d", i))
		}
		proto := fmt.Sprintf("%s %s(%s)", g.resultType(sig), name, strings.Join(ps, ", "))
		ret := "return "
		if len(sig.Results) == 0 {
			ret = ""
		}
		var call string
		if mv.Iface {
			call = fmt.Sprintf("gocpy_iface r = *(gocpy_iface *)gocpy_env; %s((%s)gocpy_iface_fn(r, %d))(%s);",
				ret, g.fnPtrType(sig, "void *"), mv.Index, strings.Join(append([]string{"r.data"}, as...), ", "))
		} else {
			call = fmt.Sprintf("%s%s(%s);", ret, mv.Func.CName, strings.Join(append([]string{"*(" + rt + " *)gocpy_env"}, as...), ", "))
		}
		fmt.Fprintf(&g.thunks, "%s { %s }\n\n", proto, call)
		fmt.Fprintf(&g.thunkProtos, "%s;\n", proto)
	}
	b := g.newTmp()
	return fmt.Sprintf("({ %s *%s = (%s *)gocpy_alloc(sizeof(%s)); *%s = %s; (gocpy_func){ (void *)%s, %s }; })",
		rt, b, rt, rt, b, recv, name, b)
}

// bswapFn returns a function reversing the byte order of every scalar in t
// on little-endian hosts (@be_to_le / @le_to_be are the same operation).
func (g *gen) bswapFn(t types.Type) string {
	t = types.Default(t)
	id := g.typeID(t)
	name := "gocpy_bswap_" + id
	if g.done[name] {
		return name
	}
	g.done[name] = true
	ct := g.cType(t)
	var body strings.Builder
	switch u := t.Underlying().(type) {
	case *types.Basic:
		switch n := u.Bits() / 8; n {
		case 1:
		case 2, 4, 8:
			fmt.Fprintf(&body, "uint%d_t u; memcpy(&u, &x, %d); u = __builtin_bswap%d(u); memcpy(&x, &u, %d); ", n*8, n, n*8, n)
		}
	case *types.Array:
		fmt.Fprintf(&body, "for (int64_t i = 0; i < %d; i++) x.a[i] = %s(x.a[i]); ", u.Len, g.bswapFn(u.Elem))
	case *types.Struct:
		for _, f := range u.Fields {
			fmt.Fprintf(&body, "x.%s = %s(x.%s); ", cIdent(f.Name), g.bswapFn(f.Type), cIdent(f.Name))
		}
	}
	fmt.Fprintf(&g.thunkProtos, "%s %s(%s x);\n", ct, name, ct)
	fmt.Fprintf(&g.thunks, "%s %s(%s x) {\n#if __BYTE_ORDER__ == __ORDER_LITTLE_ENDIAN__\n    %s\n#endif\n    return x;\n}\n\n", ct, name, ct, body.String())
	return name
}
