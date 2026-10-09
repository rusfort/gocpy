// Package lexer converts Gocpy source into tokens.
//
// Like Go, Gocpy has no semicolons in source: the lexer inserts SEMICOLON
// at a newline when the last token on the line could end a statement.
package lexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/token"
)

// Error is a lexical error.
type Error struct {
	Pos token.Pos
	Msg string
}

func (e Error) Error() string { return fmt.Sprintf("%s: %s", e.Pos, e.Msg) }

// Lexer is a Gocpy scanner.
type Lexer struct {
	file string
	src  []byte

	off  int // current reading offset
	line int
	col  int

	insertSemi bool
	Errors     []Error
	// Directives maps a line number to `//gocpy:...` comments found on it
	// (text after "//gocpy:", e.g. "ignore:unused").
	Directives map[int][]string
}

// New creates a lexer for src.
func New(file string, src []byte) *Lexer {
	// skip UTF-8 BOM
	if len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		src = src[3:]
	}
	return &Lexer{file: file, src: src, line: 1, col: 1}
}

// Tokenize returns all tokens up to and including EOF.
func Tokenize(file string, src []byte) ([]token.Token, []Error) {
	toks, l := TokenizeLexer(file, src)
	return toks, l.Errors
}

// TokenizeLexer is like Tokenize but returns the lexer (errors, directives).
func TokenizeLexer(file string, src []byte) ([]token.Token, *Lexer) {
	l := New(file, src)
	var out []token.Token
	for {
		t := l.Next()
		out = append(out, t)
		if t.Kind == token.EOF {
			break
		}
	}
	return out, l
}

func (l *Lexer) pos() token.Pos { return token.Pos{File: l.file, Line: l.line, Column: l.col} }

func (l *Lexer) errorf(p token.Pos, format string, args ...any) {
	l.Errors = append(l.Errors, Error{p, fmt.Sprintf(format, args...)})
}

func (l *Lexer) peek() rune {
	if l.off >= len(l.src) {
		return -1
	}
	r, _ := utf8.DecodeRune(l.src[l.off:])
	return r
}

func (l *Lexer) peekAt(n int) byte {
	if l.off+n >= len(l.src) {
		return 0
	}
	return l.src[l.off+n]
}

func (l *Lexer) advance() rune {
	if l.off >= len(l.src) {
		return -1
	}
	r, w := utf8.DecodeRune(l.src[l.off:])
	l.off += w
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// skipSpace skips whitespace and comments. It reports whether a newline
// was crossed (relevant for semicolon insertion).
func (l *Lexer) skipSpace() (newline bool, nlPos token.Pos) {
	for {
		r := l.peek()
		switch {
		case r == '\n':
			if !newline {
				nlPos = l.pos()
			}
			newline = true
			l.advance()
		case r == ' ' || r == '\t' || r == '\r':
			l.advance()
		case r == '/' && l.peekAt(1) == '/':
			start, line := l.off, l.line
			for l.peek() != '\n' && l.peek() != -1 {
				l.advance()
			}
			if text := string(l.src[start:l.off]); strings.HasPrefix(text, "//gocpy:") {
				if l.Directives == nil {
					l.Directives = map[int][]string{}
				}
				l.Directives[line] = append(l.Directives[line], strings.TrimSpace(text[len("//gocpy:"):]))
			}
		case r == '/' && l.peekAt(1) == '*':
			start := l.pos()
			l.advance()
			l.advance()
			closed := false
			for l.peek() != -1 {
				if l.peek() == '*' && l.peekAt(1) == '/' {
					l.advance()
					l.advance()
					closed = true
					break
				}
				if l.peek() == '\n' && !newline {
					nlPos = l.pos()
					newline = true
				}
				l.advance()
			}
			if !closed {
				l.errorf(start, "comment not terminated")
			}
		default:
			return
		}
	}
}

// Next returns the next token.
func (l *Lexer) Next() token.Token {
	newline, nlPos := l.skipSpace()
	if l.insertSemi && (newline || l.peek() == -1) {
		l.insertSemi = false
		p := nlPos
		if !newline {
			p = l.pos()
		}
		return token.Token{Kind: token.SEMICOLON, Lit: "\n", Pos: p}
	}

	p := l.pos()
	r := l.peek()
	tok := token.Token{Pos: p}

	switch {
	case r == -1:
		tok.Kind = token.EOF
	case r == 'f' && l.peekAt(1) == '"':
		l.advance()
		tok.Kind = token.FSTRING
		tok.Lit = l.scanString(p, true)
	case isLetter(r):
		start := l.off
		for isLetter(l.peek()) || isDigit(l.peek()) {
			l.advance()
		}
		tok.Lit = string(l.src[start:l.off])
		tok.Kind = token.Lookup(tok.Lit)
	case isDigit(r) || (r == '.' && isDigitByte(l.peekAt(1))):
		tok.Kind, tok.Lit = l.scanNumber(p)
	case r == '"':
		tok.Kind = token.STRING
		tok.Lit = l.scanString(p, false)
	case r == '`':
		tok.Kind = token.STRING
		tok.Lit = l.scanRawString(p)
	case r == '\'':
		tok.Kind = token.CHAR
		tok.Lit = l.scanChar(p)
	default:
		tok.Kind, tok.Lit = l.scanOperator(p)
	}

	switch tok.Kind {
	case token.IDENT, token.INT, token.FLOAT, token.CHAR, token.STRING, token.FSTRING,
		token.BREAK, token.CONTINUE, token.FALLTHROUGH, token.RETURN,
		token.INC, token.DEC, token.RPAREN, token.RBRACK, token.RBRACE, token.QUESTION:
		l.insertSemi = true
	default:
		l.insertSemi = false
	}
	return tok
}

func (l *Lexer) scanNumber(p token.Pos) (token.Kind, string) {
	start := l.off
	kind := token.INT
	digits := isDigit
	if l.peek() == '0' {
		switch l.peekAt(1) | 0x20 { // lower-case
		case 'x':
			digits = isHex
			l.advance()
			l.advance()
		case 'b':
			digits = func(r rune) bool { return r == '0' || r == '1' }
			l.advance()
			l.advance()
		case 'o':
			digits = func(r rune) bool { return r >= '0' && r <= '7' }
			l.advance()
			l.advance()
		}
	}
	for digits(l.peek()) || l.peek() == '_' {
		l.advance()
	}
	isDec := l.off-start < 2 || (l.src[start+1]|0x20 != 'x' && l.src[start+1]|0x20 != 'b' && l.src[start+1]|0x20 != 'o')
	if isDec && l.peek() == '.' && l.peekAt(1) != '.' {
		kind = token.FLOAT
		l.advance()
		for isDigit(l.peek()) || l.peek() == '_' {
			l.advance()
		}
	}
	if isDec && (l.peek() == 'e' || l.peek() == 'E') {
		kind = token.FLOAT
		l.advance()
		if l.peek() == '+' || l.peek() == '-' {
			l.advance()
		}
		if !isDigit(l.peek()) {
			l.errorf(p, "exponent has no digits")
		}
		for isDigit(l.peek()) {
			l.advance()
		}
	}
	if isLetter(l.peek()) {
		l.errorf(l.pos(), "invalid character %q in numeric literal", l.peek())
		for isLetter(l.peek()) || isDigit(l.peek()) {
			l.advance()
		}
	}
	return kind, string(l.src[start:l.off])
}

func (l *Lexer) scanEscape(quote rune) {
	p := l.pos()
	l.advance() // '\'
	r := l.advance()
	switch r {
	case 'a', 'b', 'f', 'n', 'r', 't', 'v', '\\', quote:
	case 'x':
		l.hexDigits(p, 2)
	case 'u':
		l.hexDigits(p, 4)
	case 'U':
		l.hexDigits(p, 8)
	case '0', '1', '2', '3', '4', '5', '6', '7':
		for i := 0; i < 2; i++ {
			if c := l.peek(); c >= '0' && c <= '7' {
				l.advance()
			} else {
				l.errorf(p, "invalid octal escape")
				return
			}
		}
	default:
		l.errorf(p, "unknown escape sequence")
	}
}

func (l *Lexer) hexDigits(p token.Pos, n int) {
	for i := 0; i < n; i++ {
		if !isHex(l.peek()) {
			l.errorf(p, "invalid hex escape")
			return
		}
		l.advance()
	}
}

// scanString scans "..." (current char is the opening quote) and returns
// the literal including quotes; for f-strings the returned text is the
// raw content between quotes.
func (l *Lexer) scanString(p token.Pos, fstr bool) string {
	start := l.off
	l.advance() // opening "
	depth := 0
	for {
		r := l.peek()
		if r == -1 || r == '\n' {
			l.errorf(p, "string literal not terminated")
			break
		}
		if fstr {
			// inside {expr} quotes belong to the expression
			if r == '{' {
				if l.peekAt(1) == '{' && depth == 0 {
					l.advance()
					l.advance()
					continue
				}
				depth++
			} else if r == '}' && depth > 0 {
				depth--
			} else if r == '"' && depth > 0 {
				// nested string inside expression
				l.scanString(l.pos(), false)
				continue
			}
		}
		if r == '"' && depth == 0 {
			l.advance()
			break
		}
		if r == '\\' {
			l.scanEscape('"')
			continue
		}
		l.advance()
	}
	s := string(l.src[start:l.off])
	if fstr {
		if len(s) >= 2 && s[len(s)-1] == '"' {
			return s[1 : len(s)-1]
		}
		return s[1:]
	}
	return s
}

func (l *Lexer) scanRawString(p token.Pos) string {
	start := l.off
	l.advance()
	for {
		r := l.advance()
		if r == -1 {
			l.errorf(p, "raw string literal not terminated")
			break
		}
		if r == '`' {
			break
		}
	}
	return string(l.src[start:l.off])
}

func (l *Lexer) scanChar(p token.Pos) string {
	start := l.off
	l.advance()
	n := 0
	for {
		r := l.peek()
		if r == -1 || r == '\n' {
			l.errorf(p, "rune literal not terminated")
			break
		}
		if r == '\'' {
			l.advance()
			break
		}
		if r == '\\' {
			l.scanEscape('\'')
		} else {
			l.advance()
		}
		n++
	}
	if n != 1 {
		l.errorf(p, "rune literal must contain exactly one character")
	}
	return string(l.src[start:l.off])
}

func (l *Lexer) scanOperator(p token.Pos) (token.Kind, string) {
	start := l.off
	r := l.advance()
	// helpers
	if2 := func(c byte, k2, k1 token.Kind) token.Kind {
		if l.peekAt(0) == c {
			l.advance()
			return k2
		}
		return k1
	}
	var k token.Kind
	switch r {
	case '(':
		k = token.LPAREN
	case ')':
		k = token.RPAREN
	case '[':
		k = token.LBRACK
	case ']':
		k = token.RBRACK
	case '{':
		k = token.LBRACE
	case '}':
		k = token.RBRACE
	case ',':
		k = token.COMMA
	case ';':
		k = token.SEMICOLON
	case '?':
		k = token.QUESTION
	case '@':
		k = token.AT
	case '~':
		k = token.TILDE
	case '.':
		if l.peekAt(0) == '.' && l.peekAt(1) == '.' {
			l.advance()
			l.advance()
			k = token.ELLIPSIS
		} else {
			k = token.PERIOD
		}
	case ':':
		k = if2('=', token.DEFINE, token.COLON)
	case '+':
		switch l.peekAt(0) {
		case '+':
			l.advance()
			k = token.INC
		case '=':
			l.advance()
			k = token.ADD_ASSIGN
		default:
			k = token.ADD
		}
	case '-':
		switch l.peekAt(0) {
		case '-':
			l.advance()
			k = token.DEC
		case '=':
			l.advance()
			k = token.SUB_ASSIGN
		default:
			k = token.SUB
		}
	case '*':
		k = if2('=', token.MUL_ASSIGN, token.MUL)
	case '/':
		k = if2('=', token.QUO_ASSIGN, token.QUO)
	case '%':
		k = if2('=', token.REM_ASSIGN, token.REM)
	case '^':
		k = if2('=', token.XOR_ASSIGN, token.XOR)
	case '=':
		k = if2('=', token.EQL, token.ASSIGN)
	case '!':
		k = if2('=', token.NEQ, token.NOT)
	case '<':
		switch l.peekAt(0) {
		case '-':
			l.advance()
			k = token.ARROW
		case '=':
			l.advance()
			k = token.LEQ
		case '<':
			l.advance()
			k = if2('=', token.SHL_ASSIGN, token.SHL)
		default:
			k = token.LSS
		}
	case '>':
		switch l.peekAt(0) {
		case '=':
			l.advance()
			k = token.GEQ
		case '>':
			l.advance()
			k = if2('=', token.SHR_ASSIGN, token.SHR)
		default:
			k = token.GTR
		}
	case '&':
		switch l.peekAt(0) {
		case '&':
			l.advance()
			k = token.LAND
		case '^':
			l.advance()
			k = if2('=', token.AND_NOT_ASSIGN, token.AND_NOT)
		case '=':
			l.advance()
			k = token.AND_ASSIGN
		default:
			k = token.AND
		}
	case '|':
		switch l.peekAt(0) {
		case '|':
			l.advance()
			k = token.LOR
		case '=':
			l.advance()
			k = token.OR_ASSIGN
		default:
			k = token.OR
		}
	default:
		l.errorf(p, "unexpected character %q", r)
		k = token.ILLEGAL
	}
	return k, string(l.src[start:l.off])
}

func isLetter(r rune) bool {
	return r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || (r >= utf8.RuneSelf && unicode.IsLetter(r))
}
func isDigit(r rune) bool     { return '0' <= r && r <= '9' }
func isDigitByte(b byte) bool { return '0' <= b && b <= '9' }
func isHex(r rune) bool {
	return isDigit(r) || ('a' <= r && r <= 'f') || ('A' <= r && r <= 'F')
}
