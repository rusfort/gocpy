package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gcFlags returns compile and link flags for the Boehm GC (libgc / bdw-gc).
// Order: GOCPY_GC_CFLAGS/GOCPY_GC_LIBS, pkg-config bdw-gc, Homebrew/MacPorts prefixes.
func gcFlags() (cflags, libs []string) {
	if c, l := os.Getenv("GOCPY_GC_CFLAGS"), os.Getenv("GOCPY_GC_LIBS"); c != "" || l != "" {
		return strings.Fields(c), strings.Fields(l)
	}
	if out, err := exec.Command("pkg-config", "--cflags", "bdw-gc").Output(); err == nil {
		if lo, err := exec.Command("pkg-config", "--libs", "bdw-gc").Output(); err == nil {
			return strings.Fields(string(out)), strings.Fields(string(lo))
		}
	}
	for _, p := range gcPrefixes() {
		if _, err := os.Stat(filepath.Join(p, "include", "gc.h")); err == nil {
			return []string{"-I" + filepath.Join(p, "include")}, []string{"-L" + filepath.Join(p, "lib"), "-lgc"}
		}
	}
	return nil, []string{"-lgc"}
}

func gcPrefixes() []string {
	ps := []string{"/opt/homebrew/opt/bdw-gc", "/usr/local/opt/bdw-gc", "/opt/homebrew", "/usr/local", "/opt/local"}
	if hp := os.Getenv("HOMEBREW_PREFIX"); hp != "" {
		ps = append([]string{filepath.Join(hp, "opt", "bdw-gc"), hp}, ps...)
	}
	return ps
}

// CCompiler returns the C compiler used when gocpy.yml does not set build.cc.
func CCompiler() string {
	if cc := os.Getenv("CC"); cc != "" {
		return cc
	}
	return "cc"
}

// Doctor checks the C toolchain and libgc by building a tiny program.
func Doctor(logf func(string, ...any)) error {
	cc := CCompiler()
	path, err := exec.LookPath(cc)
	if err != nil {
		return fmt.Errorf("C compiler %q not found: install gcc or clang (macOS: xcode-select --install) or set CC", cc)
	}
	ver, _ := exec.Command(path, "--version").Output()
	logf("C compiler: %s (%s)\n", path, strings.SplitN(string(ver), "\n", 2)[0])
	dir, err := os.MkdirTemp("", "gocpy-doctor")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "t.c")
	prog := "#define GC_THREADS\n#include <gc.h>\nint main(void){ GC_INIT(); void *p = GC_MALLOC(16); return p == 0; }\n"
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		return err
	}
	cf, lf := gcFlags()
	args := append(append(append([]string{}, cf...), src, "-o", filepath.Join(dir, "t")), append(lf, "-pthread")...)
	if out, err := exec.Command(path, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("libgc (Boehm GC) not usable: %v\n%s\ninstall it: macOS `brew install bdw-gc`, Debian/Ubuntu `apt install libgc-dev`, Fedora `dnf install gc-devel`, Alpine `apk add gc-dev`;\nor set GOCPY_GC_CFLAGS/GOCPY_GC_LIBS, or use `gc: none` in gocpy.yml", err, out)
	}
	if out, err := exec.Command(filepath.Join(dir, "t")).CombinedOutput(); err != nil {
		return fmt.Errorf("libgc test program failed: %v\n%s", err, out)
	}
	logf("libgc: ok (%s)\n", strings.Join(append(cf, lf...), " "))
	if g, err := exec.LookPath("go"); err == nil {
		v, _ := exec.Command(g, "env", "GOVERSION").Output()
		logf("Go toolchain: %s (%s) — require.go is available\n", g, strings.TrimSpace(string(v)))
	} else {
		logf("Go toolchain: not found (optional; needed only for require.go in gocpy.yml)\n")
	}
	if _, err := exec.LookPath("git"); err == nil {
		logf("git: ok (downloads require.gocpy modules)\n")
	} else {
		logf("git: not found (needed to download require.gocpy modules)\n")
	}
	return nil
}
