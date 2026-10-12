package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rusfort/gocpy/internal/mod"
)

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, f, a...) }

// moduleRoot finds gocpy.yml from the current directory upwards.
func moduleRoot() (string, *mod.Config, error) {
	d, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "gocpy.yml")); err == nil {
			cfg, err := mod.LoadConfig(d)
			return d, cfg, err
		}
		if filepath.Dir(d) == d {
			return "", nil, fmt.Errorf("no gocpy.yml found in the current directory or any parent (run gocpy init <module>)")
		}
		d = filepath.Dir(d)
	}
}

// gocpy init <module path>
func cmdInit(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gocpy init <module path>, e.g. gocpy init github.com/you/app")
	}
	path := args[0]
	if err := mod.CheckPath(path); err != nil && strings.Contains(path, "/") {
		return err
	}
	if _, err := os.Stat("gocpy.yml"); err == nil {
		return fmt.Errorf("gocpy.yml already exists")
	}
	yml := fmt.Sprintf("module: %s\nversion: 0.1.0\nbuild:\n  opt: 2\n  gc: boehm\n", path)
	if err := os.WriteFile("gocpy.yml", []byte(yml), 0o644); err != nil {
		return err
	}
	fmt.Println("gocpy: created gocpy.yml for", path)
	if ents, _ := filepath.Glob("*.gocpy"); len(ents) == 0 {
		src := "package main\n\nimport \"std/fmt\"\n\nfunc main() {\n    fmt.Println(\"Hello, Gocpy\")\n}\n"
		if err := os.WriteFile("main.gocpy", []byte(src), 0o644); err != nil {
			return err
		}
		fmt.Println("gocpy: created main.gocpy")
	}
	return nil
}

// gocpy get [-go|-gocpy] path[@version]
func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	asGo := fs.Bool("go", false, "the module is a Go module (require.go)")
	asGocpy := fs.Bool("gocpy", false, "the module is a Gocpy module (require.gocpy)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: gocpy get [-go|-gocpy] <module>[@version|@latest] ...")
	}
	root, cfg, err := moduleRoot()
	if err != nil {
		return err
	}
	for _, arg := range fs.Args() {
		path, ver, _ := strings.Cut(arg, "@")
		if err := mod.CheckPath(path); err != nil {
			return err
		}
		kind := mod.KindGocpy
		known := false
		for _, r := range cfg.Require.All() {
			if r.Path == path {
				kind, known = r.Kind, true
			}
		}
		switch {
		case *asGo:
			kind, known = mod.KindGo, true
		case *asGocpy:
			kind, known = mod.KindGocpy, true
		}
		if ver == "" {
			ver = "latest"
		}
		if !known || kind == mod.KindGocpy {
			v := ver
			if v == "latest" {
				v, err = mod.LatestTag(path)
			}
			if err == nil {
				_, _, err = mod.Download(path, v, logf)
			}
			var ng mod.ErrNotGocpy
			switch {
			case err == nil:
				kind, ver = mod.KindGocpy, v
			case !known && errors.As(err, &ng):
				kind = mod.KindGo
			case !known:
				return fmt.Errorf("%v\n(if %s is a Go module, use: gocpy get -go %s)", err, path, arg)
			default:
				return err
			}
		}
		if kind == mod.KindGo {
			v, err := goResolveVersion(path, ver)
			if err != nil {
				return err
			}
			ver = v
		}
		req := mod.Requirement{Path: path, Version: ver, Kind: kind}
		if err := mod.SetRequirement(root, req); err != nil {
			return err
		}
		fmt.Printf("gocpy: require.%s %s\n", kind, req)
		cfg, err = mod.LoadConfig(root)
		if err != nil {
			return err
		}
	}
	g, err := mod.Resolve(root, cfg, logf)
	if err != nil {
		return err
	}
	if err := goDownload(root, g); err != nil {
		return err
	}
	return g.WriteSum()
}

// goResolveVersion validates a Go module version (and resolves @latest).
func goResolveVersion(path, ver string) (string, error) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		if ver == "latest" {
			return "", fmt.Errorf("resolving %s@latest needs the Go toolchain in PATH; give an explicit version", path)
		}
		return ver, nil
	}
	tmp, err := os.MkdirTemp("", "gocpy-get-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module gocpyget\n\ngo 1.21\n"), 0o644)
	cmd := exec.Command(gobin, "list", "-m", "-f", "{{.Version}}", path+"@"+ver)
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go module %s@%s: %s", path, ver, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// goDownload fetches the Go requirements and records their sums in gocpy.sum.
func goDownload(root string, g *mod.Graph) error {
	if len(g.Go) == 0 {
		return nil
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		logf("gocpy: note: Go toolchain not found; Go modules will be downloaded at build time\n")
		return nil
	}
	tmp, err := os.MkdirTemp("", "gocpy-mod-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var b strings.Builder
	b.WriteString("module gocpybridge\n\ngo 1.21\n\nrequire (\n")
	var ks []string
	for k := range g.Go {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Fprintf(&b, "\t%s %s\n", k, g.Go[k])
	}
	b.WriteString(")\n")
	for _, k := range ks {
		if r, ok := g.GoRepl[k]; ok {
			if p, v, ok := strings.Cut(r, "@"); ok {
				fmt.Fprintf(&b, "replace %s => %s %s\n", k, p, v)
			} else {
				fmt.Fprintf(&b, "replace %s => %s\n", k, r)
			}
		}
	}
	os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(b.String()), 0o644)
	os.WriteFile(filepath.Join(tmp, "go.sum"), []byte(g.Sum.Text()), 0o644)
	cmd := exec.Command(gobin, "mod", "download")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod download: %s", strings.TrimSpace(errb.String()))
	}
	if data, err := os.ReadFile(filepath.Join(tmp, "go.sum")); err == nil {
		g.Sum.Merge(string(data))
	}
	return nil
}

// gocpy mod download|verify|graph
func cmdMod(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gocpy mod download | verify | graph")
	}
	root, cfg, err := moduleRoot()
	if err != nil {
		return err
	}
	g, err := mod.Resolve(root, cfg, logf)
	if err != nil {
		return err
	}
	switch args[0] {
	case "download":
		if err := goDownload(root, g); err != nil {
			return err
		}
		if err := g.WriteSum(); err != nil {
			return err
		}
		for _, p := range sortedKeys(g.Gocpy) {
			m := g.Gocpy[p]
			fmt.Printf("gocpy %s %s %s\n", m.Path, m.Version, m.Dir)
		}
		for _, p := range sortedKeys(g.Go) {
			fmt.Printf("go    %s %s\n", p, g.Go[p])
		}
	case "verify":
		if err := g.WriteSum(); err != nil {
			return err
		}
		errs := g.Verify()
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, e)
		}
		if len(errs) > 0 {
			return fmt.Errorf("verification failed")
		}
		fmt.Println("all modules verified")
	case "graph":
		for _, from := range sortedKeys(g.Edges) {
			for _, r := range g.Edges[from] {
				fmt.Printf("%s %s (%s)\n", from, r, r.Kind)
			}
		}
		for _, p := range sortedKeys(g.Gocpy) {
			fmt.Printf("selected gocpy %s@%s\n", p, g.Gocpy[p].Version)
		}
		for _, p := range sortedKeys(g.Go) {
			fmt.Printf("selected go    %s@%s\n", p, g.Go[p])
		}
	default:
		return fmt.Errorf("unknown mod command %q (download, verify, graph)", args[0])
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
