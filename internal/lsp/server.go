package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rusfort/gocpy/internal/build"
)

type document struct {
	uri     string
	path    string
	version int
	t       *text
}

type server struct {
	c       *conn
	version string
	mu      sync.Mutex // guards everything below (requests and background diagnostics)
	docs    map[string]*document
	files   *fileCache
	roots   []string
	gen     int
	cache   map[string]*pkgAnalysis
	good    map[string]*pkgAnalysis // last analysis whose target files parsed
	diagged map[string]bool         // URIs with published diagnostics
	timer   *time.Timer
	logf    func(string, ...any)
	exit    bool
	noGo    bool
}

// Serve runs the language server on r/w (stdio).
func Serve(r io.Reader, w io.Writer, version string) error {
	s := &server{
		c:       &conn{r: bufio.NewReaderSize(r, 1<<16), w: w},
		version: version,
		docs:    map[string]*document{},
		files:   &fileCache{texts: map[string]*cachedText{}},
		cache:   map[string]*pkgAnalysis{},
		good:    map[string]*pkgAnalysis{},
		diagged: map[string]bool{},
		noGo:    os.Getenv("GOCPY_LSP_NO_GO") == "1",
	}
	s.logf = func(f string, a ...any) {
		s.notify("window/logMessage", map[string]any{"type": 4, "message": fmt.Sprintf(f, a...)})
	}
	for !s.exit {
		m, err := s.c.read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		s.handle(m)
	}
	return nil
}

func (s *server) notify(method string, params any) {
	data, _ := json.Marshal(params)
	s.c.write(&message{Method: method, Params: data})
}

func (s *server) reply(id *json.RawMessage, result any, err error) {
	if id == nil {
		return
	}
	m := &message{ID: id}
	if err != nil {
		m.Error = &rpcError{Code: -32603, Message: err.Error()}
	} else if result == nil {
		m.Result = nullResult{}
	} else {
		m.Result = result
	}
	s.c.write(m)
}

func (s *server) handle(m *message) {
	if m.Method == "" { // response to a server request
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logf("gocpy lsp: panic in %s: %v", m.Method, r)
			s.reply(m.ID, nil, fmt.Errorf("internal error: %v", r))
		}
	}()
	switch m.Method {
	case "initialize":
		s.reply(m.ID, s.initialize(m.Params), nil)
		return
	case "initialized", "$/cancelRequest", "$/setTrace", "workspace/didChangeConfiguration":
		return
	case "shutdown":
		s.reply(m.ID, nil, nil)
		return
	case "exit":
		s.exit = true
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch m.Method {
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
				Text    string `json:"text"`
			} `json:"textDocument"`
		}
		json.Unmarshal(m.Params, &p)
		s.setDoc(p.TextDocument.URI, p.TextDocument.Version, p.TextDocument.Text)
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		json.Unmarshal(m.Params, &p)
		if n := len(p.ContentChanges); n > 0 {
			s.setDoc(p.TextDocument.URI, p.TextDocument.Version, p.ContentChanges[n-1].Text)
		}
	case "textDocument/didClose":
		var p struct{ TextDocument TextDocumentIdentifier }
		json.Unmarshal(m.Params, &p)
		delete(s.docs, uriToPath(p.TextDocument.URI))
		s.gen++
		s.schedule()
	case "textDocument/didSave", "workspace/didChangeWatchedFiles":
		s.gen++
		s.schedule()
	default:
		res, err := s.request(m.Method, m.Params)
		s.reply(m.ID, res, err)
	}
}

func (s *server) request(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "textDocument/semanticTokens/full":
		var p struct{ TextDocument TextDocumentIdentifier }
		json.Unmarshal(raw, &p)
		return s.semanticTokens(uriToPath(p.TextDocument.URI)), nil
	case "textDocument/hover":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.hover(p), nil
	case "textDocument/definition", "textDocument/typeDefinition":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.definition(p, method == "textDocument/typeDefinition"), nil
	case "textDocument/references":
		var p struct {
			TextDocumentPositionParams
			Context struct {
				IncludeDeclaration bool `json:"includeDeclaration"`
			} `json:"context"`
		}
		json.Unmarshal(raw, &p)
		return s.references(p.TextDocumentPositionParams, p.Context.IncludeDeclaration), nil
	case "textDocument/documentHighlight":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.highlight(p), nil
	case "textDocument/completion":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.completion(p), nil
	case "textDocument/signatureHelp":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.signatureHelp(p), nil
	case "textDocument/documentSymbol":
		var p struct{ TextDocument TextDocumentIdentifier }
		json.Unmarshal(raw, &p)
		return s.documentSymbols(uriToPath(p.TextDocument.URI)), nil
	case "workspace/symbol":
		var p struct {
			Query string `json:"query"`
		}
		json.Unmarshal(raw, &p)
		return s.workspaceSymbols(p.Query), nil
	case "textDocument/prepareRename":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.prepareRename(p)
	case "textDocument/rename":
		var p struct {
			TextDocumentPositionParams
			NewName string `json:"newName"`
		}
		json.Unmarshal(raw, &p)
		return s.rename(p.TextDocumentPositionParams, p.NewName)
	case "textDocument/prepareCallHierarchy":
		var p TextDocumentPositionParams
		json.Unmarshal(raw, &p)
		return s.prepareCallHierarchy(p), nil
	case "callHierarchy/incomingCalls":
		var p struct {
			Item CallHierarchyItem `json:"item"`
		}
		json.Unmarshal(raw, &p)
		return s.incomingCalls(p.Item), nil
	case "callHierarchy/outgoingCalls":
		var p struct {
			Item CallHierarchyItem `json:"item"`
		}
		json.Unmarshal(raw, &p)
		return s.outgoingCalls(p.Item), nil
	case "gocpy/unsafeRanges":
		var p struct{ TextDocument TextDocumentIdentifier }
		json.Unmarshal(raw, &p)
		return s.unsafeRanges(uriToPath(p.TextDocument.URI)), nil
	}
	return nil, &methodNotFound{method}
}

type methodNotFound struct{ m string }

func (e *methodNotFound) Error() string { return "method not supported: " + e.m }

func (s *server) initialize(raw json.RawMessage) any {
	var p struct {
		RootURI          string `json:"rootUri"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
	}
	json.Unmarshal(raw, &p)
	s.mu.Lock()
	for _, f := range p.WorkspaceFolders {
		s.roots = append(s.roots, uriToPath(f.URI))
	}
	if len(s.roots) == 0 && p.RootURI != "" {
		s.roots = append(s.roots, uriToPath(p.RootURI))
	}
	s.mu.Unlock()
	return map[string]any{
		"capabilities": map[string]any{
			"textDocumentSync":          map[string]any{"openClose": true, "change": 1, "save": map[string]any{"includeText": false}},
			"hoverProvider":             true,
			"definitionProvider":        true,
			"typeDefinitionProvider":    true,
			"referencesProvider":        true,
			"documentHighlightProvider": true,
			"documentSymbolProvider":    true,
			"workspaceSymbolProvider":   true,
			"renameProvider":            map[string]any{"prepareProvider": true},
			"callHierarchyProvider":     true,
			"completionProvider": map[string]any{
				"triggerCharacters": []string{".", "@", "\"", "/"},
				"resolveProvider":   false,
			},
			"signatureHelpProvider": map[string]any{"triggerCharacters": []string{"(", ","}},
			"semanticTokensProvider": map[string]any{
				"legend": map[string]any{"tokenTypes": tokenTypes, "tokenModifiers": tokenModifiers},
				"full":   true,
			},
		},
		"serverInfo": map[string]any{"name": "gocpy", "version": s.version},
	}
}

func (s *server) setDoc(uri string, version int, src string) {
	p := uriToPath(uri)
	s.docs[p] = &document{uri: uri, path: p, version: version, t: newText(src)}
	s.gen++
	s.schedule()
}

// schedule runs diagnostics shortly after the last change.
func (s *server) schedule() {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(350*time.Millisecond, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.publishDiagnostics()
	})
}

func (s *server) overlay() map[string][]byte {
	m := map[string][]byte{}
	for p, d := range s.docs {
		m[p] = []byte(d.t.src)
	}
	return m
}

// analyze returns the (cached) analysis of the package in dir.
func (s *server) analyze(dir string) *pkgAnalysis {
	if pa := s.cache[dir]; pa != nil && pa.gen == s.gen {
		return pa
	}
	pa := &pkgAnalysis{dir: dir, gen: s.gen}
	func() {
		defer func() {
			if r := recover(); r != nil {
				pa.errText = fmt.Sprintf("%s:1:1: internal error: %v", dir, r)
			}
		}()
		a, err := build.Analyze(dir, &build.Options{Overlay: s.overlay(), NoGoBridge: s.noGo})
		if err != nil {
			pa.errText = err.Error()
			return
		}
		pa.a = a
		s.buildIndex(pa)
	}()
	s.cache[dir] = pa
	if pa.a != nil && pa.a.Info != nil && !hasSyntaxErrors(pa) {
		s.good[dir] = pa
	}
	return pa
}

func hasSyntaxErrors(pa *pkgAnalysis) bool {
	for _, e := range pa.a.Errors {
		if strings.Contains(e, "expected") || strings.Contains(e, "unexpected") || strings.Contains(e, "not terminated") {
			for f := range pa.files {
				if strings.HasPrefix(e, f+":") {
					return true
				}
			}
		}
	}
	return false
}

func (s *server) analysisFor(path string) *pkgAnalysis { return s.analyze(filepath.Dir(path)) }

// bestFor returns an analysis for path that has type information if possible.
func (s *server) bestFor(path string) *pkgAnalysis {
	pa := s.analysisFor(path)
	if (pa.a == nil || pa.a.Info == nil || len(pa.byFile[path]) == 0) && s.good[filepath.Dir(path)] != nil {
		return s.good[filepath.Dir(path)]
	}
	return pa
}

// ---------------------------------------------------------------- diagnostics

var errLine = regexp.MustCompile(`^(.+?):(\d+):(\d+): (.*)$`)

func (s *server) publishDiagnostics() {
	dirs := map[string]bool{}
	for p := range s.docs {
		if strings.HasSuffix(p, ".gocpy") {
			dirs[filepath.Dir(p)] = true
		}
	}
	byURI := map[string][]Diagnostic{}
	var order []string
	for d := range dirs {
		order = append(order, d)
	}
	sort.Strings(order)
	for _, dir := range order {
		pa := s.analyze(dir)
		var msgs []string
		var warns []string
		if pa.errText != "" {
			msgs = []string{pa.errText}
		} else if pa.a != nil {
			msgs, warns = pa.a.Errors, pa.a.Warns
		}
		add := func(list []string, sev int) {
			for _, m := range splitErrors(list) {
				file, line, col, msg := parseErr(m)
				if file == "" {
					// errors without a position go to the first open file of the package
					for p := range s.docs {
						if filepath.Dir(p) == dir {
							file, line, col = p, 1, 1
							break
						}
					}
				}
				if file == "" {
					continue
				}
				if !filepath.IsAbs(file) {
					file, _ = filepath.Abs(file)
				}
				t := s.texts(file)
				n := t.wordAt(line, col)
				if n == 0 {
					n = 1
					if rs := []rune(t.line(line)); col-1 < len(rs) && rs[col-1] == '"' {
						for i := col; i < len(rs); i++ {
							if rs[i] == '"' {
								n = i - col + 2
								break
							}
						}
					}
				}
				uri := pathToURI(file)
				byURI[uri] = append(byURI[uri], Diagnostic{Range: t.span(line, col, n), Severity: sev, Source: "gocpy", Message: msg})
			}
		}
		add(msgs, sevError)
		add(warns, sevWarning)
		for p := range pa.files {
			if _, ok := byURI[pathToURI(p)]; !ok {
				byURI[pathToURI(p)] = []Diagnostic{}
			}
		}
	}
	for _, d := range s.docs {
		if _, ok := byURI[d.uri]; !ok && strings.HasSuffix(d.path, ".gocpy") {
			byURI[d.uri] = []Diagnostic{}
		}
	}
	for uri := range s.diagged {
		if _, ok := byURI[uri]; !ok {
			byURI[uri] = []Diagnostic{}
		}
	}
	s.diagged = map[string]bool{}
	for uri, ds := range byURI {
		// dedupe identical messages at the same place
		seen := map[string]bool{}
		out := ds[:0]
		for _, d := range ds {
			k := fmt.Sprint(d.Range, d.Message)
			if !seen[k] {
				seen[k] = true
				out = append(out, d)
			}
		}
		if len(out) > 0 {
			s.diagged[uri] = true
		}
		s.notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": out})
	}
}

// splitErrors splits joined error texts into messages (continuation lines
// start with a tab or do not look like a position).
func splitErrors(list []string) []string {
	var out []string
	for _, e := range list {
		for _, l := range strings.Split(e, "\n") {
			if l == "" {
				continue
			}
			if errLine.MatchString(l) || len(out) == 0 {
				out = append(out, l)
			} else {
				out[len(out)-1] += "\n" + strings.TrimLeft(l, "\t")
			}
		}
	}
	return out
}

func parseErr(m string) (file string, line, col int, msg string) {
	first, rest, _ := strings.Cut(m, "\n")
	sm := errLine.FindStringSubmatch(first)
	if sm == nil {
		return "", 1, 1, m
	}
	line, _ = strconv.Atoi(sm[2])
	col, _ = strconv.Atoi(sm[3])
	msg = sm[4]
	if rest != "" {
		msg += "\n" + rest
	}
	return sm[1], line, col, msg
}

// ---------------------------------------------------------------- workspace

// workspacePackages lists directories with .gocpy files under the roots.
func (s *server) workspacePackages() []string {
	seen := map[string]bool{}
	var out []string
	for _, root := range s.roots {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				n := d.Name()
				if p != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "vendor" || n == "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".gocpy") {
				dir := filepath.Dir(p)
				if !seen[dir] && len(out) < 400 {
					seen[dir] = true
					out = append(out, dir)
				}
			}
			return nil
		})
	}
	for p := range s.docs {
		if dir := filepath.Dir(p); strings.HasSuffix(p, ".gocpy") && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

func (s *server) inWorkspace(path string) bool {
	for _, r := range s.roots {
		if rel, err := filepath.Rel(r, path); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	_, open := s.docs[path]
	return open
}

// docPos converts an LSP position in a document to a rune column position.
func (s *server) docPos(p TextDocumentPositionParams) (path string, line, col int) {
	path = uriToPath(p.TextDocument.URI)
	t := s.texts(path)
	line = p.Position.Line + 1
	col = runeCol(t.line(line), p.Position.Character)
	return
}

func (s *server) loc(file string, line, col, n int) Location {
	return Location{URI: pathToURI(file), Range: s.texts(file).span(line, col, n)}
}

func (s *server) unsafeRanges(path string) any {
	pa := s.analysisFor(path)
	t := s.texts(path)
	out := []Range{}
	for _, r := range pa.unsafe[path] {
		out = append(out, Range{Start: t.position(r.sLine, r.sCol), End: t.position(r.eLine, r.eCol+1)})
	}
	return out
}
