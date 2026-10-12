package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGoFiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not found")
	}
	gc := gcMode(t)
	t.Setenv("GOCPY_CACHE", t.TempDir())
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"gorepo/go.mod":  "module github.com/bob/go-repo\n\ngo 1.21\n",
		"gorepo/repo.go": "package gorepo\n\nfunc Twice(s string) string { return s + s }\n",
		"app/gocpy.yml": "module: example.com/app\nbuild:\n  gc: " + gc + "\nrequire:\n  go:\n    - github.com/bob/go-repo@v0.1.0\n" +
			"replace:\n  github.com/bob/go-repo: ../gorepo\n",
		"app/mathx.go": `package mathx

import (
	"errors"
	"strings"

	"github.com/bob/go-repo"
)

const Scale = 10

func Add(a, b int) int { return a + b }

func Shout(s string) string { return strings.ToUpper(gorepo.Twice(s)) + "!" }

func Div(a, b float64) (float64, error) {
	if b == 0 {
		return 0, errors.New("division by zero")
	}
	return half(a) * 2 / b, nil
}
`,
		"app/helper.go":      "package mathx\n\nfunc half(x float64) float64 { return x / 2 }\n",
		"app/helper_test.go": "package mathx\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n",
		"app/main.gocpy": `package main

import (
    "std/fmt"
    m "./mathx.go"
)

func main() {
    fmt.Println(m.Add(2, 3) * m.Scale, m.Shout("ab"))
    q, err := m.Div(1, 4)
    fmt.Println(q, err)
    _, err2 := m.Div(1, 0)
    fmt.Println(err2)
}
`,
		"app/util/util.gocpy": "package util\n\nimport \"./conv.go\"\n\nfunc Hex(n int) string {\n    return conv.Hex(n)\n}\n",
		"app/util/conv.go":    "package conv\n\nimport \"strconv\"\n\nfunc Hex(n int) string { return strconv.FormatInt(int64(n), 16) }\n",
	})
	app := filepath.Join(root, "app")
	out, stderr, code, err := runProgram(t, app)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if want := "50 ABAB!\n0.25 <nil>\ndivision by zero\n"; out != want || code != 0 {
		t.Fatalf("got %q (code %d), want %q", out, code, want)
	}

	// editing the Go file rebuilds the bridge
	writeTree(t, root, map[string]string{"app/helper.go": "package mathx\n\nfunc half(x float64) float64 { return x }\n"})
	if out, _, _, err = runProgram(t, app); err != nil || !strings.Contains(out, "0.5 <nil>") {
		t.Fatalf("after edit: %q %v", out, err)
	}

	// a dependency package with its own Go file
	writeTree(t, root, map[string]string{"app/main.gocpy": `package main

import (
    "std/fmt"
    "./mathx.go"
    "example.com/app/util"
)

func main() {
    fmt.Println(util.Hex(255), mathx.Add(1, 1))
}
`})
	if out, stderr, _, err = runProgram(t, app); err != nil || out != "ff 2\n" {
		t.Fatalf("util: %q %v\n%s", out, err, stderr)
	}

	errCases := []struct{ imports, want string }{
		{`"./nope.go"`, `no Go file nope.go`},
		{`"./helper_test.go"`, `Go test files cannot be imported`},
		{`"../gorepo/repo.go"`, `only from the same directory`},
		{`"./mathx.go"` + "\n    " + `"./helper.go"`, `are files of the same Go package mathx`},
	}
	for _, c := range errCases {
		writeTree(t, root, map[string]string{"app/main.gocpy": "package main\n\nimport (\n    " + c.imports +
			"\n)\n\nfunc main() {\n    mathx.Add(1, 2)\n}\n"})
		_, _, _, err := runProgram(t, app)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("imports %s: got %v, want %q", c.imports, err, c.want)
		}
	}

	// package main and imports that are not in require.go
	writeTree(t, root, map[string]string{
		"app2/gocpy.yml":  "module: example.com/app2\n",
		"app2/main.gocpy": "package main\n\nimport \"./tool.go\"\n\nfunc main() {\n    tool.Run()\n}\n",
		"app2/tool.go":    "package main\n\nfunc Run() {}\n",
		"app3/gocpy.yml":  "module: example.com/app3\n",
		"app3/main.gocpy": "package main\n\nimport \"./ids.go\"\n\nfunc main() {\n    ids.New()\n}\n",
		"app3/ids.go":     "package ids\n\nimport \"github.com/google/uuid\"\n\nfunc New() string { return uuid.NewString() }\n",
	})
	for dir, want := range map[string]string{"app2": "has package main", "app3": "not in require.go"} {
		_, _, _, err := runProgram(t, filepath.Join(root, dir))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", dir, err, want)
		}
	}
	os.RemoveAll(filepath.Join(root, "app2"))
}
