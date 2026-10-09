package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

func parse(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := ParseFile("t.gocpy", []byte(src))
	if err != nil {
		t.Fatalf("parse error:\n%v\nsource:\n%s", err, src)
	}
	return f
}

func body(t *testing.T, stmts string) []ast.Stmt {
	t.Helper()
	f := parse(t, "package p\nfunc f() {\n"+stmts+"\n}\n")
	return f.Decls[0].(*ast.FuncDecl).Body.List
}

func TestExamplesParse(t *testing.T) {
	files, _ := filepath.Glob("../../examples/**/*.gocpy")
	more, _ := filepath.Glob("../../examples/*.gocpy")
	files = append(files, more...)
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	for _, p := range files {
		src, _ := os.ReadFile(p)
		if _, err := ParseFile(p, src); err != nil {
			t.Errorf("%s:\n%v", p, err)
		}
	}
}

func TestFuncDeclAnnotations(t *testing.T) {
	f := parse(t, `package p

@inline
@throws(IOError, InvalidOrder)
func Read(path string, n ...int) ([]byte, error) { return nil, nil }
`)
	d := f.Decls[0].(*ast.FuncDecl)
	if len(d.Annots) != 2 || d.Annots[0].Name != "inline" || d.Annots[1].Name != "throws" {
		t.Fatalf("annots: %s", ast.Sprint(d.Annots))
	}
	if len(d.Annots[1].Args) != 2 {
		t.Fatalf("throws args: %d", len(d.Annots[1].Args))
	}
	if _, ok := d.Type.Params.List[1].Type.(*ast.Ellipsis); !ok {
		t.Fatalf("variadic param not parsed")
	}
	if len(d.Type.Results.List) != 2 {
		t.Fatalf("results")
	}
}

func TestParamGrouping(t *testing.T) {
	f := parse(t, "package p\nfunc F(a, b int, buf []byte, m Map[K, V], s [4]int) {}\n")
	ps := f.Decls[0].(*ast.FuncDecl).Type.Params.List
	if len(ps) != 4 || len(ps[0].Names) != 2 || ps[1].Names[0].Name != "buf" ||
		ps[2].Names[0].Name != "m" || ps[3].Names[0].Name != "s" {
		t.Fatalf("got %s", ast.Sprint(ps))
	}
	if _, ok := ps[2].Type.(*ast.IndexExpr); !ok {
		t.Fatalf("generic param type: %T", ps[2].Type)
	}
	// unnamed params
	f = parse(t, "package p\ntype H func(int, *string) error\n")
	ft := f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.FuncType)
	if len(ft.Params.List) != 2 || ft.Params.List[0].Names != nil {
		t.Fatalf("unnamed: %s", ast.Sprint(ft.Params))
	}
}

func TestOperatorMethods(t *testing.T) {
	f := parse(t, `package p
@overload
func (a Money) +(b Money) Money { return a }
@overload
func (v Vec) [](i int) float64 { return 0 }
@overload
func (a Money) ==(b Money) bool { return true }
`)
	want := []string{"+", "[]", "=="}
	for i, d := range f.Decls {
		fd := d.(*ast.FuncDecl)
		if !fd.IsOperator || fd.Name.Name != want[i] {
			t.Errorf("decl %d: %q operator=%v", i, fd.Name.Name, fd.IsOperator)
		}
	}
	if _, err := ParseFile("t", []byte("package p\nfunc +(a, b int) int { return a }\n")); err == nil {
		t.Fatal("operator without receiver must fail")
	}
}

func TestGenerics(t *testing.T) {
	f := parse(t, `package p
type Number interface { ~int | int64 | float64 }
type List[T any] struct { items []T }
type Buf [N]byte
func Sum[T Number](xs []T) T { var s T; return s }
func use() { x := Sum[int](xs); m := Pair[string, int]{"a", 1} }
`)
	if ts := f.Decls[1].(*ast.GenDecl).Specs[0].(*ast.TypeSpec); ts.TypeParams == nil {
		t.Fatal("List type params")
	}
	if ts := f.Decls[2].(*ast.GenDecl).Specs[0].(*ast.TypeSpec); ts.TypeParams != nil {
		t.Fatal("Buf must be an array type, not generic")
	}
	if fd := f.Decls[3].(*ast.FuncDecl); fd.Type.TypeParams == nil {
		t.Fatal("Sum type params")
	}
}

func TestComprehension(t *testing.T) {
	s := body(t, `names := [u.Name for u in users if u.ID > 1]
pairs := [k for k, v in m]`)
	c := s[0].(*ast.AssignStmt).Rhs[0].(*ast.ComprehensionExpr)
	if c.Value.Name != "u" || c.Cond == nil {
		t.Fatalf("got %s", ast.Sprint(c))
	}
	c2 := s[1].(*ast.AssignStmt).Rhs[0].(*ast.ComprehensionExpr)
	if c2.Key.Name != "k" || c2.Value.Name != "v" {
		t.Fatal("key/value")
	}
}

func TestForForms(t *testing.T) {
	s := body(t, `for {}
for x < 10 {}
for i := 0; i < n; i++ {}
for ; p < end; p++ {}
for v in xs {}
for i, v in xs {}`)
	types := []string{"*ast.ForStmt", "*ast.ForStmt", "*ast.ForStmt", "*ast.ForStmt", "*ast.ForInStmt", "*ast.ForInStmt"}
	for i, st := range s {
		if got := typeName(st); got != types[i] {
			t.Errorf("stmt %d: %s, want %s", i, got, types[i])
		}
	}
	if s[1].(*ast.ForStmt).Cond == nil || s[2].(*ast.ForStmt).Post == nil {
		t.Fatal("for clauses")
	}
}

func TestTryCatch(t *testing.T) {
	s := body(t, `try {
    process(o)
} catch e: InvalidOrder {
    log(e)
} catch e {
    throw e
} finally {
    cleanup()
}`)
	tr := s[0].(*ast.TryStmt)
	if len(tr.Catches) != 2 || tr.Catches[0].Type == nil || tr.Catches[1].Type != nil || tr.Finally == nil {
		t.Fatalf("got %s", ast.Sprint(tr))
	}
	for _, bad := range []string{"try {}", "try {} catch {} catch e: E {}"} {
		if _, err := ParseFile("t", []byte("package p\nfunc f() {\n"+bad+"\n}\n")); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestPropagateAndUnsafe(t *testing.T) {
	s := body(t, `data := os.ReadFile(path)?
unsafe {
    p := (*Tick)(&buf[0])
    p++
    mem := malloc(64)
    defer free(mem)
    x := @be_to_le(*p)
}`)
	if _, ok := s[0].(*ast.AssignStmt).Rhs[0].(*ast.PropagateExpr); !ok {
		t.Fatal("? not parsed")
	}
	u := s[1].(*ast.UnsafeStmt)
	last := u.Body.List[4].(*ast.AssignStmt).Rhs[0].(*ast.IntrinsicExpr)
	if last.Name != "be_to_le" {
		t.Fatal("intrinsic")
	}
}

func TestCompositeInHeader(t *testing.T) {
	s := body(t, `if p == Point {
}
if x := (Point{1, 2}); x.X > 0 {
}
for v in []int{1, 2, 3} {
}`)
	if _, ok := s[0].(*ast.IfStmt).Cond.(*ast.BinaryExpr); !ok {
		t.Fatal("if cond")
	}
	if s[1].(*ast.IfStmt).Init == nil {
		t.Fatal("if init")
	}
	if _, ok := s[2].(*ast.ForInStmt).Iter.(*ast.CompositeLit); !ok {
		t.Fatal("slice literal in for-in header")
	}
}

func TestFStringParts(t *testing.T) {
	x, err := ParseExpr(`f"{a + b:>8} and {m["k"]} {{literal}}"`)
	if err != nil {
		t.Fatal(err)
	}
	fs := x.(*ast.FString)
	if len(fs.Parts) != 4 {
		t.Fatalf("parts: %s", ast.Sprint(fs))
	}
	if fs.Parts[0].Spec != ">8" {
		t.Fatalf("spec %q", fs.Parts[0].Spec)
	}
	if fs.Parts[3].Text != " {literal}" {
		t.Fatalf("text %q", fs.Parts[3].Text)
	}
	if _, err := ParseExpr(`f"{}"`); err == nil {
		t.Fatal("empty interpolation must fail")
	}
}

func TestSelectSwitchChan(t *testing.T) {
	s := body(t, `ch := chan[int](10)
go worker(ch)
select {
case v := <-ch:
    use(v)
case ch <- 1:
default:
}
switch v := x.(type) {
case int, int64:
default:
}`)
	if _, ok := s[0].(*ast.AssignStmt).Rhs[0].(*ast.CallExpr).Fun.(*ast.ChanType); !ok {
		t.Fatal("chan[int](10)")
	}
	if len(s[2].(*ast.SelectStmt).Body) != 3 {
		t.Fatal("select clauses")
	}
	if len(s[3].(*ast.SwitchStmt).Body) != 2 {
		t.Fatal("switch clauses")
	}
}

func TestPrecedence(t *testing.T) {
	x, _ := ParseExpr("a + b * c == d && !e")
	b := x.(*ast.BinaryExpr)
	if b.Op != token.LAND {
		t.Fatalf("top op %v", b.Op)
	}
	if b.X.(*ast.BinaryExpr).Op != token.EQL {
		t.Fatal("== level")
	}
}

func TestSyntaxErrors(t *testing.T) {
	cases := []string{
		"func f() {",
		"package p\nfunc f() { x := }",
		"package p\nfunc f() { go 1 }",
		"package p\nx := 1",
		"package p\nfunc f() { a.b := 1 }",
	}
	for _, src := range cases {
		if _, err := ParseFile("t", []byte(src)); err == nil {
			t.Errorf("%q: expected error", src)
		} else if !strings.Contains(err.Error(), ":") {
			t.Errorf("error without position: %v", err)
		}
	}
}

func typeName(x any) string {
	switch x.(type) {
	case *ast.ForStmt:
		return "*ast.ForStmt"
	case *ast.ForInStmt:
		return "*ast.ForInStmt"
	}
	return "?"
}
