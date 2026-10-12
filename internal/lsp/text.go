package lsp

import (
	"os"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rusfort/gocpy/internal/token"
)

// text is the content of one file split into lines.
type text struct {
	src   string
	lines []string
}

func newText(src string) *text {
	return &text{src: src, lines: strings.Split(src, "\n")}
}

func (t *text) line(n int) string { // n is 1-based
	if n < 1 || n > len(t.lines) {
		return ""
	}
	return strings.TrimSuffix(t.lines[n-1], "\r")
}

// utf16Col converts a 1-based rune column to a 0-based UTF-16 offset.
func utf16Col(line string, col int) int {
	n, i := 0, 1
	for _, r := range line {
		if i >= col {
			break
		}
		n += len(utf16.Encode([]rune{r}))
		i++
	}
	if i < col { // past the end
		n += col - i
	}
	return n
}

// runeCol converts a 0-based UTF-16 offset to a 1-based rune column.
func runeCol(line string, ch int) int {
	n, col := 0, 1
	for _, r := range line {
		if n >= ch {
			break
		}
		n += len(utf16.Encode([]rune{r}))
		col++
	}
	return col
}

// position converts a compiler position (1-based line, rune column).
func (t *text) position(line, col int) Position {
	if line < 1 {
		line = 1
	}
	return Position{Line: line - 1, Character: utf16Col(t.line(line), col)}
}

// span is a range on one line given as a rune column and a rune length.
func (t *text) span(line, col, n int) Range {
	return Range{Start: t.position(line, col), End: t.position(line, col+n)}
}

// offset returns the byte offset of a 1-based line and rune column.
func (t *text) offset(line, col int) int {
	if line < 1 {
		return 0
	}
	off := 0
	for i := 0; i < line-1 && i < len(t.lines); i++ {
		off += len(t.lines[i]) + 1
	}
	if line > len(t.lines) {
		return len(t.src)
	}
	l := t.lines[line-1]
	b := 0
	for c := 1; c < col && b < len(l); c++ {
		_, w := utf8.DecodeRuneInString(l[b:])
		b += w
	}
	return off + b
}

// lineCol converts a byte offset to a 1-based line and rune column.
func (t *text) lineCol(off int) (int, int) {
	if off > len(t.src) {
		off = len(t.src)
	}
	start := 0
	for i, l := range t.lines {
		if off <= start+len(l) {
			return i + 1, utf8.RuneCountInString(t.src[start:off]) + 1
		}
		start += len(l) + 1
	}
	return len(t.lines), 1
}

func isIdentRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordAt returns the identifier that starts at a rune column (its rune length).
func (t *text) wordAt(line, col int) int {
	l := t.line(line)
	n, i := 0, 1
	for _, r := range l {
		if i >= col {
			if !isIdentRune(r) {
				break
			}
			n++
		}
		i++
	}
	return n
}

// opAt returns the length of an operator method name at a column (+, ==, []...).
func (t *text) opAt(line, col int) int {
	l := []rune(t.line(line))
	n := 0
	for i := col - 1; i >= 0 && i < len(l) && strings.ContainsRune("+-*/%&|^<>=![]~", l[i]); i++ {
		n++
	}
	return n
}

// matchBrace returns the position of the brace that closes the one at
// (line, col), skipping strings, runes and comments.
func (t *text) matchBrace(line, col int) (int, int, bool) {
	off := t.offset(line, col)
	s := t.src
	if off >= len(s) || (s[off] != '{' && s[off] != '(' && s[off] != '[') {
		return 0, 0, false
	}
	depth := 0
	for i := off; i < len(s); i++ {
		switch c := s[i]; c {
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			depth--
			if depth == 0 {
				l, c := t.lineCol(i)
				return l, c, true
			}
		case '"', '\'':
			for i++; i < len(s) && s[i] != c && s[i] != '\n'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '`':
			for i++; i < len(s) && s[i] != '`'; i++ {
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
			} else if i+1 < len(s) && s[i+1] == '*' {
				if j := strings.Index(s[i+2:], "*/"); j >= 0 {
					i += j + 3
				} else {
					i = len(s)
				}
			}
		}
	}
	return 0, 0, false
}

// ---------------------------------------------------------------- file cache

type fileCache struct {
	texts map[string]*cachedText
}

type cachedText struct {
	t     *text
	mtime int64
	size  int64
}

func (c *fileCache) get(path string) *text {
	st, err := os.Stat(path)
	if err != nil {
		return newText("")
	}
	if ct := c.texts[path]; ct != nil && ct.mtime == st.ModTime().UnixNano() && ct.size == st.Size() {
		return ct.t
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return newText("")
	}
	ct := &cachedText{t: newText(string(data)), mtime: st.ModTime().UnixNano(), size: st.Size()}
	c.texts[path] = ct
	return ct.t
}

func posKey(p token.Pos) string {
	if p.File == "" || p.Line == 0 {
		return ""
	}
	return p.String()
}
