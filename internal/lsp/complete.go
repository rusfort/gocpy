package lsp

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/build"
	"github.com/rusfort/gocpy/internal/gobridge"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
	"github.com/rusfort/gocpy/stdlib"
)

var (
	reImportLine  = regexp.MustCompile(`^\s*import\s+(?:[A-Za-z_]\w*\s+)?"([^"]*)$`)
	reImportBlock = regexp.MustCompile(`^\s*(?:[A-Za-z_]\w*\s+)?"([^"]*)$`)
	reAnnot       = regexp.MustCompile(`@([A-Za-z_]\w*)?$`)
	reWord        = regexp.MustCompile(`[\p{L}_][\p{L}\p{N}_]*$`)
)

// lineContext classifies the text before the cursor.
func lineContext(before string) (inComment, inString bool) {
	inStr := byte(0)
	for i := 0; i < len(before); i++ {
		c := before[i]
		switch {
		case inStr != 0:
			if c == '\\' && inStr != '`' {
				i++
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'' || c == '`':
			inStr = c
		case c == '/' && i+1 < len(before) && before[i+1] == '/':
			return true, false
		}
	}
	return false, inStr != 0
}

func (s *server) completion(p TextDocumentPositionParams) any {
	path, line, col := s.docPos(p)
	t := s.texts(path)
	lt := []rune(t.line(line))
	if col-1 > len(lt) {
		col = len(lt) + 1
	}
	before := string(lt[:col-1])
	items := []CompletionItem{}
	list := func() any { return map[string]any{"isIncomplete": false, "items": items} }

	// import paths
	if m := reImportLine.FindStringSubmatch(before); m != nil || (s.inImportBlock(t, line) && reImportBlock.MatchString(before)) {
		return map[string]any{"isIncomplete": false, "items": s.importItems(path)}
	}
	inComment, inString := lineContext(before)
	if inComment || inString {
		if inComment && strings.HasSuffix(before, "//gocpy:") || strings.HasSuffix(before, "//") && inComment {
			for d, doc := range directiveDocs {
				items = append(items, CompletionItem{Label: d, Kind: ckKeyword, Documentation: md(doc), InsertText: strings.TrimPrefix(d, "gocpy:")})
				if strings.HasSuffix(before, "//") {
					items[len(items)-1].InsertText = d
				}
			}
		}
		return list()
	}
	// annotations and intrinsics
	if m := reAnnot.FindStringSubmatchIndex(before); m != nil {
		for name, d := range annotDocs {
			it := CompletionItem{Label: "@" + name, FilterText: name, Kind: ckKeyword, Documentation: md(d.doc), InsertText: name}
			if d.snippet != "" {
				it.InsertText, it.InsertTextFormat = d.snippet, formatSnippet
			}
			items = append(items, it)
		}
		return list()
	}
	word := reWord.FindString(before)
	rest := strings.TrimSuffix(before, word)
	pa := s.bestFor(path)
	if strings.HasSuffix(rest, ".") {
		chain := parseChain(strings.TrimSuffix(rest, "."))
		if len(chain) > 0 {
			items = s.memberItems(pa, path, line, chain)
		}
		return list()
	}
	items = s.scopeItems(pa, path, line, col)
	return list()
}

func md(s string) *MarkupContent { return &MarkupContent{Kind: "markdown", Value: s} }

func (s *server) inImportBlock(t *text, line int) bool {
	for l := line - 1; l >= 1 && l > line-200; l-- {
		lt := strings.TrimSpace(t.line(l))
		if strings.HasPrefix(lt, "import (") {
			return true
		}
		if strings.HasPrefix(lt, ")") || strings.HasPrefix(lt, "func ") || strings.HasPrefix(lt, "type ") {
			return false
		}
	}
	return false
}

func (s *server) importItems(path string) []CompletionItem {
	var items []CompletionItem
	seen := map[string]bool{}
	add := func(p, detail string, kind int) {
		if !seen[p] {
			seen[p] = true
			items = append(items, CompletionItem{Label: p, Kind: kind, Detail: detail})
		}
	}
	var std []string
	for p := range types.StdPackages {
		std = append(std, p)
	}
	std = append(std, stdlib.Packages()...)
	sort.Strings(std)
	for _, p := range std {
		add(p, "standard library", ckModule)
	}
	if ents, err := os.ReadDir(filepath.Dir(path)); err == nil {
		for _, e := range ents {
			if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				add("./"+n, "Go file of this directory", ckFile)
			}
		}
	}
	pa := s.analysisFor(path)
	if pa.a != nil && pa.a.Prog != nil {
		prog := pa.a.Prog
		if cfg := prog.Config; cfg != nil {
			for _, r := range cfg.Require.Gocpy {
				add(r.Path, "Gocpy "+r.Version, ckModule)
			}
			for _, r := range cfg.Require.Go {
				add(r.Path, "Go "+r.Version, ckModule)
			}
			// packages of the main module
			if cfg.Module != "" && prog.Root != "" {
				filepath.WalkDir(prog.Root, func(p string, d os.DirEntry, err error) error {
					if err != nil || !d.IsDir() {
						return nil
					}
					if n := d.Name(); p != prog.Root && (strings.HasPrefix(n, ".") || n == "node_modules") {
						return filepath.SkipDir
					}
					if p == filepath.Dir(path) {
						return nil
					}
					if m, _ := filepath.Glob(filepath.Join(p, "*.gocpy")); len(m) > 0 {
						rel, _ := filepath.Rel(prog.Root, p)
						ip := cfg.Module
						if rel != "." {
							ip += "/" + filepath.ToSlash(rel)
						}
						add(ip, "package of this module", ckFolder)
					}
					return nil
				})
			}
		}
		for _, pk := range prog.Packages() {
			if !pk.Target && pk.GoFile == "" && !strings.HasPrefix(pk.Path, "std/") {
				add(pk.Path, pk.Name, ckModule)
			}
		}
	}
	return items
}

// ---------------------------------------------------------------- scope

func (s *server) scopeItems(pa *pkgAnalysis, path string, line, col int) []CompletionItem {
	var items []CompletionItem
	seen := map[string]bool{}
	add := func(it CompletionItem) {
		if it.Label == "" || seen[it.Label+"|"+itoa(it.Kind)] {
			return
		}
		seen[it.Label+"|"+itoa(it.Kind)] = true
		items = append(items, it)
	}
	inUnsafe := pa.inUnsafe(path, line, col)
	// locals and parameters declared before the cursor in the enclosing function
	cur := s.analysisFor(path)
	fr := cur.enclosingFunc(path, line, col)
	if fr == nil && pa != cur {
		fr = pa.enclosingFunc(path, line, col)
	}
	for _, o := range pa.byFile[path] {
		if !o.def || o.obj == nil {
			continue
		}
		v, ok := o.obj.(*types.Var)
		if !ok || v.Global {
			continue
		}
		if fr != nil && !(lrange{fr.sLine, fr.sCol, fr.eLine, fr.eCol}).contains(o.line, o.col) {
			continue
		}
		if fr == nil || o.line > line {
			continue
		}
		name := nameText(s.texts(path), token.Pos{Line: o.line, Column: o.col}, o.n)
		add(CompletionItem{Label: name, Kind: ckVariable, Detail: s.typeStr(pa, v.Type()), SortText: "0" + name})
	}
	// package scope (without dependency names) and import aliases
	if pa.a != nil && pa.a.Pkg != nil && pa.target != nil {
		deps := map[string]bool{}
		for _, pk := range pa.a.Prog.Packages() {
			if !pk.Target && pk.Prefix != "" {
				deps[pk.Prefix] = true
			}
		}
		for _, n := range pa.a.Pkg.Scope.Names() {
			if i := strings.Index(n, "__"); i > 0 && deps[n[:i+2]] {
				continue
			}
			if strings.HasPrefix(n, "gocpy_") || strings.HasPrefix(n, "_") {
				continue
			}
			o := pa.a.Pkg.Scope.Lookup(n)
			if it, ok := s.objItem(pa, n, o); ok {
				it.SortText = "1" + n
				add(it)
			}
		}
	}
	for alias, ip := range s.importAliases(pa, path) {
		add(CompletionItem{Label: alias, Kind: ckModule, Detail: ip, SortText: "1" + alias})
	}
	// keywords (with snippets), builtins, predeclared
	var kws []string
	for k := range keywordDocs {
		kws = append(kws, k)
	}
	sort.Strings(kws)
	for _, k := range kws {
		d := keywordDocs[k]
		if k == "unsafe" || token.Lookup(k) != token.IDENT {
			add(CompletionItem{Label: k, Kind: ckKeyword, Documentation: md(d.doc), SortText: "2" + k})
		}
		if d.snippet != "" {
			lbl := d.label
			if lbl == "" {
				lbl = k + " …"
			}
			add(CompletionItem{Label: lbl, FilterText: k, Kind: ckSnippet, Documentation: md(d.doc),
				InsertText: d.snippet, InsertTextFormat: formatSnippet, SortText: "3" + k})
		}
	}
	for _, n := range []string{"len", "cap", "append", "make", "new", "sizeof", "close", "delete"} {
		add(CompletionItem{Label: n, Kind: ckFunction, Detail: "built-in function", Documentation: md(builtinDocs[n]),
			InsertText: n + "($1)", InsertTextFormat: formatSnippet, SortText: "2" + n})
	}
	if inUnsafe {
		for _, n := range []string{"malloc", "calloc", "realloc", "free", "memcpy", "memset", "slice"} {
			add(CompletionItem{Label: n, Kind: ckFunction, Detail: "unsafe", Documentation: md(builtinDocs[n]),
				InsertText: n + "($1)", InsertTextFormat: formatSnippet, SortText: "0" + n})
		}
	}
	for _, n := range []string{"bool", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32",
		"uint64", "uintptr", "float32", "float64", "string", "byte", "rune", "any", "error", "Exception", "comparable", "thread"} {
		add(CompletionItem{Label: n, Kind: ckClass, Detail: "built-in type", Documentation: md(builtinDocs[n]), SortText: "2" + n})
	}
	for _, n := range []string{"true", "false", "nil", "iota"} {
		add(CompletionItem{Label: n, Kind: ckConstant, Documentation: md(builtinDocs[n]), SortText: "2" + n})
	}
	return items
}

func (s *server) objItem(pa *pkgAnalysis, name string, o types.Object) (CompletionItem, bool) {
	it := CompletionItem{Label: name}
	switch o := o.(type) {
	case *types.Var:
		it.Kind, it.Detail = ckVariable, s.typeStr(pa, o.Type())
		if o.IsConst {
			it.Kind = ckConstant
		}
	case *types.Func:
		it.Kind, it.Detail = ckFunction, s.typeStr(pa, o.Type())
		it.InsertText, it.InsertTextFormat = name+"($1)", formatSnippet
	case *types.FuncSet:
		it.Kind, it.Detail = ckFunction, "overloaded"
		it.InsertText, it.InsertTextFormat = name+"($1)", formatSnippet
	case *types.GenericFunc:
		it.Kind, it.Detail = ckFunction, "generic function"
		it.InsertText, it.InsertTextFormat = name+"($1)", formatSnippet
	case *types.TypeName:
		it.Kind = ckStruct
		if _, ok := o.Type().Underlying().(*types.Interface); ok {
			it.Kind = ckInterface
		}
		it.Detail = "type"
	case *types.GenericType:
		it.Kind, it.Detail = ckStruct, "generic type"
	default:
		return it, false
	}
	if pos := o.Pos(); pos.File != "" && pos.Line > 0 {
		doc := "```gocpy\n" + s.objString(pa, o, name) + "\n```"
		if c := s.objDoc(pos); c != "" {
			doc += "\n" + c
		}
		it.Documentation = md(doc)
	}
	return it, true
}

// objDoc is the doc comment of a declaration; for Go bridge stubs it is read
// from the Go source (// go:pos above the stub declaration).
func (s *server) objDoc(pos token.Pos) string {
	t := s.texts(pos.File)
	for l := pos.Line - 1; l >= 1 && l >= pos.Line-3; l-- {
		lt := strings.TrimSpace(t.line(l))
		if strings.HasPrefix(lt, gobridge.GoPosPrefix) {
			ref := strings.TrimPrefix(lt, gobridge.GoPosPrefix)
			parts := strings.Split(ref, ":")
			if len(parts) >= 3 {
				if ln, err := strconv.Atoi(parts[len(parts)-2]); err == nil {
					return s.docComment(defLoc{file: strings.Join(parts[:len(parts)-2], ":"), line: ln})
				}
			}
			return ""
		}
		if !strings.HasPrefix(lt, "@") {
			break
		}
	}
	return s.docComment(defLoc{file: pos.File, line: pos.Line})
}

// importAliases maps package names usable in the file to import paths.
func (s *server) importAliases(pa *pkgAnalysis, path string) map[string]string {
	out := map[string]string{}
	t := s.texts(path)
	for _, im := range pa.imports {
		if im.file != path {
			continue
		}
		lt := []rune(t.line(im.line))
		alias := ""
		if im.col-1 <= len(lt) {
			pre := strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(lt[:im.col-1])), "import")))
			if len(pre) == 1 {
				alias = pre[0]
			}
		}
		if alias == "" {
			alias = lastElem(im.path)
			if pa.a != nil && pa.a.Prog != nil {
				if pk := pa.a.Prog.PackageByPath(im.path); pk != nil && pk.Name != "" {
					alias = pk.Name
				}
			}
		}
		if alias != "_" && alias != "." {
			out[alias] = im.path
		}
	}
	return out
}

// ---------------------------------------------------------------- members

type seg struct {
	name string
	kind byte // 'i' ident, 'c' call, 'x' index
}

// parseChain splits "a.b(1, 2).c[i]" (text before the final dot) into segments.
func parseChain(src string) []seg {
	rs := []rune(src)
	i := len(rs)
	var out []seg
	for i > 0 {
		switch rs[i-1] {
		case ')', ']':
			open, close := '(', ')'
			k := byte('c')
			if rs[i-1] == ']' {
				open, close, k = '[', ']', 'x'
			}
			depth := 0
			j := i - 1
			for ; j >= 0; j-- {
				if rs[j] == close {
					depth++
				} else if rs[j] == open {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j < 0 {
				return nil
			}
			out = append(out, seg{kind: k})
			i = j
			continue
		}
		j := i
		for j > 0 && isIdentRune(rs[j-1]) {
			j--
		}
		if j == i {
			return nil
		}
		out = append(out, seg{name: string(rs[j:i]), kind: 'i'})
		i = j
		if i > 0 && rs[i-1] == '.' {
			i--
			continue
		}
		break
	}
	// reverse
	for a, b := 0, len(out)-1; a < b; a, b = a+1, b-1 {
		out[a], out[b] = out[b], out[a]
	}
	if len(out) == 0 || out[0].kind != 'i' {
		return nil
	}
	return out
}

// lookupName resolves a name at a position of the file (local, package, universe).
func (s *server) lookupName(pa *pkgAnalysis, path string, line int, name string) types.Object {
	t := s.texts(path)
	var best *occ
	for _, o := range pa.byFile[path] {
		if o.obj == nil || o.qualified || o.line > line {
			continue
		}
		if nameText(t, token.Pos{Line: o.line, Column: o.col}, o.n) == name {
			if _, ok := o.obj.(*types.PkgName); ok {
				continue
			}
			best = o
		}
	}
	if best != nil {
		return best.obj
	}
	if pa.a != nil && pa.a.Pkg != nil {
		if o := pa.a.Pkg.Scope.Lookup(name); o != nil {
			return o
		}
	}
	return types.Universe.Lookup(name)
}

func (s *server) memberItems(pa *pkgAnalysis, path string, line int, chain []seg) []CompletionItem {
	items := []CompletionItem{}
	first := chain[0].name
	// package members
	if ip, ok := s.importAliases(pa, path)[first]; ok && len(chain) == 1 {
		return s.packageMembers(pa, ip)
	}
	var cur types.Type
	var pending types.Object
	if ip, ok := s.importAliases(pa, path)[first]; ok && len(chain) > 1 {
		pending = s.packageMember(pa, ip, chain[1].name)
		chain = chain[1:]
	} else {
		pending = s.lookupName(pa, path, line, first)
	}
	if pending == nil {
		return items
	}
	isType := false
	switch o := pending.(type) {
	case *types.TypeName:
		cur, isType = o.Type(), true
	case *types.FuncSet:
		if len(o.Funcs) > 0 {
			cur = o.Funcs[0].Type()
		}
	default:
		cur = o.Type()
	}
	for _, sg := range chain[1:] {
		if cur == nil {
			return items
		}
		switch sg.kind {
		case 'c':
			if sig, ok := cur.(*types.Signature); ok && len(sig.Results) > 0 {
				cur = sig.Results[0].Type()
			} else if !isType {
				return items
			}
			isType = false
		case 'x':
			cur = elemOf(cur)
		case 'i':
			cur = memberType(cur, sg.name)
			isType = false
		}
	}
	if cur == nil {
		return items
	}
	return s.typeMembers(pa, cur)
}

func elemOf(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem
	}
	switch u := t.Underlying().(type) {
	case *types.Slice:
		return u.Elem
	case *types.Array:
		return u.Elem
	case *types.Map:
		return u.Elem
	case *types.Basic:
		return types.Typ[types.Uint8]
	}
	return nil
}

func memberType(t types.Type, name string) types.Type {
	base := t
	if p, ok := t.(*types.Pointer); ok {
		base = p.Elem
	}
	if n, ok := base.(*types.Named); ok {
		if ms := n.Methods[name]; len(ms) > 0 {
			return ms[0].Type()
		}
	}
	if base == nil || base.Underlying() == nil {
		return nil
	}
	switch u := base.Underlying().(type) {
	case *types.Struct:
		if _, f := u.Field(name); f != nil {
			return f.Type
		}
	case *types.Interface:
		for _, m := range u.Methods {
			if m.Name == name {
				return m.Sig
			}
		}
	}
	return nil
}

func (s *server) typeMembers(pa *pkgAnalysis, t types.Type) []CompletionItem {
	items := []CompletionItem{}
	base := t
	if p, ok := t.(*types.Pointer); ok {
		base = p.Elem
	}
	foreign := false
	n, _ := base.(*types.Named)
	if n != nil {
		if n.Pkg != "" {
			foreign = true
		} else if n.Obj != nil && pa.a != nil && pa.a.Prog != nil && pa.target != nil {
			if owner, _ := pa.a.Prog.OwnerOf(n.Obj.Name()); owner != nil && !owner.Target {
				foreign = true
			}
		}
		var names []string
		for m := range n.Methods {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, m := range names {
			if !validIdent(m) || (foreign && !build.Exported(m)) {
				continue
			}
			fs := n.Methods[m]
			detail := s.typeStr(pa, fs[0].Type())
			if len(fs) > 1 {
				detail += " (+" + itoa(len(fs)-1) + ")"
			}
			items = append(items, CompletionItem{Label: m, Kind: ckMethod, Detail: detail,
				InsertText: m + "($1)", InsertTextFormat: formatSnippet, SortText: "1" + m})
		}
	}
	if base == nil || base.Underlying() == nil {
		return items
	}
	switch u := base.Underlying().(type) {
	case *types.Struct:
		for _, f := range u.Fields {
			if foreign && !build.Exported(f.Name) {
				continue
			}
			items = append(items, CompletionItem{Label: f.Name, Kind: ckField, Detail: s.typeStr(pa, f.Type), SortText: "0" + f.Name})
		}
	case *types.Interface:
		for _, m := range u.Methods {
			items = append(items, CompletionItem{Label: m.Name, Kind: ckMethod, Detail: s.typeStr(pa, m.Sig),
				InsertText: m.Name + "($1)", InsertTextFormat: formatSnippet})
		}
	case *types.Slice:
		items = append(items, CompletionItem{Label: "append", Kind: ckMethod, Detail: "append elements to the slice",
			InsertText: "append($1)", InsertTextFormat: formatSnippet})
	}
	return items
}

func (s *server) packageMember(pa *pkgAnalysis, ip, name string) types.Object {
	if m := types.StdMembers[ip][name]; m != nil {
		return m
	}
	if pa.a == nil || pa.a.Prog == nil || pa.a.Pkg == nil {
		return nil
	}
	if pk := pa.a.Prog.PackageByPath(ip); pk != nil {
		return pa.a.Pkg.Scope.Lookup(pk.Prefix + name)
	}
	return nil
}

func (s *server) packageMembers(pa *pkgAnalysis, ip string) []CompletionItem {
	items := []CompletionItem{}
	if bs, ok := types.StdPackages[ip]; ok {
		for name := range bs {
			items = append(items, CompletionItem{Label: name, Kind: ckFunction, Detail: ip,
				InsertText: name + "($1)", InsertTextFormat: formatSnippet})
		}
		for name, o := range types.StdMembers[ip] {
			if it, ok := s.objItem(pa, name, o); ok {
				it.Detail = s.demangle(pa, it.Detail)
				items = append(items, it)
			}
		}
		return items
	}
	if pa.a == nil || pa.a.Prog == nil || pa.a.Pkg == nil {
		return items
	}
	pk := pa.a.Prog.PackageByPath(ip)
	if pk == nil {
		return items
	}
	for _, n := range pa.a.Pkg.Scope.Names() {
		if !strings.HasPrefix(n, pk.Prefix) {
			continue
		}
		name := strings.TrimPrefix(n, pk.Prefix)
		if !build.Exported(name) || strings.Contains(name, "__") {
			continue
		}
		if it, ok := s.objItem(pa, name, pa.a.Pkg.Scope.Lookup(n)); ok {
			items = append(items, it)
		}
	}
	return items
}

// ---------------------------------------------------------------- signature help

func (s *server) signatureHelp(p TextDocumentPositionParams) any {
	path, line, col := s.docPos(p)
	t := s.texts(path)
	end := t.offset(line, col)
	src := t.src[:end]
	// find the innermost unclosed '(' (at most 4000 bytes back)
	depth, commas := 0, 0
	open := -1
	for i := len(src) - 1; i >= 0 && i >= len(src)-4000; i-- {
		switch src[i] {
		case ')', ']', '}':
			depth++
		case '(', '[', '{':
			if depth == 0 {
				if src[i] == '(' {
					open = i
				}
				i = -1 // stop
				break
			}
			depth--
		case ',':
			if depth == 0 {
				commas++
			}
		case '\n':
			// continue across lines
		}
		if i == -1 {
			break
		}
	}
	if open < 0 {
		return nil
	}
	ln, _ := t.lineCol(open)
	before := src[:open]
	if k := strings.LastIndexByte(before, '\n'); k >= 0 {
		before = before[k+1:]
	}
	chain := parseChain(strings.TrimRight(before, " \t"))
	if len(chain) == 0 {
		return nil
	}
	pa := s.bestFor(path)
	sigs := s.calleeSignatures(pa, path, ln, chain)
	if len(sigs) == 0 {
		return nil
	}
	var out []map[string]any
	name := chain[len(chain)-1].name
	active := 0
	for i, sg := range sigs {
		label, params := s.sigLabel(pa, name, sg)
		out = append(out, map[string]any{"label": label, "parameters": params})
		if commas < len(sg.Params) && active == 0 && len(sigs) > 1 && commas+1 <= len(sg.Params) {
			active = i
		}
	}
	return map[string]any{"signatures": out, "activeSignature": active, "activeParameter": commas}
}

func (s *server) calleeSignatures(pa *pkgAnalysis, path string, line int, chain []seg) []*types.Signature {
	var obj types.Object
	if len(chain) == 1 {
		obj = s.lookupName(pa, path, line, chain[0].name)
	} else {
		last := chain[len(chain)-1].name
		if ip, ok := s.importAliases(pa, path)[chain[0].name]; ok && len(chain) == 2 {
			obj = s.packageMember(pa, ip, last)
			if obj == nil {
				if _, ok := types.StdPackages[ip][last]; ok {
					return []*types.Signature{{Variadic: true, Params: []*types.Var{types.NewVar("args", types.AnyType, token.Pos{})}}}
				}
			}
		} else {
			// method: resolve the receiver chain and take every overload
			recv := s.chainType(pa, path, line, chain[:len(chain)-1])
			if recv == nil {
				return nil
			}
			base := recv
			if p, ok := recv.(*types.Pointer); ok {
				base = p.Elem
			}
			if n, ok := base.(*types.Named); ok {
				var out []*types.Signature
				for _, f := range n.Methods[last] {
					if sg, ok := f.Type().(*types.Signature); ok {
						out = append(out, sg)
					}
				}
				if len(out) > 0 {
					return out
				}
			}
			if sg, ok := memberType(recv, last).(*types.Signature); ok {
				return []*types.Signature{sg}
			}
			return nil
		}
	}
	switch o := obj.(type) {
	case *types.Func:
		if sg, ok := o.Type().(*types.Signature); ok {
			return []*types.Signature{sg}
		}
	case *types.FuncSet:
		var out []*types.Signature
		for _, f := range o.Funcs {
			if sg, ok := f.Type().(*types.Signature); ok {
				out = append(out, sg)
			}
		}
		return out
	case *types.Var:
		if sg, ok := o.Type().(*types.Signature); ok {
			return []*types.Signature{sg}
		}
	}
	return nil
}

func (s *server) chainType(pa *pkgAnalysis, path string, line int, chain []seg) types.Type {
	if len(chain) == 0 {
		return nil
	}
	var o types.Object
	if ip, ok := s.importAliases(pa, path)[chain[0].name]; ok && len(chain) > 1 {
		o = s.packageMember(pa, ip, chain[1].name)
		chain = chain[1:]
	} else {
		o = s.lookupName(pa, path, line, chain[0].name)
	}
	if o == nil {
		return nil
	}
	cur := o.Type()
	if fs, ok := o.(*types.FuncSet); ok && len(fs.Funcs) > 0 {
		cur = fs.Funcs[0].Type()
	}
	for _, sg := range chain[1:] {
		if cur == nil {
			return nil
		}
		switch sg.kind {
		case 'c':
			if sig, ok := cur.(*types.Signature); ok && len(sig.Results) > 0 {
				cur = sig.Results[0].Type()
			}
		case 'x':
			cur = elemOf(cur)
		case 'i':
			cur = memberType(cur, sg.name)
		}
	}
	return cur
}

func (s *server) sigLabel(pa *pkgAnalysis, name string, sg *types.Signature) (string, []map[string]any) {
	var b strings.Builder
	b.WriteString(name + "(")
	var params []map[string]any
	for i, p := range sg.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		start := utf16Len(b.String())
		pt := s.typeStr(pa, p.Type())
		if sg.Variadic && i == len(sg.Params)-1 {
			pt = "..." + strings.TrimPrefix(pt, "[]")
		}
		ps := strings.TrimSpace(p.Name() + " " + pt)
		b.WriteString(ps)
		params = append(params, map[string]any{"label": []int{start, utf16Len(b.String())}})
	}
	b.WriteString(")")
	switch len(sg.Results) {
	case 0:
	case 1:
		b.WriteString(" " + s.typeStr(pa, sg.Results[0].Type()))
	default:
		var rs []string
		for _, r := range sg.Results {
			rs = append(rs, s.typeStr(pa, r.Type()))
		}
		b.WriteString(" (" + strings.Join(rs, ", ") + ")")
	}
	if sg.Throws {
		b.WriteString("  @throws")
	}
	return b.String(), params
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	_ = utf8.RuneLen
	return n
}
