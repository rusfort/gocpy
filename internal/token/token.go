// Package token defines lexical tokens of the Gocpy language.
package token

import "fmt"

// Kind is the lexical class of a token.
type Kind int

const (
	ILLEGAL Kind = iota
	EOF

	literalBeg
	IDENT   // main
	INT     // 123, 0xFF, 0b1010, 0o17, 1_000
	FLOAT   // 1.5, 1e9
	CHAR    // 'a'
	STRING  // "abc", `raw`
	FSTRING // f"Hello, {name}"
	literalEnd

	operatorBeg
	ADD     // +
	SUB     // -
	MUL     // *
	QUO     // /
	REM     // %
	AND     // &
	OR      // |
	XOR     // ^
	SHL     // <<
	SHR     // >>
	AND_NOT // &^

	ADD_ASSIGN     // +=
	SUB_ASSIGN     // -=
	MUL_ASSIGN     // *=
	QUO_ASSIGN     // /=
	REM_ASSIGN     // %=
	AND_ASSIGN     // &=
	OR_ASSIGN      // |=
	XOR_ASSIGN     // ^=
	SHL_ASSIGN     // <<=
	SHR_ASSIGN     // >>=
	AND_NOT_ASSIGN // &^=

	LAND  // &&
	LOR   // ||
	ARROW // <-
	INC   // ++
	DEC   // --

	EQL    // ==
	LSS    // <
	GTR    // >
	ASSIGN // =
	NOT    // !

	NEQ      // !=
	LEQ      // <=
	GEQ      // >=
	DEFINE   // :=
	ELLIPSIS // ...

	LPAREN    // (
	LBRACK    // [
	LBRACE    // {
	COMMA     // ,
	PERIOD    // .
	RPAREN    // )
	RBRACK    // ]
	RBRACE    // }
	SEMICOLON // ; (explicit or inserted at newline)
	COLON     // :
	QUESTION  // ?  postfix error propagation
	AT        // @  annotations and intrinsics
	TILDE     // ~  approximate constraint
	operatorEnd

	keywordBeg
	BREAK
	CASE
	CHAN
	CONST
	CONTINUE
	DEFAULT
	DEFER
	ELSE
	FALLTHROUGH
	FOR
	FUNC
	GO
	IF
	IMPORT
	INTERFACE
	MAP
	PACKAGE
	RETURN
	SELECT
	STRUCT
	SWITCH
	TYPE
	VAR
	// Gocpy-specific keywords
	IN
	TRY
	CATCH
	FINALLY
	THROW
	UNSAFE
	keywordEnd
)

var names = [...]string{
	ILLEGAL: "ILLEGAL", EOF: "EOF",
	IDENT: "IDENT", INT: "INT", FLOAT: "FLOAT", CHAR: "CHAR", STRING: "STRING", FSTRING: "FSTRING",

	ADD: "+", SUB: "-", MUL: "*", QUO: "/", REM: "%", AND: "&", OR: "|", XOR: "^",
	SHL: "<<", SHR: ">>", AND_NOT: "&^",
	ADD_ASSIGN: "+=", SUB_ASSIGN: "-=", MUL_ASSIGN: "*=", QUO_ASSIGN: "/=", REM_ASSIGN: "%=",
	AND_ASSIGN: "&=", OR_ASSIGN: "|=", XOR_ASSIGN: "^=", SHL_ASSIGN: "<<=", SHR_ASSIGN: ">>=",
	AND_NOT_ASSIGN: "&^=",
	LAND:           "&&", LOR: "||", ARROW: "<-", INC: "++", DEC: "--",
	EQL: "==", LSS: "<", GTR: ">", ASSIGN: "=", NOT: "!",
	NEQ: "!=", LEQ: "<=", GEQ: ">=", DEFINE: ":=", ELLIPSIS: "...",
	LPAREN: "(", LBRACK: "[", LBRACE: "{", COMMA: ",", PERIOD: ".",
	RPAREN: ")", RBRACK: "]", RBRACE: "}", SEMICOLON: ";", COLON: ":",
	QUESTION: "?", AT: "@", TILDE: "~",

	BREAK: "break", CASE: "case", CHAN: "chan", CONST: "const", CONTINUE: "continue",
	DEFAULT: "default", DEFER: "defer", ELSE: "else", FALLTHROUGH: "fallthrough",
	FOR: "for", FUNC: "func", GO: "go", IF: "if", IMPORT: "import", INTERFACE: "interface",
	MAP: "map", PACKAGE: "package", RETURN: "return", SELECT: "select", STRUCT: "struct",
	SWITCH: "switch", TYPE: "type", VAR: "var",
	IN: "in", TRY: "try", CATCH: "catch", FINALLY: "finally", THROW: "throw", UNSAFE: "unsafe",
}

func (k Kind) String() string {
	if k >= 0 && int(k) < len(names) && names[k] != "" {
		return names[k]
	}
	return fmt.Sprintf("token(%d)", int(k))
}

func (k Kind) IsLiteral() bool  { return literalBeg < k && k < literalEnd }
func (k Kind) IsOperator() bool { return operatorBeg < k && k < operatorEnd }
func (k Kind) IsKeyword() bool  { return keywordBeg < k && k < keywordEnd }

var keywords map[string]Kind

func init() {
	keywords = make(map[string]Kind)
	for k := keywordBeg + 1; k < keywordEnd; k++ {
		keywords[names[k]] = k
	}
}

// Lookup maps an identifier to its keyword kind, or IDENT.
func Lookup(ident string) Kind {
	if k, ok := keywords[ident]; ok {
		return k
	}
	return IDENT
}

// Precedence of binary operators (higher binds tighter). 0 = not binary.
func (k Kind) Precedence() int {
	switch k {
	case LOR:
		return 1
	case LAND:
		return 2
	case EQL, NEQ, LSS, LEQ, GTR, GEQ:
		return 3
	case ADD, SUB, OR, XOR:
		return 4
	case MUL, QUO, REM, SHL, SHR, AND, AND_NOT:
		return 5
	}
	return 0
}

// Pos is a source position.
type Pos struct {
	File   string
	Line   int
	Column int
}

func (p Pos) String() string {
	if p.File == "" {
		return fmt.Sprintf("%d:%d", p.Line, p.Column)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Column)
}

// Token is a lexed token.
type Token struct {
	Kind Kind
	Lit  string // source text for identifiers, literals; "\n" for inserted semicolons
	Pos  Pos
}

func (t Token) String() string {
	if t.Kind.IsLiteral() || t.Kind == SEMICOLON || t.Kind == ILLEGAL {
		return fmt.Sprintf("%s %s %q", t.Pos, t.Kind, t.Lit)
	}
	return fmt.Sprintf("%s %s", t.Pos, t.Kind)
}
