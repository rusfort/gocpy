package lsp

import (
	"sort"

	"github.com/rusfort/gocpy/internal/lexer"
	"github.com/rusfort/gocpy/internal/token"
)

// Semantic token types: the categories of the Gocpy IDE. The VS Code
// extension declares them (with standard super types for other themes).
var tokenTypes = []string{
	"var", "field", "fieldDecl", "label",
	"funcDecl", "funcCall", "funcMethod", "funcBuiltin", "funcStd", "funcOperator",
	"pkgName", "pkgDecl", "pkgMember",
	"constUser", "constBool", "constNil",
	"typeBuiltin", "typeError", "typeUser", "typeStd", "typeParam", "typeConstraint",
	"unsafeFunc", "unsafeCast", "opPtr",
}

var tokenModifiers = []string{"declaration", "readonly", "defaultLibrary"}

var tokenIndex = func() map[string]int {
	m := map[string]int{}
	for i, t := range tokenTypes {
		m[t] = i
	}
	return m
}()

type semTok struct {
	line, col, n int // 1-based line, rune column, rune length
	typ          int
	mods         int
}

var builtinFuncs = map[string]bool{"len": true, "cap": true, "append": true, "make": true, "new": true,
	"sizeof": true, "close": true, "delete": true}
var unsafeFuncs = map[string]bool{"malloc": true, "calloc": true, "realloc": true, "free": true,
	"memcpy": true, "memset": true, "slice": true}

func (s *server) semanticTokens(path string) any {
	t := s.texts(path)
	pa := s.analysisFor(path)
	var toks []semTok
	covered := map[[2]int]bool{}
	for _, o := range pa.byFile[path] {
		ti, ok := tokenIndex[o.cls]
		if !ok {
			continue
		}
		mods := 0
		if o.def {
			mods |= 1
		}
		switch o.cls {
		case "constUser", "constBool", "constNil", "pkgMember":
			mods |= 2
		case "funcBuiltin", "funcStd", "typeBuiltin", "typeError", "typeStd", "typeConstraint":
			mods |= 4
		}
		toks = append(toks, semTok{o.line, o.col, o.n, ti, mods})
		covered[[2]int{o.line, o.col}] = true
	}
	for _, p := range pa.ptrOps[path] {
		toks = append(toks, semTok{p[0], p[1], 1, tokenIndex["opPtr"], 0})
		covered[[2]int{p[0], p[1]}] = true
	}
	// lexical classification for identifiers the analysis did not resolve
	toks = append(toks, s.lexicalTokens(path, t, pa, covered)...)
	sort.Slice(toks, func(i, j int) bool {
		if toks[i].line != toks[j].line {
			return toks[i].line < toks[j].line
		}
		return toks[i].col < toks[j].col
	})
	data := make([]int, 0, len(toks)*5)
	pl, pc := 0, 0
	lastLine, lastEnd := -1, -1
	for _, tk := range toks {
		line := tk.line - 1
		lt := t.line(tk.line)
		c := utf16Col(lt, tk.col)
		e := utf16Col(lt, tk.col+tk.n)
		if e <= c {
			continue
		}
		if line == lastLine && c < lastEnd { // overlapping
			continue
		}
		dl := line - pl
		dc := c
		if dl == 0 {
			dc = c - pc
		}
		data = append(data, dl, dc, e-c, tk.typ, tk.mods)
		pl, pc = line, c
		lastLine, lastEnd = line, e
	}
	return map[string]any{"data": data}
}

// lexicalTokens is the IDE's heuristic classification (used when the file
// does not type-check, and for identifiers without type information).
func (s *server) lexicalTokens(path string, t *text, pa *pkgAnalysis, covered map[[2]int]bool) []semTok {
	toks, _ := lexer.Tokenize(path, []byte(t.src))
	pkgNames := map[string]bool{}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].Kind == token.IMPORT {
			// import alias "path" | import "path" | import ( ... )
			for j := i + 1; j < len(toks) && toks[j].Kind != token.EOF; j++ {
				k := toks[j]
				if k.Kind == token.STRING {
					p := k.Lit
					if len(p) >= 2 {
						p = p[1 : len(p)-1]
					}
					if j > 0 && toks[j-1].Kind == token.IDENT {
						pkgNames[toks[j-1].Lit] = true
					} else {
						pkgNames[lastElem(p)] = true
					}
				}
				if k.Kind == token.RPAREN || (k.Kind == token.SEMICOLON && toks[i+1].Kind != token.LPAREN) {
					break
				}
			}
		}
	}
	var out []semTok
	analysed := len(pa.byFile[path]) > 0
	for i, k := range toks {
		if k.Kind != token.IDENT || covered[[2]int{k.Pos.Line, k.Pos.Column}] {
			continue
		}
		var prev, next token.Kind = token.ILLEGAL, token.ILLEGAL
		if i > 0 {
			prev = toks[i-1].Kind
		}
		if i+1 < len(toks) {
			next = toks[i+1].Kind
		}
		cls := ""
		name := k.Lit
		switch {
		case prev == token.FUNC || (prev == token.RPAREN && i >= 2 && funcRecvBefore(toks, i)):
			cls = "funcDecl"
		case prev == token.PACKAGE:
			cls = "pkgDecl"
		case next == token.PERIOD && pkgNames[name]:
			cls = "pkgName"
		case prev == token.PERIOD && i >= 2 && toks[i-2].Kind == token.IDENT && pkgNames[toks[i-2].Lit]:
			if next == token.LPAREN {
				cls = "funcStd"
			} else if isUpper(name) {
				cls = "pkgMember"
			}
		case prev == token.PERIOD:
			if next == token.LPAREN {
				cls = "funcMethod"
			} else {
				cls = "field"
			}
		case name == "true" || name == "false":
			cls = "constBool"
		case name == "nil":
			cls = "constNil"
		case name == "error" || name == "Exception":
			cls = "typeError"
		case name == "comparable":
			cls = "typeConstraint"
		case universeTypeNames[name]:
			cls = "typeBuiltin"
		case builtinFuncs[name] && next == token.LPAREN:
			cls = "funcBuiltin"
		case unsafeFuncs[name] && next == token.LPAREN && pa.inUnsafeLex(path, t, k.Pos):
			cls = "unsafeFunc"
		case analysed:
			// resolved file: leave the rest to the grammar
		case next == token.LPAREN:
			cls = "funcCall"
		case prev == token.TYPE:
			cls = "typeUser"
		default:
			cls = "var"
		}
		if cls == "" {
			continue
		}
		n := t.wordAt(k.Pos.Line, k.Pos.Column)
		out = append(out, semTok{k.Pos.Line, k.Pos.Column, n, tokenIndex[cls], 0})
	}
	return out
}

func funcRecvBefore(toks []token.Token, i int) bool {
	// func (r *T) Name — find the matching '(' and check that FUNC precedes it
	depth := 0
	for j := i - 1; j >= 0; j-- {
		switch toks[j].Kind {
		case token.RPAREN:
			depth++
		case token.LPAREN:
			depth--
			if depth == 0 {
				return j > 0 && toks[j-1].Kind == token.FUNC
			}
		case token.SEMICOLON, token.LBRACE, token.RBRACE:
			return false
		}
	}
	return false
}

func (pa *pkgAnalysis) inUnsafeLex(path string, t *text, p token.Pos) bool {
	if len(pa.unsafe[path]) > 0 {
		return pa.inUnsafe(path, p.Line, p.Column)
	}
	return true
}

func isUpper(s string) bool { return s != "" && s[0] >= 'A' && s[0] <= 'Z' }

func lastElem(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
