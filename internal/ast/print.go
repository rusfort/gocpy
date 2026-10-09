package ast

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/rusfort/gocpy/internal/token"
)

var (
	posType  = reflect.TypeOf(token.Pos{})
	kindType = reflect.TypeOf(token.Kind(0))
	nodeType = reflect.TypeOf((*Node)(nil)).Elem()
)

// Fprint writes a readable tree dump of n (positions omitted).
func Fprint(w io.Writer, n any) {
	p := &printer{w: w}
	p.value(reflect.ValueOf(n), 0)
	fmt.Fprintln(w)
}

// Sprint returns the tree dump as a string.
func Sprint(n any) string {
	var sb strings.Builder
	Fprint(&sb, n)
	return sb.String()
}

type printer struct{ w io.Writer }

func (p *printer) indent(d int) { fmt.Fprint(p.w, strings.Repeat("  ", d)) }

func (p *printer) value(v reflect.Value, d int) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			fmt.Fprint(p.w, "nil")
			return
		}
		p.value(v.Elem(), d)
	case reflect.Ptr:
		if v.IsNil() {
			fmt.Fprint(p.w, "nil")
			return
		}
		p.value(v.Elem(), d)
	case reflect.Slice:
		if v.Len() == 0 {
			fmt.Fprint(p.w, "[]")
			return
		}
		fmt.Fprintln(p.w, "[")
		for i := 0; i < v.Len(); i++ {
			p.indent(d + 1)
			p.value(v.Index(i), d+1)
			fmt.Fprintln(p.w)
		}
		p.indent(d)
		fmt.Fprint(p.w, "]")
	case reflect.Struct:
		t := v.Type()
		// compact forms
		switch x := v.Addr().Interface().(type) {
		case *Ident:
			fmt.Fprintf(p.w, "Ident(%s)", x.Name)
			return
		case *BasicLit:
			fmt.Fprintf(p.w, "%s(%s)", x.Kind, x.Value)
			return
		}
		fmt.Fprintf(p.w, "%s {", t.Name())
		any := false
		for i := 0; i < t.NumField(); i++ {
			f := v.Field(i)
			if f.Type() == posType || isZero(f) {
				continue
			}
			if !any {
				fmt.Fprintln(p.w)
				any = true
			}
			p.indent(d + 1)
			fmt.Fprintf(p.w, "%s: ", t.Field(i).Name)
			p.value(f, d+1)
			fmt.Fprintln(p.w)
		}
		if any {
			p.indent(d)
		}
		fmt.Fprint(p.w, "}")
	default:
		if v.Type() == kindType {
			fmt.Fprint(p.w, token.Kind(v.Int()).String())
			return
		}
		if v.Kind() == reflect.String {
			fmt.Fprintf(p.w, "%q", v.String())
			return
		}
		fmt.Fprint(p.w, v.Interface())
	}
}

func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice:
		return v.IsNil() || (v.Kind() == reflect.Slice && v.Len() == 0)
	}
	return v.IsZero()
}

// Inspect traverses the tree in depth-first order, calling f for every
// node. If f returns false, children of that node are skipped.
func Inspect(n Node, f func(Node) bool) {
	if n == nil || reflect.ValueOf(n).IsNil() {
		return
	}
	if !f(n) {
		return
	}
	walkChildren(reflect.ValueOf(n), f)
}

func walkChildren(v reflect.Value, f func(Node) bool) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < v.NumField(); i++ {
		visitValue(v.Field(i), f)
	}
}

func visitValue(fv reflect.Value, f func(Node) bool) {
	switch fv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if fv.IsNil() {
			return
		}
		if fv.Type().Implements(nodeType) || (fv.Kind() == reflect.Interface && fv.Elem().Type().Implements(nodeType)) {
			Inspect(fv.Interface().(Node), f)
		}
	case reflect.Slice:
		for j := 0; j < fv.Len(); j++ {
			visitValue(fv.Index(j), f)
		}
	}
}
