package types

import (
	"go/constant"
	"sort"
	"strings"

	"github.com/rusfort/gocpy/internal/token"
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
	StdPackages["std/time"] = map[string]BuiltinID{}
	StdMembers["std/time"] = tm
}
