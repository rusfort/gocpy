package types

import (
	"go/constant"
	"sort"
	"strings"

	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/stdlib"
)

// StdMembers holds non-builtin members of std packages: types, functions
// and constants implemented by the C runtime.
var StdMembers = map[string]map[string]Object{}

// StdList returns the sorted list of std package paths.
func StdList() string {
	var ps []string
	for p := range StdPackages {
		ps = append(ps, p)
	}
	ps = append(ps, stdlib.Packages()...)
	sort.Strings(ps)
	return strings.Join(ps, ", ")
}

func stdType(pkg, name, cname string, u Type) *Named {
	tn := &TypeName{object{name: name}}
	n := &Named{Obj: tn, underlying: u, Methods: map[string][]*Func{}, CName: cname, Pkg: pkg}
	tn.typ = n
	return n
}

func stdMethod(n *Named, name, cname string, ptr bool, params []*Var, results []*Var) {
	var rt Type = n
	if ptr {
		rt = &Pointer{Elem: n}
	}
	sig := &Signature{Recv: NewVar("r", rt, token.Pos{}), Params: params, Results: results}
	n.Methods[name] = []*Func{{object: object{name: name, typ: sig}, RecvNamed: n, PtrRecv: ptr, CName: cname}}
}

func stdFunc(name, cname string, params []*Var, results []*Var) *Func {
	return &Func{object: object{name: name, typ: &Signature{Params: params, Results: results}}, CName: cname}
}

func param(t Type) []*Var { return []*Var{NewVar("x", t, token.Pos{})} }

func initStd() {
	// std/sync
	wg := stdType("sync", "WaitGroup", "gocpy_waitgroup", &Struct{Fields: []*Field{{Name: "n", Type: Typ[Int64]}}})
	stdMethod(wg, "Add", "gocpy_wg_add", true, param(Typ[Int]), nil)
	stdMethod(wg, "Done", "gocpy_wg_done", true, nil, nil)
	stdMethod(wg, "Wait", "gocpy_wg_wait", true, nil, nil)
	mu := stdType("sync", "Mutex", "gocpy_mutex", &Struct{Fields: []*Field{{Name: "locked", Type: Typ[Bool]}}})
	stdMethod(mu, "Lock", "gocpy_mutex_lock", true, nil, nil)
	stdMethod(mu, "Unlock", "gocpy_mutex_unlock", true, nil, nil)
	StdPackages["std/sync"] = map[string]BuiltinID{}
	StdMembers["std/sync"] = map[string]Object{"WaitGroup": wg.Obj, "Mutex": mu.Obj}

	// std/time
	dur := stdType("time", "Duration", "int64_t", Typ[Int64])
	tm := map[string]Object{"Duration": dur.Obj}
	for name, ns := range map[string]int64{
		"Nanosecond": 1, "Microsecond": 1e3, "Millisecond": 1e6, "Second": 1e9, "Minute": 60e9, "Hour": 3600e9,
	} {
		tm[name] = &Var{object: object{name: name, typ: dur}, IsConst: true, Value: constant.MakeInt64(ns)}
	}
	stdMethod(dur, "Milliseconds", "gocpy_dur_ms", false, nil, []*Var{NewVar("", Typ[Int64], token.Pos{})})
	stdMethod(dur, "Seconds", "gocpy_dur_seconds", false, nil, []*Var{NewVar("", Typ[Float64], token.Pos{})})
	tm["Sleep"] = stdFunc("Sleep", "gocpy_sleep", param(dur), nil)
	tm["Since"] = stdFunc("Since", "gocpy_since", param(Typ[Int64]), []*Var{NewVar("", dur, token.Pos{})})
	tm["Now"] = stdFunc("Now", "gocpy_now", nil, []*Var{NewVar("", Typ[Int64], token.Pos{})})
	tm["UnixNano"] = stdFunc("UnixNano", "gocpy_unix_nano", nil, []*Var{NewVar("", Typ[Int64], token.Pos{})})
	StdPackages["std/time"] = map[string]BuiltinID{}
	StdMembers["std/time"] = tm
}

// initFmtLib declares the fmt functions implemented in gocpy_lib.c (the
// Printf and Scan families) and fmt.Stringer.
func initFmtLib() {
	pos := token.Pos{}
	v := func(name string, t Type) *Var { return NewVar(name, t, pos) }
	bytesT := &Slice{Elem: Typ[Uint8]}
	anys := v("a", &Slice{Elem: AnyType})
	intErr := []*Var{v("n", Typ[Int]), v("err", ErrorType)}
	rw := func(name string) *Interface {
		return &Interface{Methods: []*IMethod{{Name: name, Sig: &Signature{
			Params: []*Var{v("p", bytesT)}, Results: intErr}}}}
	}
	writer, reader := rw("Write"), rw("Read")
	vf := func(name, cname string, params []*Var, results []*Var) *Func {
		f := stdFunc(name, cname, append(params, anys), results)
		f.Sig().Variadic = true
		return f
	}
	str := []*Var{v("", Typ[String])}
	m := map[string]Object{
		"Printf":   vf("Printf", "gocpy_fmt_printf", []*Var{v("format", Typ[String])}, intErr),
		"Sprintf":  vf("Sprintf", "gocpy_fmt_sprintf", []*Var{v("format", Typ[String])}, str),
		"Sprintln": vf("Sprintln", "gocpy_fmt_sprintln", nil, str),
		"Errorf":   vf("Errorf", "gocpy_fmt_errorf", []*Var{v("format", Typ[String])}, []*Var{v("", ErrorType)}),
		"Fprint":   vf("Fprint", "gocpy_fmt_fprint", []*Var{v("w", writer)}, intErr),
		"Fprintln": vf("Fprintln", "gocpy_fmt_fprintln", []*Var{v("w", writer)}, intErr),
		"Fprintf":  vf("Fprintf", "gocpy_fmt_fprintf", []*Var{v("w", writer), v("format", Typ[String])}, intErr),
		"Scan":     vf("Scan", "gocpy_fmt_scan", nil, intErr),
		"Scanln":   vf("Scanln", "gocpy_fmt_scanln", nil, intErr),
		"Scanf":    vf("Scanf", "gocpy_fmt_scanf", []*Var{v("format", Typ[String])}, intErr),
		"Sscan":    vf("Sscan", "gocpy_fmt_sscan", []*Var{v("str", Typ[String])}, intErr),
		"Sscanln":  vf("Sscanln", "gocpy_fmt_sscanln", []*Var{v("str", Typ[String])}, intErr),
		"Sscanf":   vf("Sscanf", "gocpy_fmt_sscanf", []*Var{v("str", Typ[String]), v("format", Typ[String])}, intErr),
		"Fscan":    vf("Fscan", "gocpy_fmt_fscan", []*Var{v("r", reader)}, intErr),
		"Fscanln":  vf("Fscanln", "gocpy_fmt_fscanln", []*Var{v("r", reader)}, intErr),
		"Fscanf":   vf("Fscanf", "gocpy_fmt_fscanf", []*Var{v("r", reader), v("format", Typ[String])}, intErr),
	}
	stringer := stdType("fmt", "Stringer", "gocpy_iface", &Interface{Methods: []*IMethod{{Name: "String",
		Sig: &Signature{Results: str}}}})
	m["Stringer"] = stringer.Obj
	StdMembers["std/fmt"] = m

	e := StdMembers["std/errors"]
	errT := []*Var{v("", ErrorType)}
	e["Is"] = stdFunc("Is", "gocpy_errors_is", []*Var{v("err", ErrorType), v("target", ErrorType)}, []*Var{v("", Typ[Bool])})
	e["As"] = stdFunc("As", "gocpy_errors_as", []*Var{v("err", ErrorType), v("target", AnyType)}, []*Var{v("", Typ[Bool])})
	e["Unwrap"] = stdFunc("Unwrap", "gocpy_errors_unwrap", []*Var{v("err", ErrorType)}, errT)
	join := stdFunc("Join", "gocpy_errors_join", []*Var{v("errs", &Slice{Elem: ErrorType})}, errT)
	join.Sig().Variadic = true
	e["Join"] = join
}
