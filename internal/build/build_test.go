package build

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gcMode returns "boehm" if libgc is available, else "none".
// It uses the same libgc discovery as Build (pkg-config, Homebrew prefixes).
// With GOCPY_REQUIRE_GC=1 (CI) a missing libgc fails the test instead.
func gcMode(t *testing.T) string {
	cc := CCompiler()
	if _, err := exec.LookPath(cc); err != nil {
		t.Skip("no C compiler")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "x.c")
	os.WriteFile(src, []byte("#include <gc.h>\nint main(void){GC_INIT();return 0;}\n"), 0o644)
	cf, lf := gcFlags()
	args := append(append(append([]string{}, cf...), src, "-o", filepath.Join(dir, "x")), lf...)
	if out, err := exec.Command(cc, args...).CombinedOutput(); err == nil {
		return "boehm"
	} else if os.Getenv("GOCPY_REQUIRE_GC") == "1" {
		t.Fatalf("libgc not found (GOCPY_REQUIRE_GC=1): %v\n%s", err, out)
	}
	t.Log("libgc not found: testing with gc: none")
	return "none"
}

// gcOnly lists tests that need the Boehm GC (checks done by the GC runtime).
var gcOnly = map[string]bool{"panic_freegc": true}

func buildAndRun(t *testing.T, gocpyFile, gc string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	src, err := os.ReadFile(gocpyFile)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, filepath.Base(gocpyFile)), src, 0o644)
	os.WriteFile(filepath.Join(dir, "gocpy.yml"), []byte("module: test\nbuild:\n  gc: "+gc+"\n"), 0o644)
	var log bytes.Buffer
	res, err := Build(dir, Options{Out: filepath.Join(dir, "prog"), Log: &log})
	if err != nil {
		t.Fatalf("build %s: %v\n%s", gocpyFile, err, log.String())
	}
	cmd := exec.Command(res.Binary)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func TestGolden(t *testing.T) {
	gc := gcMode(t)
	files, _ := filepath.Glob("testdata/*.gocpy")
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".gocpy")
		t.Run(name, func(t *testing.T) {
			if gc != "boehm" && gcOnly[name] {
				t.Skip("requires gc: boehm")
			}
			want, err := os.ReadFile(strings.TrimSuffix(f, ".gocpy") + ".out")
			if err != nil {
				t.Fatal(err)
			}
			out, errOut, code := buildAndRun(t, f, gc)
			if strings.HasPrefix(name, "panic_") {
				if code != 2 || !(strings.Contains(errOut, "runtime error:") || strings.Contains(errOut, "fatal error:")) {
					t.Errorf("want runtime error exit 2, got %d stderr=%q", code, errOut)
				}
			} else if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if out != string(want) {
				t.Errorf("output mismatch\n--- got\n%s\n--- want\n%s", out, want)
			}
		})
	}
}

func TestBothGCModes(t *testing.T) {
	if gcMode(t) != "boehm" {
		t.Skip("libgc not installed")
	}
	want, _ := os.ReadFile("testdata/slices.out")
	for _, gc := range []string{"boehm", "none"} {
		out, _, code := buildAndRun(t, "testdata/slices.gocpy", gc)
		if code != 0 || out != string(want) {
			t.Errorf("gc=%s: exit %d, output mismatch", gc, code)
		}
	}
}

func TestPerFileYml(t *testing.T) {
	gc := gcMode(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gocpy.yml"), []byte("module: t\nbuild:\n  gc: "+gc+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.gocpy"), []byte("package main\nimport \"std/fmt\"\nfunc main() { fmt.Println(twice(21)) }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "math.gocpy"), []byte("package main\n@inline\nfunc twice(x int) int { return x * 2 }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "math.yml"), []byte("file: math.gocpy\nopt: 3\nflags: [-DGOCPY_TEST_FLAG=1]\n"), 0o644)
	var log bytes.Buffer
	res, err := Build(dir, Options{Out: filepath.Join(dir, "p"), Verbose: true, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "-O3 -DGOCPY_TEST_FLAG=1 -c "+filepath.Join(filepath.Dir(log.String()[:0]+""), "")) &&
		!strings.Contains(log.String(), "-O3 -DGOCPY_TEST_FLAG=1") {
		t.Errorf("per-file flags not applied:\n%s", log.String())
	}
	out, _ := exec.Command(res.Binary).Output()
	if string(out) != "42\n" {
		t.Errorf("got %q", out)
	}
}

func TestExternC(t *testing.T) {
	gc := gcMode(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "native"), 0o755)
	os.WriteFile(filepath.Join(dir, "gocpy.yml"), []byte("module: t\nbuild:\n  gc: "+gc+"\nnative:\n  c: [\"native/*.c\"]\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "native", "crc.c"), []byte(`#include <stdint.h>
int64_t c_mix(int64_t a, int64_t b) { return a * 31 + b; }
`), 0o644)
	os.WriteFile(filepath.Join(dir, "main.gocpy"), []byte(`package main
import "std/fmt"

@extern("c_mix")
func mix(a, b int) int

func main() { fmt.Println(mix(2, 5)) }
`), 0o644)
	res, err := Build(dir, Options{Out: filepath.Join(dir, "p")})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command(res.Binary).Output()
	if string(out) != "67\n" {
		t.Errorf("got %q", out)
	}
}
