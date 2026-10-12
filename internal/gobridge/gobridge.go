// Package gobridge links Go packages into Gocpy programs.
//
// For the Go packages a program imports, it creates a small Go module that
// requires the Go modules listed under require.go in gocpy.yml, generates cgo
// exports for the supported functions, builds them with
// `go build -buildmode=c-archive`, and emits
//   - a Gocpy stub package per Go package (@extern declarations + wrappers),
//   - a C shim that adapts Gocpy values (strings, []byte, out-parameters).
//     With Boehm GC, Go runs on a pool of threads unknown to the collector
//     while the calling goroutine waits in GC_do_blocking.
//
// Supported in 0.2: package-level functions whose parameters and results are
// bool, integer and float types, string and []byte (named types with these
// underlying types included), with an optional trailing error result; and
// constants of those types. Go panics abort the program like Gocpy panics.
package gobridge

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/rusfort/gocpy/internal/mod"
)

//go:embed gen/main.go.txt
var genSource string

// genVersion changes whenever generated code changes (cache key).
const genVersion = "gocpy-gobridge-4"

// Request describes the Go side of a program.
type Request struct {
	Root     string            // main module root
	Requires map[string]string // Go module path -> version
	Replace  map[string]string // Go module path -> dir or path@version
	Packages []string          // imported Go packages
	CC       string
	Sum      *mod.SumFile
	Log      io.Writer
}

// Package is the Gocpy view of a Go package.
type Package struct {
	Path        string            `json:"path"`
	Name        string            `json:"name"`
	Stub        string            `json:"stub"` // generated .gocpy file
	Unsupported map[string]string `json:"unsupported"`
}

// Result of a bridge build.
type Result struct {
	Packages  map[string]*Package `json:"packages"`
	Archive   string              `json:"archive"` // libgocpygo.a ("" if no functions are used)
	Shim      string              `json:"shim"`    // C file to compile with the program
	LinkFlags []string            `json:"link_flags"`
}

type apiFunc struct {
	Name    string   `json:"name"`
	ID      string   `json:"id"`
	Params  []string `json:"params"`
	Results []string `json:"results"`
	Error   bool     `json:"error"`
}

type apiConst struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type apiPkg struct {
	Path        string            `json:"path"`
	Name        string            `json:"name"`
	Funcs       []apiFunc         `json:"funcs"`
	Consts      []apiConst        `json:"consts"`
	Unsupported map[string]string `json:"unsupported"`
}

func goTool() (string, error) {
	if g := os.Getenv("GOCPY_GO"); g != "" {
		return g, nil
	}
	g, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("require.go in gocpy.yml needs the Go toolchain (go 1.21 or newer) in PATH; install it from https://go.dev/dl/ or set GOCPY_GO")
	}
	return g, nil
}

func cacheRoot() (string, error) {
	if d := os.Getenv("GOCPY_CACHE"); d != "" {
		return filepath.Join(d, "gobridge"), nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "gocpy", "gobridge"), nil
}

func (r *Request) goMod() string {
	var b strings.Builder
	b.WriteString("module gocpybridge\n\ngo 1.21\n")
	ks := keys(r.Requires)
	if len(ks) > 0 {
		b.WriteString("\nrequire (\n")
		for _, k := range ks {
			fmt.Fprintf(&b, "\t%s %s\n", k, r.Requires[k])
		}
		b.WriteString(")\n")
	}
	rs := keys(r.Replace)
	if len(rs) > 0 {
		b.WriteString("\nreplace (\n")
		for _, k := range rs {
			v := r.Replace[k]
			if p, ver, ok := strings.Cut(v, "@"); ok {
				fmt.Fprintf(&b, "\t%s => %s %s\n", k, p, ver)
			} else {
				fmt.Fprintf(&b, "\t%s => %s\n", k, v)
			}
		}
		b.WriteString(")\n")
	}
	return b.String()
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Build prepares (or reuses from the cache) the bridge for req.
func Build(req Request) (*Result, error) {
	gobin, err := goTool()
	if err != nil {
		return nil, err
	}
	gover, err := exec.Command(gobin, "env", "GOVERSION").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOVERSION: %v", err)
	}
	goMod := req.goMod()
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n%s/%s\n%s\n%s\n", genVersion, strings.TrimSpace(string(gover)), goMod,
		runtime.GOOS, runtime.GOARCH, req.CC, strings.Join(req.Packages, " "))
	for _, v := range req.Replace { // local replacements: rebuild when sources change
		if !strings.Contains(v, "@") {
			fmt.Fprintf(h, "%s\n", treeStamp(v))
		}
	}
	h.Write([]byte(genSource))
	key := fmt.Sprintf("%x", h.Sum(nil))[:20]
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, key)
	if res, err := loadResult(dir); err == nil {
		mergeSum(req.Sum, dir)
		return res, nil
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(root, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	files := map[string]string{
		"go.mod":      goMod,
		"go.sum":      req.Sum.Text(),
		"gen/main.go": genSource,
	}
	js, _ := json.Marshal(map[string]any{"packages": req.Packages})
	files["request.json"] = string(js)
	for name, content := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "CGO_ENABLED=1", "CC="+req.CC, "GOWORK=off")
	if req.Log != nil {
		fmt.Fprintf(req.Log, "gocpy: preparing Go packages %s\n", strings.Join(req.Packages, ", "))
	}
	if err := runGo(gobin, work, env, "run", "./gen", "request.json"); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(work, "api.json"))
	if err != nil {
		return nil, err
	}
	var api struct {
		Packages []apiPkg `json:"packages"`
	}
	if err := json.Unmarshal(data, &api); err != nil {
		return nil, err
	}
	res := &Result{Packages: map[string]*Package{}}
	nfuncs := 0
	var shim strings.Builder
	shim.WriteString(shimHeader)
	for i, p := range api.Packages {
		stub := filepath.Join(dir, "stubs", fmt.Sprintf("p%d", i), p.Name+".gocpy")
		res.Packages[p.Path] = &Package{Path: p.Path, Name: p.Name, Stub: stub, Unsupported: p.Unsupported}
		src := stubSource(p)
		sp := filepath.Join(work, "stubs", fmt.Sprintf("p%d", i), p.Name+".gocpy")
		os.MkdirAll(filepath.Dir(sp), 0o755)
		if err := os.WriteFile(sp, []byte(src), 0o644); err != nil {
			return nil, err
		}
		for _, f := range p.Funcs {
			nfuncs++
			shim.WriteString(shimFunc(f))
		}
	}
	res.Shim = filepath.Join(dir, "gocpy_gobridge.c")
	if err := os.WriteFile(filepath.Join(work, "gocpy_gobridge.c"), []byte(shim.String()), 0o644); err != nil {
		return nil, err
	}
	if nfuncs > 0 {
		if err := runGo(gobin, work, env, "build", "-buildmode=c-archive", "-trimpath", "-o", "libgocpygo.a", "./bridge"); err != nil {
			return nil, err
		}
		res.Archive = filepath.Join(dir, "libgocpygo.a")
		switch runtime.GOOS {
		case "darwin":
			res.LinkFlags = []string{"-framework", "CoreFoundation", "-framework", "Security", "-lresolv"}
		case "linux":
			res.LinkFlags = []string{"-ldl", "-lpthread"}
		default:
			res.LinkFlags = []string{"-lpthread"}
		}
	}
	mergeSum(req.Sum, work)
	rj, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "result.json"), rj, 0o644); err != nil {
		return nil, err
	}
	os.RemoveAll(dir)
	if err := os.Rename(work, dir); err != nil {
		return nil, err
	}
	return res, nil
}

func runGo(gobin, dir string, env []string, args ...string) error {
	cmd := exec.Command(gobin, args...)
	cmd.Dir, cmd.Env = dir, env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %v\n%s", args[0], err, strings.TrimSpace(out.String()))
	}
	return nil
}

func loadResult(dir string) (*Result, error) {
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil, err
	}
	res := &Result{}
	if err := json.Unmarshal(data, res); err != nil {
		return nil, err
	}
	for _, p := range res.Packages {
		if _, err := os.Stat(p.Stub); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func mergeSum(sum *mod.SumFile, dir string) {
	if sum == nil {
		return
	}
	if data, err := os.ReadFile(filepath.Join(dir, "go.sum")); err == nil {
		sum.Merge(string(data))
	}
}

// treeStamp summarizes modification times of .go files in a local replacement.
func treeStamp(dir string) string {
	h := sha256.New()
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && (strings.HasSuffix(p, ".go") || filepath.Base(p) == "go.mod") {
			fmt.Fprintf(h, "%s %d %d\n", p, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ---------------------------------------------------------------- Gocpy stub

func stubSource(p apiPkg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by gocpy from Go package %q; DO NOT EDIT.\npackage %s\n", p.Path, p.Name)
	needErrors := false
	for _, f := range p.Funcs {
		needErrors = needErrors || f.Error
	}
	if needErrors {
		b.WriteString("\nimport \"std/errors\"\n")
	}
	for _, c := range p.Consts {
		if c.Type != "" {
			fmt.Fprintf(&b, "\nconst %s %s = %s", c.Name, c.Type, c.Value)
		} else {
			fmt.Fprintf(&b, "\nconst %s = %s", c.Name, c.Value)
		}
	}
	b.WriteString("\n")
	for _, f := range p.Funcs {
		ext := strings.Replace(f.ID, "gobrx", "gobr", 1)
		var params, args, outs []string
		for i, t := range f.Params {
			params = append(params, fmt.Sprintf("a%d %s", i, t))
			args = append(args, fmt.Sprintf("a%d", i))
		}
		for i, t := range f.Results {
			outs = append(outs, fmt.Sprintf("o%d *%s", i, t))
		}
		low := "gobr_" + strings.TrimPrefix(f.ID, "gocpy_gobrx_")
		fmt.Fprintf(&b, "\n@extern(%q)\nfunc %s(%s) int\n", ext, low,
			strings.Join(append(append(append([]string{}, params...), outs...), "e *string"), ", "))
		var rs []string
		for _, t := range f.Results {
			rs = append(rs, t)
		}
		if f.Error {
			rs = append(rs, "error")
		}
		ret := ""
		switch {
		case len(rs) == 1:
			ret = " " + rs[0]
		case len(rs) > 1:
			ret = " (" + strings.Join(rs, ", ") + ")"
		}
		fmt.Fprintf(&b, "\nfunc %s(%s)%s {\n", f.Name, strings.Join(params, ", "), ret)
		var callArgs, vals []string
		callArgs = append(callArgs, args...)
		for i, t := range f.Results {
			fmt.Fprintf(&b, "\tvar o%d %s\n", i, t)
			callArgs = append(callArgs, fmt.Sprintf("&o%d", i))
			vals = append(vals, fmt.Sprintf("o%d", i))
		}
		b.WriteString("\tvar e string\n")
		callArgs = append(callArgs, "&e")
		call := fmt.Sprintf("%s(%s)", low, strings.Join(callArgs, ", "))
		if f.Error {
			fmt.Fprintf(&b, "\tif %s != 0 {\n\t\treturn %s\n\t}\n", call, strings.Join(append(append([]string{}, vals...), "errors.New(e)"), ", "))
			fmt.Fprintf(&b, "\treturn %s\n", strings.Join(append(vals, "nil"), ", "))
		} else {
			fmt.Fprintf(&b, "\t_ = %s\n", call)
			if len(vals) > 0 {
				fmt.Fprintf(&b, "\treturn %s\n", strings.Join(vals, ", "))
			}
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- C shim

const shimHeader = `/* generated by gocpy (Go bridge) — do not edit */
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include "gocpy_rt.h"

_Noreturn void gocpy_panic(const char *msg);
gocpy_string gocpy_string_from_bytes(gocpy_slice b);
gocpy_slice gocpy_bytes_from_string(gocpy_string s);

#ifdef GOCPY_GC_BOEHM
/* Go code never runs on threads known to the Boehm collector: calls are
 * executed by a pool of plain pthreads (created before gc.h redirects
 * pthread_create), while the calling goroutine waits in GC_do_blocking.
 * Go only sees copies of Gocpy data, so the pool needs no GC roots. */
typedef struct gocpy_gobr_job {
    void *(*fn)(void *);
    void *arg;
    int done;
    pthread_cond_t cv;
    struct gocpy_gobr_job *next;
} gocpy_gobr_job;

static pthread_mutex_t gocpy_gobr_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t gocpy_gobr_work = PTHREAD_COND_INITIALIZER;
static gocpy_gobr_job *gocpy_gobr_head, *gocpy_gobr_tail;
static int gocpy_gobr_idle, gocpy_gobr_queued, gocpy_gobr_workers;

static void *gocpy_gobr_worker(void *unused) {
    (void)unused;
    pthread_mutex_lock(&gocpy_gobr_mu);
    for (;;) {
        while (!gocpy_gobr_head) {
            gocpy_gobr_idle++;
            pthread_cond_wait(&gocpy_gobr_work, &gocpy_gobr_mu);
            gocpy_gobr_idle--;
        }
        gocpy_gobr_job *j = gocpy_gobr_head;
        gocpy_gobr_head = j->next;
        if (!gocpy_gobr_head) gocpy_gobr_tail = NULL;
        gocpy_gobr_queued--;
        pthread_mutex_unlock(&gocpy_gobr_mu);
        j->fn(j->arg);
        pthread_mutex_lock(&gocpy_gobr_mu);
        j->done = 1;
        pthread_cond_signal(&j->cv);
    }
    return NULL;
}

static void *gocpy_gobr_submit(void *p) {
    gocpy_gobr_job *j = p;
    pthread_mutex_lock(&gocpy_gobr_mu);
    j->next = NULL;
    if (gocpy_gobr_tail) gocpy_gobr_tail->next = j; else gocpy_gobr_head = j;
    gocpy_gobr_tail = j;
    gocpy_gobr_queued++;
    if (gocpy_gobr_idle < gocpy_gobr_queued) {
        pthread_attr_t attr;
        pthread_t t;
        pthread_attr_init(&attr);
        pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
        int err = pthread_create(&t, &attr, gocpy_gobr_worker, NULL);
        pthread_attr_destroy(&attr);
        if (err == 0) {
            gocpy_gobr_workers++;
        } else if (gocpy_gobr_workers == 0) {
            pthread_mutex_unlock(&gocpy_gobr_mu);
            gocpy_panic("cannot start a Go bridge thread (pthread_create failed)");
        } /* otherwise an existing worker will take the job */
    } else {
        pthread_cond_signal(&gocpy_gobr_work);
    }
    while (!j->done) pthread_cond_wait(&j->cv, &gocpy_gobr_mu);
    pthread_mutex_unlock(&gocpy_gobr_mu);
    return NULL;
}

#define GC_THREADS
#include <gc.h>

static void gocpy_gobr_call(void *(*fn)(void *), void *arg) {
    gocpy_gobr_job j;
    memset(&j, 0, sizeof j);
    j.fn = fn;
    j.arg = arg;
    pthread_cond_init(&j.cv, NULL);
    GC_do_blocking(gocpy_gobr_submit, &j);
    pthread_cond_destroy(&j.cv);
}
#define GOCPY_GOBR_CALL(f, x) gocpy_gobr_call((f), (x))
#else
#define GOCPY_GOBR_CALL(f, x) (void)(f)(x)
#endif

static gocpy_string gocpy_gobr_str(char *p, int64_t n) {
    if (!p || n <= 0) { free(p); return (gocpy_string){ NULL, 0 }; }
    gocpy_string s = gocpy_string_from_bytes((gocpy_slice){ p, n, n });
    free(p);
    return s;
}

static gocpy_slice gocpy_gobr_bytes(void *p, int64_t n) {
    if (!p || n <= 0) { free(p); return (gocpy_slice){ NULL, 0, 0 }; }
    gocpy_slice s = gocpy_bytes_from_string((gocpy_string){ (const uint8_t *)p, n });
    free(p);
    return s;
}

static void gocpy_gobr_fail(char *msg) {
    static char buf[1024];
    snprintf(buf, sizeof buf, "panic in Go code: %s", msg ? msg : "?");
    gocpy_panic(buf);
}

`

var cOf = map[string]string{
	"bool": "bool", "int": "int64_t", "int8": "int8_t", "int16": "int16_t", "int32": "int32_t", "int64": "int64_t",
	"uint": "uint64_t", "uint8": "uint8_t", "uint16": "uint16_t", "uint32": "uint32_t", "uint64": "uint64_t",
	"uintptr": "uintptr_t", "float32": "float", "float64": "double", "string": "gocpy_string", "[]byte": "gocpy_slice",
}

func shimFunc(f apiFunc) string {
	var b strings.Builder
	n := strings.TrimPrefix(f.ID, "gocpy_gobrx_")
	ext := "gocpy_gobr_" + n
	st := "gocpy_gobr_" + n + "_t"
	// Go export prototype
	var gp []string
	for i, t := range f.Params {
		switch t {
		case "string":
			gp = append(gp, "char *", "int64_t")
		case "[]byte":
			gp = append(gp, "void *", "int64_t")
		default:
			gp = append(gp, cOf[t])
			_ = i
		}
	}
	for _, t := range f.Results {
		switch t {
		case "string":
			gp = append(gp, "char **", "int64_t *")
		case "[]byte":
			gp = append(gp, "void **", "int64_t *")
		default:
			gp = append(gp, cOf[t]+" *")
		}
	}
	gp = append(gp, "char **", "int64_t *")
	fmt.Fprintf(&b, "extern int %s(%s);\n", f.ID, strings.Join(gp, ", "))
	// argument block
	fmt.Fprintf(&b, "typedef struct {")
	for i, t := range f.Params {
		fmt.Fprintf(&b, " %s a%d;", cOf[t], i)
	}
	for i, t := range f.Results {
		switch t {
		case "string":
			fmt.Fprintf(&b, " char *o%dp; int64_t o%dn;", i, i)
		case "[]byte":
			fmt.Fprintf(&b, " void *o%dp; int64_t o%dn;", i, i)
		default:
			fmt.Fprintf(&b, " %s o%d;", cOf[t], i)
		}
	}
	fmt.Fprintf(&b, " char *ep; int64_t en; int st; } %s;\n", st)
	// call in blocking mode
	var ca []string
	for i, t := range f.Params {
		switch t {
		case "string":
			ca = append(ca, fmt.Sprintf("(char *)x->a%d.ptr", i), fmt.Sprintf("x->a%d.len", i))
		case "[]byte":
			ca = append(ca, fmt.Sprintf("x->a%d.data", i), fmt.Sprintf("x->a%d.len", i))
		default:
			ca = append(ca, fmt.Sprintf("x->a%d", i))
		}
	}
	for i, t := range f.Results {
		switch t {
		case "string", "[]byte":
			ca = append(ca, fmt.Sprintf("&x->o%dp", i), fmt.Sprintf("&x->o%dn", i))
		default:
			ca = append(ca, fmt.Sprintf("&x->o%d", i))
		}
	}
	ca = append(ca, "&x->ep", "&x->en")
	fmt.Fprintf(&b, "static void *%s_call(void *p) { %s *x = p; x->st = %s(%s); return NULL; }\n", ext, st, f.ID, strings.Join(ca, ", "))
	// Gocpy-facing function
	var ps []string
	for i, t := range f.Params {
		ps = append(ps, fmt.Sprintf("%s a%d", cOf[t], i))
	}
	for i, t := range f.Results {
		ps = append(ps, fmt.Sprintf("%s *o%d", cOf[t], i))
	}
	ps = append(ps, "gocpy_string *e")
	fmt.Fprintf(&b, "int64_t %s(%s) {\n    %s x;\n    memset(&x, 0, sizeof x);\n", ext, strings.Join(ps, ", "), st)
	for i := range f.Params {
		fmt.Fprintf(&b, "    x.a%d = a%d;\n", i, i)
	}
	fmt.Fprintf(&b, "    GOCPY_GOBR_CALL(%s_call, &x);\n", ext)
	fmt.Fprintf(&b, "    if (x.st == 2) gocpy_gobr_fail(x.ep);\n")
	for i, t := range f.Results {
		switch t {
		case "string":
			fmt.Fprintf(&b, "    *o%d = gocpy_gobr_str(x.o%dp, x.o%dn);\n", i, i, i)
		case "[]byte":
			fmt.Fprintf(&b, "    *o%d = gocpy_gobr_bytes(x.o%dp, x.o%dn);\n", i, i, i)
		default:
			fmt.Fprintf(&b, "    *o%d = x.o%d;\n", i, i)
		}
	}
	fmt.Fprintf(&b, "    if (x.st == 1) *e = gocpy_gobr_str(x.ep, x.en);\n    return x.st;\n}\n\n")
	return b.String()
}
