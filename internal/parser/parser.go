// Package parser implements a hand-written recursive-descent parser for Gocpy.
package parser

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/lexer"
	"github.com/rusfort/gocpy/internal/token"
)

// Error is a syntax error.
type Error struct {
	Pos token.Pos
	Msg string
}

func (e Error) Error() string { return fmt.Sprintf("%s: %s", e.Pos, e.Msg) }

// ErrorList is returned when parsing fails.
type ErrorList []Error

func (l ErrorList) Error() string {
	var sb strings.Builder
	for i, e := range l {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(e.Error())
	}
	return sb.String()
}

const maxErrors = 20

type bailout struct{}

type parser struct {
	toks []token.Token
	i    int
	tok  token.Token

	// exprLev < 0: in control clause (if/for/switch header) where
	// `T {` must not be parsed as a composite literal.
	exprLev int

	errors ErrorList
}

// ParseFile parses one .gocpy source file.
func ParseFile(filename string, src []byte) (f *ast.File, err error) {
	toks, lx := lexer.TokenizeLexer(filename, src)
	p := &parser{toks: toks}
	for _, e := range lx.Errors {
		p.errors = append(p.errors, Error{e.Pos, e.Msg})
	}
	p.tok = p.toks[0]
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
		}
		if len(p.errors) > 0 {
			err = p.errors
		}
	}()
	f = p.parseFile()
	f.Directives = lx.Directives
	return f, err
}

// ParseExpr parses a single expression (used for tests and f-strings).
func ParseExpr(src string) (x ast.Expr, err error) {
	toks, lexErrs := lexer.Tokenize("", []byte(src))
	p := &parser{toks: toks}
	for _, e := range lexErrs {
		p.errors = append(p.errors, Error{e.Pos, e.Msg})
	}
	p.tok = p.toks[0]
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(bailout); !ok {
				panic(r)
			}
		}
		if len(p.errors) > 0 {
			err = p.errors
		}
	}()
	x = p.parseExpr()
	if p.tok.Kind == token.SEMICOLON && p.tok.Lit == "\n" {
		p.next()
	}
	if p.tok.Kind != token.EOF {
		p.errorf(p.tok.Pos, "unexpected %s after expression", p.tok.Kind)
	}
	return x, err
}

// ---------------------------------------------------------------- helpers

func (p *parser) next() {
	if p.i < len(p.toks)-1 {
		p.i++
	}
	p.tok = p.toks[p.i]
}

func (p *parser) peek(n int) token.Token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) errorf(pos token.Pos, format string, args ...any) {
	p.errors = append(p.errors, Error{pos, fmt.Sprintf(format, args...)})
	if len(p.errors) >= maxErrors {
		panic(bailout{})
	}
}

func describe(t token.Token) string {
	switch {
	case t.Kind == token.SEMICOLON && t.Lit == "\n":
		return "newline"
	case t.Kind == token.IDENT:
		return "name " + t.Lit
	case t.Kind.IsLiteral():
		return "literal " + t.Lit
	case t.Kind.IsKeyword():
		return "keyword " + t.Kind.String()
	}
	return "'" + t.Kind.String() + "'"
}

func (p *parser) expect(k token.Kind) token.Pos {
	pos := p.tok.Pos
	if p.tok.Kind != k {
		p.errorf(pos, "expected '%s', found %s", k, describe(p.tok))
		p.advanceTo(k)
	}
	p.next()
	return pos
}

// advanceTo skips tokens for error recovery.
func (p *parser) advanceTo(k token.Kind) {
	for p.tok.Kind != token.EOF && p.tok.Kind != k &&
		p.tok.Kind != token.SEMICOLON && p.tok.Kind != token.RBRACE {
		p.next()
	}
	if p.tok.Kind != k {
		// do not consume statement terminators while recovering
		p.i--
		if p.i < 0 {
			p.i = 0
		}
		p.tok = p.toks[p.i]
	}
}

func (p *parser) got(k token.Kind) bool {
	if p.tok.Kind == k {
		p.next()
		return true
	}
	return false
}

// expectSemi consumes a statement terminator.
func (p *parser) expectSemi() {
	switch p.tok.Kind {
	case token.RPAREN, token.RBRACE:
		return // optional before closing
	case token.SEMICOLON:
		p.next()
	default:
		p.errorf(p.tok.Pos, "unexpected %s at end of statement", describe(p.tok))
		for p.tok.Kind != token.SEMICOLON && p.tok.Kind != token.RBRACE && p.tok.Kind != token.EOF {
			p.next()
		}
		p.got(token.SEMICOLON)
	}
}

func (p *parser) skipSemis() {
	for p.tok.Kind == token.SEMICOLON {
		p.next()
	}
}

func (p *parser) parseIdent() *ast.Ident {
	pos := p.tok.Pos
	name := "_"
	if p.tok.Kind == token.IDENT {
		name = p.tok.Lit
		p.next()
	} else {
		p.expect(token.IDENT)
	}
	return &ast.Ident{NamePos: pos, Name: name}
}

func (p *parser) parseIdentList() []*ast.Ident {
	list := []*ast.Ident{p.parseIdent()}
	for p.got(token.COMMA) {
		list = append(list, p.parseIdent())
	}
	return list
}

// ---------------------------------------------------------------- file

func (p *parser) parseFile() *ast.File {
	f := &ast.File{}
	p.skipSemis()
	f.Package = p.expect(token.PACKAGE)
	f.Name = p.parseIdent()
	p.expectSemi()
	p.skipSemis()

	for p.tok.Kind == token.IMPORT {
		p.next()
		if p.got(token.LPAREN) {
			p.skipSemis()
			for p.tok.Kind != token.RPAREN && p.tok.Kind != token.EOF {
				f.Imports = append(f.Imports, p.parseImportSpec())
				p.expectSemi()
				p.skipSemis()
			}
			p.expect(token.RPAREN)
		} else {
			f.Imports = append(f.Imports, p.parseImportSpec())
		}
		p.expectSemi()
		p.skipSemis()
	}

	for p.tok.Kind != token.EOF {
		if d := p.parseDecl(true); d != nil {
			f.Decls = append(f.Decls, d)
		}
		p.skipSemis()
	}
	return f
}

func (p *parser) parseImportSpec() *ast.ImportSpec {
	s := &ast.ImportSpec{}
	if p.tok.Kind == token.IDENT {
		s.Alias = p.parseIdent()
	} else if p.tok.Kind == token.PERIOD {
		s.Alias = &ast.Ident{NamePos: p.tok.Pos, Name: "."}
		p.next()
	}
	if p.tok.Kind != token.STRING {
		p.errorf(p.tok.Pos, "expected import path string, found %s", describe(p.tok))
		p.next()
		s.Path = &ast.BasicLit{Kind: token.STRING, Value: `""`}
		return s
	}
	s.Path = &ast.BasicLit{ValuePos: p.tok.Pos, Kind: token.STRING, Value: p.tok.Lit}
	p.next()
	return s
}

// ---------------------------------------------------------------- annotations

func (p *parser) parseAnnotations() ast.Annotations {
	var list ast.Annotations
	for p.tok.Kind == token.AT {
		a := &ast.Annotation{At: p.tok.Pos}
		p.next()
		if p.tok.Kind == token.IDENT || p.tok.Kind.IsKeyword() {
			a.Name = p.tok.Lit
			p.next()
		} else {
			p.errorf(p.tok.Pos, "expected annotation name after '@'")
		}
		if p.tok.Kind == token.LPAREN {
			a.Args = p.parseCallArgs(nil).Args
		}
		list = append(list, a)
		p.skipSemis() // annotations usually sit on their own line
	}
	return list
}

// ---------------------------------------------------------------- declarations

func (p *parser) parseDecl(topLevel bool) ast.Decl {
	annots := p.parseAnnotations()
	switch p.tok.Kind {
	case token.FUNC:
		return p.parseFuncDecl(annots)
	case token.TYPE, token.VAR, token.CONST:
		return p.parseGenDecl(annots)
	}
	p.errorf(p.tok.Pos, "expected declaration, found %s", describe(p.tok))
	for p.tok.Kind != token.EOF && p.tok.Kind != token.FUNC && p.tok.Kind != token.TYPE &&
		p.tok.Kind != token.VAR && p.tok.Kind != token.CONST && p.tok.Kind != token.AT {
		p.next()
	}
	return nil
}

func (p *parser) parseGenDecl(annots ast.Annotations) *ast.GenDecl {
	d := &ast.GenDecl{Annots: annots, TokPos: p.tok.Pos, Tok: p.tok.Kind}
	p.next()
	spec := func() ast.Node {
		if d.Tok == token.TYPE {
			return p.parseTypeSpec()
		}
		return p.parseValueSpec(d.Tok == token.CONST)
	}
	if p.got(token.LPAREN) {
		p.skipSemis()
		for p.tok.Kind != token.RPAREN && p.tok.Kind != token.EOF {
			d.Specs = append(d.Specs, spec())
			p.expectSemi()
			p.skipSemis()
		}
		p.expect(token.RPAREN)
	} else {
		d.Specs = append(d.Specs, spec())
	}
	p.expectSemi()
	return d
}

func (p *parser) parseTypeSpec() *ast.TypeSpec {
	s := &ast.TypeSpec{Name: p.parseIdent()}
	// `type List[T any] ...` vs `type Buf [N]byte`
	if p.tok.Kind == token.LBRACK && p.peek(1).Kind == token.IDENT && p.peek(2).Kind != token.RBRACK {
		s.TypeParams = p.parseTypeParams()
	}
	if p.got(token.ASSIGN) {
		s.Alias = true
	}
	s.Type = p.parseType()
	return s
}

func (p *parser) parseValueSpec(isConst bool) *ast.ValueSpec {
	s := &ast.ValueSpec{Names: p.parseIdentList()}
	if p.tok.Kind != token.ASSIGN && p.tok.Kind != token.SEMICOLON && p.tok.Kind != token.RPAREN {
		s.Type = p.parseType()
	}
	if p.got(token.ASSIGN) {
		s.Values = p.parseExprList()
	}
	if isConst && s.Values == nil && s.Type != nil {
		p.errorf(s.Pos(), "const declaration requires a value")
	}
	return s
}

// operator method names allowed with @overload
var operatorNames = map[token.Kind]string{
	token.ADD: "+", token.SUB: "-", token.MUL: "*", token.QUO: "/", token.REM: "%",
	token.EQL: "==", token.LSS: "<", token.SHL: "<<", token.SHR: ">>",
	token.AND: "&", token.OR: "|", token.XOR: "^", token.NOT: "!",
}

func (p *parser) parseFuncDecl(annots ast.Annotations) *ast.FuncDecl {
	d := &ast.FuncDecl{Annots: annots}
	funcPos := p.expect(token.FUNC)

	if p.tok.Kind == token.LPAREN {
		d.Recv = p.parseParams()
	}

	// name: identifier or operator (operators only for methods)
	switch {
	case p.tok.Kind == token.IDENT:
		d.Name = p.parseIdent()
	case p.tok.Kind == token.LBRACK && p.peek(1).Kind == token.RBRACK:
		d.Name = &ast.Ident{NamePos: p.tok.Pos, Name: "[]"}
		d.IsOperator = true
		p.next()
		p.next()
	case operatorNames[p.tok.Kind] != "":
		d.Name = &ast.Ident{NamePos: p.tok.Pos, Name: operatorNames[p.tok.Kind]}
		d.IsOperator = true
		p.next()
	default:
		p.errorf(p.tok.Pos, "expected function name, found %s", describe(p.tok))
		d.Name = &ast.Ident{NamePos: p.tok.Pos, Name: "_"}
	}
	if d.IsOperator && d.Recv == nil {
		p.errorf(d.Name.Pos(), "operator %s must be declared as a method", d.Name.Name)
	}

	ft := &ast.FuncType{Func: funcPos}
	if p.tok.Kind == token.LBRACK {
		if d.IsOperator {
			p.errorf(p.tok.Pos, "operator methods cannot have type parameters")
		}
		ft.TypeParams = p.parseTypeParams()
	}
	ft.Params, ft.Results = p.parseSignature()
	d.Type = ft

	if p.tok.Kind == token.LBRACE {
		p.exprLev++
		d.Body = p.parseBlock()
		p.exprLev--
	}
	p.expectSemi()
	return d
}

// ---------------------------------------------------------------- types

func (p *parser) tryType() ast.Expr {
	switch p.tok.Kind {
	case token.IDENT:
		return p.parseTypeName()
	case token.MUL:
		pos := p.tok.Pos
		p.next()
		return &ast.StarExpr{Star: pos, X: p.parseType()}
	case token.LBRACK:
		pos := p.tok.Pos
		p.next()
		if p.got(token.RBRACK) {
			return &ast.ArrayType{Lbrack: pos, Elt: p.parseType()}
		}
		p.exprLev++
		var n ast.Expr
		if p.tok.Kind == token.ELLIPSIS {
			n = &ast.Ellipsis{EllipsisPos: p.tok.Pos}
			p.next()
		} else {
			n = p.parseRhs()
		}
		p.exprLev--
		p.expect(token.RBRACK)
		return &ast.ArrayType{Lbrack: pos, Len: n, Elt: p.parseType()}
	case token.MAP:
		return p.parseMapType()
	case token.CHAN:
		return p.parseChanType()
	case token.FUNC:
		return p.parseFuncType(nil)
	case token.AT:
		annots := p.parseAnnotations()
		if p.tok.Kind != token.FUNC {
			p.errorf(p.tok.Pos, "annotations in type position are only allowed before func types")
			return p.tryType()
		}
		return p.parseFuncType(annots)
	case token.STRUCT:
		return p.parseStructType()
	case token.INTERFACE:
		return p.parseInterfaceType()
	case token.LPAREN:
		pos := p.tok.Pos
		p.next()
		t := p.parseType()
		p.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: pos, X: t}
	}
	return nil
}

func (p *parser) parseType() ast.Expr {
	t := p.tryType()
	if t == nil {
		p.errorf(p.tok.Pos, "expected type, found %s", describe(p.tok))
		pos := p.tok.Pos
		p.next()
		return &ast.Ident{NamePos: pos, Name: "_"}
	}
	return t
}

// parseTypeName: T, pkg.T, T[A, B]
func (p *parser) parseTypeName() ast.Expr {
	var x ast.Expr = p.parseIdent()
	if p.tok.Kind == token.PERIOD {
		p.next()
		x = &ast.SelectorExpr{X: x, Sel: p.parseIdent()}
	}
	if p.tok.Kind == token.LBRACK && p.peek(1).Kind != token.RBRACK {
		p.next()
		p.exprLev++
		idx := &ast.IndexExpr{X: x}
		for {
			idx.Indices = append(idx.Indices, p.parseType())
			if !p.got(token.COMMA) || p.tok.Kind == token.RBRACK {
				break
			}
		}
		p.exprLev--
		p.expect(token.RBRACK)
		x = idx
	}
	return x
}

func (p *parser) parseMapType() *ast.MapType {
	pos := p.expect(token.MAP)
	p.expect(token.LBRACK)
	k := p.parseType()
	p.expect(token.RBRACK)
	return &ast.MapType{Map: pos, Key: k, Value: p.parseType()}
}

// chan T  |  chan[T]
func (p *parser) parseChanType() *ast.ChanType {
	pos := p.expect(token.CHAN)
	if p.got(token.LBRACK) {
		t := p.parseType()
		p.expect(token.RBRACK)
		return &ast.ChanType{Begin: pos, Value: t}
	}
	return &ast.ChanType{Begin: pos, Value: p.parseType()}
}

func (p *parser) parseFuncType(annots ast.Annotations) *ast.FuncType {
	ft := &ast.FuncType{Func: p.expect(token.FUNC), Annots: annots}
	ft.Params, ft.Results = p.parseSignature()
	return ft
}

func (p *parser) parseSignature() (params, results *ast.FieldList) {
	params = p.parseParams()
	if p.tok.Kind == token.LPAREN {
		results = p.parseParams()
	} else if t := p.tryResultType(); t != nil {
		results = &ast.FieldList{Opening: t.Pos(), List: []*ast.Field{{Type: t}}}
	}
	return
}

func (p *parser) tryResultType() ast.Expr {
	switch p.tok.Kind {
	case token.IDENT, token.MUL, token.LBRACK, token.MAP, token.CHAN,
		token.FUNC, token.STRUCT, token.INTERFACE:
		return p.tryType()
	}
	return nil
}

func (p *parser) parseTypeParams() *ast.FieldList {
	fl := &ast.FieldList{Opening: p.expect(token.LBRACK)}
	for p.tok.Kind != token.RBRACK && p.tok.Kind != token.EOF {
		names := p.parseIdentList()
		fl.List = append(fl.List, &ast.Field{Names: names, Type: p.parseConstraint()})
		if !p.got(token.COMMA) {
			break
		}
	}
	p.expect(token.RBRACK)
	if len(fl.List) == 0 {
		p.errorf(fl.Opening, "empty type parameter list")
	}
	return fl
}

// parseConstraint: T | ~U | V
func (p *parser) parseConstraint() ast.Expr {
	term := func() ast.Expr {
		if p.tok.Kind == token.TILDE {
			pos := p.tok.Pos
			p.next()
			return &ast.UnaryExpr{OpPos: pos, Op: token.TILDE, X: p.parseType()}
		}
		return p.parseType()
	}
	x := term()
	for p.tok.Kind == token.OR {
		pos := p.tok.Pos
		p.next()
		x = &ast.BinaryExpr{X: x, OpPos: pos, Op: token.OR, Y: term()}
	}
	return x
}

func isTypeStart(k token.Kind) bool {
	switch k {
	case token.IDENT, token.MUL, token.LBRACK, token.MAP, token.CHAN, token.FUNC,
		token.STRUCT, token.INTERFACE, token.ELLIPSIS, token.LPAREN, token.AT:
		return true
	}
	return false
}

// matchingBracket returns the index of the token after the bracket that
// matches the one at p.i+off.
func (p *parser) afterMatching(off int) int {
	depth := 0
	for j := p.i + off; j < len(p.toks); j++ {
		switch p.toks[j].Kind {
		case token.LBRACK, token.LPAREN, token.LBRACE:
			depth++
		case token.RBRACK, token.RPAREN, token.RBRACE:
			depth--
			if depth == 0 {
				return j + 1
			}
		case token.EOF:
			return j
		}
	}
	return len(p.toks) - 1
}

// parseParams parses ( a, b int, c ...string ) or ( int, string ).
func (p *parser) parseParams() *ast.FieldList {
	fl := &ast.FieldList{Opening: p.expect(token.LPAREN)}

	type entry struct {
		name *ast.Ident
		typ  ast.Expr
	}
	var entries []entry
	named := false

	for p.tok.Kind != token.RPAREN && p.tok.Kind != token.EOF {
		p.skipSemis()
		if p.tok.Kind == token.IDENT && p.isNamedParam() {
			name := p.parseIdent()
			entries = append(entries, entry{name, p.parseParamType()})
			named = true
		} else {
			entries = append(entries, entry{nil, p.parseParamType()})
		}
		p.skipSemis()
		if !p.got(token.COMMA) {
			break
		}
	}
	p.skipSemis()
	p.expect(token.RPAREN)

	if !named {
		for _, e := range entries {
			fl.List = append(fl.List, &ast.Field{Type: e.typ})
		}
		return fl
	}
	// grouped names: a, b int
	var pending []*ast.Ident
	for _, e := range entries {
		if e.name == nil {
			id, ok := e.typ.(*ast.Ident)
			if !ok {
				p.errorf(e.typ.Pos(), "mixed named and unnamed parameters")
				continue
			}
			pending = append(pending, id)
			continue
		}
		fl.List = append(fl.List, &ast.Field{Names: append(pending, e.name), Type: e.typ})
		pending = nil
	}
	if len(pending) > 0 {
		p.errorf(pending[0].Pos(), "missing parameter type")
	}
	return fl
}

// isNamedParam: at IDENT, decide whether it is `name Type`.
func (p *parser) isNamedParam() bool {
	nk := p.peek(1).Kind
	switch nk {
	case token.COMMA, token.RPAREN, token.PERIOD:
		return false
	case token.LBRACK:
		// name []T / name [5]T  vs  Generic[T]
		if p.peek(2).Kind == token.RBRACK || p.peek(2).Kind == token.INT || p.peek(2).Kind == token.ELLIPSIS {
			return true
		}
		after := p.toks[p.afterMatching(1)].Kind
		return after != token.COMMA && after != token.RPAREN
	}
	return isTypeStart(nk)
}

func (p *parser) parseParamType() ast.Expr {
	if p.tok.Kind == token.ELLIPSIS {
		pos := p.tok.Pos
		p.next()
		return &ast.Ellipsis{EllipsisPos: pos, Elt: p.parseType()}
	}
	return p.parseType()
}

func (p *parser) parseStructType() *ast.StructType {
	st := &ast.StructType{Struct: p.expect(token.STRUCT)}
	fl := &ast.FieldList{Opening: p.expect(token.LBRACE)}
	p.skipSemis()
	for p.tok.Kind != token.RBRACE && p.tok.Kind != token.EOF {
		f := &ast.Field{Annots: p.parseAnnotations()}
		if p.tok.Kind == token.MUL || (p.tok.Kind == token.IDENT &&
			(p.peek(1).Kind == token.SEMICOLON || p.peek(1).Kind == token.RBRACE ||
				p.peek(1).Kind == token.PERIOD || p.peek(1).Kind == token.STRING)) {
			f.Type = p.parseType() // embedded
		} else {
			f.Names = p.parseIdentList()
			f.Type = p.parseType()
		}
		if p.tok.Kind == token.STRING {
			f.Tag = &ast.BasicLit{ValuePos: p.tok.Pos, Kind: token.STRING, Value: p.tok.Lit}
			p.next()
		}
		fl.List = append(fl.List, f)
		p.expectSemi()
		p.skipSemis()
	}
	p.expect(token.RBRACE)
	st.Fields = fl
	return st
}

func (p *parser) parseInterfaceType() *ast.InterfaceType {
	it := &ast.InterfaceType{Interface: p.expect(token.INTERFACE)}
	fl := &ast.FieldList{Opening: p.expect(token.LBRACE)}
	p.skipSemis()
	for p.tok.Kind != token.RBRACE && p.tok.Kind != token.EOF {
		annots := p.parseAnnotations()
		if p.tok.Kind == token.IDENT && p.peek(1).Kind == token.LPAREN {
			name := p.parseIdent()
			ft := &ast.FuncType{Func: name.Pos(), Annots: annots}
			ft.Params, ft.Results = p.parseSignature()
			fl.List = append(fl.List, &ast.Field{Annots: annots, Names: []*ast.Ident{name}, Type: ft})
		} else {
			fl.List = append(fl.List, &ast.Field{Annots: annots, Type: p.parseConstraint()})
		}
		p.expectSemi()
		p.skipSemis()
	}
	p.expect(token.RBRACE)
	it.Methods = fl
	return it
}

// ---------------------------------------------------------------- statements

func (p *parser) parseBlock() *ast.BlockStmt {
	b := &ast.BlockStmt{Lbrace: p.expect(token.LBRACE)}
	b.List = p.parseStmtList()
	p.expect(token.RBRACE)
	return b
}

func (p *parser) parseStmtList() []ast.Stmt {
	var list []ast.Stmt
	p.skipSemis()
	for p.tok.Kind != token.RBRACE && p.tok.Kind != token.EOF &&
		p.tok.Kind != token.CASE && p.tok.Kind != token.DEFAULT {
		if s := p.parseStmt(); s != nil {
			list = append(list, s)
		}
		p.skipSemis()
	}
	return list
}

func (p *parser) parseStmt() ast.Stmt {
	switch p.tok.Kind {
	case token.VAR, token.CONST, token.TYPE:
		return &ast.DeclStmt{Decl: p.parseGenDecl(nil)}
	case token.LBRACE:
		b := p.parseBlock()
		p.expectSemi()
		return b
	case token.IF:
		s := p.parseIf()
		p.expectSemi()
		return s
	case token.FOR:
		s := p.parseFor()
		p.expectSemi()
		return s
	case token.SWITCH:
		s := p.parseSwitch()
		p.expectSemi()
		return s
	case token.SELECT:
		s := p.parseSelect()
		p.expectSemi()
		return s
	case token.TRY:
		s := p.parseTry()
		p.expectSemi()
		return s
	case token.UNSAFE:
		pos := p.tok.Pos
		p.next()
		s := &ast.UnsafeStmt{Unsafe: pos, Body: p.parseBlock()}
		p.expectSemi()
		return s
	case token.RETURN:
		s := &ast.ReturnStmt{Return: p.tok.Pos}
		p.next()
		if p.tok.Kind != token.SEMICOLON && p.tok.Kind != token.RBRACE {
			s.Results = p.parseExprList()
		}
		p.expectSemi()
		return s
	case token.THROW:
		s := &ast.ThrowStmt{Throw: p.tok.Pos}
		p.next()
		s.X = p.parseExpr()
		p.expectSemi()
		return s
	case token.GO, token.DEFER:
		kind, pos := p.tok.Kind, p.tok.Pos
		p.next()
		x := p.parseExpr()
		call, ok := x.(*ast.CallExpr)
		if !ok {
			p.errorf(x.Pos(), "expression in %s must be a function call", kind)
			call = &ast.CallExpr{Fun: x}
		}
		p.expectSemi()
		if kind == token.GO {
			return &ast.GoStmt{Go: pos, Call: call}
		}
		return &ast.DeferStmt{Defer: pos, Call: call}
	case token.BREAK, token.CONTINUE, token.FALLTHROUGH:
		s := &ast.BranchStmt{TokPos: p.tok.Pos, Tok: p.tok.Kind}
		p.next()
		if p.tok.Kind == token.IDENT && s.Tok != token.FALLTHROUGH {
			s.Label = p.parseIdent()
		}
		p.expectSemi()
		return s
	case token.SEMICOLON:
		s := &ast.EmptyStmt{Semicolon: p.tok.Pos}
		p.next()
		return s
	case token.FUNC:
		// func literal as statement, e.g. func() { ... }()
		s := p.parseSimpleStmt()
		p.expectSemi()
		return s
	}
	if p.tok.Kind == token.AT && p.peek(2).Kind != token.LPAREN && p.peek(1).Kind == token.IDENT {
		// statement-level annotations are reserved for future use (@likely, @unroll ...)
		p.errorf(p.tok.Pos, "annotations are not allowed on statements yet")
		p.parseAnnotations()
	}
	s := p.parseSimpleStmt()
	p.expectSemi()
	return s
}

func (p *parser) parseSimpleStmt() ast.Stmt {
	lhs := p.parseExprList()
	switch p.tok.Kind {
	case token.DEFINE, token.ASSIGN, token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN,
		token.QUO_ASSIGN, token.REM_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN,
		token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN, token.AND_NOT_ASSIGN:
		s := &ast.AssignStmt{Lhs: lhs, TokPos: p.tok.Pos, Tok: p.tok.Kind}
		p.next()
		s.Rhs = p.parseExprList()
		if s.Tok == token.DEFINE {
			for _, x := range lhs {
				if _, ok := x.(*ast.Ident); !ok {
					p.errorf(x.Pos(), "non-name on left side of :=")
				}
			}
		}
		return s
	}
	if len(lhs) > 1 {
		p.errorf(lhs[0].Pos(), "expected ':=' or '=' after expression list")
	}
	switch p.tok.Kind {
	case token.INC, token.DEC:
		s := &ast.IncDecStmt{X: lhs[0], Tok: p.tok.Kind}
		p.next()
		return s
	case token.ARROW:
		s := &ast.SendStmt{Chan: lhs[0], Arrow: p.tok.Pos}
		p.next()
		s.Value = p.parseExpr()
		return s
	}
	return &ast.ExprStmt{X: lhs[0]}
}

// parseHeaderSimple parses a simple statement in a control clause.
func (p *parser) parseHeaderSimple() ast.Stmt {
	old := p.exprLev
	p.exprLev = -1
	defer func() { p.exprLev = old }()
	return p.parseSimpleStmt()
}

func (p *parser) parseHeaderExpr() ast.Expr {
	old := p.exprLev
	p.exprLev = -1
	defer func() { p.exprLev = old }()
	return p.parseExpr()
}

func stmtToExpr(p *parser, s ast.Stmt) ast.Expr {
	if s == nil {
		return nil
	}
	if es, ok := s.(*ast.ExprStmt); ok {
		return es.X
	}
	p.errorf(s.Pos(), "expected boolean expression, found statement")
	return nil
}

func (p *parser) parseIf() *ast.IfStmt {
	s := &ast.IfStmt{If: p.expect(token.IF)}
	if p.tok.Kind == token.LBRACE {
		p.errorf(p.tok.Pos, "missing condition in if statement")
	} else {
		var init ast.Stmt
		if p.tok.Kind != token.SEMICOLON {
			init = p.parseHeaderSimple()
		}
		if p.tok.Kind == token.SEMICOLON {
			p.next()
			s.Init = init
			s.Cond = p.parseHeaderExpr()
		} else {
			s.Cond = stmtToExpr(p, init)
		}
	}
	s.Body = p.parseBlock()
	if p.tok.Kind == token.ELSE {
		p.next()
		switch p.tok.Kind {
		case token.IF:
			s.Else = p.parseIf()
		case token.LBRACE:
			s.Else = p.parseBlock()
		default:
			p.errorf(p.tok.Pos, "expected 'if' or '{' after else")
		}
	}
	return s
}

func (p *parser) parseFor() ast.Stmt {
	pos := p.expect(token.FOR)

	// for {}
	if p.tok.Kind == token.LBRACE {
		return &ast.ForStmt{For: pos, Body: p.parseBlock()}
	}

	// for v in xs {}  /  for k, v in xs {}
	if p.tok.Kind == token.IDENT {
		if p.peek(1).Kind == token.IN {
			s := &ast.ForInStmt{For: pos, Value: p.parseIdent()}
			p.next() // in
			s.Iter = p.parseHeaderExpr()
			s.Body = p.parseBlock()
			return s
		}
		if p.peek(1).Kind == token.COMMA && p.peek(2).Kind == token.IDENT && p.peek(3).Kind == token.IN {
			s := &ast.ForInStmt{For: pos, Key: p.parseIdent()}
			p.next() // ,
			s.Value = p.parseIdent()
			p.next() // in
			s.Iter = p.parseHeaderExpr()
			s.Body = p.parseBlock()
			return s
		}
	}

	s := &ast.ForStmt{For: pos}
	var first ast.Stmt
	if p.tok.Kind != token.SEMICOLON {
		first = p.parseHeaderSimple()
	}
	if p.tok.Kind == token.SEMICOLON {
		// three-clause loop
		p.next()
		s.Init = first
		if p.tok.Kind != token.SEMICOLON {
			s.Cond = p.parseHeaderExpr()
		}
		p.expect(token.SEMICOLON)
		if p.tok.Kind != token.LBRACE {
			s.Post = p.parseHeaderSimple()
		}
	} else {
		s.Cond = stmtToExpr(p, first)
	}
	s.Body = p.parseBlock()
	return s
}

func (p *parser) parseSwitch() *ast.SwitchStmt {
	s := &ast.SwitchStmt{Switch: p.expect(token.SWITCH)}
	if p.tok.Kind != token.LBRACE {
		var first ast.Stmt
		if p.tok.Kind != token.SEMICOLON {
			first = p.parseHeaderSimple()
		}
		if p.tok.Kind == token.SEMICOLON {
			p.next()
			s.Init = first
			if p.tok.Kind != token.LBRACE {
				s.Tag = p.parseHeaderSimple()
			}
		} else {
			s.Tag = first
		}
	}
	p.expect(token.LBRACE)
	p.skipSemis()
	for p.tok.Kind == token.CASE || p.tok.Kind == token.DEFAULT {
		c := &ast.CaseClause{Case: p.tok.Pos}
		if p.tok.Kind == token.CASE {
			p.next()
			c.List = p.parseTypeOrExprList()
		} else {
			p.next()
		}
		p.expect(token.COLON)
		c.Body = p.parseStmtList()
		s.Body = append(s.Body, c)
	}
	p.expect(token.RBRACE)
	return s
}

// case lists may contain types (type switch) or expressions.
func (p *parser) parseTypeOrExprList() []ast.Expr {
	return p.parseExprList()
}

func (p *parser) parseSelect() *ast.SelectStmt {
	s := &ast.SelectStmt{Select: p.expect(token.SELECT)}
	p.expect(token.LBRACE)
	p.skipSemis()
	for p.tok.Kind == token.CASE || p.tok.Kind == token.DEFAULT {
		c := &ast.CommClause{Case: p.tok.Pos}
		if p.tok.Kind == token.CASE {
			p.next()
			c.Comm = p.parseSimpleStmt()
		} else {
			p.next()
		}
		p.expect(token.COLON)
		c.Body = p.parseStmtList()
		s.Body = append(s.Body, c)
	}
	p.expect(token.RBRACE)
	return s
}

func (p *parser) parseTry() *ast.TryStmt {
	s := &ast.TryStmt{Try: p.expect(token.TRY)}
	s.Body = p.parseBlock()
	catchAll := false
	for p.tok.Kind == token.CATCH {
		c := &ast.CatchClause{Catch: p.tok.Pos}
		p.next()
		if p.tok.Kind == token.IDENT {
			c.Name = p.parseIdent()
			if p.got(token.COLON) {
				c.Type = p.parseType()
			}
		}
		if catchAll {
			p.errorf(c.Catch, "unreachable catch clause after catch-all")
		}
		if c.Type == nil {
			catchAll = true
		}
		c.Body = p.parseBlock()
		s.Catches = append(s.Catches, c)
	}
	if p.tok.Kind == token.FINALLY {
		p.next()
		s.Finally = p.parseBlock()
	}
	if len(s.Catches) == 0 && s.Finally == nil {
		p.errorf(s.Try, "try requires at least one catch or finally")
	}
	return s
}

// ---------------------------------------------------------------- expressions

func (p *parser) parseExprList() []ast.Expr {
	list := []ast.Expr{p.parseExpr()}
	for p.got(token.COMMA) {
		list = append(list, p.parseExpr())
	}
	return list
}

func (p *parser) parseRhs() ast.Expr { return p.parseExpr() }

func (p *parser) parseExpr() ast.Expr { return p.parseBinary(1) }

func (p *parser) parseBinary(prec int) ast.Expr {
	x := p.parseUnary()
	for {
		op := p.tok.Kind
		oprec := op.Precedence()
		if oprec < prec {
			return x
		}
		pos := p.tok.Pos
		p.next()
		y := p.parseBinary(oprec + 1)
		x = &ast.BinaryExpr{X: x, OpPos: pos, Op: op, Y: y}
	}
}

func (p *parser) parseUnary() ast.Expr {
	switch p.tok.Kind {
	case token.ADD, token.SUB, token.NOT, token.XOR, token.AND, token.ARROW:
		pos, op := p.tok.Pos, p.tok.Kind
		p.next()
		return &ast.UnaryExpr{OpPos: pos, Op: op, X: p.parseUnary()}
	case token.MUL:
		pos := p.tok.Pos
		p.next()
		return &ast.StarExpr{Star: pos, X: p.parseUnary()}
	}
	return p.parsePrimary()
}

func isLiteralType(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.Ident, *ast.ArrayType, *ast.MapType, *ast.StructType:
		return true
	case *ast.SelectorExpr:
		_, ok := t.X.(*ast.Ident)
		return ok
	case *ast.IndexExpr:
		return isLiteralType(t.X)
	}
	return false
}

func (p *parser) parseOperand() ast.Expr {
	t := p.tok
	switch t.Kind {
	case token.IDENT:
		return p.parseIdent()
	case token.INT, token.FLOAT, token.CHAR, token.STRING:
		p.next()
		return &ast.BasicLit{ValuePos: t.Pos, Kind: t.Kind, Value: t.Lit}
	case token.FSTRING:
		p.next()
		return p.parseFString(t)
	case token.LPAREN:
		p.next()
		p.exprLev++
		x := p.parseTypeOrExpr()
		p.exprLev--
		p.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: t.Pos, X: x}
	case token.FUNC:
		return p.parseFuncLitOrType(nil)
	case token.LBRACK:
		return p.parseBracketOperand()
	case token.MAP, token.CHAN, token.STRUCT, token.INTERFACE:
		return p.tryType()
	case token.AT:
		if p.peek(1).Kind == token.IDENT && p.peek(2).Kind == token.FUNC {
			// @throws func(...) { ... }: annotated function literal or type
			return p.parseFuncLitOrType(p.parseAnnotations())
		}
		p.next()
		x := &ast.IntrinsicExpr{At: t.Pos}
		if p.tok.Kind == token.IDENT {
			x.Name = p.tok.Lit
			p.next()
		} else {
			p.errorf(p.tok.Pos, "expected intrinsic name after '@'")
		}
		if p.tok.Kind != token.LPAREN {
			p.errorf(p.tok.Pos, "intrinsic @%s requires arguments in parentheses", x.Name)
			return x
		}
		x.Args = p.parseCallArgs(nil).Args
		return x
	}
	p.errorf(t.Pos, "expected expression, found %s", describe(t))
	p.next()
	return &ast.Ident{NamePos: t.Pos, Name: "_"}
}

func (p *parser) parseFuncLitOrType(annots ast.Annotations) ast.Expr {
	ft := p.parseFuncType(annots)
	if p.tok.Kind == token.LBRACE {
		p.exprLev++
		body := p.parseBlock()
		p.exprLev--
		return &ast.FuncLit{Type: ft, Body: body}
	}
	return ft
}

// parseTypeOrExpr allows pointer types like (*T) inside parentheses.
func (p *parser) parseTypeOrExpr() ast.Expr { return p.parseExpr() }

// '[' starts: []T, [N]T, or a comprehension [x for x in xs if c].
func (p *parser) parseBracketOperand() ast.Expr {
	pos := p.tok.Pos
	if p.peek(1).Kind == token.RBRACK || p.peek(1).Kind == token.ELLIPSIS {
		return p.tryType()
	}
	p.next()
	p.exprLev++
	first := p.parseExpr()
	if p.tok.Kind == token.FOR {
		c := &ast.ComprehensionExpr{Lbrack: pos, Elt: first}
		p.next()
		id := p.parseIdent()
		if p.got(token.COMMA) {
			c.Key = id
			c.Value = p.parseIdent()
		} else {
			c.Value = id
		}
		p.expect(token.IN)
		c.Iter = p.parseBinary(1)
		if p.tok.Kind == token.IF {
			p.next()
			c.Cond = p.parseExpr()
		}
		p.exprLev--
		p.expect(token.RBRACK)
		return c
	}
	p.exprLev--
	p.expect(token.RBRACK)
	return &ast.ArrayType{Lbrack: pos, Len: first, Elt: p.parseType()}
}

func (p *parser) parsePrimary() ast.Expr {
	x := p.parseOperand()
	for {
		switch p.tok.Kind {
		case token.PERIOD:
			p.next()
			switch p.tok.Kind {
			case token.IDENT:
				x = &ast.SelectorExpr{X: x, Sel: p.parseIdent()}
			case token.LPAREN:
				p.next()
				ta := &ast.TypeAssertExpr{X: x}
				if p.tok.Kind == token.TYPE {
					p.next()
				} else {
					ta.Type = p.parseType()
				}
				p.expect(token.RPAREN)
				x = ta
			default:
				p.errorf(p.tok.Pos, "expected selector or type assertion, found %s", describe(p.tok))
				return x
			}
		case token.LBRACK:
			x = p.parseIndexOrSlice(x)
		case token.LPAREN:
			x = p.parseCallArgs(x)
		case token.QUESTION:
			x = &ast.PropagateExpr{X: x, Question: p.tok.Pos}
			p.next()
		case token.LBRACE:
			if isLiteralType(x) && (p.exprLev >= 0 || !isTypeNameExpr(x)) {
				x = p.parseCompositeLit(x)
			} else {
				return x
			}
		default:
			return x
		}
	}
}

// isTypeNameExpr: in control clauses `if x == T {` the brace belongs to the block.
func isTypeNameExpr(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	case *ast.IndexExpr:
		return isTypeNameExpr(t.X)
	}
	return false
}

func (p *parser) parseIndexOrSlice(x ast.Expr) ast.Expr {
	p.expect(token.LBRACK)
	p.exprLev++
	defer func() { p.exprLev-- }()

	var idx [3]ast.Expr
	ncolons := 0
	if p.tok.Kind != token.COLON {
		idx[0] = p.parseTypeOrExpr()
	}
	// generic instantiation with several types: F[int, string]
	if p.tok.Kind == token.COMMA {
		ix := &ast.IndexExpr{X: x, Indices: []ast.Expr{idx[0]}}
		for p.got(token.COMMA) && p.tok.Kind != token.RBRACK {
			ix.Indices = append(ix.Indices, p.parseType())
		}
		p.expect(token.RBRACK)
		return ix
	}
	for p.tok.Kind == token.COLON && ncolons < 2 {
		ncolons++
		p.next()
		if p.tok.Kind != token.COLON && p.tok.Kind != token.RBRACK {
			idx[ncolons] = p.parseExpr()
		}
	}
	p.expect(token.RBRACK)
	if ncolons == 0 {
		return &ast.IndexExpr{X: x, Indices: []ast.Expr{idx[0]}}
	}
	s := &ast.SliceExpr{X: x, Low: idx[0], High: idx[1], Max: idx[2], Slice3: ncolons == 2}
	if s.Slice3 && (s.High == nil || s.Max == nil) {
		p.errorf(p.tok.Pos, "middle and final index required in 3-index slice")
	}
	return s
}

func (p *parser) parseCallArgs(fun ast.Expr) *ast.CallExpr {
	c := &ast.CallExpr{Fun: fun}
	p.expect(token.LPAREN)
	p.exprLev++
	p.skipSemis()
	for p.tok.Kind != token.RPAREN && p.tok.Kind != token.EOF {
		c.Args = append(c.Args, p.parseTypeOrExpr())
		if p.tok.Kind == token.ELLIPSIS {
			c.Ellipsis = true
			p.next()
		}
		p.skipSemis()
		if !p.got(token.COMMA) {
			break
		}
		p.skipSemis()
	}
	p.exprLev--
	p.expect(token.RPAREN)
	return c
}

func (p *parser) parseCompositeLit(typ ast.Expr) *ast.CompositeLit {
	c := &ast.CompositeLit{Type: typ, Lbrace: p.expect(token.LBRACE)}
	p.exprLev++
	p.skipSemis()
	for p.tok.Kind != token.RBRACE && p.tok.Kind != token.EOF {
		c.Elts = append(c.Elts, p.parseElement())
		p.skipSemis()
		if !p.got(token.COMMA) {
			break
		}
		p.skipSemis()
	}
	p.exprLev--
	p.skipSemis()
	p.expect(token.RBRACE)
	return c
}

func (p *parser) parseElementValue() ast.Expr {
	if p.tok.Kind == token.LBRACE {
		return p.parseCompositeLit(nil)
	}
	return p.parseExpr()
}

func (p *parser) parseElement() ast.Expr {
	x := p.parseElementValue()
	if p.got(token.COLON) {
		return &ast.KeyValueExpr{Key: x, Value: p.parseElementValue()}
	}
	return x
}

// ---------------------------------------------------------------- f-strings

// parseFString splits f"Hello {name}, {price:.2f}" into parts.
func (p *parser) parseFString(t token.Token) *ast.FString {
	fs := &ast.FString{StrPos: t.Pos}
	s := t.Lit
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			fs.Parts = append(fs.Parts, &ast.FStringPart{Text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '{' && i+1 < len(s) && s[i+1] == '{':
			text.WriteByte('{')
			i++
		case c == '}' && i+1 < len(s) && s[i+1] == '}':
			text.WriteByte('}')
			i++
		case c == '{':
			flush()
			// find matching '}' honoring nesting and strings; top-level ':' starts spec
			depth, j, colon := 0, i+1, -1
			inStr := false
			for ; j < len(s); j++ {
				ch := s[j]
				if inStr {
					if ch == '\\' {
						j++
					} else if ch == '"' {
						inStr = false
					}
					continue
				}
				switch ch {
				case '"':
					inStr = true
				case '(', '[', '{':
					depth++
				case ')', ']':
					depth--
				case '}':
					if depth == 0 {
						goto done
					}
					depth--
				case ':':
					if depth == 0 && colon < 0 {
						colon = j
					}
				}
			}
		done:
			if j >= len(s) {
				p.errorf(t.Pos, "unterminated '{' in f-string")
				return fs
			}
			exprSrc, spec := s[i+1:j], ""
			if colon >= 0 {
				exprSrc, spec = s[i+1:colon], s[colon+1:j]
			}
			if strings.TrimSpace(exprSrc) == "" {
				p.errorf(t.Pos, "empty expression in f-string")
			} else if x, err := ParseExpr(exprSrc); err != nil {
				p.errorf(t.Pos, "in f-string expression {%s}: %v", exprSrc, err)
			} else {
				// positions inside the literal: f" is 2 columns, then the text before {expr
				base := t.Pos.Column + 2 + utf8.RuneCountInString(s[:i+1])
				shiftPos(reflect.ValueOf(x), t.Pos.File, t.Pos.Line, base-1)
				fs.Parts = append(fs.Parts, &ast.FStringPart{Expr: x, Spec: spec})
			}
			i = j
		case c == '}':
			p.errorf(t.Pos, "single '}' in f-string (use '}}')")
		case c == '\\' && i+1 < len(s):
			text.WriteByte(c)
			text.WriteByte(s[i+1])
			i++
		default:
			text.WriteByte(c)
		}
	}
	flush()
	return fs
}

var posType = reflect.TypeOf(token.Pos{})

// shiftPos moves positions of an f-string expression parsed on its own
// (line 1, column 1) to its place in the source file.
func shiftPos(v reflect.Value, file string, line, dcol int) {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			shiftPos(v.Elem(), file, line, dcol)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			shiftPos(v.Index(i), file, line, dcol)
		}
	case reflect.Struct:
		if v.Type() == posType {
			if v.CanSet() {
				p := v.Interface().(token.Pos)
				if p.Line == 1 && p.File == "" {
					v.Set(reflect.ValueOf(token.Pos{File: file, Line: line, Column: p.Column + dcol}))
				}
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() || v.Field(i).Kind() == reflect.Ptr || v.Field(i).Kind() == reflect.Interface || v.Field(i).Kind() == reflect.Slice {
				shiftPos(v.Field(i), file, line, dcol)
			}
		}
	}
}
