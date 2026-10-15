// Package gobridge links Go packages into Gocpy programs.
//
// For the Go packages a program imports, it creates a small Go module that
// requires the Go modules listed under require.go in gocpy.yml, runs a
// generator that reads the API of the packages and writes
//   - a table of the Go functions, methods, field accessors and variables
//     called through the bridge (built with -buildmode=c-archive together
//     with a static runtime: handles of Go values and a reflect-based codec);
//   - a Gocpy stub package per Go package. Go types become Gocpy types that
//     hold a handle of the Go value (std/gobridge.Ref); any, slices, maps and
//     basic values are copied.
//
// The C shim sets the hook of the C runtime that runs calls on the Go side.
// With Boehm GC, Go runs on a pool of threads unknown to the collector while
// the calling goroutine waits in GC_do_blocking.
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

//go:embed gen/runtime.go.txt
var runtimeSource string

// genVersion changes whenever generated code changes (cache key).
const genVersion = "gocpy-gobridge-9"

// Request describes the Go side of a program.
type Request struct {
	Root     string            // main module root
	Requires map[string]string // Go module path -> version
	Replace  map[string]string // Go module path -> dir or path@version
	Packages []string          // imported Go packages
	// Local Go packages: bridge import path (gocpybridge/local/...) -> the
	// directory with their .go files. They are copied into the bridge module.
	Local map[string]string
	CC    string
	Sum   *mod.SumFile
	Log   io.Writer
}

// Package is the Gocpy view of a Go package.
type Package struct {
	Path        string            `json:"path"`
	Name        string            `json:"name"`
	Stub        string            `json:"stub"` // generated .gocpy file
	Unsupported map[string]string `json:"unsupported"`
	// Direct: imported by the program (false: only its types are used by
	// other Go packages).
	Direct bool `json:"direct"`
}

// Result of a bridge build.
type Result struct {
	Packages  map[string]*Package `json:"packages"`
	Archive   string              `json:"archive"` // libgocpygo.a ("" if no functions are used)
	Shim      string              `json:"shim"`    // C file to compile with the program
	LinkFlags []string            `json:"link_flags"`
}

// GoPosPrefix starts the comment placed above each stub declaration with the
// position of the Go declaration (used by the language server).
const GoPosPrefix = "// go:pos "

type apiPkg struct {
	Path        string            `json:"path"`
	Name        string            `json:"name"`
	Stub        string            `json:"stub"`
	Unsupported map[string]string `json:"unsupported"`
	Direct      bool              `json:"direct"`
}

func goTool() (string, error) {
	if g := os.Getenv("GOCPY_GO"); g != "" {
		return g, nil
	}
	g, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("Go packages (require.go in gocpy.yml or imported .go files) need the Go toolchain (go 1.21 or newer) in PATH; install it from https://go.dev/dl/ or set GOCPY_GO")
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
	for _, k := range keys(req.Local) {
		fmt.Fprintf(h, "local %s %s\n", k, dirStamp(req.Local[k]))
	}
	h.Write([]byte(genSource))
	h.Write([]byte(runtimeSource))
	h.Write([]byte(shimSource))
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
		"go.mod":            goMod,
		"gen/main.go":       genSource,
		"bridge/runtime.go": runtimeSource,
	}
	if req.Sum != nil {
		files["go.sum"] = req.Sum.Text()
	}
	for k, src := range req.Local {
		names, err := localGoFiles(src)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			data, err := os.ReadFile(filepath.Join(src, n))
			if err != nil {
				return nil, err
			}
			files[strings.TrimPrefix(k, "gocpybridge/")+"/"+n] = string(data)
		}
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
		var shown []string
		for _, p := range req.Packages {
			if d, ok := req.Local[p]; ok {
				p = "Go files in " + d
			}
			shown = append(shown, p)
		}
		fmt.Fprintf(req.Log, "gocpy: preparing Go packages %s\n", strings.Join(shown, ", "))
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
	// positions in copied local Go files point to the original files
	var fixes []string
	for path, src := range req.Local {
		for _, w := range []string{work, evalLinks(work)} {
			pre := filepath.Join(w, filepath.FromSlash(strings.TrimPrefix(path, "gocpybridge/"))) + string(filepath.Separator)
			fixes = append(fixes, GoPosPrefix+pre, GoPosPrefix+src+string(filepath.Separator))
		}
	}
	fix := strings.NewReplacer(fixes...)
	res := &Result{Packages: map[string]*Package{}}
	for _, p := range api.Packages {
		sp := filepath.Join(work, filepath.FromSlash(p.Stub))
		if len(fixes) > 0 {
			data, err := os.ReadFile(sp)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(sp, []byte(fix.Replace(string(data))), 0o644); err != nil {
				return nil, err
			}
		}
		res.Packages[p.Path] = &Package{Path: p.Path, Name: p.Name, Stub: filepath.Join(dir, filepath.FromSlash(p.Stub)),
			Unsupported: p.Unsupported, Direct: p.Direct}
	}
	res.Shim = filepath.Join(dir, "gocpy_gobridge.c")
	if err := os.WriteFile(filepath.Join(work, "gocpy_gobridge.c"), []byte(shimSource), 0o644); err != nil {
		return nil, err
	}
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

// localGoFiles lists the non-test .go files of a directory.
func localGoFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// dirStamp hashes the contents of the local Go files of a directory.
func dirStamp(dir string) string {
	h := sha256.New()
	names, _ := localGoFiles(dir)
	for _, n := range names {
		data, _ := os.ReadFile(filepath.Join(dir, n))
		fmt.Fprintf(h, "%s %d\n", n, len(data))
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func evalLinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
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

// ---------------------------------------------------------------- C shim

const shimSource = `/* generated by gocpy (Go bridge) — do not edit */
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include "gocpy_rt.h"


#ifdef GOCPY_GC_BOEHM
/* Go code never runs on threads known to the Boehm collector: calls are
 * executed by a pool of plain pthreads (created before gc.h redirects
 * pthread_create), while the calling goroutine waits in GC_do_blocking.
 * Go only sees copies of Gocpy data, so the pool needs no GC roots. */
typedef struct gocpy_gobr_job {
    void *(*fn)(void *);
    void *arg;
    int done;
    int sleeping;
    pthread_cond_t cv;
    struct gocpy_gobr_job *next;
} gocpy_gobr_job;

static pthread_mutex_t gocpy_gobr_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t gocpy_gobr_work = PTHREAD_COND_INITIALIZER;
static gocpy_gobr_job *gocpy_gobr_head, *gocpy_gobr_tail;
static int gocpy_gobr_idle, gocpy_gobr_spinning, gocpy_gobr_queued, gocpy_gobr_workers;

/* Calls are short: workers and callers spin for a while before sleeping. */
#define GOCPY_GOBR_SPIN 20000

static void *gocpy_gobr_worker(void *unused) {
    (void)unused;
    pthread_mutex_lock(&gocpy_gobr_mu);
    for (;;) {
        if (!gocpy_gobr_head) {
            gocpy_gobr_spinning++;
            pthread_mutex_unlock(&gocpy_gobr_mu);
            for (int i = 0; i < GOCPY_GOBR_SPIN && !__atomic_load_n(&gocpy_gobr_head, __ATOMIC_ACQUIRE); i++) __asm__ volatile("" ::: "memory");
            pthread_mutex_lock(&gocpy_gobr_mu);
            gocpy_gobr_spinning--;
        }
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
        __atomic_store_n(&j->done, 1, __ATOMIC_RELEASE);
        if (j->sleeping) pthread_cond_signal(&j->cv);
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
    if (gocpy_gobr_idle + gocpy_gobr_spinning < gocpy_gobr_queued) {
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
    pthread_mutex_unlock(&gocpy_gobr_mu);
    for (int i = 0; i < GOCPY_GOBR_SPIN && !__atomic_load_n(&j->done, __ATOMIC_ACQUIRE); i++) __asm__ volatile("" ::: "memory");
    pthread_mutex_lock(&gocpy_gobr_mu);
    j->sleeping = 1;
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

extern int gocpy_gobrx_dispatch(int64_t id, void *in, int64_t inn, void **out, int64_t *outn, uint64_t *rel, int64_t nrel);

typedef struct {
    int64_t id;
    const void *in;
    int64_t inn;
    void **out;
    int64_t *outn;
    const uint64_t *rel;
    int64_t nrel;
    int st;
} gocpy_gobr_args;

static void *gocpy_gobr_run(void *p) {
    gocpy_gobr_args *x = p;
    x->st = gocpy_gobrx_dispatch(x->id, (void *)x->in, x->inn, x->out, x->outn, (uint64_t *)x->rel, x->nrel);
    return NULL;
}

static int gocpy_gobr_dispatch(int64_t id, const void *in, int64_t inn, void **out, int64_t *outn,
                               const uint64_t *rel, int64_t nrel) {
    gocpy_gobr_args x = { id, in, inn, out, outn, rel, nrel, 0 };
    GOCPY_GOBR_CALL(gocpy_gobr_run, &x);
    return x.st;
}

__attribute__((constructor)) static void gocpy_gobr_init(void) {
    gocpy_gobr_hook = gocpy_gobr_dispatch;
}
`
