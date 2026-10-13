package ast

import (
	"reflect"

	"github.com/rusfort/gocpy/internal/token"
)

// CloneExpr returns a deep copy of e. When dropPos is set, all positions in
// the copy are cleared, so tools that map nodes to source text (semantic
// highlighting, references) ignore the copy.
func CloneExpr(e Expr, dropPos bool) Expr {
	if e == nil {
		return nil
	}
	return cloneValue(reflect.ValueOf(e), dropPos).Interface().(Expr)
}

func cloneValue(v reflect.Value, dropPos bool) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		n := reflect.New(v.Elem().Type())
		n.Elem().Set(cloneValue(v.Elem(), dropPos))
		return n
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		c := cloneValue(v.Elem(), dropPos)
		n := reflect.New(v.Type()).Elem()
		n.Set(c)
		return n
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			n.Index(i).Set(cloneValue(v.Index(i), dropPos))
		}
		return n
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			n.SetMapIndex(it.Key(), cloneValue(it.Value(), dropPos))
		}
		return n
	case reflect.Struct:
		if v.Type() == posType {
			if dropPos {
				return reflect.Zero(posType)
			}
			return v
		}
		n := reflect.New(v.Type()).Elem()
		n.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if n.Field(i).CanSet() {
				n.Field(i).Set(cloneValue(v.Field(i), dropPos))
			}
		}
		return n
	}
	return v
}

// ReplaceIota replaces every identifier iota in e with the integer literal n.
func ReplaceIota(e *Expr, n string) {
	replaceIota(reflect.ValueOf(e).Elem(), n)
}

var identPtr = reflect.TypeOf((*Ident)(nil))

func replaceIota(v reflect.Value, n string) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		if id, ok := v.Interface().(*Ident); ok && id.Name == "iota" {
			if v.Type() == reflect.TypeOf((*Expr)(nil)).Elem() {
				v.Set(reflect.ValueOf(Expr(&BasicLit{ValuePos: id.NamePos, Kind: token.INT, Value: n})))
			}
			return
		}
		replaceIota(v.Elem(), n)
	case reflect.Ptr:
		if v.IsNil() || v.Type() == identPtr {
			return
		}
		replaceIota(v.Elem(), n)
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			replaceIota(v.Index(i), n)
		}
	case reflect.Struct:
		if v.Type() == posType {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				replaceIota(v.Field(i), n)
			}
		}
	}
}
