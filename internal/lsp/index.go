package lsp

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/build"
	"github.com/rusfort/gocpy/internal/token"
	"github.com/rusfort/gocpy/internal/types"
)

// occ is one occurrence of a name in a source file.
type occ struct {
	file      string
	line, col int // 1-based, rune column
	n         int // rune length
	cls       string
	key       string // identity of the referenced entity ("" = unknown)
	def       bool
	call      bool
	obj       types.Object
	field     *types.Field
	fieldOf   types.Type // struct type that declares field
	pkgPath   string     // package name occurrences
	qualified bool       // pkg.Name member
}

type funcRange struct {
	decl       *ast.FuncDecl
	key        string
	name       string
	file       string
	sLine      int
	sCol       int
	eLine      int
	eCol       int
	nameLine   int
	nameCol    int
	nameLen    int
	isMethod   bool
	recvString string
}

type lrange struct{ sLine, sCol, eLine, eCol int }

func (r lrange) contains(line, col int) bool {
	if line < r.sLine || line > r.eLine {
		return false
	}
	if line == r.sLine && col < r.sCol {
		return false
	}
	if line == r.eLine && col > r.eCol {
		return false
	}
	return true
}

type tparamScope struct {
	file  string
	r     lrange
	names map[string]bool
}

type importOcc struct {
	file      string
	line, col int
	n         int
	path      string
}

// pkgAnalysis is an analyzed package with an occurrence index of its files.
type pkgAnalysis struct {
	dir     string
	gen     int
	a       *build.Analysis
	target  *build.PkgInfo
	files   map[string]*ast.File
	occs    []*occ
	byFile  map[string][]*occ
	byKey   map[string][]*occ
	objs    map[string]types.Object
	funcs   map[string][]*funcRange // per file
	unsafe  map[string][]lrange
	ptrOps  map[string][][2]int // file -> (line, col) of unary * and &
	imports []importOcc
	tparams []tparamScope
	errText string // fatal load error
}

func (s *server) texts(path string) *text {
	if d := s.docs[path]; d != nil {
		return d.t
	}
	return s.files.get(path)
}

func objKey(o types.Object) string {
	switch o := o.(type) {
	case nil:
		return ""
	case *types.PkgName:
		return "pkg:" + o.Path
	case *types.Builtin:
		return "builtin:" + o.Name()
	}
	if k := posKey(o.Pos()); k != "" {
		return k
	}
	return "builtin:" + o.Name()
}

var universeTypeNames = map[string]bool{}

func init() {
	for _, n := range strings.Fields("bool int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr float32 float64 string byte rune any thread") {
		universeTypeNames[n] = true
	}
}

func (pa *pkgAnalysis) isTypeParam(file string, line, col int, name string) bool {
	for _, sc := range pa.tparams {
		if sc.file == file && sc.names[name] && sc.r.contains(line, col) {
			return true
		}
	}
	return false
}

func (pa *pkgAnalysis) classify(o types.Object, id *ast.Ident, def, call, qualified bool) string {
	switch o := o.(type) {
	case *types.PkgName:
		return "pkgName"
	case *types.Var:
		if o.IsConst {
			if o.Pos().File == "" && (o.Name() == "true" || o.Name() == "false") {
				return "constBool"
			}
			if qualified {
				return "pkgMember"
			}
			return "constUser"
		}
		if qualified {
			return "pkgMember"
		}
		return "var"
	case *types.Func:
		if def {
			if o.IsOperator {
				return "funcOperator"
			}
			return "funcDecl"
		}
		if o.RecvNamed != nil {
			return "funcMethod"
		}
		if qualified {
			return "funcStd"
		}
		return "funcCall"
	case *types.FuncSet, *types.GenericFunc:
		if def {
			return "funcDecl"
		}
		if qualified {
			return "funcStd"
		}
		return "funcCall"
	case *types.TypeName:
		if id != nil && pa.isTypeParam(id.NamePos.File, id.NamePos.Line, id.NamePos.Column, id.Name) {
			return "typeParam"
		}
		if o.Pos().File == "" {
			switch o.Name() {
			case "error", "Exception":
				return "typeError"
			case "comparable":
				return "typeConstraint"
			}
			if n, ok := o.Type().(*types.Named); ok && n.Pkg != "" {
				return "typeStd"
			}
			return "typeBuiltin"
		}
		return "typeUser"
	case *types.GenericType:
		return "typeUser"
	case *types.Builtin:
		if o.ID >= 100 {
			return "unsafeFunc"
		}
		if o.ID >= types.FmtPrintln {
			return "funcStd"
		}
		return "funcBuiltin"
	case *types.Nil:
		return "constNil"
	}
	return "var"
}

// buildIndex indexes the target package of an analysis.
func (s *server) buildIndex(pa *pkgAnalysis) {
	a := pa.a
	pa.files = map[string]*ast.File{}
	pa.byFile = map[string][]*occ{}
	pa.byKey = map[string][]*occ{}
	pa.objs = map[string]types.Object{}
	pa.funcs = map[string][]*funcRange{}
	pa.unsafe = map[string][]lrange{}
	pa.ptrOps = map[string][][2]int{}
	if a == nil || a.Prog == nil {
		return
	}
	for _, p := range a.Prog.Packages() {
		if p.Target {
			pa.target = p
			for _, f := range p.Files {
				pa.files[f.Package.File] = f
			}
		}
	}
	info := a.Info
	seen := map[string]*occ{}
	add := func(o *occ) {
		if o.file == "" || o.line == 0 || o.n <= 0 {
			return
		}
		if _, ok := pa.files[o.file]; !ok {
			return
		}
		k := o.file + ":" + itoa(o.line) + ":" + itoa(o.col)
		if prev := seen[k]; prev != nil {
			if prev.key == "" && o.key != "" {
				*prev = *o
			}
			return
		}
		seen[k] = o
		pa.occs = append(pa.occs, o)
	}
	callees := map[*ast.Ident]*ast.CallExpr{}
	var litKeys []struct {
		id  *ast.Ident
		lit *ast.CompositeLit
	}
	type conv struct {
		call *ast.CallExpr
		file string
	}
	var convs []conv

	for path, f := range pa.files {
		t := s.texts(path)
		path := path
		// package clause
		if f.Name != nil {
			add(&occ{file: path, line: f.Name.NamePos.Line, col: f.Name.NamePos.Column, n: t.wordAt(f.Name.NamePos.Line, f.Name.NamePos.Column),
				cls: "pkgDecl", key: "pkgdecl:" + pa.target.Path, def: true, pkgPath: pa.target.Path})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				fr := &funcRange{decl: n, name: n.Name.Name, file: path, nameLine: n.Name.NamePos.Line, nameCol: n.Name.NamePos.Column}
				start := n.Type.Func
				if len(n.Annots) > 0 {
					start = n.Annots[0].At
				}
				fr.sLine, fr.sCol = start.Line, start.Column
				fr.eLine, fr.eCol = n.Name.NamePos.Line, 1<<30
				if n.Body != nil {
					if l, c, ok := t.matchBrace(n.Body.Lbrace.Line, n.Body.Lbrace.Column); ok {
						fr.eLine, fr.eCol = l, c
					}
				}
				if n.IsOperator {
					fr.nameLen = t.opAt(fr.nameLine, fr.nameCol)
				} else {
					fr.nameLen = t.wordAt(fr.nameLine, fr.nameCol)
				}
				fr.key = posKey(n.Name.NamePos)
				if n.Recv != nil && len(n.Recv.List) > 0 {
					fr.isMethod = true
					fr.recvString = recvTypeName(n.Recv.List[0].Type)
				}
				pa.funcs[path] = append(pa.funcs[path], fr)
				names := map[string]bool{}
				if n.Type.TypeParams != nil {
					for _, fl := range n.Type.TypeParams.List {
						for _, id := range fl.Names {
							names[id.Name] = true
						}
					}
				}
				if n.Recv != nil {
					for _, fl := range n.Recv.List {
						collectRecvTParams(fl.Type, names)
					}
				}
				if len(names) > 0 {
					pa.tparams = append(pa.tparams, tparamScope{file: path, names: names,
						r: lrange{fr.sLine, 1, fr.eLine, 1 << 30}})
				}
			case *ast.GenDecl:
				for _, sp := range n.Specs {
					ts, ok := sp.(*ast.TypeSpec)
					if !ok || ts.TypeParams == nil {
						continue
					}
					names := map[string]bool{}
					for _, fl := range ts.TypeParams.List {
						for _, id := range fl.Names {
							names[id.Name] = true
						}
					}
					end := ts.Name.NamePos.Line
					if l := typeEndLine(t, ts.Type); l > end {
						end = l
					}
					pa.tparams = append(pa.tparams, tparamScope{file: path, names: names,
						r: lrange{ts.Name.NamePos.Line, 1, end, 1 << 30}})
				}
			case *ast.CallExpr:
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					callees[fn] = n
				case *ast.SelectorExpr:
					callees[fn.Sel] = n
				case *ast.IndexExpr:
					if id, ok := fn.X.(*ast.Ident); ok {
						callees[id] = n
					} else if se, ok := fn.X.(*ast.SelectorExpr); ok {
						callees[se.Sel] = n
					}
				}
				convs = append(convs, conv{n, path})
			case *ast.StructType:
				if n.Fields != nil {
					for _, fl := range n.Fields.List {
						for _, id := range fl.Names {
							add(&occ{file: path, line: id.NamePos.Line, col: id.NamePos.Column, n: t.wordAt(id.NamePos.Line, id.NamePos.Column),
								cls: "fieldDecl", key: posKey(id.NamePos), def: true})
						}
					}
				}
			case *ast.InterfaceType:
				if n.Methods != nil {
					for _, fl := range n.Methods.List {
						for _, id := range fl.Names {
							add(&occ{file: path, line: id.NamePos.Line, col: id.NamePos.Column, n: t.wordAt(id.NamePos.Line, id.NamePos.Column),
								cls: "funcDecl", def: true})
						}
					}
				}
			case *ast.CompositeLit:
				for _, e := range n.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							litKeys = append(litKeys, struct {
								id  *ast.Ident
								lit *ast.CompositeLit
							}{id, n})
						}
					}
				}
			case *ast.BranchStmt:
				if n.Label != nil {
					add(&occ{file: path, line: n.Label.NamePos.Line, col: n.Label.NamePos.Column, n: t.wordAt(n.Label.NamePos.Line, n.Label.NamePos.Column), cls: "label"})
				}
			case *ast.StarExpr:
				pa.ptrOps[path] = append(pa.ptrOps[path], [2]int{n.Star.Line, n.Star.Column})
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					pa.ptrOps[path] = append(pa.ptrOps[path], [2]int{n.OpPos.Line, n.OpPos.Column})
				}
			case *ast.UnsafeStmt:
				if n.Body != nil {
					if l, c, ok := t.matchBrace(n.Body.Lbrace.Line, n.Body.Lbrace.Column); ok {
						pa.unsafe[path] = append(pa.unsafe[path], lrange{n.Unsafe.Line, n.Unsafe.Column, l, c})
					}
				}
			}
			return true
		})
		for _, imp := range f.Imports {
			pa.addImport(path, t, imp)
		}
	}
	// user imports were removed from f.Imports by the loader: read them from the source
	for path := range pa.files {
		pa.scanImports(path, s.texts(path))
	}

	if info == nil {
		pa.finish()
		return
	}
	qual := a.Prog.Qual
	record := func(id *ast.Ident, o types.Object, def bool) {
		if hiddenName(id.Name) && !strings.HasPrefix(id.Name, "GocpyGet_") && !strings.HasPrefix(id.Name, "GocpySet_") {
			return // generated for Go values (composite literals, new)
		}
		file := id.NamePos.File
		if _, ok := pa.files[file]; !ok {
			return
		}
		t := s.texts(file)
		call := callees[id] != nil
		if fs, ok := o.(*types.FuncSet); ok && call {
			if ci := info.Calls[callees[id]]; ci != nil && ci.Func != nil {
				o = ci.Func
			} else if len(fs.Funcs) > 0 {
				o = fs.Funcs[0]
			}
		}
		if q := qual[id]; q != nil {
			add(&occ{file: file, line: q.Pkg.NamePos.Line, col: q.Pkg.NamePos.Column, n: utf8.RuneCountInString(q.Pkg.Name),
				cls: "pkgName", key: "pkg:" + q.Path, pkgPath: q.Path})
			k := objKey(o)
			pa.objs[k] = o
			add(&occ{file: file, line: q.Sel.NamePos.Line, col: q.Sel.NamePos.Column, n: utf8.RuneCountInString(q.Sel.Name),
				cls: pa.classify(o, q.Sel, false, call, true), key: k, call: call, obj: o, qualified: true})
			return
		}
		n := t.wordAt(id.NamePos.Line, id.NamePos.Column)
		if fn, ok := o.(*types.Func); ok && def && fn.IsOperator {
			n = t.opAt(id.NamePos.Line, id.NamePos.Column)
		}
		k := objKey(o)
		if k != "" {
			pa.objs[k] = o
		}
		oc := &occ{file: file, line: id.NamePos.Line, col: id.NamePos.Column, n: n,
			cls: pa.classify(o, id, def, call, false), key: k, def: def, call: call, obj: o}
		if pn, ok := o.(*types.PkgName); ok {
			oc.pkgPath = pn.Path
		}
		add(oc)
	}
	for id, o := range info.Defs {
		if o != nil {
			record(id, o, true)
		}
	}
	for id, o := range info.Uses {
		if o != nil {
			record(id, o, false)
		}
	}
	for sel, sl := range info.Selections {
		id := sel.Sel
		file := id.NamePos.File
		if _, ok := pa.files[file]; !ok {
			continue
		}
		t := s.texts(file)
		n := t.wordAt(id.NamePos.Line, id.NamePos.Column)
		call := callees[id] != nil
		o := &occ{file: file, line: id.NamePos.Line, col: id.NamePos.Column, n: n, call: call}
		switch sl.Kind {
		case types.PkgSel:
			path := ""
			if x, ok := sel.X.(*ast.Ident); ok {
				if pn, ok := info.Uses[x].(*types.PkgName); ok {
					path = pn.Path
				}
			}
			o.key = "std:" + path + "." + id.Name
			o.qualified = true
			if m := types.StdMembers[path][id.Name]; m != nil {
				o.obj = m
				switch m.(type) {
				case *types.TypeName:
					o.cls = "typeStd"
				case *types.Var:
					o.cls = "pkgMember"
				default:
					o.cls = "funcStd"
				}
			} else {
				o.cls = "funcStd"
				if sl.Builtin != nil {
					o.obj = sl.Builtin
				}
			}
			pa.objs[o.key] = o.obj
		case types.FieldSel:
			o.cls = "field"
			if tv, ok := info.Types[sel.X]; ok && tv.Type != nil {
				if st := structOf(tv.Type); st != nil && sl.Index >= 0 && sl.Index < len(st.Fields) {
					f := st.Fields[sl.Index]
					o.field, o.fieldOf, o.key = f, tv.Type, posKey(f.Pos)
				}
			}
		case types.MethodSel:
			o.cls = "funcMethod"
			var fn *types.Func
			if c := callees[id]; c != nil {
				if ci := info.Calls[c]; ci != nil && ci.Func != nil {
					fn = ci.Func
				}
			}
			if fn == nil && len(sl.Methods) > 0 {
				fn = sl.Methods[0]
			}
			if fn != nil {
				o.obj, o.key = fn, objKey(fn)
				if o.key != "" {
					pa.objs[o.key] = fn
				}
			}
			if strings.HasPrefix(id.Name, "GocpyGet_") || strings.HasPrefix(id.Name, "GocpySet_") {
				o.cls = "field" // a field of a Go value
			}
		case types.IfaceMethodSel:
			o.cls = "funcMethod"
			if len(sl.Methods) > 0 {
				o.obj = sl.Methods[0]
			}
			if tv, ok := info.Types[sel.X]; ok && tv.Type != nil {
				o.key = "iface:" + tv.Type.String() + "." + id.Name
			}
		default:
			o.cls = "funcBuiltin"
		}
		add(o)
	}
	for _, lk := range litKeys {
		file := lk.id.NamePos.File
		tv, ok := info.Types[lk.lit]
		if !ok || tv.Type == nil {
			continue
		}
		st := structOf(tv.Type)
		if st == nil {
			continue
		}
		_, f := st.Field(lk.id.Name)
		if f == nil {
			continue
		}
		t := s.texts(file)
		add(&occ{file: file, line: lk.id.NamePos.Line, col: lk.id.NamePos.Column, n: t.wordAt(lk.id.NamePos.Line, lk.id.NamePos.Column),
			cls: "field", key: posKey(f.Pos), field: f, fieldOf: tv.Type})
	}
	// pointer conversions inside unsafe blocks: (*T)(p), uintptr(p)
	for _, c := range convs {
		ci := info.Calls[c.call]
		if ci == nil || ci.Kind != types.CallConversion || !pa.inUnsafe(c.file, c.call.Pos().Line, c.call.Pos().Column) {
			continue
		}
		ast.Inspect(c.call.Fun, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				for _, o := range pa.occs {
					if o.file == c.file && o.line == id.NamePos.Line && o.col == id.NamePos.Column {
						o.cls = "unsafeCast"
					}
				}
			}
			return true
		})
	}
	pa.finish()
}

func (pa *pkgAnalysis) addImport(path string, t *text, imp *ast.ImportSpec) {
	p := strings.Trim(imp.Path.Value, "\"`")
	pa.imports = append(pa.imports, importOcc{file: path, line: imp.Path.ValuePos.Line, col: imp.Path.ValuePos.Column,
		n: utf8.RuneCountInString(imp.Path.Value), path: p})
}

// scanImports finds import paths in the source text (the loader drops
// non-std imports from the AST).
func (pa *pkgAnalysis) scanImports(path string, t *text) {
	have := map[int]bool{}
	for _, im := range pa.imports {
		if im.file == path {
			have[im.line] = true
		}
	}
	in := false
	for i := 1; i <= len(t.lines); i++ {
		l := strings.TrimSpace(t.line(i))
		switch {
		case strings.HasPrefix(l, "import ("):
			in = true
			continue
		case in && strings.HasPrefix(l, ")"):
			in = false
			continue
		case strings.HasPrefix(l, "func ") || strings.HasPrefix(l, "type ") || strings.HasPrefix(l, "var ") || strings.HasPrefix(l, "const "):
			return
		}
		if !in && !strings.HasPrefix(l, "import ") {
			continue
		}
		raw := t.line(i)
		q := strings.IndexByte(raw, '"')
		if q < 0 || have[i] {
			continue
		}
		e := strings.IndexByte(raw[q+1:], '"')
		if e < 0 {
			continue
		}
		col := utf8.RuneCountInString(raw[:q]) + 1
		p := raw[q+1 : q+1+e]
		pa.imports = append(pa.imports, importOcc{file: path, line: i, col: col, n: utf8.RuneCountInString(p) + 2, path: p})
	}
}

func (pa *pkgAnalysis) finish() {
	sort.Slice(pa.occs, func(i, j int) bool {
		a, b := pa.occs[i], pa.occs[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.col < b.col
	})
	for _, o := range pa.occs {
		pa.byFile[o.file] = append(pa.byFile[o.file], o)
		if o.key != "" {
			pa.byKey[o.key] = append(pa.byKey[o.key], o)
		}
	}
}

func (pa *pkgAnalysis) inUnsafe(file string, line, col int) bool {
	for _, r := range pa.unsafe[file] {
		if r.contains(line, col) {
			return true
		}
	}
	return false
}

// at returns the occurrence at a position (rune column).
func (pa *pkgAnalysis) at(file string, line, col int) *occ {
	for _, o := range pa.byFile[file] {
		if o.line == line && col >= o.col && col <= o.col+o.n {
			return o
		}
	}
	return nil
}

func (pa *pkgAnalysis) enclosingFunc(file string, line, col int) *funcRange {
	var best *funcRange
	for _, fr := range pa.funcs[file] {
		if (lrange{fr.sLine, fr.sCol, fr.eLine, fr.eCol}).contains(line, col) {
			best = fr
		}
	}
	return best
}

func structOf(t types.Type) *types.Struct {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem
	}
	if t == nil || t.Underlying() == nil {
		return nil
	}
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem
		if t == nil || t.Underlying() == nil {
			return nil
		}
	}
	st, _ := t.Underlying().(*types.Struct)
	return st
}

func recvTypeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return recvTypeName(e.X)
	}
	return ""
}

func collectRecvTParams(e ast.Expr, names map[string]bool) {
	switch e := e.(type) {
	case *ast.StarExpr:
		collectRecvTParams(e.X, names)
	case *ast.IndexExpr:
		for _, x := range e.Indices {
			if id, ok := x.(*ast.Ident); ok {
				names[id.Name] = true
			}
		}
	}
}

func typeEndLine(t *text, e ast.Expr) int {
	var lb token.Pos
	switch e := e.(type) {
	case *ast.StructType:
		if e.Fields != nil {
			lb = e.Fields.Opening
		}
	case *ast.InterfaceType:
		if e.Methods != nil {
			lb = e.Methods.Opening
		}
	}
	if lb.Line == 0 {
		return 0
	}
	if l, _, ok := t.matchBrace(lb.Line, lb.Column); ok {
		return l
	}
	return 0
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		b[n] = digits[i%10]
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
