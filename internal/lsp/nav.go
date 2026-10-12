package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/gobridge"
	"github.com/rusfort/gocpy/internal/parser"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

type defLoc struct {
	file      string
	line, col int
	n         int
}

// occAt finds the occurrence under the cursor (also just after its end).
func (s *server) occAt(p TextDocumentPositionParams) (*pkgAnalysis, *occ, string, int, int) {
	path, line, col := s.docPos(p)
	pa := s.analysisFor(path)
	o := pa.at(path, line, col)
	return pa, o, path, line, col
}

func (s *server) demangle(pa *pkgAnalysis, str string) string {
	if pa != nil && pa.a != nil && pa.a.Prog != nil {
		return pa.a.Prog.Demangle(str)
	}
	return str
}

// definitionOf returns where the entity of an occurrence is declared.
func (s *server) definitionOf(pa *pkgAnalysis, o *occ) (defLoc, bool) {
	var p token.Pos
	switch {
	case o.field != nil:
		p = o.field.Pos
	case o.pkgPath != "" && (o.cls == "pkgName" || o.cls == "pkgDecl"):
		return s.packageLoc(pa, o.pkgPath)
	case o.obj != nil:
		p = o.obj.Pos()
	}
	if p.File == "" || p.Line == 0 {
		return defLoc{}, false
	}
	d := defLoc{file: p.File, line: p.Line, col: p.Column}
	t := s.texts(d.file)
	d.n = t.wordAt(d.line, d.col)
	if d.n == 0 {
		d.n = max(1, t.opAt(d.line, d.col))
	}
	return s.goSource(d), true
}

// goSource maps a declaration in a Go bridge stub to the Go source (the stub
// has a "// go:pos file:line:col" comment above each declaration).
func (s *server) goSource(d defLoc) defLoc {
	if !strings.Contains(filepath.ToSlash(d.file), "gobridge") {
		return d
	}
	t := s.texts(d.file)
	for l := d.line - 1; l >= 1 && l >= d.line-3; l-- {
		line := strings.TrimSpace(t.line(l))
		if !strings.HasPrefix(line, gobridge.GoPosPrefix) {
			continue
		}
		ref := strings.TrimPrefix(line, gobridge.GoPosPrefix)
		i := strings.LastIndexByte(ref, ':')
		if i < 0 {
			break
		}
		j := strings.LastIndexByte(ref[:i], ':')
		if j < 0 {
			break
		}
		ln, err1 := strconv.Atoi(ref[j+1 : i])
		cl, err2 := strconv.Atoi(ref[i+1:])
		if err1 != nil || err2 != nil {
			break
		}
		file := ref[:j]
		if _, err := os.Stat(file); err != nil {
			break
		}
		gt := s.texts(file)
		n := gt.wordAt(ln, cl)
		// "func Name" / "const Name": point at the name
		lt := gt.line(ln)
		name := s.texts(d.file).line(d.line)
		_ = name
		if rs := []rune(lt); cl-1 < len(rs) {
			rest := string(rs[cl-1:])
			for _, kw := range []string{"func ", "const ", "var ", "type "} {
				if strings.HasPrefix(rest, kw) {
					cl += len(kw)
					rest = rest[len(kw):]
					if strings.HasPrefix(rest, "(") { // method receiver
						if k := strings.Index(rest, ") "); k >= 0 {
							cl += utf8.RuneCountInString(rest[:k+2])
						}
					}
					n = gt.wordAt(ln, cl)
					break
				}
			}
		}
		return defLoc{file: file, line: ln, col: cl, n: max(n, 1)}
	}
	return d
}

func (s *server) packageLoc(pa *pkgAnalysis, path string) (defLoc, bool) {
	if pa.a == nil || pa.a.Prog == nil {
		return defLoc{}, false
	}
	pk := pa.a.Prog.PackageByPath(path)
	if pk == nil || len(pk.Files) == 0 {
		return defLoc{}, false
	}
	files := append([]*ast.File{}, pk.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Package.File < files[j].Package.File })
	f := files[0]
	d := defLoc{file: f.Package.File, line: f.Package.Line, col: f.Package.Column, n: 7}
	if f.Name != nil {
		d.line, d.col = f.Name.NamePos.Line, f.Name.NamePos.Column
		d.n = utf8.RuneCountInString(f.Name.Name)
	}
	return d, true
}

// ---------------------------------------------------------------- definition

func (s *server) definition(p TextDocumentPositionParams, typeDef bool) any {
	pa, o, path, line, col := s.occAt(p)
	if o == nil {
		for _, im := range pa.imports {
			if im.file == path && im.line == line && col >= im.col && col <= im.col+im.n {
				if d, ok := s.packageLoc(pa, im.path); ok {
					return s.loc(d.file, d.line, d.col, d.n)
				}
			}
		}
		return nil
	}
	if typeDef && o.obj != nil {
		t := o.obj.Type()
		if o.field != nil {
			t = o.field.Type
		}
		if pt, ok := t.(*types.Pointer); ok {
			t = pt.Elem
		}
		if n, ok := t.(*types.Named); ok && n.Obj != nil && n.Obj.Pos().File != "" {
			pp := n.Obj.Pos()
			return s.loc(pp.File, pp.Line, pp.Column, s.texts(pp.File).wordAt(pp.Line, pp.Column))
		}
		return nil
	}
	d, ok := s.definitionOf(pa, o)
	if !ok {
		return nil
	}
	return s.loc(d.file, d.line, d.col, d.n)
}

// ---------------------------------------------------------------- hover

func (s *server) hover(p TextDocumentPositionParams) any {
	pa, o, path, line, col := s.occAt(p)
	t := s.texts(path)
	var md string
	var rng *Range
	if o != nil {
		md = s.describe(pa, o)
		r := t.span(o.line, o.col, o.n)
		rng = &r
	} else {
		w, c := wordAround(t.line(line), col)
		if w == "" {
			return nil
		}
		lt := []rune(t.line(line))
		switch {
		case c >= 2 && lt[c-2] == '@':
			if d, ok := annotDocs[w]; ok {
				md = "```gocpy\n@" + w + "\n```\n" + d.doc
			}
		case keywordDocs[w].doc != "":
			md = "```gocpy\n" + w + "\n```\n" + keywordDocs[w].doc
		case builtinDocs[w] != "":
			md = builtinDocs[w]
		default:
			for _, im := range pa.imports {
				if im.file == path && im.line == line && col >= im.col && col <= im.col+im.n {
					md = s.describeImport(pa, im.path)
				}
			}
		}
		if md == "" {
			return nil
		}
		r := t.span(line, c, utf8.RuneCountInString(w))
		rng = &r
	}
	if md == "" {
		return nil
	}
	return map[string]any{"contents": MarkupContent{Kind: "markdown", Value: md}, "range": rng}
}

func (s *server) describeImport(pa *pkgAnalysis, path string) string {
	if strings.HasPrefix(path, "std/") {
		return fmt.Sprintf("```gocpy\nimport %q\n```\nСтандартная библиотека Gocpy.", path)
	}
	if pa.a != nil && pa.a.Prog != nil {
		if pk := pa.a.Prog.PackageByPath(path); pk != nil {
			kind := "пакет Gocpy"
			if pk.IsGo {
				kind = "пакет Go (через мост)"
			}
			md := fmt.Sprintf("```gocpy\npackage %s // %q\n```\n%s", pk.Name, path, kind)
			if pk.ModPath != "" {
				md += ", модуль `" + pk.ModPath + "`"
			}
			if !pk.IsGo {
				md += "\n\n" + pk.Dir
			}
			return md
		}
	}
	return fmt.Sprintf("```gocpy\nimport %q\n```", path)
}

func wordAround(line string, col int) (string, int) {
	rs := []rune(line)
	i := col - 1
	if i > len(rs) {
		i = len(rs)
	}
	if i < 0 {
		i = 0
	}
	st := i
	for st > 0 && isIdentRune(rs[st-1]) {
		st--
	}
	en := i
	for en < len(rs) && isIdentRune(rs[en]) {
		en++
	}
	if st == en {
		return "", 0
	}
	return string(rs[st:en]), st + 1
}

func (s *server) typeStr(pa *pkgAnalysis, t types.Type) string {
	if t == nil {
		return ""
	}
	return s.demangle(pa, t.String())
}

func (s *server) describe(pa *pkgAnalysis, o *occ) string {
	var code, doc, extra string
	if d, ok := s.definitionOf(pa, o); ok {
		doc = s.docComment(d)
		if strings.HasSuffix(d.file, ".go") {
			extra = fmt.Sprintf("Определено в Go: `%s:%d`", filepath.Base(d.file), d.line)
		}
	}
	name := s.texts(o.file).line(o.line)
	if rs := []rune(name); o.col-1+o.n <= len(rs) {
		name = string(rs[o.col-1 : o.col-1+o.n])
	}
	switch {
	case o.field != nil:
		code = fmt.Sprintf("field %s %s", o.field.Name, s.typeStr(pa, o.field.Type))
		if o.fieldOf != nil {
			extra = "поле типа `" + s.typeStr(pa, deref(o.fieldOf)) + "`"
		}
	case o.cls == "pkgName" || o.cls == "pkgDecl":
		return s.describeImport(pa, o.pkgPath)
	case o.cls == "fieldDecl":
		code = "field " + name
	case o.obj != nil:
		code = s.objString(pa, o.obj, name)
	default:
		if d := builtinDocs[name]; d != "" {
			return d
		}
		return ""
	}
	md := "```gocpy\n" + code + "\n```"
	if doc != "" {
		md += "\n" + doc
	}
	if extra != "" {
		md += "\n\n" + extra
	}
	return md
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem
	}
	return t
}

func (s *server) objString(pa *pkgAnalysis, obj types.Object, name string) string {
	switch o := obj.(type) {
	case *types.Var:
		if o.IsConst {
			v := ""
			if o.Value != nil {
				v = " = " + o.Value.ExactString()
			}
			return fmt.Sprintf("const %s %s%s", name, s.typeStr(pa, o.Type()), v)
		}
		return fmt.Sprintf("var %s %s", name, s.typeStr(pa, o.Type()))
	case *types.Func:
		if o.Decl != nil && o.Pos().File != "" {
			if src := s.declSource(o.Pos().File, o.Decl); src != "" {
				return src
			}
		}
		sig := strings.TrimPrefix(s.typeStr(pa, o.Type()), "func")
		if o.RecvNamed != nil {
			return fmt.Sprintf("func (%s) %s%s", s.typeStr(pa, o.RecvNamed), name, sig)
		}
		return "func " + name + sig
	case *types.GenericFunc:
		if o.Decl != nil {
			if src := s.declSource(o.Pos().File, o.Decl); src != "" {
				return src
			}
		}
		return "func " + name
	case *types.FuncSet:
		var lines []string
		for _, f := range o.Funcs {
			lines = append(lines, s.objString(pa, f, name))
		}
		return strings.Join(lines, "\n")
	case *types.TypeName:
		if p := o.Pos(); p.File != "" {
			return s.typeSource(p)
		}
		if n, ok := o.Type().(*types.Named); ok && n.Pkg != "" {
			return fmt.Sprintf("type %s.%s", n.Pkg, o.Name())
		}
		if d := builtinDocs[o.Name()]; d != "" {
			return "type " + o.Name() + "\n```\n" + d + "\n```gocpy"
		}
		return "type " + o.Name()
	case *types.GenericType:
		return s.typeSource(o.Pos())
	case *types.PkgName:
		return fmt.Sprintf("package %s // %q", name, o.Path)
	case *types.Builtin:
		if d := builtinDocs[name]; d != "" {
			return name + "\n```\n" + d + "\n```gocpy"
		}
		return "func " + name
	case *types.Nil:
		return "nil"
	}
	return name
}

// declSource is the source text of a function header.
func (s *server) declSource(file string, d *ast.FuncDecl) string {
	t := s.texts(file)
	start := t.offset(d.Type.Func.Line, d.Type.Func.Column)
	end := len(t.src)
	if d.Body != nil {
		end = t.offset(d.Body.Lbrace.Line, d.Body.Lbrace.Column)
	} else if i := strings.IndexByte(t.src[start:], '\n'); i >= 0 {
		end = start + i
	}
	if start >= end || end > len(t.src) {
		return ""
	}
	src := strings.TrimSpace(t.src[start:end])
	var annots []string
	for _, a := range d.Annots {
		annots = append(annots, strings.TrimSpace(t.line(a.At.Line)))
	}
	if len(annots) > 0 {
		src = strings.Join(dedupe(annots), "\n") + "\n" + src
	}
	return src
}

func dedupe(xs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func (s *server) typeSource(p token.Pos) string {
	t := s.texts(p.File)
	rs := []rune(t.line(p.Line))
	if p.Column-1 > len(rs) {
		return ""
	}
	first := "type " + strings.TrimSpace(string(rs[p.Column-1:]))
	if !strings.HasSuffix(first, "{") {
		return first
	}
	lines := []string{first}
	for l := p.Line + 1; l <= len(t.lines) && l < p.Line+30; l++ {
		lt := t.line(l)
		lines = append(lines, lt)
		if strings.HasPrefix(strings.TrimSpace(lt), "}") && leadingWS(lt) <= leadingWS(t.line(p.Line)) {
			break
		}
	}
	return strings.Join(lines, "\n")
}

func leadingWS(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// docComment collects the // comments directly above a declaration (and a
// trailing comment on the same line).
func (s *server) docComment(d defLoc) string {
	t := s.texts(d.file)
	var lines []string
	for l := d.line - 1; l >= 1; l-- {
		lt := strings.TrimSpace(t.line(l))
		if strings.HasPrefix(lt, "@") || strings.HasPrefix(lt, gobridge.GoPosPrefix) || strings.HasPrefix(lt, "//gocpy:") {
			continue
		}
		if !strings.HasPrefix(lt, "//") {
			break
		}
		lines = append([]string{strings.TrimPrefix(strings.TrimPrefix(lt, "//"), " ")}, lines...)
	}
	if lt := t.line(d.line); strings.Contains(lt, "//") && len(lines) == 0 {
		if i := strings.Index(lt, "//"); i >= 0 && !strings.Contains(lt[:i], "\"") {
			lines = append(lines, strings.TrimSpace(lt[i+2:]))
		}
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- references

// searchSet returns the analyses that may reference key.
func (s *server) searchSet(pa *pkgAnalysis, o *occ) []*pkgAnalysis {
	out := []*pkgAnalysis{pa}
	name := ""
	if o.obj != nil {
		name = o.obj.Name()
	} else if o.field != nil {
		name = o.field.Name
	}
	if pa.target != nil {
		name = strings.TrimPrefix(name, pa.target.Prefix)
	}
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	local := o.obj != nil && !isUpper(name) && !strings.HasPrefix(o.key, "pkg:") && !strings.HasPrefix(o.key, "std:")
	if o.field != nil {
		local = !isUpper(name)
	}
	if v, ok := o.obj.(*types.Var); ok && !v.Global {
		local = true
	}
	if local {
		return out
	}
	for _, dir := range s.workspacePackages() {
		if dir != pa.dir {
			out = append(out, s.analyze(dir))
		}
	}
	return out
}

func (s *server) refsOf(pa *pkgAnalysis, o *occ, includeDecl bool) []*occ {
	if o.key == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []*occ
	for _, a := range s.searchSet(pa, o) {
		for _, r := range a.byKey[o.key] {
			if r.def && !includeDecl {
				continue
			}
			k := r.file + ":" + itoa(r.line) + ":" + itoa(r.col)
			if !seen[k] {
				seen[k] = true
				out = append(out, r)
			}
		}
	}
	// the declaration may be outside the analysed packages (module cache)
	if includeDecl {
		if d, ok := s.definitionOf(pa, o); ok {
			k := d.file + ":" + itoa(d.line) + ":" + itoa(d.col)
			if !seen[k] && strings.HasSuffix(d.file, ".gocpy") {
				out = append(out, &occ{file: d.file, line: d.line, col: d.col, n: d.n, def: true, key: o.key})
			}
		}
	}
	return out
}

func (s *server) references(p TextDocumentPositionParams, includeDecl bool) any {
	pa, o, _, _, _ := s.occAt(p)
	if o == nil {
		return []Location{}
	}
	out := []Location{}
	for _, r := range s.refsOf(pa, o, includeDecl) {
		out = append(out, s.loc(r.file, r.line, r.col, r.n))
	}
	return out
}

func (s *server) highlight(p TextDocumentPositionParams) any {
	pa, o, path, _, _ := s.occAt(p)
	out := []map[string]any{}
	if o == nil || o.key == "" {
		return out
	}
	t := s.texts(path)
	for _, r := range pa.byKey[o.key] {
		if r.file != path {
			continue
		}
		kind := hlRead
		if r.def {
			kind = hlWrite
		}
		out = append(out, map[string]any{"range": t.span(r.line, r.col, r.n), "kind": kind})
	}
	return out
}

// ---------------------------------------------------------------- rename

type rpcErr struct{ msg string }

func (e *rpcErr) Error() string { return e.msg }

func (s *server) renameTarget(p TextDocumentPositionParams) (*pkgAnalysis, *occ, error) {
	pa, o, _, _, _ := s.occAt(p)
	if o == nil || o.key == "" {
		return nil, nil, &rpcErr{"здесь нечего переименовывать"}
	}
	for _, pre := range []string{"builtin:", "std:", "pkg:", "pkgdecl:", "iface:"} {
		if strings.HasPrefix(o.key, pre) {
			return nil, nil, &rpcErr{"это имя нельзя переименовать (встроенное, стандартная библиотека или пакет)"}
		}
	}
	d, ok := s.definitionOf(pa, o)
	if !ok || !strings.HasSuffix(d.file, ".gocpy") || !s.inWorkspace(d.file) {
		return nil, nil, &rpcErr{"объявление находится вне рабочей области"}
	}
	return pa, o, nil
}

func (s *server) prepareRename(p TextDocumentPositionParams) (any, error) {
	_, o, err := s.renameTarget(p)
	if err != nil {
		return nil, err
	}
	return s.texts(o.file).span(o.line, o.col, o.n), nil
}

func (s *server) rename(p TextDocumentPositionParams, newName string) (any, error) {
	pa, o, err := s.renameTarget(p)
	if err != nil {
		return nil, err
	}
	if !validIdent(newName) {
		return nil, &rpcErr{fmt.Sprintf("%q — недопустимое имя", newName)}
	}
	changes := map[string][]TextEdit{}
	for _, r := range s.refsOf(pa, o, true) {
		if !strings.HasSuffix(r.file, ".gocpy") {
			continue
		}
		uri := pathToURI(r.file)
		changes[uri] = append(changes[uri], TextEdit{Range: s.texts(r.file).span(r.line, r.col, r.n), NewText: newName})
	}
	return map[string]any{"changes": changes}, nil
}

func validIdent(s string) bool {
	if s == "" || token.Lookup(s) != token.IDENT {
		return false
	}
	for i, r := range s {
		if !isIdentRune(r) || (i == 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- symbols

func (s *server) fileAST(path string) *ast.File {
	if pa := s.analysisFor(path); pa.files[path] != nil {
		return pa.files[path]
	}
	t := s.texts(path)
	f, _ := safeParse(path, t.src)
	return f
}

func safeParse(path, src string) (f *ast.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, fmt.Errorf("parser panic: %v", r)
		}
	}()
	f, err = parser.ParseFile(path, []byte(src))
	if f != nil && f.Name == nil {
		f = nil
	}
	return
}

func (s *server) documentSymbols(path string) any {
	out := []DocumentSymbol{}
	f := s.fileAST(path)
	if f == nil {
		return out
	}
	t := s.texts(path)
	pfx := ""
	if pa := s.analysisFor(path); pa.target != nil {
		pfx = pa.target.Prefix
	}
	name := func(n string) string { return strings.TrimPrefix(n, pfx) }
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			nl := t.wordAt(d.Name.NamePos.Line, d.Name.NamePos.Column)
			kind := skFunction
			if d.IsOperator {
				nl = t.opAt(d.Name.NamePos.Line, d.Name.NamePos.Column)
				kind = skOperator
			}
			sym := DocumentSymbol{Name: nameText(t, d.Name.NamePos, nl), Kind: kind}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sym.Kind = skMethod
				if d.IsOperator {
					sym.Kind = skOperator
				}
				sym.Name = "(" + name(recvTypeName(d.Recv.List[0].Type)) + ") " + sym.Name
			}
			start := d.Type.Func
			if len(d.Annots) > 0 {
				start = d.Annots[0].At
			}
			endL, endC := d.Name.NamePos.Line, 1<<20
			if d.Body != nil {
				if l, c, ok := t.matchBrace(d.Body.Lbrace.Line, d.Body.Lbrace.Column); ok {
					endL, endC = l, c+1
				}
			}
			sym.Range = Range{Start: t.position(start.Line, start.Column), End: t.position(endL, min(endC, utf8.RuneCountInString(t.line(endL))+1))}
			sym.SelectionRange = t.span(d.Name.NamePos.Line, d.Name.NamePos.Column, nl)
			sym.Detail = strings.TrimSpace(strings.SplitN(s.declSource(path, d), "\n", 8)[len(d.Annots)])
			out = append(out, sym)
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					kind := skClass
					var children []DocumentSymbol
					switch tt := sp.Type.(type) {
					case *ast.StructType:
						kind = skStruct
						if tt.Fields != nil {
							for _, fl := range tt.Fields.List {
								for _, id := range fl.Names {
									n := t.wordAt(id.NamePos.Line, id.NamePos.Column)
									r := t.span(id.NamePos.Line, id.NamePos.Column, n)
									children = append(children, DocumentSymbol{Name: id.Name, Kind: skField, Range: lineRange(t, id.NamePos.Line), SelectionRange: r})
								}
							}
						}
					case *ast.InterfaceType:
						kind = skInterface
						if tt.Methods != nil {
							for _, fl := range tt.Methods.List {
								for _, id := range fl.Names {
									n := t.wordAt(id.NamePos.Line, id.NamePos.Column)
									children = append(children, DocumentSymbol{Name: id.Name, Kind: skMethod, Range: lineRange(t, id.NamePos.Line),
										SelectionRange: t.span(id.NamePos.Line, id.NamePos.Column, n)})
								}
							}
						}
					}
					n := t.wordAt(sp.Name.NamePos.Line, sp.Name.NamePos.Column)
					end := sp.Name.NamePos.Line
					if l := typeEndLine(t, sp.Type); l > end {
						end = l
					}
					out = append(out, DocumentSymbol{Name: name(nameText(t, sp.Name.NamePos, n)), Kind: kind,
						Range:          Range{Start: t.position(sp.Name.NamePos.Line, 1), End: t.position(end, utf8.RuneCountInString(t.line(end))+1)},
						SelectionRange: t.span(sp.Name.NamePos.Line, sp.Name.NamePos.Column, n), Children: children})
				case *ast.ValueSpec:
					kind := skVariable
					if d.Tok == token.CONST {
						kind = skConstant
					}
					for _, id := range sp.Names {
						n := t.wordAt(id.NamePos.Line, id.NamePos.Column)
						out = append(out, DocumentSymbol{Name: nameText(t, id.NamePos, n), Kind: kind, Range: lineRange(t, id.NamePos.Line),
							SelectionRange: t.span(id.NamePos.Line, id.NamePos.Column, n)})
					}
				}
			}
		}
	}
	return out
}

func nameText(t *text, p token.Pos, n int) string {
	rs := []rune(t.line(p.Line))
	if p.Column-1+n <= len(rs) && n > 0 {
		return string(rs[p.Column-1 : p.Column-1+n])
	}
	return "?"
}

func lineRange(t *text, line int) Range {
	return Range{Start: t.position(line, 1), End: t.position(line, utf8.RuneCountInString(t.line(line))+1)}
}

func (s *server) workspaceSymbols(query string) any {
	q := strings.ToLower(query)
	out := []SymbolInformation{}
	for _, dir := range s.workspacePackages() {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".gocpy") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			syms, _ := s.documentSymbols(path).([]DocumentSymbol)
			var walk func(ds []DocumentSymbol, container string)
			walk = func(ds []DocumentSymbol, container string) {
				for _, d := range ds {
					if q == "" || strings.Contains(strings.ToLower(d.Name), q) {
						out = append(out, SymbolInformation{Name: d.Name, Kind: d.Kind, ContainerName: container,
							Location: Location{URI: pathToURI(path), Range: d.SelectionRange}})
					}
					walk(d.Children, d.Name)
				}
			}
			walk(syms, filepath.Base(dir))
			if len(out) > 500 {
				return out
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- call hierarchy

func isFuncObj(o types.Object) bool {
	switch o.(type) {
	case *types.Func, *types.GenericFunc, *types.FuncSet:
		return true
	}
	return false
}

func (s *server) itemFor(file string, line, col int, key string) *CallHierarchyItem {
	pa := s.analysisFor(file)
	t := s.texts(file)
	for _, fr := range pa.funcs[file] {
		if fr.nameLine == line && fr.nameCol == col {
			name := nameText(t, token.Pos{Line: line, Column: col}, fr.nameLen)
			kind := skFunction
			detail := ""
			if pa.target != nil {
				detail = pa.target.Name
			}
			if fr.isMethod {
				kind = skMethod
				rn := fr.recvString
				if pa.target != nil {
					rn = strings.TrimPrefix(rn, pa.target.Prefix)
				}
				name = rn + "." + name
			}
			endC := fr.eCol + 1
			if fr.eCol == 1<<30 {
				endC = utf8.RuneCountInString(t.line(fr.eLine)) + 1
			}
			return &CallHierarchyItem{Name: name, Kind: kind, Detail: detail, URI: pathToURI(file),
				Range:          Range{Start: t.position(fr.sLine, fr.sCol), End: t.position(fr.eLine, endC)},
				SelectionRange: t.span(line, col, fr.nameLen),
				Data:           map[string]any{"key": key, "file": file, "line": line, "col": col}}
		}
	}
	return nil
}

func (s *server) prepareCallHierarchy(p TextDocumentPositionParams) any {
	pa, o, _, _, _ := s.occAt(p)
	if o == nil || !isFuncObj(o.obj) {
		return nil
	}
	d, ok := s.definitionOf(pa, o)
	if !ok || !strings.HasSuffix(d.file, ".gocpy") {
		return nil
	}
	it := s.itemFor(d.file, d.line, d.col, o.key)
	if it == nil {
		return nil
	}
	return []CallHierarchyItem{*it}
}

func itemData(it CallHierarchyItem) (key, file string, line, col int) {
	m, _ := it.Data.(map[string]any)
	key, _ = m["key"].(string)
	file, _ = m["file"].(string)
	l, _ := m["line"].(float64)
	c, _ := m["col"].(float64)
	return key, file, int(l), int(c)
}

func (s *server) incomingCalls(it CallHierarchyItem) any {
	key, file, line, col := itemData(it)
	out := []map[string]any{}
	pa := s.analysisFor(file)
	var o *occ
	for _, x := range pa.byKey[key] {
		if x.file == file && x.line == line && x.col == col {
			o = x
		}
	}
	if o == nil {
		o = &occ{key: key, file: file, line: line, col: col}
		if objs := pa.objs[key]; objs != nil {
			o.obj = objs
		}
	}
	type group struct {
		item   *CallHierarchyItem
		ranges []Range
	}
	groups := map[string]*group{}
	var order []string
	for _, a := range s.searchSet(pa, o) {
		for _, r := range a.byKey[key] {
			if !r.call {
				continue
			}
			fr := a.enclosingFunc(r.file, r.line, r.col)
			if fr == nil {
				continue
			}
			gk := r.file + ":" + itoa(fr.nameLine) + ":" + itoa(fr.nameCol)
			g := groups[gk]
			if g == nil {
				itm := s.itemFor(r.file, fr.nameLine, fr.nameCol, fr.key)
				if itm == nil {
					continue
				}
				g = &group{item: itm}
				groups[gk] = g
				order = append(order, gk)
			}
			g.ranges = append(g.ranges, s.texts(r.file).span(r.line, r.col, r.n))
		}
	}
	for _, k := range order {
		out = append(out, map[string]any{"from": groups[k].item, "fromRanges": groups[k].ranges})
	}
	return out
}

func (s *server) outgoingCalls(it CallHierarchyItem) any {
	_, file, line, col := itemData(it)
	out := []map[string]any{}
	pa := s.analysisFor(file)
	var fr *funcRange
	for _, f := range pa.funcs[file] {
		if f.nameLine == line && f.nameCol == col {
			fr = f
		}
	}
	if fr == nil {
		return out
	}
	rg := lrange{fr.sLine, fr.sCol, fr.eLine, fr.eCol}
	type group struct {
		item   *CallHierarchyItem
		ranges []Range
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range pa.byFile[file] {
		if !r.call || r.key == "" || !rg.contains(r.line, r.col) || !isFuncObj(r.obj) {
			continue
		}
		d, ok := s.definitionOf(pa, r)
		if !ok || !strings.HasSuffix(d.file, ".gocpy") {
			continue
		}
		g := groups[r.key]
		if g == nil {
			itm := s.itemFor(d.file, d.line, d.col, r.key)
			if itm == nil {
				continue
			}
			g = &group{item: itm}
			groups[r.key] = g
			order = append(order, r.key)
		}
		g.ranges = append(g.ranges, s.texts(file).span(r.line, r.col, r.n))
	}
	for _, k := range order {
		out = append(out, map[string]any{"to": groups[k].item, "fromRanges": groups[k].ranges})
	}
	return out
}
