// Package build drives the full pipeline: .gocpy -> AST -> types -> C -> native binary.
package build

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/check"
	cgen "github.com/rusfort/gocpy/internal/codegen/c"
	"github.com/rusfort/gocpy/internal/parser"
	"github.com/rusfort/gocpy/internal/types"
	rt "github.com/rusfort/gocpy/runtime"
)

// ModuleConfig is gocpy.yml.
type ModuleConfig struct {
	Module  string `yaml:"module"`
	Version string `yaml:"version"`
	Gocpy   string `yaml:"gocpy"`
	Build   struct {
		Targets []string `yaml:"targets"`
		CC      string   `yaml:"cc"`
		Opt     *int     `yaml:"opt"`
		GC      string   `yaml:"gc"` // boehm | none
		Flags   []string `yaml:"flags"`
	} `yaml:"build"`
	Native struct {
		C       []string `yaml:"c"`
		Include []string `yaml:"include"`
		Link    []string `yaml:"link"`
		Go      []string `yaml:"go"`
	} `yaml:"native"`
	Require map[string]string `yaml:"require"`
}

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
			cfg = &ModuleConfig{}
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return "", nil, fmt.Errorf("%s: %v", p, err)
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

// Frontend parses and checks a package; returns typed package or diagnostics.
func Frontend(paths []string) (*types.Package, *types.Info, []string, error) {
	var files []*ast.File
	var errs []string
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, nil, err
		}
		f, err := parser.ParseFile(p, src)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		files = append(files, f)
	}
	if len(errs) > 0 {
		return nil, nil, nil, errors.New(strings.Join(errs, "\n"))
	}
	for _, f := range files[1:] {
		if f.Name.Name != files[0].Name.Name {
			errs = append(errs, fmt.Sprintf("%s: package %s, expected %s (one package per directory)",
				f.Package, f.Name.Name, files[0].Name.Name))
		}
	}
	var warnings []string
	for _, d := range check.Package(files) {
		if d.Severity == check.Warning {
			warnings = append(warnings, d.String())
		} else {
			errs = append(errs, d.String())
		}
	}
	if len(errs) > 0 {
		return nil, nil, warnings, errors.New(strings.Join(errs, "\n"))
	}
	pkg, info, err := types.Check(files)
	if err != nil {
		return nil, nil, warnings, err
	}
	return pkg, info, warnings, nil
}

// Build compiles the package at target (directory or single file).
func Build(target string, opts Options) (*Result, error) {
	dir, paths, err := Sources(target)
	if err != nil {
		return nil, err
	}
	root, mod, err := findModule(dir)
	if err != nil {
		return nil, err
	}
	pkg, info, warnings, err := Frontend(paths)
	for _, w := range warnings {
		opts.logf("%s\n", w)
	}
	if err != nil {
		return nil, err
	}
	if pkg.Name != "main" {
		return nil, fmt.Errorf("%s: package %s is not a command (package main required for build)", target, pkg.Name)
	}
	if len(mod.Native.Go) > 0 {
		return nil, fmt.Errorf("gocpy.yml: native.go (linking .go files) is not supported yet (stage 6)")
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
	for _, inc := range mod.Native.Include {
		base = append(base, "-I", filepath.Join(root, inc))
	}
	base = append(base, mod.Build.Flags...)

	type unit struct {
		src   string
		flags []string
	}
	units := []unit{{src: filepath.Join(work, "gocpy_rt.c"), flags: []string{fmt.Sprintf("-O%d", opt)}}}
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
		units = append(units, unit{src: filepath.Join(work, u.CFile), flags: fl})
	}
	for _, pattern := range mod.Native.C {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			units = append(units, unit{src: m, flags: []string{fmt.Sprintf("-O%d", opt)}})
		}
	}

	var objs []string
	for i, u := range units {
		obj := filepath.Join(work, fmt.Sprintf("u%d_%s.o", i, strings.TrimSuffix(filepath.Base(u.src), ".c")))
		args := append(append(append([]string{}, base...), u.flags...), "-c", u.src, "-o", obj)
		if err := run(cc, args, opts); err != nil {
			return nil, err
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
	if gc == "boehm" {
		link = append(link, gcL...)
	}
	for _, l := range mod.Native.Link {
		link = append(link, "-l"+l)
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
		return fmt.Errorf("C compiler failed (%v):\n%s", err, stderr.String())
	}
	if stderr.Len() > 0 && opts.Verbose {
		opts.logf("%s", stderr.String())
	}
	return nil
}
