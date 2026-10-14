// Package build drives the full pipeline: .gocpy -> AST -> types -> C -> native binary.
package build

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rusfort/gocpy/internal/mod"

	"github.com/rusfort/gocpy/internal/check"
	cgen "github.com/rusfort/gocpy/internal/codegen/c"
	"github.com/rusfort/gocpy/internal/types"
	rt "github.com/rusfort/gocpy/runtime"
)

// ModuleConfig is gocpy.yml.
type ModuleConfig = mod.Config

// FileConfig is <file>.yml next to <file>.gocpy.
type FileConfig struct {
	File  string   `yaml:"file"`
	Opt   *int     `yaml:"opt"`
	Flags []string `yaml:"flags"`
	NoGC  bool     `yaml:"nogc"`
}

// Options control a build.
type Options struct {
	Out     string    // output binary (default: directory name)
	EmitC   string    // keep generated C in this directory
	Verbose bool      // print compiler commands
	Log     io.Writer // diagnostics (warnings, commands)

	// Language server mode: keep going after errors (partial results) and
	// read unsaved editor buffers from Overlay (absolute path -> content).
	Tolerant bool
	Overlay  map[string][]byte
	// NoGoBridge skips building the Go bridge (Go packages are left unresolved).
	NoGoBridge bool
}

func (o *Options) readFile(p string) ([]byte, error) {
	if o != nil && o.Overlay != nil {
		if abs, err := filepath.Abs(p); err == nil {
			if src, ok := o.Overlay[abs]; ok {
				return src, nil
			}
		}
	}
	return os.ReadFile(p)
}

// Result of a successful build.
type Result struct {
	Binary   string
	Warnings []string
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format, args...)
	}
}

// findModule walks up from dir looking for gocpy.yml.
func findModule(dir string) (root string, cfg *ModuleConfig, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	for d := abs; ; d = filepath.Dir(d) {
		p := filepath.Join(d, "gocpy.yml")
		if data, err := os.ReadFile(p); err == nil {
			cfg, err = mod.ParseConfig(p, data)
			if err != nil {
				return "", nil, err
			}
			return d, cfg, nil
		}
		if filepath.Dir(d) == d {
			return abs, &ModuleConfig{}, nil
		}
	}
}

// Sources lists .gocpy files of a package directory, or returns [path] for a file.
func Sources(target string) (dir string, files []string, err error) {
	st, err := os.Stat(target)
	if err != nil {
		return "", nil, err
	}
	if !st.IsDir() {
		if !strings.HasSuffix(target, ".gocpy") {
			return "", nil, fmt.Errorf("%s: not a .gocpy file", target)
		}
		return filepath.Dir(target), []string{target}, nil
	}
	ents, err := os.ReadDir(target)
	if err != nil {
		return "", nil, err
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".gocpy") {
			files = append(files, filepath.Join(target, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return "", nil, fmt.Errorf("no .gocpy files in %s", target)
	}
	return target, files, nil
}

// Frontend loads the target package with its imports, merges them into one
// program and type-checks it.
func Frontend(target string, opts *Options) (*Program, *types.Package, *types.Info, []string, error) {
	prog, err := Load(target, opts)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	types.Demangle = prog.Demangle
	files := prog.Files
	var errs, warnings []string
	for _, d := range check.Package(files) {
		if d.Severity == check.Warning {
			warnings = append(warnings, prog.Demangle(d.String()))
		} else {
			errs = append(errs, prog.Demangle(d.String()))
		}
	}
	if len(errs) > 0 {
		return prog, nil, nil, warnings, errors.New(strings.Join(errs, "\n"))
	}
	pkg, info, err := types.Check(files)
	if err != nil {
		return prog, nil, nil, warnings, errors.New(prog.Demangle(err.Error()))
	}
	if errs := prog.checkCrossAccess(info); len(errs) > 0 {
		return prog, nil, nil, warnings, errors.New(strings.Join(errs, "\n"))
	}
	return prog, pkg, info, warnings, nil
}

// Build compiles the package at target (directory or single file).
func Build(target string, opts Options) (*Result, error) {
	dir, paths, err := Sources(target)
	if err != nil {
		return nil, err
	}
	prog, pkg, info, warnings, err := Frontend(target, &opts)
	for _, w := range warnings {
		opts.logf("%s\n", w)
	}
	if err != nil {
		return nil, err
	}
	root, mod := prog.Root, prog.Config
	if pkg.Name != "main" {
		return nil, fmt.Errorf("%s: package %s is not a command (package main required for build)", target, pkg.Name)
	}
	if len(mod.Native.Go) > 0 {
		return nil, fmt.Errorf("gocpy.yml: native.go is not used; import the Go files of the package directory instead: import \"./file.go\"")
	}
	for _, t := range mod.Build.Targets {
		if t != runtime.GOOS+"/"+runtime.GOARCH {
			opts.logf("warning: target %s ignored: cross-compilation is not supported yet, building for %s/%s\n",
				t, runtime.GOOS, runtime.GOARCH)
		}
	}

	out, err := cgen.Generate(pkg, info)
	if err != nil {
		return nil, err
	}

	work := opts.EmitC
	if work == "" {
		work, err = os.MkdirTemp("", "gocpy-build-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
	} else if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	for name, content := range out.Files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	err = fs.WalkDir(rt.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := rt.Files.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(work, filepath.Base(p)), data, 0o644)
	})
	if err != nil {
		return nil, err
	}

	cc := mod.Build.CC
	if cc == "" {
		cc = CCompiler()
	}
	opt := 2
	if mod.Build.Opt != nil {
		opt = *mod.Build.Opt
	}
	gc := mod.Build.GC
	if gc == "" {
		gc = "boehm"
	}
	if gc != "boehm" && gc != "none" {
		return nil, fmt.Errorf("gocpy.yml: build.gc must be boehm or none, got %q", gc)
	}
	base := []string{"-std=gnu11", "-pthread", "-fwrapv", "-fno-strict-aliasing", "-Wall",
		"-Wno-unused-variable", "-Wno-unused-label", "-Wno-unused-but-set-variable",
		"-Wno-missing-braces", "-Wno-unused-function", "-Wno-unused-value", "-Wno-parentheses",
		"-I", work}
	gcC, gcL := gcFlags()
	if gc == "boehm" {
		base = append(base, "-DGOCPY_GC_BOEHM")
		base = append(base, gcC...)
	}
	// native code of the main module and of every Gocpy module the program uses
	type native struct {
		root string
		cfg  *ModuleConfig
	}
	natives := []native{{root, mod}}
	seenRoot := map[string]bool{root: true}
	if prog.Graph != nil {
		for _, u := range prog.Units {
			if u.isGo || seenRoot[u.modRoot] {
				continue
			}
			seenRoot[u.modRoot] = true
			for _, m := range prog.Graph.Gocpy {
				if m.Dir == u.modRoot {
					natives = append(natives, native{m.Dir, m.Config})
				}
			}
		}
	}
	for _, n := range natives {
		for _, inc := range n.cfg.Native.Include {
			base = append(base, "-I", filepath.Join(n.root, inc))
		}
	}
	base = append(base, mod.Build.Flags...)

	type cunit struct {
		src     string
		flags   []string
		runtime bool // the embedded C runtime: the object file is cached
	}
	units := []cunit{{src: filepath.Join(work, "gocpy_rt.c"), flags: []string{fmt.Sprintf("-O%d", opt)}, runtime: true},
		{src: filepath.Join(work, "gocpy_lib.c"), flags: []string{fmt.Sprintf("-O%d", opt)}, runtime: true}}
	for _, u := range out.Units {
		fl := []string{fmt.Sprintf("-O%d", opt)}
		if u.Source != "" {
			fc, err := loadFileConfig(u.Source)
			if err != nil {
				return nil, err
			}
			if fc != nil {
				if fc.Opt != nil {
					fl = []string{fmt.Sprintf("-O%d", *fc.Opt)}
				}
				fl = append(fl, fc.Flags...)
				if fc.NoGC {
					opts.logf("warning: %s: nogc is not enforced yet (stage 5)\n", u.Source)
				}
			}
		}
		units = append(units, cunit{src: filepath.Join(work, u.CFile), flags: fl})
	}
	for _, n := range natives {
		for _, pattern := range n.cfg.Native.C {
			matches, err := filepath.Glob(filepath.Join(n.root, pattern))
			if err != nil {
				return nil, err
			}
			for _, m := range matches {
				units = append(units, cunit{src: m, flags: []string{fmt.Sprintf("-O%d", opt)}})
			}
		}
	}
	if prog.Bridge != nil {
		units = append(units, cunit{src: prog.Bridge.Shim, flags: []string{fmt.Sprintf("-O%d", opt)}})
	}

	var objs []string
	for i, u := range units {
		obj := filepath.Join(work, fmt.Sprintf("u%d_%s.o", i, strings.TrimSuffix(filepath.Base(u.src), ".c")))
		args := append(append(append([]string{}, base...), u.flags...), "-c", u.src, "-o", obj)
		if u.runtime {
			if cached, ok := runtimeObject(cc, args, work, u.src, obj, opts); ok {
				objs = append(objs, cached)
				continue
			}
		}
		if err := run(cc, args, opts); err != nil {
			return nil, err
		}
		if u.runtime {
			storeRuntimeObject(cc, args, work, u.src, obj)
		}
		objs = append(objs, obj)
	}

	bin := opts.Out
	if bin == "" {
		abs, _ := filepath.Abs(dir)
		bin = filepath.Base(abs)
		if len(paths) == 1 && strings.HasSuffix(target, ".gocpy") {
			bin = strings.TrimSuffix(filepath.Base(target), ".gocpy")
		}
	}
	link := append([]string{}, objs...)
	link = append(link, "-o", bin)
	if prog.Bridge != nil && prog.Bridge.Archive != "" {
		link = append(link, prog.Bridge.Archive)
		link = append(link, prog.Bridge.LinkFlags...)
	}
	if gc == "boehm" {
		link = append(link, gcL...)
	}
	for _, n := range natives {
		for _, l := range n.cfg.Native.Link {
			link = append(link, "-l"+l)
		}
	}
	link = append(link, "-lm", "-pthread")
	link = append(link, mod.Build.Flags...) // e.g. -fsanitize=... must reach the linker too
	if err := run(cc, link, opts); err != nil {
		return nil, err
	}
	return &Result{Binary: bin, Warnings: warnings}, nil
}

func loadFileConfig(src string) (*FileConfig, error) {
	p := strings.TrimSuffix(src, ".gocpy") + ".yml"
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fc := &FileConfig{}
	if err := yaml.Unmarshal(data, fc); err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	if fc.File != "" && fc.File != filepath.Base(src) {
		return nil, fmt.Errorf("%s: file: %s does not match %s", p, fc.File, filepath.Base(src))
	}
	return fc, nil
}

// runtimeObjectPath returns the cache path of the object file of a runtime
// source compiled with the given compiler and arguments.
func runtimeObjectPath(cc string, args []string, work, src string) string {
	dir := os.Getenv("GOCPY_CACHE")
	if dir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(d, "gocpy")
	}
	h := sha256.New()
	ccPath, err := exec.LookPath(cc)
	if err != nil {
		return ""
	}
	fmt.Fprintf(h, "%s\n", ccPath)
	if st, err := os.Stat(ccPath); err == nil {
		fmt.Fprintf(h, "%d %d\n", st.Size(), st.ModTime().UnixNano())
	}
	for _, a := range args {
		fmt.Fprintf(h, "%s\x00", strings.ReplaceAll(a, work, "$WORK"))
	}
	for _, f := range []string{filepath.Join(work, "gocpy_rt.h"), src} {
		data, err := os.ReadFile(f)
		if err != nil {
			return ""
		}
		h.Write(data)
	}
	return filepath.Join(dir, "rt", fmt.Sprintf("%x.o", h.Sum(nil)[:16]))
}

func runtimeObject(cc string, args []string, work, src, obj string, opts Options) (string, bool) {
	p := runtimeObjectPath(cc, args, work, src)
	if p == "" {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	if opts.Verbose {
		opts.logf("using cached %s\n", p)
	}
	return p, true
}

func storeRuntimeObject(cc string, args []string, work, src, obj string) {
	p := runtimeObjectPath(cc, args, work, src)
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	data, err := os.ReadFile(obj)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "obj-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
		os.Remove(tmp.Name())
	}
}

func run(cc string, args []string, opts Options) error {
	if opts.Verbose {
		opts.logf("%s %s\n", cc, strings.Join(args, " "))
	}
	cmd := exec.Command(cc, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("C compiler failed (%v):\n%s", err, ccErrors(stderr.String()))
	}
	if stderr.Len() > 0 && opts.Verbose {
		opts.logf("%s", stderr.String())
	}
	return nil
}

var ccDiag = regexp.MustCompile(`^\S.*:\d+:(\d+:)? (warning|error|fatal error|note):`)

// ccErrors drops warnings (with their notes and source snippets) from C
// compiler output, so that the error is not hidden behind them. The full
// output is returned when nothing else is left.
func ccErrors(out string) string {
	var keep, included []string
	inWarning := false
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "In file included from ") || strings.HasPrefix(line, "                 from ") {
			included = append(included, line) // belongs to the next diagnostic
			continue
		}
		pending := included
		included = nil
		if m := ccDiag.FindStringSubmatch(line); m != nil {
			switch m[2] {
			case "warning":
				inWarning = true
				continue
			case "note":
				if inWarning {
					continue
				}
			default:
				inWarning = false
			}
			keep = append(keep, pending...)
		} else if strings.HasSuffix(line, " generated.") && strings.Contains(line, "warning") {
			continue
		} else if inWarning && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			continue // source snippet of a warning
		} else {
			inWarning = false
		}
		keep = append(keep, line)
	}
	if len(keep) == 0 {
		return out
	}
	return strings.Join(keep, "\n") + "\n"
}
