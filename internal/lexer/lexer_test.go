package lexer

import (
	"testing"

	"github.com/rusfort/gocpy/internal/token"
)

func kinds(t *testing.T, src string) []token.Kind {
	t.Helper()
	toks, errs := Tokenize("t", []byte(src))
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var out []token.Kind
	for _, tk := range toks {
		out = append(out, tk.Kind)
	}
	return out
}

func eq(t *testing.T, got, want []token.Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("token %d: got %v, want %v\nall: %v", i, got[i], want[i], got)
		}
	}
}

func TestSemicolonInsertion(t *testing.T) {
	src := "x := 1\ny++\nreturn\n}\nfoo(a,\n b)\n"
	eq(t, kinds(t, src), []token.Kind{
		token.IDENT, token.DEFINE, token.INT, token.SEMICOLON,
		token.IDENT, token.INC, token.SEMICOLON,
		token.RETURN, token.SEMICOLON,
		token.RBRACE, token.SEMICOLON,
		token.IDENT, token.LPAREN, token.IDENT, token.COMMA, token.IDENT, token.RPAREN, token.SEMICOLON,
		token.EOF,
	})
}

func TestPropagateEndsLine(t *testing.T) {
	eq(t, kinds(t, "d := read(p)?\n"), []token.Kind{
		token.IDENT, token.DEFINE, token.IDENT, token.LPAREN, token.IDENT, token.RPAREN,
		token.QUESTION, token.SEMICOLON, token.EOF,
	})
}

func TestAnnotationsAndKeywords(t *testing.T) {
	eq(t, kinds(t, "@throws(E) try catch finally throw unsafe in"), []token.Kind{
		token.AT, token.IDENT, token.LPAREN, token.IDENT, token.RPAREN,
		token.TRY, token.CATCH, token.FINALLY, token.THROW, token.UNSAFE, token.IN, token.EOF,
	})
}

func TestNumbers(t *testing.T) {
	cases := map[string]token.Kind{
		"42": token.INT, "0xFF": token.INT, "0b1010": token.INT, "0o17": token.INT,
		"1_000_000": token.INT, "1.5": token.FLOAT, "1e9": token.FLOAT, ".5": token.FLOAT,
	}
	for src, k := range cases {
		toks, errs := Tokenize("t", []byte(src))
		if len(errs) > 0 || toks[0].Kind != k || toks[0].Lit != src {
			t.Errorf("%s: got %v %q errs=%v", src, toks[0].Kind, toks[0].Lit, errs)
		}
	}
}

func TestFString(t *testing.T) {
	toks, errs := Tokenize("t", []byte(`f"Hi {name} {m["k"]} {{x}}"`))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if toks[0].Kind != token.FSTRING || toks[0].Lit != `Hi {name} {m["k"]} {{x}}` {
		t.Fatalf("got %v %q", toks[0].Kind, toks[0].Lit)
	}
	// plain identifier f is still an identifier
	if k := kinds(t, "f(1)")[0]; k != token.IDENT {
		t.Fatalf("f( lexed as %v", k)
	}
}

func TestOperators(t *testing.T) {
	eq(t, kinds(t, "<- <<= &^= ... := != ~"), []token.Kind{
		token.ARROW, token.SHL_ASSIGN, token.AND_NOT_ASSIGN, token.ELLIPSIS,
		token.DEFINE, token.NEQ, token.TILDE, token.EOF,
	})
}

func TestComments(t *testing.T) {
	eq(t, kinds(t, "a // c\n/* multi\nline */ b"), []token.Kind{
		token.IDENT, token.SEMICOLON, token.IDENT, token.SEMICOLON, token.EOF,
	})
}

func TestErrors(t *testing.T) {
	for _, src := range []string{`"open`, "'ab'", "/* x", "#", "12abc"} {
		if _, errs := Tokenize("t", []byte(src)); len(errs) == 0 {
			t.Errorf("%q: expected error", src)
		}
	}
}
