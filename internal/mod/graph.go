package mod

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Module is a selected Gocpy module.
type Module struct {
	Path    string
	Version string
	Dir     string
	Config  *Config
	Local   bool // replaced by a local directory
}

// Graph is the result of minimal version selection over Gocpy requirements,
// plus the merged Go requirements of every module in the graph.
type Graph struct {
	Root   string
	Main   *Config
	Gocpy  map[string]*Module // module path -> selected version
	Go     map[string]string  // Go module path -> highest required version
	GoRepl map[string]string  // Go module path -> replacement (absolute dir or path@version)
	Sum    *SumFile
	// Edges: module@version -> its requirements (for `gocpy mod graph`).
	Edges map[string][]Requirement
}

// Resolve loads the requirement graph of the main module at root.
// It downloads missing modules and verifies them against gocpy.sum.
func Resolve(root string, cfg *Config, logf func(string, ...any)) (*Graph, error) {
	sum, err := ReadSum(root)
	if err != nil {
		return nil, err
	}
	g := &Graph{Root: root, Main: cfg, Gocpy: map[string]*Module{}, Go: map[string]string{},
		GoRepl: map[string]string{}, Sum: sum, Edges: map[string][]Requirement{}}
	for p, r := range cfg.Replace {
		if strings.Contains(r, "@") {
			g.GoRepl[p] = r
		} else {
			g.GoRepl[p] = absFrom(root, r)
		}
	}

	type item struct {
		req  Requirement
		from string
	}
	var work []item
	push := func(from string, c *Config) {
		key := from
		g.Edges[key] = c.Require.All()
		for _, r := range c.Require.Go {
			if cur, ok := g.Go[r.Path]; !ok || CompareVersions(r.Version, cur) > 0 {
				g.Go[r.Path] = r.Version
			}
		}
		for _, r := range c.Require.Gocpy {
			work = append(work, item{r, from})
		}
	}
	mainName := cfg.Module
	if mainName == "" {
		mainName = "main"
	}
	push(mainName, cfg)
	visited := map[string]bool{}
	for len(work) > 0 {
		it := work[0]
		work = work[1:]
		r := it.req
		if r.Path == cfg.Module {
			return nil, fmt.Errorf("%s requires the main module %s", it.from, r.Path)
		}
		if cur := g.Gocpy[r.Path]; cur != nil && CompareVersions(r.Version, cur.Version) <= 0 {
			continue
		}
		key := r.String()
		m := &Module{Path: r.Path, Version: r.Version}
		if repl, ok := cfg.Replace[r.Path]; ok && !strings.Contains(repl, "@") {
			m.Dir, m.Local = absFrom(root, repl), true
			c, err := LoadConfig(m.Dir)
			if err != nil {
				return nil, fmt.Errorf("replace %s => %s: %v", r.Path, repl, err)
			}
			if c.Module != r.Path {
				return nil, fmt.Errorf("replace %s => %s: gocpy.yml declares module %q", r.Path, repl, c.Module)
			}
			m.Config = c
		} else {
			dir, hash, err := Download(r.Path, r.Version, logf)
			if err != nil {
				return nil, fmt.Errorf("%s (required by %s): %v", key, it.from, err)
			}
			if err := sum.Check(r.Path, r.Version, hash); err != nil {
				return nil, err
			}
			c, err := LoadConfig(dir)
			if err != nil {
				return nil, err
			}
			m.Dir, m.Config = dir, c
		}
		g.Gocpy[r.Path] = m
		if !visited[key] {
			visited[key] = true
			push(key, m.Config)
		}
	}
	return g, nil
}

func absFrom(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

// Lookup finds the Gocpy module that provides import path p (longest prefix).
func (g *Graph) Lookup(p string) *Module {
	var best *Module
	for mp, m := range g.Gocpy {
		if (p == mp || strings.HasPrefix(p, mp+"/")) && (best == nil || len(mp) > len(best.Path)) {
			best = m
		}
	}
	return best
}

// LookupGo finds the Go module that provides import path p (longest prefix).
func (g *Graph) LookupGo(p string) string {
	best := ""
	for mp := range g.Go {
		if (p == mp || strings.HasPrefix(p, mp+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

// Verify re-hashes downloaded modules and compares them with gocpy.sum.
func (g *Graph) Verify() []error {
	var errs []error
	for _, p := range sortedKeys(g.Gocpy) {
		m := g.Gocpy[p]
		if m.Local {
			continue
		}
		h, err := HashDir(m.Dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if want := g.Sum.lines[m.Path+" "+m.Version]; want != h {
			errs = append(errs, fmt.Errorf("%s@%s: dir has been modified (%s, gocpy.sum %s)", m.Path, m.Version, h, want))
		}
	}
	return errs
}

// WriteSum saves gocpy.sum if it changed.
func (g *Graph) WriteSum() error { return g.Sum.Write() }

// Exists reports whether a directory exists.
func Exists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}
