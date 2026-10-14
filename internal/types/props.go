package types

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/token"
)

// Go values used through the Go bridge are Gocpy structs that hold a handle
// (std/gobridge). Their fields are accessed with generated methods:
//
//	x.F          -> x.GocpyGet_F()
//	x.F = v      -> x.GocpySet_F(v)        (also op=, ++ and --)
//	T{F: v}      -> a new Go value with the fields set (also &T{...})
//	new(T)       -> GocpyNew_T()
//
// The first checking pass records such expressions; the files are rewritten
// and checked again.

type goRewrites struct {
	props map[*ast.SelectorExpr]bool
	lits  map[*ast.CompositeLit]*Named
	news  map[*ast.CallExpr]*Named
	keep  map[*ast.SelectorExpr]bool // left as is: reported by the second pass
	n     int
}

func (r *goRewrites) empty() bool {
	return r == nil || len(r.props)+len(r.lits)+len(r.news) == 0
}

func (c *checker) rw() *goRewrites {
	if c.rewrites == nil {
		c.rewrites = &goRewrites{props: map[*ast.SelectorExpr]bool{}, lits: map[*ast.CompositeLit]*Named{}, news: map[*ast.CallExpr]*Named{}, keep: map[*ast.SelectorExpr]bool{}}
	}
	return c.rewrites
}

// goWrapper reports whether n is the Gocpy type of a Go value with a
// constructor, and returns the constructor name.
func (c *checker) goWrapper(t Type) (*Named, string) {
	n := namedOf(t)
	if n == nil || len(n.Methods["GocpyHandle"]) == 0 {
		return nil, ""
	}
	st, ok := n.Underlying().(*Struct)
	if !ok {
		return nil, ""
	}
	if _, f := st.Field("GocpyRef"); f == nil {
		return nil, ""
	}
	name := n.Obj.name
	prefix, base := "", name
	if i := strings.LastIndex(name, "__"); i >= 0 {
		prefix, base = name[:i+2], name[i+2:]
	}
	ctor := prefix + "GocpyNew_" + base
	if _, ok := c.pkg.Scope.Lookup(ctor).(*Func); !ok {
		return n, ""
	}
	return n, ctor
}

// property: x.F of a Go value (nil if there is no such field).
func (c *checker) property(e *ast.SelectorExpr, t Type) *operand {
	n := namedOf(t)
	if n == nil {
		return nil
	}
	ms := n.Methods["GocpyGet_"+e.Sel.Name]
	if len(ms) == 0 {
		return nil
	}
	sig, _ := ms[0].typ.(*Signature)
	if sig == nil || len(sig.Results) != 1 {
		return nil
	}
	if c.rewritten {
		// left after rewriting: a context where a Go field cannot be used
		c.errorf(e.Sel.Pos(), "%s.%s is a field of a Go value: it can be read, assigned alone (=, op=, ++, --) or set in a composite literal, but not addressed",
			exprString(e.X), e.Sel.Name)
		return c.invalid(e)
	}
	c.rw().props[e] = true
	return &operand{mode: Value, typ: sig.Results[0].typ}
}

func ident(pos token.Pos, name string) *ast.Ident { return &ast.Ident{NamePos: pos, Name: name} }

func (r *goRewrites) call(x ast.Expr, pos token.Pos, method string, args ...ast.Expr) ast.Expr {
	return &ast.CallExpr{Fun: &ast.SelectorExpr{X: x, Sel: ident(pos, method)}, Args: args}
}

// literal builds func() T { p := GocpyNew_T(); p.GocpySet_F(v); ...; return *p }()
func (r *goRewrites) literal(lit *ast.CompositeLit, n *Named, ptr bool, ctor string) ast.Expr {
	pos := lit.Pos()
	r.n++
	v := fmt.Sprintf("gocpyLit%d", r.n)
	body := &ast.BlockStmt{Lbrace: pos}
	body.List = append(body.List, &ast.AssignStmt{Lhs: []ast.Expr{ident(pos, v)}, TokPos: pos, Tok: token.DEFINE,
		Rhs: []ast.Expr{&ast.CallExpr{Fun: ident(pos, ctor)}}})
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue // reported by the second pass (positional values)
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		body.List = append(body.List, &ast.ExprStmt{X: r.call(ident(key.Pos(), v), key.Pos(), "GocpySet_"+key.Name, kv.Value)})
	}
	var ret ast.Expr = ident(pos, v)
	typ := ast.Expr(ident(pos, n.Obj.name))
	if !ptr {
		ret = &ast.StarExpr{Star: pos, X: ret}
	} else {
		typ = &ast.StarExpr{Star: pos, X: typ}
	}
	body.List = append(body.List, &ast.ReturnStmt{Return: pos, Results: []ast.Expr{ret}})
	ft := &ast.FuncType{Func: pos, Params: &ast.FieldList{Opening: pos}, Results: &ast.FieldList{Opening: pos, List: []*ast.Field{{Type: typ}}}}
	return &ast.CallExpr{Fun: &ast.FuncLit{Type: ft, Body: body}}
}

func keyedLit(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		if _, ok := kv.Key.(*ast.Ident); !ok {
			return false
		}
	}
	return true
}

func (r *goRewrites) transform(c *checker, node any) any {
	switch x := node.(type) {
	case *ast.SelectorExpr:
		if r.props[x] && !r.keep[x] {
			return r.call(x.X, x.Sel.Pos(), "GocpyGet_"+x.Sel.Name)
		}
	case *ast.AssignStmt:
		if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
			for _, l := range x.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && r.props[sel] {
					r.keep[sel] = true
				}
			}
			break
		}
		sel, ok := x.Lhs[0].(*ast.SelectorExpr)
		if !ok || !r.props[sel] {
			break
		}
		val := x.Rhs[0]
		switch {
		case x.Tok == token.ASSIGN:
		case AssignOp(x.Tok) != 0:
			val = &ast.BinaryExpr{X: r.call(sel.X, sel.Sel.Pos(), "GocpyGet_"+sel.Sel.Name), OpPos: x.TokPos, Op: AssignOp(x.Tok), Y: &ast.ParenExpr{Lparen: val.Pos(), X: val}}
		default:
			return node
		}
		return &ast.ExprStmt{X: r.call(sel.X, sel.Sel.Pos(), "GocpySet_"+sel.Sel.Name, val)}
	case *ast.IncDecStmt:
		sel, ok := x.X.(*ast.SelectorExpr)
		if !ok || !r.props[sel] {
			break
		}
		op := token.ADD
		if x.Tok == token.DEC {
			op = token.SUB
		}
		one := &ast.BasicLit{ValuePos: sel.Sel.Pos(), Kind: token.INT, Value: "1"}
		val := &ast.BinaryExpr{X: r.call(sel.X, sel.Sel.Pos(), "GocpyGet_"+sel.Sel.Name), OpPos: sel.Sel.Pos(), Op: op, Y: one}
		return &ast.ExprStmt{X: r.call(sel.X, sel.Sel.Pos(), "GocpySet_"+sel.Sel.Name, val)}
	case *ast.UnaryExpr:
		if x.Op != token.AND {
			break
		}
		if sel, ok := x.X.(*ast.SelectorExpr); ok && r.props[sel] {
			r.keep[sel] = true
		}
		if lit, ok := x.X.(*ast.CompositeLit); ok {
			if n := r.lits[lit]; n != nil && keyedLit(lit) {
				_, ctor := c.goWrapper(n)
				delete(r.lits, lit)
				return r.literal(lit, n, true, ctor)
			}
		}
	case *ast.CompositeLit:
		if n := r.lits[x]; n != nil && keyedLit(x) {
			_, ctor := c.goWrapper(n)
			return r.literal(x, n, false, ctor)
		}
	case *ast.CallExpr:
		if n := r.news[x]; n != nil {
			_, ctor := c.goWrapper(n)
			return &ast.CallExpr{Fun: ident(x.Fun.Pos(), ctor)}
		}
	}
	return node
}

var (
	astPkg   = reflect.TypeOf(ast.File{}).PkgPath()
	exprType = reflect.TypeOf((*ast.Expr)(nil)).Elem()
	stmtType = reflect.TypeOf((*ast.Stmt)(nil)).Elem()
)

// rewrite walks v (pre-order) and replaces Expr and Stmt values.
func (r *goRewrites) rewrite(c *checker, v reflect.Value) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		if (v.Type() == exprType || v.Type() == stmtType) && v.CanSet() {
			nv := r.transform(c, v.Interface())
			if nv != v.Interface() {
				v.Set(reflect.ValueOf(nv))
			}
		}
		r.rewrite(c, v.Elem())
	case reflect.Pointer:
		if v.IsNil() || v.Type().Elem().PkgPath() != astPkg {
			return
		}
		r.rewrite(c, v.Elem())
	case reflect.Struct:
		if v.Type().PkgPath() != astPkg {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				r.rewrite(c, v.Field(i))
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			r.rewrite(c, v.Index(i))
		}
	}
}
