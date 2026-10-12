package build

import (
	"fmt"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/check"
	"github.com/rusfort/gocpy/internal/types"
)

// Analysis is the result of a tolerant front-end run (language server).
type Analysis struct {
	Prog   *Program
	Pkg    *types.Package // nil if type checking did not run
	Info   *types.Info    // may be partial
	Errors []string       // "file:line:col: msg" (multi-line messages allowed)
	Warns  []string
}

// Analyze loads and type-checks the package at target, collecting all errors
// instead of stopping at the first stage that fails.
func Analyze(target string, opts *Options) (*Analysis, error) {
	if opts == nil {
		opts = &Options{}
	}
	opts.Tolerant = true
	prog, err := Load(target, opts)
	if err != nil {
		return nil, err
	}
	a := &Analysis{Prog: prog, Errors: append([]string{}, prog.Errors...)}
	types.Demangle = prog.Demangle
	files := prog.Files
	func() {
		defer func() {
			if r := recover(); r != nil {
				a.Errors = append(a.Errors, fmt.Sprintf("%s:1:1: internal checker error: %v", target, r))
			}
		}()
		for _, d := range check.Package(files) {
			if d.Severity == check.Warning {
				a.Warns = append(a.Warns, prog.Demangle(d.Pos.String()+": "+d.Msg))
			} else {
				a.Errors = append(a.Errors, prog.Demangle(d.Pos.String()+": "+d.Msg))
			}
		}
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				a.Errors = append(a.Errors, fmt.Sprintf("%s:1:1: internal type checker error: %v", target, r))
			}
		}()
		pkg, info, err := types.Check(files)
		a.Pkg, a.Info = pkg, info
		if el, ok := err.(types.ErrorList); ok {
			for _, e := range el {
				a.Errors = append(a.Errors, prog.Demangle(e.Error()))
			}
		} else if err != nil {
			a.Errors = append(a.Errors, prog.Demangle(err.Error()))
		}
		if info != nil {
			a.Errors = append(a.Errors, prog.checkCrossAccess(info)...)
		}
	}()
	return a, nil
}

// PkgInfo describes one package of a loaded program.
type PkgInfo struct {
	Path    string // import path ("command-line-file" for a single file)
	Name    string // package name
	Dir     string
	Prefix  string // prefix of package-level names in the merged program
	Display string
	IsGo    bool // Go package through the bridge (files are generated stubs)
	Target  bool
	Files   []*ast.File
	ModPath string
	ModRoot string
}

func (u *unit) info() *PkgInfo {
	return &PkgInfo{Path: u.path, Name: u.name, Dir: u.dir, Prefix: u.prefix, Display: u.display,
		IsGo: u.isGo, Target: u.target, Files: u.files, ModPath: u.modPath, ModRoot: u.modRoot}
}

// Packages lists the packages of the program (dependencies first).
func (p *Program) Packages() []*PkgInfo {
	var out []*PkgInfo
	for _, u := range p.Units {
		out = append(out, u.info())
	}
	return out
}

// PackageOf returns the package that contains the file.
func (p *Program) PackageOf(file string) *PkgInfo {
	if u := p.fileUnit[file]; u != nil {
		return u.info()
	}
	return nil
}

// PackageByPath returns a package of the program by import path.
func (p *Program) PackageByPath(path string) *PkgInfo {
	for _, u := range p.Units {
		if u.path == path {
			return u.info()
		}
	}
	return nil
}

// OwnerOf returns the package that declares a package-level name of the
// merged program, and the name without the package prefix.
func (p *Program) OwnerOf(name string) (*PkgInfo, string) {
	u := p.ownerOf(name)
	if u == nil {
		return nil, name
	}
	return u.info(), strings.TrimPrefix(name, u.prefix)
}

// Exported reports whether a name is exported (starts with an upper-case letter).
func Exported(name string) bool { return exported(name) }
