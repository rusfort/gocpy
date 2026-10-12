package build

import (
	"crypto/sha256"
	"fmt"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/rusfort/gocpy/internal/token"
)

// Local Go files: a Gocpy package may import a Go file that lies in its own
// directory with `import "./name.go"`. All non-test .go files of the
// directory form one Go package (as in Go); it is built by the Go bridge
// together with the Go modules from require.go, so the Go code may import
// them too.

// IsLocalGoImport reports whether an import path names a Go file in the
// importing package's directory ("./name.go").
func IsLocalGoImport(p string) bool {
	if !strings.HasPrefix(p, "./") || !strings.HasSuffix(p, ".go") {
		return false
	}
	name := p[2:]
	return len(name) > 3 && !strings.ContainsAny(name, `/\`)
}

// LocalGoKey is the bridge import path of the Go package formed by the .go
// files of dir.
func LocalGoKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	h := sha256.Sum256([]byte(abs))
	base := strings.Map(func(r rune) rune {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return unicode.ToLower(r)
		}
		return -1
	}, filepath.Base(abs))
	if base == "" {
		base = "pkg"
	}
	return fmt.Sprintf("gocpybridge/local/%s_%x", base, h[:5])
}

// importKey maps an import path written in a file of u to the unit key.
func (l *loader) importKey(u *unit, p string) string {
	if IsLocalGoImport(p) {
		return LocalGoKey(u.dir)
	}
	return p
}

func (l *loader) resolveLocalGo(from *unit, p string, pos token.Pos) (*unit, error) {
	if from.isGo {
		return nil, fmt.Errorf("import %q: not allowed here", p)
	}
	name := p[2:]
	if strings.HasSuffix(name, "_test.go") {
		return nil, fmt.Errorf("import %q: Go test files cannot be imported", p)
	}
	file := filepath.Join(from.dir, name)
	if _, err := os.Stat(file); err != nil {
		return nil, fmt.Errorf("import %q: no Go file %s in the package directory", p, name)
	}
	key := LocalGoKey(from.dir)
	if u := l.units[key]; u != nil {
		return u, nil
	}
	f, err := goparser.ParseFile(gotoken.NewFileSet(), file, nil, goparser.PackageClauseOnly)
	if err != nil {
		return nil, fmt.Errorf("import %q: %v", p, err)
	}
	pkgName := f.Name.Name
	if pkgName == "main" {
		suggest := strings.TrimSuffix(name, ".go")
		if !goIdent(suggest) {
			suggest = "util"
		}
		return nil, fmt.Errorf("import %q: the Go file has package main; give the Go files of the directory a library package name (for example package %s)", p, suggest)
	}
	if err := l.checkLocalGoImports(from.dir); err != nil {
		return nil, fmt.Errorf("import %q: %v", p, err)
	}
	u := &unit{path: key, dir: from.dir, name: pkgName, isGo: true, goLocal: from.dir, goFile: file,
		modPath: from.modPath, modRoot: from.modRoot}
	l.units[key] = u
	l.state[key] = 2
	l.goPkgs[key] = append(l.goPkgs[key], pos)
	l.goLocal[key] = from.dir
	return u, nil
}

func goIdent(s string) bool {
	for i, r := range s {
		if !(r == '_' || unicode.IsLetter(r) || i > 0 && unicode.IsDigit(r)) {
			return false
		}
	}
	return s != ""
}

// checkLocalGoImports requires that the non-standard imports of the local Go
// files come from Go modules listed under require.go (versions stay pinned).
func (l *loader) checkLocalGoImports(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	fset := gotoken.NewFileSet()
	var missing []string
	seen := map[string]bool{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, filepath.Join(dir, n), nil, goparser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, "\"`")
			first, _, _ := strings.Cut(ip, "/")
			if ip == "C" || !strings.Contains(first, ".") || seen[ip] {
				continue // standard library (or cgo)
			}
			seen[ip] = true
			if err := l.needGraph(); err != nil {
				return fmt.Errorf("%s imports %s: %v", n, ip, l.graphErr)
			}
			if l.graph.LookupGo(ip) == "" {
				missing = append(missing, fmt.Sprintf("%s (%s)", ip, fset.Position(imp.Pos())))
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the Go files import packages that are not in require.go of gocpy.yml: %s;\n\tadd the modules (gocpy get <module>@<version>)",
			strings.Join(missing, ", "))
	}
	return nil
}
