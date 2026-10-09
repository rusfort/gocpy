// Package check performs early semantic checks that only need the AST:
// annotation validity, @overload rules and @throws propagation.
// Full type checking will live in internal/types.
package check

import (
	"fmt"
	"sort"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// Severity of a diagnostic.
type Severity int

const (
	Error Severity = iota
	Warning
)

// Diagnostic is a checker message.
type Diagnostic struct {
	Pos      token.Pos
	Severity Severity
	Msg      string
}

func (d Diagnostic) String() string {
	s := "error"
	if d.Severity == Warning {
		s = "warning"
	}
	return fmt.Sprintf("%s: %s: %s", d.Pos, s, d.Msg)
}

// Known annotations and where they may appear.
var known = map[string]bool{
	"inline": true, "noinline": true, "hot": true, "cold": true, "nogc": true,
	"packed": true, "align": true, "export": true, "target": true,
	"throws": true, "overload": true, "deprecated": true, "extern": true,
}

// Package checks a set of files belonging to one package (one directory).
func Package(files []*ast.File) []Diagnostic {
	c := &checker{throwing: map[string]bool{}}
	var funcs []*ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				c.annots(d.Annots)
				funcs = append(funcs, d)
			case *ast.GenDecl:
				c.annots(d.Annots)
				for _, s := range d.Specs {
					ast.Inspect(s, func(n ast.Node) bool {
						if fl, ok := n.(*ast.Field); ok {
							c.annots(fl.Annots)
						}
						return true
					})
				}
			}
		}
	}
	c.overloads(funcs)
	c.throws(funcs)
	sort.SliceStable(c.diags, func(i, j int) bool {
		a, b := c.diags[i].Pos, c.diags[j].Pos
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return c.diags
}

type checker struct {
	diags    []Diagnostic
	throwing map[string]bool // function key -> declared @throws
}

func (c *checker) errorf(p token.Pos, f string, a ...any) {
	c.diags = append(c.diags, Diagnostic{p, Error, fmt.Sprintf(f, a...)})
}
func (c *checker) warnf(p token.Pos, f string, a ...any) {
	c.diags = append(c.diags, Diagnostic{p, Warning, fmt.Sprintf(f, a...)})
}

func (c *checker) annots(as ast.Annotations) {
	seen := map[string]bool{}
	for _, a := range as {
		if !known[a.Name] {
			c.errorf(a.At, "unknown annotation @%s", a.Name)
		}
		if seen[a.Name] {
			c.errorf(a.At, "duplicate annotation @%s", a.Name)
		}
		seen[a.Name] = true
	}
	if seen["inline"] && seen["noinline"] {
		c.errorf(as[0].At, "@inline and @noinline are mutually exclusive")
	}
}

// recvTypeName returns "T" for (x T), (x *T), (x T[K]).
func recvTypeName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	t := fl.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return "?"
		}
	}
}

func funcKey(d *ast.FuncDecl) string {
	if r := recvTypeName(d.Recv); r != "" {
		return r + "." + d.Name.Name
	}
	return d.Name.Name
}

// ---------------------------------------------------------------- @overload

func (c *checker) overloads(funcs []*ast.FuncDecl) {
	groups := map[string][]*ast.FuncDecl{}
	var order []string
	for _, d := range funcs {
		k := funcKey(d)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], d)
	}
	for _, k := range order {
		g := groups[k]
		for _, d := range g {
			if d.IsOperator && !d.Annots.Has("overload") {
				c.errorf(d.Name.Pos(), "operator method %s requires @overload", k)
			}
		}
		if len(g) == 1 {
			d := g[0]
			if d.Annots.Has("overload") && !d.IsOperator {
				c.warnf(d.Name.Pos(), "@overload on %s, but it has only one variant", k)
			}
			continue
		}
		for _, d := range g {
			if !d.Annots.Has("overload") {
				c.errorf(d.Name.Pos(), "%s is declared %d times; every variant must be marked @overload", k, len(g))
			}
			if d.Name.Name == "main" && d.Recv == nil {
				c.errorf(d.Name.Pos(), "main cannot be overloaded")
			}
		}
		// identical parameter shapes are ambiguous
		sigs := map[string]*ast.FuncDecl{}
		for _, d := range g {
			s := paramSig(d.Type.Params)
			if prev, ok := sigs[s]; ok {
				c.errorf(d.Name.Pos(), "ambiguous overload of %s: same parameters (%s) as declaration at %s", k, s, prev.Name.Pos())
				continue
			}
			sigs[s] = d
		}
	}
}

// paramSig renders parameter types (names ignored) for ambiguity detection.
func paramSig(fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	s := ""
	for _, f := range fl.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			if s != "" {
				s += ", "
			}
			s += typeString(f.Type)
		}
	}
	return s
}

func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + typeString(t.Elt)
		}
		if l, ok := t.Len.(*ast.BasicLit); ok {
			return "[" + l.Value + "]" + typeString(t.Elt)
		}
		return "[N]" + typeString(t.Elt)
	case *ast.MapType:
		return "map[" + typeString(t.Key) + "]" + typeString(t.Value)
	case *ast.ChanType:
		return "chan " + typeString(t.Value)
	case *ast.Ellipsis:
		return "..." + typeString(t.Elt)
	case *ast.IndexExpr:
		s := typeString(t.X) + "["
		for i, x := range t.Indices {
			if i > 0 {
				s += ","
			}
			s += typeString(x)
		}
		return s + "]"
	case *ast.FuncType:
		return "func(" + paramSig(t.Params) + ")"
	case *ast.ParenExpr:
		return typeString(t.X)
	}
	return fmt.Sprintf("%T", e)
}

// ---------------------------------------------------------------- @throws

func (c *checker) throws(funcs []*ast.FuncDecl) {
	for _, d := range funcs {
		if d.Annots.Has("throws") {
			// a call by name is considered throwing if ANY variant throws
			c.throwing[d.Name.Name] = true
			c.throwing[funcKey(d)] = true
		}
	}
	for _, d := range funcs {
		declared := d.Annots.Has("throws")
		if d.Recv == nil && d.Name.Name == "main" && declared {
			c.errorf(d.Name.Pos(), "main cannot be @throws: uncaught exceptions must be handled")
		}
		// Whether exceptions can escape is checked precisely (with resolved
		// overloads and methods) by internal/types.
		if d.Body == nil {
			continue
		}
	}
}

type site struct {
	pos  token.Pos
	what string
}

func (c *checker) callThrows(call *ast.CallExpr) (string, bool) {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		if c.throwing[f.Name] {
			return f.Name, true
		}
	case *ast.SelectorExpr:
		// method call: we do not know the receiver type yet, match by name
		if c.throwing[f.Sel.Name] {
			return f.Sel.Name, true
		}
	case *ast.IndexExpr: // generic call F[T](...)
		if id, ok := f.X.(*ast.Ident); ok && c.throwing[id.Name] {
			return id.Name, true
		}
	}
	return "", false
}

// escaping returns throw sites that are not protected by a catch-all.
func (c *checker) escaping(n ast.Node) []site {
	var out []site
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false // closures carry @throws in their own type
		case *ast.ThrowStmt:
			out = append(out, site{x.Throw, "exception"})
		case *ast.CallExpr:
			if name, ok := c.callThrows(x); ok {
				out = append(out, site{x.Pos(), "exception from " + name + "()"})
			}
		case *ast.TryStmt:
			catchAll := false
			for _, cc := range x.Catches {
				if cc.Type == nil {
					catchAll = true
				}
			}
			if !catchAll {
				out = append(out, c.escaping(x.Body)...)
			}
			for _, cc := range x.Catches {
				out = append(out, c.escaping(cc.Body)...)
			}
			if x.Finally != nil {
				out = append(out, c.escaping(x.Finally)...)
			}
			return false
		case *ast.GoStmt:
			return false // checked separately
		}
		return true
	})
	return out
}
