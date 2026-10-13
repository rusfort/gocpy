package types

import (
	"strings"
	"testing"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/parser"
)

func checkSrc(t *testing.T, src string) error {
	t.Helper()
	f, err := parser.ParseFile("t.gocpy", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, _, err = Check([]*ast.File{f})
	return err
}

func ok(t *testing.T, body string) {
	t.Helper()
	if err := checkSrc(t, "package main\nimport \"std/fmt\"\n"+body+"\nfunc _use() { fmt.Println() }\n"); err != nil {
		t.Errorf("unexpected error:\n%v\nsource:\n%s", err, body)
	}
}

func fails(t *testing.T, body, want string) {
	t.Helper()
	err := checkSrc(t, "package main\nimport \"std/fmt\"\n"+body+"\nfunc _use() { fmt.Println() }\n")
	if err == nil {
		t.Errorf("expected error containing %q\nsource:\n%s", want, body)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func TestNoRedeclarationInSameScope(t *testing.T) {
	fails(t, `func f() (int, int) { return 1, 2 }
func main() {
    a, err := f()
    b, err := f()
    _ = a + b + err
}`, "err already declared in this scope")
	fails(t, `func main() { x := 1; x := 2; _ = x }`, "x already declared")
	fails(t, `func g(x int) { x := 2; _ = x }
func main() {}`, "x already declared")
	// assignment with = is the way
	ok(t, `func f() (int, int) { return 1, 2 }
func main() {
    a, err := f()
    var b int
    b, err = f()
    _ = a + b + err
}`)
	ok(t, `func main() { if true { x := 2; _ = x }; x := 1; _ = x }`)
}

func TestImportNameConflicts(t *testing.T) {
	fails(t, `func main() { fmt := 1; _ = fmt }`, `conflicts with imported package "std/fmt"`)
	fails(t, `func main() {}
func fmt() {}`, "conflicts with imported package")
	fails(t, `var fmt = 3
func main() {}`, "conflicts with imported package")
	fails(t, `func f(fmt int) {}
func main() {}`, "conflicts with imported package")
	fails(t, `func main() { for fmt in 3 { } }`, "conflicts with imported package")
	fails(t, `type fmt struct{}
func main() {}`, "conflicts with imported package")
}

func TestInIsKeyword(t *testing.T) {
	_, err := parser.ParseFile("t", []byte("package main\nfunc main() { in := 1 }\n"))
	if err == nil {
		t.Fatal("`in` must not be usable as an identifier")
	}
}

func TestTypeErrors(t *testing.T) {
	fails(t, `func main() { var x int = "s"; _ = x }`, "cannot use")
	fails(t, `func main() { var x int8 = 300; _ = x }`, "overflows")
	fails(t, `func main() { x := 1; y := 2.5; _ = x + y }`, "mismatched types")
	fails(t, `func main() { _ = undefinedThing }`, "undefined: undefinedThing")
	fails(t, `func f() int { if true { return 1 } }
func main() {}`, "missing return")
	fails(t, `func main() { x := 1; x.foo() }`, "has no field or method")
	fails(t, `func main() { return 1 }`, "too many return values")
	fails(t, `func main() { break }`, "break is not in a loop")
	fails(t, `func main() { x := 1 / 0; _ = x }`, "division by zero")
	fails(t, `type P struct{ X int }
func main() { a := P{1}; b := P{2}; _ = a == b }`, "@overload func (a P) ==")
	fails(t, `func main() { xs := []int{1}; _ = xs[-1] }`, "must not be negative")
	fails(t, `func main() { var a [3]int; _ = a[5] }`, "out of bounds")
	fails(t, `func main() { _ = f"{1:.2f}" }`, "requires a float")
	fails(t, `func main() { 1 + 2 }`, "is not used")
}

func TestOverloadResolution(t *testing.T) {
	src := `@overload
func Max(a, b int) int { return a }
@overload
func Max(a, b float64) float64 { return a }
`
	ok(t, src+`func main() { _ = Max(1, 2); _ = Max(1.5, 2.5) }`)
	fails(t, src+`func main() { _ = Max(1, 2.0) }`, "ambiguous call")
	fails(t, src+`func main() { _ = Max("a", "b") }`, "no overload of Max matches")
	fails(t, src+`func main() { var x int = 1; var y float64 = 2; _ = Max(x, y) }`, "no overload")
}

func TestNotYetSupported(t *testing.T) {
}

func TestConstants(t *testing.T) {
	ok(t, `const N = 1 << 62
const F = N * 2.0
func main() { var x uint64 = N * 2; var y float64 = F; _ = x; _ = y; var a [N >> 60]int; _ = a }`)
	fails(t, `const N = 1 << 62
func main() { var x int64 = N * 2; _ = x }`, "overflows")
}

func TestNoShadowing(t *testing.T) {
	fails(t, `func main() { x := 1; if true { x := 2; _ = x }; _ = x }`, "shadowing is not allowed")
	fails(t, `func main() { x := 1; for i := 0; i < 3; i++ { x := i; _ = x }; _ = x }`, "shadows the declaration")
	fails(t, `func f(n int) { if true { n := 1; _ = n } }
func main() {}`, "shadows")
	fails(t, `var total = 1
func main() { total := 2; _ = total }`, "shadows")
	fails(t, `func helper() {}
func main() { helper := 1; _ = helper }`, "shadows")
	fails(t, `func main() { xs := []int{1}; x := 0; ys := [x for x in xs]; _ = ys; _ = x }`, "shadows")
	fails(t, `func main() { len := 3; _ = len }`, "shadows a predeclared identifier")
	fails(t, `type int struct{}
func main() {}`, "predeclared")
	// sequential sibling scopes are fine
	ok(t, `func main() { for i in 3 { _ = i }; for i in 3 { _ = i } }`)
}

func TestUnused(t *testing.T) {
	fails(t, `func main() { x := 1 }`, "x declared and not used")
	fails(t, `func main() { var x int; x = 2 }`, "x declared and not used")
	fails(t, `func main() { x := 1; x++ }`, "declared and not used")
	fails(t, `func main() { x := 1; x += 2 }`, "declared and not used")
	fails(t, `func main() { for i, v in []int{1} { _ = v } }`, "i declared and not used")
	ok(t, `func main() { x := 1 //gocpy:ignore:unused
}`)
	ok(t, `func main() { x := 1; p := &x; _ = p }`)
	ok(t, `func f(unusedParam int) (r int) { return 0 }
func main() { _ = f(1) }`)
	ok(t, `func main() { const k = 5 }`)
	fails(t, `func main() { //gocpy:ignore:everything
}`, "unknown directive")
	// directive applies only to its own line
	fails(t, `func main() {
    //gocpy:ignore:unused
    x := 1
}`, "x declared and not used")
}

const excDecls = `type ParseError struct { Line int }
type IOError struct { Path string }
`

func TestExceptions(t *testing.T) {
	ok(t, excDecls+`@throws
func parse(s string) int {
    if s == "" { throw ParseError{1} }
    return len(s)
}
func main() {
    try {
        _ = parse("x")
    } catch e: ParseError {
        _ = e.Line
    } catch e {
        _ = e.Message()
    } finally {
        _ = 1
    }
}`)
	fails(t, excDecls+`func f() { throw ParseError{1} }
func main() {}`, "may escape f; add @throws")
	fails(t, excDecls+`@throws
func f() { throw ParseError{1} }
func g() { f() }
func main() {}`, "exception from f() may escape g")
	fails(t, excDecls+`@throws
func f() { throw ParseError{1} }
func g() { try { f() } catch e: ParseError { _ = e } }
func main() {}`, "may escape g")
	fails(t, `@throws
func f() {}
func main() {}`, "never throws")
	fails(t, `func main() { throw 42 }`, "named type")
	fails(t, excDecls+`func main() { try { } catch e: IOError { _ = e } catch e: IOError { _ = e } catch {} }`, "duplicate catch")
	fails(t, `func main() { for i in 3 { _ = i; try { } finally { break } } }`, "cannot leave a finally")
	fails(t, `func f() int { try { } finally { return 1 } }
func main() {}`, "return cannot leave a finally")
	ok(t, `func main() { try { } finally { for i in 3 { if i > 1 { break } } } }`)
	fails(t, excDecls+`@throws
func mk() int { throw IOError{"x"} }
var g = mk()
func main() {}`, "package-level initializer")
	// rethrow of a caught Exception
	ok(t, excDecls+`@throws
func f() { throw IOError{"a"} }
@throws
func g() { try { f() } catch e { throw e } }
func main() { try { g() } catch {} }`)
	// precise resolution: same method name on different types
	ok(t, excDecls+`type A struct{}
type B struct{}
@throws
func (a A) Run() { throw IOError{"a"} }
func (b B) Run() {}
func main() { var b B; b.Run() }`)
}

func TestConcurrency(t *testing.T) {
	ok(t, `func w(ch chan int, n int) { ch <- n }
func main() {
    ch := make(chan int, 3)
    go w(ch, 1)
    v := <-ch
    v2, more := <-ch
    close(ch)
    for x in ch { _ = x }
    select {
    case a := <-ch:
        _ = a
    case b, okb := <-ch:
        _ = b; _ = okb
    case ch <- 5:
    case <-ch:
    default:
    }
    _ = v + v2 + len(ch) + cap(ch)
    _ = more
    var nc chan int
    _ = nc == nil
}`)
	fails(t, `func main() { x := 1; x <- 2 }`, "non-channel")
	fails(t, `func main() { x := 1; _ = <-x }`, "non-channel")
	fails(t, `func main() { x := 1; close(x) }`, "close of non-channel")
	fails(t, `func main() { ch := make(chan int); ch <- "s" }`, "cannot use")
	fails(t, `func main() { ch := make(chan int, 1, 2); _ = ch }`, "at most one size")
	fails(t, `func main() { ch := make(chan int); for i, v in ch { _ = i; _ = v } }`, "only one value")
	fails(t, `@throws
func f() { throw Exception("x") }
func main() { go f() }`, "cannot be @throws")
	fails(t, `func main() { go len("x") }`, "function or method call")
	fails(t, `func main() { ch := make(chan int); select { case x := 1: _ = x } }`, "send or receive")
	fails(t, `func main() { select { default: default: } }`, "multiple defaults")
}

func TestStdPackages(t *testing.T) {
	src := func(body string) string {
		return "package main\nimport (\n\"std/sync\"\n\"std/time\"\n)\nvar _ sync.Mutex\nvar _ time.Duration\n" + body
	}
	for _, body := range []string{
		`func main() { var wg sync.WaitGroup; wg.Add(1); wg.Done(); wg.Wait(); var mu sync.Mutex; mu.Lock(); mu.Unlock() }`,
		`func main() { d := 2 * time.Second; time.Sleep(d); start := time.Now(); _ = time.Since(start).Milliseconds() }`,
		`func f(wg *sync.WaitGroup, d time.Duration) { wg.Done(); time.Sleep(d) }
func main() { var wg sync.WaitGroup; f(&wg, time.Millisecond) }`,
	} {
		if err := checkSrc(t, src(body)); err != nil {
			t.Errorf("%v\n%s", err, body)
		}
	}
	if err := checkSrc(t, src(`func main() { var d time.Duration = 5; var x int = d; _ = x }`)); err == nil {
		t.Error("Duration must not be assignable to int")
	}
	if err := checkSrc(t, "package main\nimport \"std/nope\"\nfunc main() {}"); err == nil || !strings.Contains(err.Error(), "available: std/errors, std/fmt, std/http, std/io, std/strconv, std/strings, std/sync, std/time, std/udp") {
		t.Errorf("unknown package: %v", err)
	}
}

func TestExceptionBuiltin(t *testing.T) {
	ok(t, `@throws
func f() { throw Exception("boom") }
func main() {
    try { f() } catch e: Exception { _ = e.Message() + e.TypeName() + e.String() }
    e := Exception("x")
    _ = e
}`)
	fails(t, `func main() { _ = Exception(42) }`, "cannot use")
	fails(t, `type E struct{}
func main() { try { } catch e: Exception { _ = e } catch x: E { _ = x } }`, "catch-all must be the last")
}

func TestGenerics(t *testing.T) {
	ok(t, `func Id[T any](x T) T { return x }
type Box[T any] struct { v T }
func (b Box[T]) Get() T { return b.v }
func main() { _ = Id(1); _ = Id[string]("s"); b := Box[int]{v: 1}; _ = b.Get() }`)
	fails(t, `func Max[T int | float64](a, b T) T { return a }
func main() { _ = Max("a", "b") }`, "does not satisfy")
	fails(t, `func Z[T any]() T { var z T; return z }
func main() { _ = Z() }`, "cannot infer T")
	fails(t, `type Box[T any] struct { v T }
func main() { var b Box; _ = b }`, "requires type arguments")
	fails(t, `type N interface { ~int }
func main() { var n N; _ = n }`, "outside a type parameter list")
	fails(t, `func K[T comparable](x T) bool { return x == x }
func main() { _ = K([]int{1}) }`, "does not satisfy comparable")
	fails(t, `func Bad[T any](x T) T { return x + 1 }
func main() { _ = Bad("s") }`, "in instance Bad[string]")
	fails(t, `func Rec[T any](x T) int { return Rec([]T{x}) }
func main() { _ = Rec(1) }`, "infinite recursive instantiation")
	fails(t, `@overload
func G[T any](x T) {}
func main() {}`, "cannot be @overload")
}

func TestMethodValues(t *testing.T) {
	ok(t, `type C struct { n int }
func (c *C) Inc() int { c.n++; return c.n }
func main() { var c C; f := c.Inc; _ = f() }`)
	fails(t, `type C struct { n int }
func (c *C) Inc() int { c.n++; return c.n }
func main() { f := C{}.Inc; _ = f }`, "not addressable")
}

func TestUnsafe(t *testing.T) {
	ok(t, `func main() { unsafe { p := (*int64)(malloc(16)); defer free(p); p[1] = 2; q := p + 1; _ = q - p; _ = q > p; p++; _ = uintptr(p) } }`)
	fails(t, `func main() { p := malloc(8); _ = p }`, "only available inside an unsafe block")
	fails(t, `func main() { var x int64; p := &x; _ = p + 1 }`, "requires an unsafe block")
	fails(t, `func main() { var x int64; p := &x; _ = p[0] }`, "requires an unsafe block")
	fails(t, `func main() { var b [8]byte; unsafe { _ = (*string)(&b[0]) } }`, "contains GC pointers")
	fails(t, `func main() { free := 1; unsafe { _ = free } }`, "rename it")
	fails(t, `func main() { _ = @nope(1) }`, "unknown intrinsic")
	ok(t, `func main() { _ = @be_to_le(uint32(1)) }`)
}

// Imports used only inside generic declarations count as used even when the
// generic is never instantiated (for example, a library package checked alone).
func TestImportsUsedInGenerics(t *testing.T) {
	src := `package lib

import (
	"std/errors"
	"std/fmt"
	"std/sync"
)

type Slice[T any] struct {
	mtx sync.Mutex
	sl  []T
}

func (s *Slice[T]) Err() error {
	return errors.New("x")
}

func Show[T any](x T) {
	fmt.Println(x)
}
`
	if err := checkSrc(t, src); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	src = strings.Replace(src, "\t\"std/sync\"\n", "\t\"std/sync\"\n\t\"std/time\"\n", 1)
	err := checkSrc(t, src)
	if err == nil || !strings.Contains(err.Error(), `"std/time" imported and not used`) {
		t.Errorf("want unused std/time, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "std/sync") {
		t.Errorf("std/sync reported as unused: %v", err)
	}
}
