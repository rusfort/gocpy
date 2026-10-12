package build

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates files (path -> content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runProgram(t *testing.T, dir string) (string, string, int, error) {
	t.Helper()
	var log bytes.Buffer
	res, err := Build(dir, Options{Out: filepath.Join(dir, "prog"), Log: &log})
	if err != nil {
		return "", log.String(), -1, err
	}
	cmd := exec.Command(res.Binary)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return o.String(), e.String(), code, nil
}

const libGeo = `package geo

import "std/fmt"

const Origin = "origin"

type Point struct {
    X int
    Y int
    tag string
}

type ParseError struct {
    Msg string
}

func New(x int, y int) Point {
    return Point{X: x, Y: y, tag: label(x, y)}
}

func label(x int, y int) string {
    return f"({x},{y})"
}

func (p Point) Tag() string {
    return p.tag
}

func (p *Point) Move(dx int, dy int) {
    p.X += dx
    p.Y += dy
    p.tag = label(p.X, p.Y)
}

func Max[T int | float64](a T, b T) T {
    if a > b {
        return a
    }
    return b
}

@throws(ParseError)
func MustPositive(v int) int {
    if v < 0 {
        throw ParseError{Msg: fmt.Sprint("negative: ", v)}
    }
    return v
}

var Count = 0

func Inc() int {
    Count += 1
    return Count
}

func Codes() map[string]int {
    return map[string]int{Origin: 1}
}
`

// modProject creates a main module using a local package and a Gocpy
// dependency replaced by a local directory.
func modProject(t *testing.T, gc string, mainSrc string) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"lib/gocpy.yml":           "module: github.com/john/gocpy-repo\n",
		"lib/geo/geo.gocpy":       libGeo,
		"lib/shapes/shapes.gocpy": "package shapes\n\nimport \"github.com/john/gocpy-repo/geo\"\n\ntype Rect struct {\n    Min geo.Point\n    Max geo.Point\n}\n\nfunc Square(n int) Rect {\n    return Rect{Min: geo.New(0, 0), Max: geo.New(n, n)}\n}\n\nfunc (r Rect) Area() int {\n    return (r.Max.X - r.Min.X) * (r.Max.Y - r.Min.Y)\n}\n",
		"lib/dup/geo/geo.gocpy":   "package geo\n\nfunc Name() string {\n    return \"dup\"\n}\n",
		"app/gocpy.yml": "module: github.com/rusfort/gocpy-first-app\nversion: 0.1.0\nbuild:\n  opt: 2\n  gc: " + gc +
			"\nrequire:\n  gocpy:\n    - github.com/john/gocpy-repo@v0.1.0\nreplace:\n  github.com/john/gocpy-repo: ../lib\n",
		"app/util/util.gocpy": "package util\n\nimport \"github.com/john/gocpy-repo/geo\"\n\nfunc Twice(p geo.Point) geo.Point {\n    return geo.New(p.X * 2, p.Y * 2)\n}\n",
		"app/main.gocpy":      mainSrc,
	})
	return filepath.Join(root, "app")
}

func TestModulesLocalAndReplace(t *testing.T) {
	gc := gcMode(t)
	app := modProject(t, gc, `package main

import (
    "std/fmt"
    "github.com/john/gocpy-repo/geo"
    "github.com/john/gocpy-repo/shapes"
    g2 "github.com/john/gocpy-repo/dup/geo"
    "github.com/rusfort/gocpy-first-app/util"
)

func main() {
    p := geo.New(1, 2)
    p.Move(2, 2)
    fmt.Println(p.X, p.Y, p.Tag(), geo.Origin)
    fmt.Println(util.Twice(p).Tag())
    fmt.Println(geo.Max(3, 9), geo.Max(2.5, 1.5))
    fmt.Println(shapes.Square(4).Area())
    fmt.Println(geo.Inc(), geo.Inc(), geo.Count)
    fmt.Println(geo.Codes()["origin"], g2.Name())
    try {
        geo.MustPositive(-5)
    } catch e: geo.ParseError {
        fmt.Println("caught:", e.Msg)
    } catch {
        fmt.Println("other")
    }
    var x any = p
    fmt.Println(fmt.Sprint(x))
}
`)
	out, stderr, code, err := runProgram(t, app)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	want := "3 4 (3,4) origin\n(6,8)\n9 2.5\n16\n1 2 2\n1 dup\ncaught: negative: -5\n"
	if code != 0 || !strings.HasPrefix(out, want) {
		t.Fatalf("exit %d\n got:\n%s\nwant prefix:\n%s\nstderr: %s", code, out, want, stderr)
	}
}

func TestModulesErrors(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"unexported": {"package main\nimport \"github.com/john/gocpy-repo/geo\"\nfunc main() { _ = geo.label(1, 2) }\n",
			"cannot refer to unexported name geo.label"},
		"field": {"package main\nimport (\n\"std/fmt\"\n\"github.com/john/gocpy-repo/geo\"\n)\nfunc main() { p := geo.New(1, 2); fmt.Println(p.tag) }\n",
			"cannot refer to unexported field or method tag of package geo"},
		"undefined": {"package main\nimport \"github.com/john/gocpy-repo/geo\"\nfunc main() { geo.Nope() }\n",
			"undefined: geo.Nope"},
		"unused": {"package main\nimport \"github.com/john/gocpy-repo/geo\"\nfunc main() {}\n",
			`"github.com/john/gocpy-repo/geo" imported and not used`},
		"conflict": {"package main\nimport \"github.com/john/gocpy-repo/geo\"\nfunc main() { geo := geo.New(1, 2); _ = geo }\n",
			`geo conflicts with imported package`},
		"notrequired": {"package main\nimport \"github.com/alice/x\"\nfunc main() { x.F() }\n",
			`package "github.com/alice/x" is not in std, the main module`},
		"twice": {"package main\nimport (\n\"github.com/john/gocpy-repo/geo\"\n\"github.com/john/gocpy-repo/dup/geo\"\n)\nfunc main() { geo.Inc() }\n",
			"geo imported twice"},
		"typeinfo": {"package main\nimport \"github.com/john/gocpy-repo/geo\"\nfunc main() { var x int = geo.New(1, 2); _ = x }\n",
			"(type geo.Point) as int"},
		"relative": {"package main\nimport \"./util\"\nfunc main() {}\n", "relative import"},
		"main":     {"package main\nimport \"github.com/rusfort/gocpy-first-app/cmdx\"\nfunc main() { cmdx.F() }\n", "is a program (package main)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			app := modProject(t, "none", c.src)
			writeTree(t, app, map[string]string{"cmdx/m.gocpy": "package main\nfunc F() {}\nfunc main() {}\n"})
			_, _, _, err := runProgram(t, app)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestModulesImportCycle(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"gocpy.yml":  "module: example.com/cyc\nbuild:\n  gc: none\n",
		"main.gocpy": "package main\nimport \"example.com/cyc/a\"\nfunc main() { a.A() }\n",
		"a/a.gocpy":  "package a\nimport \"example.com/cyc/b\"\nfunc A() { b.B() }\n",
		"b/b.gocpy":  "package b\nimport \"example.com/cyc/a\"\nfunc B() { a.A() }\n",
	})
	_, _, _, err := runProgram(t, root)
	if err == nil || !strings.Contains(err.Error(), "import cycle") {
		t.Fatalf("want import cycle, got %v", err)
	}
}

func TestGoBridge(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not found")
	}
	gc := gcMode(t)
	t.Setenv("GOCPY_CACHE", t.TempDir())
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"gorepo/go.mod": "module github.com/bob/go-repo\n\ngo 1.21\n",
		"gorepo/repo.go": `package gorepo

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

const Version = "0.2.0"
const Answer = 42
const Timeout time.Duration = 3 * time.Second

func Add(a, b int) int                 { return a + b }
func Greet(name string) string         { return "Hello, " + name + " from Go" }
func Upper(b []byte) []byte            { return []byte(strings.ToUpper(string(b))) }
func Split2(s string) (string, string) { a, b, _ := strings.Cut(s, ":"); return a, b }
func Half(x float64) float64           { return x / 2 }
func Wait(d time.Duration) bool        { time.Sleep(d); return true }
func Boom()                            { panic("boom from Go") }
func Join(xs []string) string          { return strings.Join(xs, ",") }
func Parse(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty input")
	}
	return strconv.Atoi(s)
}

type Thing struct{}
`,
		"gorepo/textx/textx.go": "package textx\n\nimport \"strings\"\n\nfunc Repeat(s string, n int) string { return strings.Repeat(s, n) }\n",
		"app/gocpy.yml":         "module: example.com/app\nbuild:\n  gc: " + gc + "\nrequire:\n  go:\n    - github.com/bob/go-repo@v0.2.0\nreplace:\n  github.com/bob/go-repo: ../gorepo\n",
		"app/main.gocpy": `package main

import (
    "std/fmt"
    "std/sync"
    gr "github.com/bob/go-repo"
    "github.com/bob/go-repo/textx"
)

func main() {
    fmt.Println(gr.Add(2, 3), gr.Greet("Gocpy"), gr.Half(5.0), gr.Version, gr.Answer, gr.Timeout)
    n, err := gr.Parse("123")
    fmt.Println(n, err)
    _, err2 := gr.Parse("")
    fmt.Println(err2)
    a, b := gr.Split2("left:right")
    fmt.Println(a, b, string(gr.Upper([]byte("bytes"))))
    fmt.Println(textx.Repeat("ab", 3), gr.Wait(1000000))
    var wg sync.WaitGroup
    var mu sync.Mutex
    total := 0
    for i := 0; i < 6; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            k := 0
            for j := 0; j < 3000; j++ {
                xs := []string{}
                xs = append(xs, gr.Greet(f"g{j}"))
                k += len(xs[0])
            }
            mu.Lock()
            total += k
            mu.Unlock()
        }()
    }
    wg.Wait()
    fmt.Println("total", total)
    gr.Boom()
}
`,
	})
	app := filepath.Join(root, "app")
	out, stderr, code, err := runProgram(t, app)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	want := "5 Hello, Gocpy from Go 2.5 0.2.0 42 3000000000\n123 <nil>\nempty input\nleft right BYTES\nababab true\ntotal 353340\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if code != 2 || !strings.Contains(stderr, "panic in Go code: boom from Go") {
		t.Errorf("Go panic: exit %d, stderr %q", code, stderr)
	}
	// unsupported API is reported with a reason
	writeTree(t, app, map[string]string{"main.gocpy": "package main\nimport gr \"github.com/bob/go-repo\"\nfunc main() { _ = gr.Join(nil); var t gr.Thing; _ = t }\n"})
	_, _, _, err = runProgram(t, app)
	if err == nil || !strings.Contains(err.Error(), "gr.Join: parameter 1 has type []string") ||
		!strings.Contains(err.Error(), "gr.Thing: Go types are not supported") {
		t.Errorf("unsupported: %v", err)
	}
}
