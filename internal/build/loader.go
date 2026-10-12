package build

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/gobridge"
	"github.com/rusfort/gocpy/internal/mod"
	"github.com/rusfort/gocpy/internal/parser"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

// unit is one package of the program.
type unit struct {
	path    string // import path
	dir     string
	name    string
	files   []*ast.File
	modPath string // module that contains the package
	modRoot string
	isGo    bool
	target  bool
	goLocal string // for local Go packages: the directory with the .go files
	goFile  string // for local Go packages: the imported .go file

	prefix  string          // C/AST prefix of package-level names ("" for the target)
	display string          // name shown in messages
	top     map[string]bool // package-level names
	structs map[string]bool // package-level struct types
	unsup   map[string]string
}

var errReported = errors.New("reported")

// Program is a loaded, merged multi-package program.
type Program struct {
	Root     string
	Config   *mod.Config
	Target   string
	Files    []*ast.File // dependencies first, the target package last
	Units    []*unit
	Graph    *mod.Graph
	Bridge   *gobridge.Result
	fileUnit map[string]*unit
	demangle func(string) string
	// Qual maps identifiers that replaced pkg.Name selectors to the original
	// selector parts (used by the language server).
	Qual map[*ast.Ident]*QualRef
	// Errors collected in tolerant mode ("file:line:col: msg").
	Errors []string
}

// QualRef is the original form of a qualified identifier pkg.Name.
type QualRef struct {
	Pkg  *ast.Ident // package name as written
	Sel  *ast.Ident // member name as written
	Path string     // import path
}

// Demangle rewrites prefixed names in a message back to pkg.Name.
func (p *Program) Demangle(s string) string {
	if p == nil || p.demangle == nil {
		return s
	}
	return p.demangle(s)
}

type loader struct {
	graphErr error
	opts     *Options
	root     string
	cfg      *mod.Config
	graph    *mod.Graph
	units    map[string]*unit
	state    map[string]int // 1 = loading, 2 = done
	order    []*unit
	goPkgs   map[string][]token.Pos
	goLocal  map[string]string // bridge import path of a local Go package -> directory
	errs     []string
	qual     map[*ast.Ident]*QualRef
}

func (l *loader) errorf(p token.Pos, f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	if p.File != "" {
		msg = p.String() + ": " + msg
	}
	l.errs = append(l.errs, msg)
}

func parsePackage(paths []string) ([]*ast.File, error) {
	files, err, _ := parsePackageOpts(paths, nil)
	return files, err
}

// parsePackageOpts parses a package. In tolerant mode it also returns the
// (possibly partial) files when there are syntax errors.
func parsePackageOpts(paths []string, opts *Options) ([]*ast.File, error, []*ast.File) {
	var files, partial []*ast.File
	var errs []string
	tolerant := opts != nil && opts.Tolerant
	for _, p := range paths {
		src, err := opts.readFile(p)
		if err != nil {
			return nil, err, nil
		}
		f, err := safeParse(p, src)
		if err != nil {
			errs = append(errs, err.Error())
			if tolerant && f != nil {
				partial = append(partial, f)
			}
			continue
		}
		files = append(files, f)
		partial = append(partial, f)
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n")), partial
	}
	for _, f := range files[1:] {
		if f.Name.Name != files[0].Name.Name {
			errs = append(errs, fmt.Sprintf("%s: package %s, expected %s (one package per directory)",
				f.Package, f.Name.Name, files[0].Name.Name))
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n")), partial
	}
	return files, nil, files
}

func safeParse(p string, src []byte) (f *ast.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, fmt.Errorf("%s:1:1: internal parser error: %v", p, r)
		}
	}()
	f, err = parser.ParseFile(p, src)
	if f != nil && f.Name == nil {
		f = nil
	}
	return f, err
}

// Load parses the target package and everything it imports.
func Load(target string, opts *Options) (*Program, error) {
	if opts == nil {
		opts = &Options{}
	}
	dir, paths, err := Sources(target)
	if err != nil {
		return nil, err
	}
	root, cfg, err := findModule(dir)
	if err != nil {
		return nil, err
	}
	files, err, partial := parsePackageOpts(paths, opts)
	var parseErrs []string
	if err != nil {
		if !opts.Tolerant || len(partial) == 0 {
			return nil, err
		}
		parseErrs = append(parseErrs, err.Error())
		files = partial
	}
	l := &loader{opts: opts, root: root, cfg: cfg, units: map[string]*unit{}, state: map[string]int{},
		goPkgs: map[string][]token.Pos{}, goLocal: map[string]string{}, qual: map[*ast.Ident]*QualRef{}, errs: parseErrs}
	absDir, _ := filepath.Abs(dir)
	rel, _ := filepath.Rel(root, absDir)
	modPath := cfg.Module
	if modPath == "" {
		modPath = "main"
	}
	tpath := modPath
	if rel != "." {
		tpath = modPath + "/" + filepath.ToSlash(rel)
	}
	if len(paths) == 1 && strings.HasSuffix(target, ".gocpy") {
		tpath = "command-line-file"
	}
	t := &unit{path: tpath, dir: absDir, name: files[0].Name.Name, files: files, modPath: modPath, modRoot: root, target: true}
	l.units[tpath] = t
	l.visit(t)
	if len(l.errs) > 0 && !opts.Tolerant {
		return nil, errors.New(strings.Join(l.errs, "\n"))
	}

	prog := &Program{Root: root, Config: cfg, Target: target, Graph: l.graph, Qual: l.qual}
	// Go packages through the bridge
	var goUnits []*unit
	if len(l.goPkgs) > 0 && opts.NoGoBridge {
		for p := range l.goPkgs {
			delete(l.units, p)
		}
	} else if len(l.goPkgs) > 0 {
		if l.graph == nil && len(l.goLocal) > 0 && len(cfg.Require.Go) > 0 {
			// the local Go files may import the Go modules of require.go
			if err := l.needGraph(); err != nil && !opts.Tolerant {
				return nil, errors.New(strings.Join(l.errs, "\n"))
			}
		}
		req := gobridge.Request{Root: root, Replace: map[string]string{}, Local: l.goLocal,
			CC: cfg.Build.CC, Log: opts.Log}
		if l.graph != nil {
			req.Requires, req.Sum = l.graph.Go, l.graph.Sum
		}
		if req.CC == "" {
			req.CC = CCompiler()
		}
		if l.graph != nil {
			for p, r := range l.graph.GoRepl {
				if _, ok := l.graph.Go[p]; ok {
					req.Replace[p] = r
				}
			}
		}
		for p := range l.goPkgs {
			req.Packages = append(req.Packages, p)
		}
		sort.Strings(req.Packages)
		res, err := gobridge.Build(req)
		if err != nil {
			if !opts.Tolerant {
				return nil, err
			}
			for p, ps := range l.goPkgs {
				for _, pos := range ps {
					l.errorf(pos, "Go package %s: %v", p, err)
				}
				delete(l.units, p)
			}
			req.Packages = nil
		} else {
			if l.graph != nil {
				if err := l.graph.WriteSum(); err != nil && !opts.Tolerant {
					return nil, err
				}
			}
			prog.Bridge = res
		}
		for _, p := range req.Packages {
			bp := res.Packages[p]
			fs, err := parsePackage([]string{bp.Stub})
			if err != nil {
				if opts.Tolerant {
					delete(l.units, p)
					continue
				}
				return nil, fmt.Errorf("go bridge for %q: %v", p, err)
			}
			u := l.units[p]
			u.files, u.name, u.isGo, u.unsup = fs, bp.Name, true, bp.Unsupported
			if u.goLocal == "" {
				u.dir = filepath.Dir(bp.Stub)
			}
			goUnits = append(goUnits, u)
		}
	} else if l.graph != nil {
		if err := l.graph.WriteSum(); err != nil && !opts.Tolerant {
			return nil, err
		}
	}
	all := append(goUnits, l.order...)

	// names
	byName := map[string]int{}
	for _, u := range all {
		if !u.target {
			byName[u.name]++
		}
	}
	used := map[string]bool{}
	var repl []string
	for _, u := range all {
		u.top, u.structs = topNames(u.files)
		if u.target {
			continue
		}
		base := u.name
		u.display = u.name
		if byName[u.name] > 1 {
			u.display = u.path
		}
		p := base
		for n := 2; used[p]; n++ {
			p = fmt.Sprintf("%s_%d", base, n)
		}
		used[p] = true
		u.prefix = p + "__"
		repl = append(repl, u.prefix, u.display+".")
	}
	if len(repl) > 0 {
		var alts []string
		for i := 0; i < len(repl); i += 2 {
			alts = append(alts, regexp.QuoteMeta(repl[i]))
		}
		re := regexp.MustCompile(`\b(` + strings.Join(alts, "|") + `)`)
		m := map[string]string{}
		for i := 0; i < len(repl); i += 2 {
			m[repl[i]] = repl[i+1]
		}
		prog.demangle = func(s string) string {
			return re.ReplaceAllStringFunc(s, func(x string) string { return m[x] })
		}
	}

	prog.fileUnit = map[string]*unit{}
	for _, u := range all {
		for _, f := range u.files {
			prog.fileUnit[f.Package.File] = u
			l.rewriteFile(u, f)
		}
	}
	if len(l.errs) > 0 && !opts.Tolerant {
		return nil, errors.New(prog.Demangle(strings.Join(l.errs, "\n")))
	}
	for i, e := range l.errs {
		l.errs[i] = prog.Demangle(e)
	}
	prog.Errors = l.errs
	for _, u := range all {
		for _, f := range u.files {
			f.Name = &ast.Ident{NamePos: f.Name.NamePos, Name: t.name}
			prog.Files = append(prog.Files, f)
		}
	}
	prog.Units = all
	return prog, nil
}

func (l *loader) needGraph() error {
	if l.graphErr != nil {
		if l.opts.Tolerant {
			l.errs = append(l.errs, l.graphErr.Error())
		}
		return errReported
	}
	if l.graph != nil {
		return nil
	}
	g, err := mod.Resolve(l.root, l.cfg, l.opts.logf)
	if err != nil {
		l.graphErr = err
		l.errs = append(l.errs, err.Error())
		return errReported
	}
	l.graph = g
	return nil
}

// visit loads the imports of u depth-first (u is appended after its imports).
func (l *loader) visit(u *unit) {
	l.state[u.path] = 1
	for _, f := range u.files {
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || strings.HasPrefix(p, "std/") {
				continue // checked by the type checker
			}
			dep, err := l.resolve(u, p, imp.Pos())
			if err == errReported {
				if !l.opts.Tolerant {
					return
				}
				l.errs = l.errs[:len(l.errs)-1] // positioned at the import instead
				l.errorf(imp.Pos(), "%v", l.graphErr)
				continue
			}
			if err != nil {
				l.errorf(imp.Pos(), "%v", err)
				continue
			}
			if dep == nil {
				continue
			}
			switch l.state[dep.path] {
			case 1:
				l.errorf(imp.Pos(), "import cycle: %s imports %s", u.path, dep.path)
			case 0:
				if !dep.isGo {
					l.visit(dep)
				}
			}
		}
	}
	l.state[u.path] = 2
	if !u.isGo {
		l.order = append(l.order, u)
	}
}

func (l *loader) resolve(from *unit, p string, pos token.Pos) (*unit, error) {
	if IsLocalGoImport(p) {
		return l.resolveLocalGo(from, p, pos)
	}
	if u := l.units[p]; u != nil {
		if u.target && !from.target {
			return nil, fmt.Errorf("import cycle: %s imports %s", from.path, p)
		}
		return u, nil
	}
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		if strings.HasSuffix(p, ".go") {
			return nil, fmt.Errorf("import %q: a Go file can be imported only from the same directory (\"./file.go\")", p)
		}
		return nil, fmt.Errorf("relative import %q is not allowed; use the module path (%s/...)", p, from.modPath)
	}
	var dir, modPath, modRoot string
	switch {
	case p == from.modPath || strings.HasPrefix(p, from.modPath+"/"):
		modPath, modRoot = from.modPath, from.modRoot
		dir = filepath.Join(from.modRoot, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, from.modPath), "/")))
	default:
		if err := l.needGraph(); err != nil {
			return nil, err
		}
		if m := l.graph.Lookup(p); m != nil {
			modPath, modRoot = m.Path, m.Dir
			dir = filepath.Join(m.Dir, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, m.Path), "/")))
		} else if gm := l.graph.LookupGo(p); gm != "" {
			u := &unit{path: p, isGo: true}
			l.units[p] = u
			l.state[p] = 2
			l.goPkgs[p] = append(l.goPkgs[p], pos)
			return u, nil
		} else {
			return nil, fmt.Errorf("package %q is not in std, the main module (%s) or any required module;\n"+
				"\tadd it to require: in gocpy.yml (gocpy get %s@<version>)", p, l.cfg.Module, p)
		}
	}
	_, paths, err := Sources(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("package %q: no directory %s in module %s", p, dir, modPath)
		}
		return nil, fmt.Errorf("package %q: %v", p, err)
	}
	files, err, partial := parsePackageOpts(paths, l.opts)
	if err != nil {
		if !l.opts.Tolerant || len(partial) == 0 {
			return nil, err
		}
		l.errs = append(l.errs, err.Error())
		files = partial
	}
	if files[0].Name.Name == "main" {
		return nil, fmt.Errorf("import %q: it is a program (package main), not an importable package", p)
	}
	u := &unit{path: p, dir: dir, name: files[0].Name.Name, files: files, modPath: modPath, modRoot: modRoot}
	l.units[p] = u
	return u, nil
}

// topNames collects package-level declarations and struct types.
func topNames(files []*ast.File) (map[string]bool, map[string]bool) {
	top, structs := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name != "_" {
					top[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						top[s.Name.Name] = true
						if _, ok := s.Type.(*ast.StructType); ok {
							structs[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.Name != "_" {
								top[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return top, structs
}

func exported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// ---------------------------------------------------------------- rewriting

type fileRW struct {
	l       *loader
	u       *unit
	imports map[string]*unit // alias -> package
	used    map[string]bool
	std     map[string]bool // aliases of std imports
}

func (l *loader) rewriteFile(u *unit, f *ast.File) {
	rw := &fileRW{l: l, u: u, imports: map[string]*unit{}, used: map[string]bool{}, std: map[string]bool{}}
	var keep []*ast.ImportSpec
	aliasPos := map[string]token.Pos{}
	aliasSpec := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || strings.HasPrefix(p, "std/") {
			keep = append(keep, imp)
			if err == nil {
				name := path.Base(p)
				if imp.Alias != nil {
					name = imp.Alias.Name
				}
				if _, dup := aliasPos[name]; dup {
					l.errorf(imp.Pos(), "%s imported twice (import conflicts are not allowed; use an alias)", name)
				}
				aliasPos[name] = imp.Pos()
				rw.std[name] = true
			}
			continue
		}
		dep := l.units[l.importKey(u, p)]
		if dep == nil {
			continue // already reported
		}
		name := dep.name
		if imp.Alias != nil {
			name = imp.Alias.Name
			if name == "." || name == "_" {
				l.errorf(imp.Alias.Pos(), "dot and blank imports are not allowed")
				continue
			}
		}
		if _, dup := aliasPos[name]; dup {
			if rw.imports[name] == dep && dep.goLocal != "" {
				l.errorf(imp.Pos(), "%q and %q are files of the same Go package %s; import only one of them", aliasSpec[name], p, dep.name)
				continue
			}
			l.errorf(imp.Pos(), "%s imported twice (import conflicts are not allowed; use an alias)", name)
			continue
		}
		if u.top[name] {
			l.errorf(imp.Pos(), "import name %s conflicts with a package-level declaration of %s", name, u.name)
		}
		aliasPos[name] = imp.Pos()
		aliasSpec[name] = p
		rw.imports[name] = dep
	}
	f.Imports = keep
	for i := range f.Decls {
		rw.walk(reflect.ValueOf(&f.Decls[i]).Elem(), ctxNone)
	}
	var names []string
	for n := range rw.imports {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !rw.used[n] {
			l.errorf(aliasPos[n], "%q imported and not used", aliasSpec[n])
		}
	}
}

type walkCtx int

const (
	ctxNone   walkCtx = iota
	ctxFields         // field/method names of struct and interface types are not renamed
)

var (
	posT      = reflect.TypeOf(token.Pos{})
	exprIface = reflect.TypeOf((*ast.Expr)(nil)).Elem()
)

func (rw *fileRW) rename(id *ast.Ident) {
	if id == nil {
		return
	}
	if dep, ok := rw.imports[id.Name]; ok {
		rw.l.errorf(id.Pos(), "%s conflicts with imported package %q", id.Name, dep.path)
		return
	}
	if rw.u.prefix != "" && rw.u.top[id.Name] {
		id.Name = rw.u.prefix + id.Name
	}
}

// qualified replaces pkg.Name with the prefixed identifier.
func (rw *fileRW) qualified(s *ast.SelectorExpr) (*ast.Ident, bool) {
	x, ok := s.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	dep, ok := rw.imports[x.Name]
	if !ok {
		return nil, false
	}
	rw.used[x.Name] = true
	name := s.Sel.Name
	switch {
	case !dep.top[name]:
		if why, ok := dep.unsup[name]; ok {
			rw.l.errorf(s.Sel.Pos(), "%s.%s: %s", x.Name, name, why)
		} else {
			rw.l.errorf(s.Sel.Pos(), "undefined: %s.%s", x.Name, name)
		}
	case !exported(name):
		rw.l.errorf(s.Sel.Pos(), "cannot refer to unexported name %s.%s", x.Name, name)
	}
	id := &ast.Ident{NamePos: x.NamePos, Name: dep.prefix + name}
	rw.l.qual[id] = &QualRef{Pkg: x, Sel: s.Sel, Path: dep.path}
	return id, true
}

func (rw *fileRW) walk(v reflect.Value, ctx walkCtx) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		if s, ok := v.Interface().(*ast.SelectorExpr); ok && v.Type().Implements(exprIface) {
			if id, ok := rw.qualified(s); ok {
				v.Set(reflect.ValueOf(id))
				return
			}
		}
		e := v.Elem()
		rw.walk(e, ctx)
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			rw.walk(v.Index(i), ctx)
		}
	case reflect.Ptr:
		if v.IsNil() {
			return
		}
		rw.walkNode(v, ctx)
	}
}

func (rw *fileRW) walkNode(v reflect.Value, ctx walkCtx) {
	switch n := v.Interface().(type) {
	case *ast.Ident:
		rw.rename(n)
		return
	case *ast.SelectorExpr:
		rw.walk(reflect.ValueOf(&n.X).Elem(), ctxNone)
		return
	case *ast.StructType, *ast.InterfaceType:
		rw.walkFields(v.Elem(), ctxFields)
		return
	case *ast.Field:
		if ctx != ctxFields {
			for _, id := range n.Names {
				rw.rename(id)
			}
		}
		rw.walk(reflect.ValueOf(&n.Type).Elem(), ctxNone)
		rw.walk(reflect.ValueOf(&n.Annots).Elem(), ctxNone)
		return
	case *ast.FieldList:
		for _, f := range n.List {
			rw.walkNode(reflect.ValueOf(f), ctx)
		}
		return
	case *ast.FuncDecl:
		if n.Recv == nil {
			rw.rename(n.Name)
		} else {
			rw.walkNode(reflect.ValueOf(n.Recv), ctxNone)
		}
		rw.walk(reflect.ValueOf(&n.Annots).Elem(), ctxNone)
		rw.walkNode(reflect.ValueOf(n.Type), ctxNone)
		if n.Body != nil {
			rw.walkNode(reflect.ValueOf(n.Body), ctxNone)
		}
		return
	case *ast.BranchStmt:
		return // labels
	case *ast.CompositeLit:
		rw.walk(reflect.ValueOf(&n.Type).Elem(), ctxNone)
		structKeys := rw.isStructLit(n.Type)
		for i := range n.Elts {
			if kv, ok := n.Elts[i].(*ast.KeyValueExpr); ok && structKeys {
				if _, ok := kv.Key.(*ast.Ident); ok {
					rw.walk(reflect.ValueOf(&kv.Value).Elem(), ctxNone)
					continue
				}
			}
			rw.walk(reflect.ValueOf(&n.Elts[i]).Elem(), ctxNone)
		}
		return
	}
	rw.walkFields(v.Elem(), ctx)
}

func (rw *fileRW) walkFields(s reflect.Value, ctx walkCtx) {
	if s.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < s.NumField(); i++ {
		f := s.Field(i)
		if f.Type() == posT || !f.CanSet() {
			continue
		}
		rw.walk(f, ctx)
	}
}

// isStructLit reports whether keys of a composite literal of type t are
// field names. Literals with an elided type are element literals of an outer
// composite; they are treated as struct literals unless the type is known.
func (rw *fileRW) isStructLit(t ast.Expr) bool {
	switch t := t.(type) {
	case nil:
		return true
	case *ast.StructType:
		return true
	case *ast.Ident: // already renamed
		units := []*unit{rw.u}
		for _, dep := range rw.imports {
			units = append(units, dep)
		}
		for _, u := range units {
			if u.prefix == "" {
				if u.structs[t.Name] {
					return true
				}
				if u.top[t.Name] {
					return false
				}
			} else if strings.HasPrefix(t.Name, u.prefix) {
				return u.structs[strings.TrimPrefix(t.Name, u.prefix)]
			}
		}
		return !isKnownNonStruct(t.Name)
	case *ast.IndexExpr: // generic instance
		return rw.isStructLit(t.X)
	}
	return false
}

func isKnownNonStruct(name string) bool { return types.Universe.Lookup(name) != nil }

// checkCrossAccess reports selections of unexported fields and methods of
// types declared in another package.
func (p *Program) checkCrossAccess(info *types.Info) []string {
	if len(p.Units) < 2 {
		return nil
	}
	var errs []string
	for sel := range info.Selections {
		name := sel.Sel.Name
		if exported(name) {
			continue
		}
		tv, ok := info.Types[sel.X]
		if !ok || tv.Type == nil {
			continue
		}
		t := tv.Type
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem
		}
		n, ok := t.(*types.Named)
		if !ok {
			continue
		}
		owner := p.ownerOf(n.Obj.Name())
		here := p.fileUnit[sel.Sel.NamePos.File]
		if owner != nil && here != nil && owner != here {
			errs = append(errs, fmt.Sprintf("%s: %s.%s undefined (cannot refer to unexported field or method %s of package %s)",
				sel.Sel.NamePos, p.Demangle(n.String()), name, name, owner.display))
		}
	}
	sort.Strings(errs)
	return errs
}

func (p *Program) ownerOf(name string) *unit {
	var best, target *unit
	for _, u := range p.Units {
		if u.target {
			target = u
			continue
		}
		if strings.HasPrefix(name, u.prefix) && (best == nil || len(u.prefix) > len(best.prefix)) {
			best = u
		}
	}
	if best != nil {
		return best
	}
	return target
}
