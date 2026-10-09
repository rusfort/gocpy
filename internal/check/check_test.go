package check

import (
	"strings"
	"testing"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/parser"
)

func diags(t *testing.T, src string) []Diagnostic {
	t.Helper()
	f, err := parser.ParseFile("t.gocpy", []byte("package p\n"+src))
	if err != nil {
		t.Fatal(err)
	}
	return Package([]*ast.File{f})
}

func expect(t *testing.T, src string, wants ...string) {
	t.Helper()
	ds := diags(t, src)
	if len(wants) == 0 && len(ds) > 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	for _, w := range wants {
		found := false
		for _, d := range ds {
			if strings.Contains(d.String(), w) {
				found = true
			}
		}
		if !found {
			t.Errorf("want diagnostic containing %q, got %v", w, ds)
		}
	}
}

func TestMainCannotThrow(t *testing.T) {
	expect(t, `@throws
func main() { throw E{} }`, "main cannot be @throws")
}

func TestOverloadRules(t *testing.T) {
	expect(t, `@overload
func Max(a, b int) int { return a }
@overload
func Max(a, b float64) float64 { return a }`)
	expect(t, `func Max(a, b int) int { return a }
@overload
func Max(a, b float64) float64 { return a }`, "must be marked @overload")
	expect(t, `@overload
func Max(a, b int) int { return a }
@overload
func Max(x, y int) int { return x }`, "ambiguous overload")
	expect(t, `func (a M) +(b M) M { return a }`, "requires @overload")
	expect(t, `@overload
func Only(a int) {}`, "only one variant")
	// methods on different types do not clash
	expect(t, `func (a A) Len() int { return 0 }
func (b B) Len() int { return 0 }`)
}

func TestUnknownAnnotation(t *testing.T) {
	expect(t, `@fast
func f() {}`, "unknown annotation @fast")
	expect(t, `@inline
@noinline
func f() {}`, "mutually exclusive")
}
