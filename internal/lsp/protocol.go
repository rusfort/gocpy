// Package lsp implements the Gocpy language server (gocpy lsp): semantic
// highlighting, diagnostics, navigation across packages, completion, hover,
// signature help, references, rename, symbols and call hierarchy.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------- JSON-RPC

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

func (c *conn) read() (*message, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	m := &message{}
	if err := json.Unmarshal(buf, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *conn) write(m *message) {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(data))
	c.w.Write(data)
}

// nullResult marshals as JSON null (Result: nil would be omitted).
type nullResult struct{}

func (nullResult) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// ---------------------------------------------------------------- protocol types

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type CompletionItem struct {
	Label            string         `json:"label"`
	Kind             int            `json:"kind,omitempty"`
	Detail           string         `json:"detail,omitempty"`
	Documentation    *MarkupContent `json:"documentation,omitempty"`
	InsertText       string         `json:"insertText,omitempty"`
	InsertTextFormat int            `json:"insertTextFormat,omitempty"`
	SortText         string         `json:"sortText,omitempty"`
	FilterText       string         `json:"filterText,omitempty"`
}

type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	Detail         string `json:"detail,omitempty"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
	Data           any    `json:"data,omitempty"`
}

// LSP enums used here.
const (
	sevError   = 1
	sevWarning = 2

	ckText        = 1
	ckMethod      = 2
	ckFunction    = 3
	ckField       = 5
	ckVariable    = 6
	ckInterface   = 8
	ckModule      = 9
	ckKeyword     = 14
	ckSnippet     = 15
	ckConstant    = 21
	ckStruct      = 22
	ckOperator    = 24
	ckTypeParam   = 25
	ckClass       = 7
	ckValue       = 12
	ckEnumMember  = 20
	ckReference   = 18
	ckFolder      = 19
	skFile        = 1
	skModule      = 2
	skPackage     = 4
	skMethod      = 6
	skField       = 8
	skInterface   = 11
	skFunction    = 12
	skVariable    = 13
	skConstant    = 14
	skStruct      = 23
	skOperator    = 25
	skTypeParam   = 26
	skClass       = 5
	hlText        = 1
	hlRead        = 2
	hlWrite       = 3
	formatPlain   = 1
	formatSnippet = 2
)

// ---------------------------------------------------------------- URIs

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.Clean(filepath.FromSlash(p))
}

func pathToURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
