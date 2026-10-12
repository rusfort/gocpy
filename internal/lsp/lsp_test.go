package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type client struct {
	t     *testing.T
	in    *io.PipeWriter
	c     *conn
	id    int
	diags map[string][]Diagnostic
	resp  chan *message
	notes chan *message
}

func startClient(t *testing.T, root string) *client {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go Serve(sr, sw, "test")
	cl := &client{t: t, in: cw, c: &conn{r: bufio.NewReader(cr), w: cw}, diags: map[string][]Diagnostic{},
		resp: make(chan *message, 16), notes: make(chan *message, 256)}
	go func() {
		for {
			m, err := cl.c.read()
			if err != nil {
				return
			}
			if m.ID != nil && m.Method == "" {
				cl.resp <- m
			} else {
				cl.notes <- m
			}
		}
	}()
	cl.call("initialize", map[string]any{"rootUri": pathToURI(root)}, nil)
	cl.notify("initialized", map[string]any{})
	return cl
}

func (c *client) notify(method string, params any) {
	data, _ := json.Marshal(params)
	c.c.write(&message{Method: method, Params: data})
}

func (c *client) call(method string, params any, out any) {
	c.t.Helper()
	c.id++
	data, _ := json.Marshal(params)
	id := json.RawMessage(fmt.Sprint(c.id))
	c.c.write(&message{ID: &id, Method: method, Params: data})
	select {
	case m := <-c.resp:
		if m.Error != nil {
			c.t.Fatalf("%s: %s", method, m.Error.Message)
		}
		if out != nil {
			raw, _ := json.Marshal(m.Result)
			// Result was decoded as any: re-encode and decode into out
			if err := json.Unmarshal(raw, out); err != nil {
				c.t.Fatalf("%s: decode: %v (%s)", method, err, raw)
			}
		}
	case <-time.After(60 * time.Second):
		c.t.Fatalf("%s: timeout", method)
	}
}

func (c *client) waitDiags(uri string) []Diagnostic {
	c.t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case m := <-c.notes:
			if m.Method == "textDocument/publishDiagnostics" {
				var p struct {
					URI         string       `json:"uri"`
					Diagnostics []Diagnostic `json:"diagnostics"`
				}
				json.Unmarshal(m.Params, &p)
				c.diags[p.URI] = p.Diagnostics
				if p.URI == uri {
					return p.Diagnostics
				}
			}
		case <-deadline:
			c.t.Fatalf("no diagnostics for %s", uri)
		}
	}
}

func (c *client) open(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		c.t.Fatal(err)
	}
	uri := pathToURI(path)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "gocpy", "version": 1, "text": string(data)}})
	return uri
}

func (c *client) change(uri string, version int, src string) {
	c.notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": src}}})
}

func posOf(t *testing.T, src, needle string, delta int) Position {
	t.Helper()
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("%q not found", needle)
	}
	i += delta
	line := strings.Count(src[:i], "\n")
	ls := strings.LastIndex(src[:i], "\n") + 1
	return Position{Line: line, Character: utf16Len(src[ls:i])}
}

func tdp(uri string, p Position) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": p}
}

const geoSrc = `package geo

// Point — точка на плоскости.
type Point struct {
    X int
    Y int
    tag string
}

// New создаёт точку.
func New(x int, y int) Point {
    return Point{X: x, Y: y, tag: "p"}
}

func (p Point) Sum() int {
    return p.X + p.Y
}

const Zero = 0
`

const mainSrc = `package main

import (
    "std/fmt"
    "example.com/app/geo"
)

func twice(n int) int {
    return n * 2
}

func main() {
    p := geo.New(3, 4)
    s := p.Sum()
    fmt.Println(f"сумма {s} и {twice(p.X)}", geo.Zero)
    var q *geo.Point = &p
    fmt.Println(q.Y, twice(1))
}
`

func setup(t *testing.T) (string, *client) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "geo"), 0o755)
	os.WriteFile(filepath.Join(dir, "gocpy.yml"), []byte("module: example.com/app\nversion: 0.1.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "geo", "geo.gocpy"), []byte(geoSrc), 0o644)
	os.WriteFile(filepath.Join(dir, "main.gocpy"), []byte(mainSrc), 0o644)
	t.Setenv("GOCPY_LSP_NO_GO", "1")
	return dir, startClient(t, dir)
}

func TestLSP(t *testing.T) {
	dir, c := setup(t)
	mainPath := filepath.Join(dir, "main.gocpy")
	geoPath := filepath.Join(dir, "geo", "geo.gocpy")
	uri := c.open(mainPath)
	if d := c.waitDiags(uri); len(d) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", d)
	}

	// semantic tokens: decode and check classes of some names
	var st struct{ Data []int }
	c.call("textDocument/semanticTokens/full", map[string]any{"textDocument": map[string]any{"uri": uri}}, &st)
	lines := strings.Split(mainSrc, "\n")
	got := map[string]string{}
	line, ch := 0, 0
	for i := 0; i+4 < len(st.Data); i += 5 {
		if st.Data[i] > 0 {
			line, ch = line+st.Data[i], st.Data[i+1]
		} else {
			ch += st.Data[i+1]
		}
		rs := []rune(lines[line])
		word := string(rs[ch : ch+st.Data[i+2]])
		got[fmt.Sprintf("%d:%s", line+1, word)] = tokenTypes[st.Data[i+3]]
	}
	want := map[string]string{
		"1:main": "pkgDecl", "8:twice": "funcDecl", "8:n": "var", "8:int": "typeBuiltin",
		"13:p": "var", "13:geo": "pkgName", "13:New": "funcStd", "14:Sum": "funcMethod",
		"15:fmt": "pkgName", "15:Println": "funcStd", "15:s": "var", "15:twice": "funcCall", "15:X": "field",
		"15:Zero": "pkgMember", "16:Point": "typeUser", "16:&": "opPtr", "17:Y": "field",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("token %s: got %q, want %q", k, got[k], v)
		}
	}

	// hover over geo.New shows the signature and the doc comment
	var hv struct{ Contents MarkupContent }
	c.call("textDocument/hover", tdp(uri, posOf(t, mainSrc, "New(3", 1)), &hv)
	if !strings.Contains(hv.Contents.Value, "func New(x int, y int) Point") || !strings.Contains(hv.Contents.Value, "создаёт точку") {
		t.Errorf("hover New: %q", hv.Contents.Value)
	}
	c.call("textDocument/hover", tdp(uri, posOf(t, mainSrc, "func main", 1)), &hv)
	if !strings.Contains(hv.Contents.Value, "Объявление функции") {
		t.Errorf("hover func: %q", hv.Contents.Value)
	}

	// definition across packages
	var loc []Location
	var one Location
	c.call("textDocument/definition", tdp(uri, posOf(t, mainSrc, "Sum()", 0)), &one)
	if uriToPath(one.URI) != geoPath || one.Range.Start.Line != 14 {
		t.Errorf("definition Sum: %+v", one)
	}
	c.call("textDocument/definition", tdp(uri, posOf(t, mainSrc, "q.Y", 2)), &one)
	if uriToPath(one.URI) != geoPath || one.Range.Start.Line != 5 {
		t.Errorf("definition field Y: %+v", one)
	}
	c.call("textDocument/definition", tdp(uri, posOf(t, mainSrc, "example.com/app/geo", 3)), &one)
	if uriToPath(one.URI) != geoPath {
		t.Errorf("definition import: %+v", one)
	}

	// references of field X across packages (decl, literal key, selector)
	c.call("textDocument/references", map[string]any{"textDocument": map[string]any{"uri": uri},
		"position": posOf(t, mainSrc, "p.X", 2), "context": map[string]any{"includeDeclaration": true}}, &loc)
	if len(loc) != 4 { // X int, Point{X:, p.X (geo), p.X (main)
		t.Errorf("references X: %d %+v", len(loc), loc)
	}
	// references of twice (local function)
	c.call("textDocument/references", map[string]any{"textDocument": map[string]any{"uri": uri},
		"position": posOf(t, mainSrc, "twice(n", 1), "context": map[string]any{"includeDeclaration": false}}, &loc)
	if len(loc) != 2 {
		t.Errorf("references twice: %+v", loc)
	}

	// rename New across packages
	var we struct{ Changes map[string][]TextEdit }
	c.call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": uri},
		"position": posOf(t, mainSrc, "New(3", 0), "newName": "Make"}, &we)
	if len(we.Changes[pathToURI(geoPath)]) != 1 || len(we.Changes[uri]) != 1 {
		t.Errorf("rename: %+v", we.Changes)
	}

	// completion: members of a package, of a value, keywords, annotations
	labels := func(params map[string]any) map[string]bool {
		var cl struct{ Items []CompletionItem }
		c.call("textDocument/completion", params, &cl)
		m := map[string]bool{}
		for _, it := range cl.Items {
			m[it.Label] = true
		}
		return m
	}
	src2 := strings.Replace(mainSrc, "    s := p.Sum()", "    s := p.Sum()\n    geo.\n    p.\n    @", 1)
	c.change(uri, 2, src2)
	l := labels(tdp(uri, posOf(t, src2, "geo.\n", 4)))
	if !l["New"] || !l["Point"] || !l["Zero"] || l["tag"] {
		t.Errorf("geo. completion: %v", l)
	}
	l = labels(tdp(uri, posOf(t, src2, "    p.\n", 6)))
	if !l["X"] || !l["Sum"] || l["tag"] {
		t.Errorf("p. completion: %v", l)
	}
	l = labels(tdp(uri, posOf(t, src2, "    @", 5)))
	if !l["@throws"] || !l["@overload"] {
		t.Errorf("@ completion: %v", l)
	}
	l = labels(tdp(uri, posOf(t, src2, "    var q", 4)))
	for _, k := range []string{"try", "catch", "unsafe", "in", "func", "p", "s", "twice", "geo", "fmt", "len", "Exception"} {
		if !l[k] {
			t.Errorf("scope completion: no %q", k)
		}
	}
	c.change(uri, 3, mainSrc)

	// signature help
	var sh struct {
		Signatures []struct {
			Label string
		}
		ActiveParameter int
	}
	c.call("textDocument/signatureHelp", tdp(uri, posOf(t, mainSrc, "New(3, 4", 7)), &sh)
	if len(sh.Signatures) != 1 || !strings.Contains(sh.Signatures[0].Label, "New(x int, y int) geo.Point") || sh.ActiveParameter != 1 {
		t.Errorf("signature help: %+v", sh)
	}

	// call hierarchy: incoming calls of twice come from main
	var items []CallHierarchyItem
	c.call("textDocument/prepareCallHierarchy", tdp(uri, posOf(t, mainSrc, "twice(n", 1)), &items)
	if len(items) != 1 {
		t.Fatalf("prepareCallHierarchy: %+v", items)
	}
	var inc []struct {
		From       CallHierarchyItem
		FromRanges []Range
	}
	c.call("callHierarchy/incomingCalls", map[string]any{"item": items[0]}, &inc)
	if len(inc) != 1 || inc[0].From.Name != "main" || len(inc[0].FromRanges) != 2 {
		t.Errorf("incoming: %+v", inc)
	}
	c.call("textDocument/prepareCallHierarchy", tdp(uri, posOf(t, mainSrc, "func main", 6)), &items)
	var outc []struct{ To CallHierarchyItem }
	if len(items) == 1 {
		c.call("callHierarchy/outgoingCalls", map[string]any{"item": items[0]}, &outc)
	}
	names := map[string]bool{}
	for _, o := range outc {
		names[o.To.Name] = true
	}
	if !names["New"] || !names["Point.Sum"] || !names["twice"] {
		t.Errorf("outgoing: %+v", names)
	}

	// document symbols
	var syms []DocumentSymbol
	c.call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": pathToURI(geoPath)}}, &syms)
	if len(syms) != 4 || syms[0].Name != "Point" || len(syms[0].Children) != 3 || syms[2].Name != "(Point) Sum" {
		t.Errorf("symbols: %+v", syms)
	}

	// diagnostics: errors in the open file are reported while typing
	bad := strings.Replace(mainSrc, "s := p.Sum()", "s := p.Sum()\n    undefinedThing()", 1)
	c.change(uri, 4, bad)
	d := c.waitDiags(uri)
	for len(d) == 0 {
		d = c.waitDiags(uri)
	}
	if !strings.Contains(d[0].Message, "undefinedThing") || d[0].Range.Start.Line != 14 {
		t.Errorf("diagnostics: %+v", d)
	}
	// partial analysis keeps working with a syntax error
	broken := strings.Replace(mainSrc, "s := p.Sum()", "s := p.Sum(\n", 1)
	c.change(uri, 5, broken)
	c.call("textDocument/semanticTokens/full", map[string]any{"textDocument": map[string]any{"uri": uri}}, &st)
	if len(st.Data) == 0 {
		t.Errorf("no tokens for a broken file")
	}
}

func TestTextConv(t *testing.T) {
	line := "x := \"Привет 😀\" + y"
	// 😀 is 2 UTF-16 units; the rune column of y is 19
	if c := utf16Col(line, 19); c != 19 {
		t.Errorf("utf16Col = %d", c)
	}
	if c := runeCol(line, 19); c != 19 {
		t.Errorf("runeCol = %d", c)
	}
	tx := newText("a\nfunc f() {\n  if x { \"}\" }\n}\n")
	if l, c, ok := tx.matchBrace(2, 10); !ok || l != 4 || c != 1 {
		t.Errorf("matchBrace = %d %d %v", l, c, ok)
	}
	ch := parseChain("geo.New(1, 2).pts[0]")
	if len(ch) != 5 || ch[0].name != "geo" || ch[1].name != "New" || ch[2].kind != 'c' || ch[3].name != "pts" || ch[4].kind != 'x' {
		t.Errorf("parseChain = %+v", ch)
	}
}
