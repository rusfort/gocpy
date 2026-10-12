// Package mod implements Gocpy modules: gocpy.yml, versioned requirements
// (Gocpy and Go), the module cache, gocpy.sum and minimal version selection.
package mod

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind of a required module.
type Kind int

const (
	KindGocpy Kind = iota // Gocpy source module (has gocpy.yml)
	KindGo                // Go module, linked through the Go bridge
)

func (k Kind) String() string {
	if k == KindGo {
		return "go"
	}
	return "gocpy"
}

// Requirement is "path@version".
type Requirement struct {
	Path    string
	Version string
	Kind    Kind
}

func (r Requirement) String() string { return r.Path + "@" + r.Version }

// Requires is the require: section of gocpy.yml.
//
//	require:
//	  gocpy:
//	    - github.com/john/gocpy-repo@v0.1.0
//	  go:
//	    - github.com/bob/go-repo@v0.2.0
type Requires struct {
	Gocpy []Requirement
	Go    []Requirement
}

// All returns Gocpy requirements followed by Go requirements.
func (r Requires) All() []Requirement {
	return append(append([]Requirement{}, r.Gocpy...), r.Go...)
}

// UnmarshalYAML accepts the sectioned form and, for compatibility with 0.1,
// a flat map path: version (treated as Gocpy modules).
func (r *Requires) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: require must be a mapping with gocpy: and go: lists", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch k.Value {
		case "gocpy", "go":
			kind := KindGocpy
			if k.Value == "go" {
				kind = KindGo
			}
			reqs, err := parseReqList(v, kind)
			if err != nil {
				return err
			}
			if kind == KindGo {
				r.Go = append(r.Go, reqs...)
			} else {
				r.Gocpy = append(r.Gocpy, reqs...)
			}
		default: // legacy: path: version
			if v.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: unknown require section %q (expected gocpy or go)", k.Line, k.Value)
			}
			req := Requirement{Path: k.Value, Version: v.Value, Kind: KindGocpy}
			if err := req.validate(k.Line); err != nil {
				return err
			}
			r.Gocpy = append(r.Gocpy, req)
		}
	}
	seen := map[string]Kind{}
	for _, q := range r.All() {
		if prev, ok := seen[q.Path]; ok {
			if prev != q.Kind {
				return fmt.Errorf("require: %s is listed both under gocpy and go", q.Path)
			}
			return fmt.Errorf("require: %s is listed twice", q.Path)
		}
		seen[q.Path] = q.Kind
	}
	return nil
}

func parseReqList(n *yaml.Node, kind Kind) ([]Requirement, error) {
	var out []Requirement
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil, nil
		}
		return nil, fmt.Errorf("line %d: require.%s must be a list of path@version", n.Line, kind)
	case yaml.SequenceNode:
		for _, e := range n.Content {
			if e.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: require.%s: expected path@version", e.Line, kind)
			}
			path, ver, ok := strings.Cut(strings.TrimSpace(e.Value), "@")
			if !ok {
				return nil, fmt.Errorf("line %d: require.%s: %q: missing @version", e.Line, kind, e.Value)
			}
			req := Requirement{Path: path, Version: ver, Kind: kind}
			if err := req.validate(e.Line); err != nil {
				return nil, err
			}
			out = append(out, req)
		}
	case yaml.MappingNode: // path: version
		for i := 0; i+1 < len(n.Content); i += 2 {
			req := Requirement{Path: n.Content[i].Value, Version: n.Content[i+1].Value, Kind: kind}
			if err := req.validate(n.Content[i].Line); err != nil {
				return nil, err
			}
			out = append(out, req)
		}
	}
	return out, nil
}

func (r Requirement) validate(line int) error {
	if err := CheckPath(r.Path); err != nil {
		return fmt.Errorf("line %d: require.%s: %v", line, r.Kind, err)
	}
	if !IsValidVersion(r.Version) {
		return fmt.Errorf("line %d: require.%s: %s: invalid version %q (want vMAJOR.MINOR.PATCH, e.g. v0.1.0)",
			line, r.Kind, r.Path, r.Version)
	}
	return nil
}

// CheckPath validates a module path: host with a dot, then path elements.
func CheckPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty module path")
	}
	if strings.HasPrefix(p, "std/") || p == "std" {
		return fmt.Errorf("%s: std/ is reserved for the standard library", p)
	}
	elems := strings.Split(p, "/")
	if !strings.Contains(elems[0], ".") {
		return fmt.Errorf("%s: module path must start with a domain (e.g. github.com/user/repo)", p)
	}
	for _, e := range elems {
		if e == "" || e == "." || e == ".." {
			return fmt.Errorf("%s: malformed module path", p)
		}
		for _, c := range e {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
				return fmt.Errorf("%s: invalid character %q in module path", p, c)
			}
		}
	}
	return nil
}

// Config is gocpy.yml.
type Config struct {
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
	Require Requires `yaml:"require"`
	// Replace maps a module path to a local directory (relative to gocpy.yml)
	// or, for Go modules, to another path@version. Only the main module's
	// replacements apply, as in Go.
	Replace map[string]string `yaml:"replace"`
}

// LoadConfig reads dir/gocpy.yml.
func LoadConfig(dir string) (*Config, error) {
	p := filepath.Join(dir, "gocpy.yml")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return ParseConfig(p, data)
}

// ParseConfig parses gocpy.yml contents.
func ParseConfig(name string, data []byte) (*Config, error) {
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	if cfg.Module != "" && cfg.Module != "main" && strings.Contains(cfg.Module, "/") {
		if err := CheckPath(cfg.Module); err != nil {
			return nil, fmt.Errorf("%s: module: %v", name, err)
		}
	}
	for k := range cfg.Replace {
		if err := CheckPath(k); err != nil {
			return nil, fmt.Errorf("%s: replace: %v", name, err)
		}
	}
	return cfg, nil
}

// sortedKeys returns map keys in order.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
