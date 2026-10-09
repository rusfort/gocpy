/* Gocpy runtime — stage 2.
 * Strings, slices, allocation (Boehm GC or malloc), formatting, runtime errors. */
#ifndef GOCPY_RT_H
#define GOCPY_RT_H

#include <stdint.h>
#include <stdbool.h>
#include <stddef.h>
#include <string.h>

typedef struct { const uint8_t *ptr; int64_t len; } gocpy_string;
typedef struct { void *data; int64_t len; int64_t cap; } gocpy_slice;
typedef struct { uint8_t *buf; int64_t len; int64_t cap; } gocpy_sb;

#define GOCPY_STR(lit) ((gocpy_string){ (const uint8_t *)(lit), (int64_t)(sizeof(lit) - 1) })
#define GOCPY_ABSENT INT64_MIN

/* ---- startup / errors ---- */
void gocpy_init(int argc, char **argv);
_Noreturn void gocpy_panic(const char *msg);
_Noreturn void gocpy_panic_index(int64_t i, int64_t len);
_Noreturn void gocpy_panic_slice(int64_t lo, int64_t hi, int64_t cap);

static inline int64_t gocpy_bounds(int64_t i, int64_t len) {
    if ((uint64_t)i >= (uint64_t)len) gocpy_panic_index(i, len);
    return i;
}
static inline void *gocpy_nonnil(void *p) {
    if (p == NULL) gocpy_panic("invalid memory address or nil pointer dereference");
    return p;
}
#define GOCPY_DIV_CHECK(b) do { if ((b) == 0) gocpy_panic("integer divide by zero"); } while (0)

/* ---- memory ---- */
/* manual memory (unsafe blocks): not scanned by the GC */
uint8_t *gocpy_raw_malloc(int64_t n, bool zero);
uint8_t *gocpy_raw_realloc(void *p, int64_t n);
void gocpy_raw_free(void *p);
void gocpy_raw_memcpy(void *dst, const void *src, int64_t n);
void gocpy_raw_memset(void *p, uint8_t b, int64_t n);
void *gocpy_alloc(size_t n);          /* zeroed, GC-managed when built with gc: boehm */

/* ---- strings ---- */
gocpy_string gocpy_string_concat(gocpy_string a, gocpy_string b);
bool gocpy_string_eq(gocpy_string a, gocpy_string b);
int gocpy_string_cmp(gocpy_string a, gocpy_string b);
uint8_t gocpy_string_at(gocpy_string s, int64_t i);
gocpy_string gocpy_string_slice(gocpy_string s, int64_t lo, int64_t hi);
gocpy_string gocpy_string_from_bytes(gocpy_slice b);
gocpy_slice gocpy_bytes_from_string(gocpy_string s);
gocpy_string gocpy_string_from_rune(int64_t r);
int64_t gocpy_decode_rune(gocpy_string s, int64_t i, int32_t *r);

/* ---- slices ---- */
gocpy_slice gocpy_make_slice(int64_t len, int64_t cap, size_t es);
gocpy_slice gocpy_slice_lit(int64_t n, size_t es, const void *src);
gocpy_slice gocpy_slice_slice(gocpy_slice s, int64_t lo, int64_t hi, int64_t max, size_t es);
gocpy_slice gocpy_slice_from_array(void *data, int64_t len);
void gocpy_slice_append(gocpy_slice *s, const void *elem, size_t es);
void gocpy_slice_append_slice(gocpy_slice *s, gocpy_slice other, size_t es);

/* ---- string builder / formatting (Go %v semantics) ---- */
gocpy_sb gocpy_sb_new(void);
void gocpy_sb_bytes(gocpy_sb *sb, const void *p, int64_t n);
void gocpy_sb_byte(gocpy_sb *sb, uint8_t c);
void gocpy_sb_cstr(gocpy_sb *sb, const char *s);
void gocpy_sb_str(gocpy_sb *sb, gocpy_string s);
void gocpy_sb_i64(gocpy_sb *sb, int64_t v);
void gocpy_sb_u64(gocpy_sb *sb, uint64_t v);
void gocpy_sb_hex(gocpy_sb *sb, uint64_t v, bool upper);
void gocpy_sb_f64(gocpy_sb *sb, double v, int bits);
void gocpy_sb_fprec(gocpy_sb *sb, double v, int prec, char verb);
void gocpy_sb_bool(gocpy_sb *sb, bool v);
void gocpy_sb_ptr(gocpy_sb *sb, const void *p);
void gocpy_sb_pad(gocpy_sb *sb, int64_t mark, int width, char align);
gocpy_string gocpy_sb_string(gocpy_sb *sb);
void gocpy_sb_write_stdout(gocpy_sb *sb);

/* ---------------------------------------------------------------- exceptions
 * Exceptions are propagated explicitly: `throw` stores the exception in
 * gocpy_exc and jumps to the innermost handler; after every call to a
 * @throws function the caller tests gocpy_exc. No setjmp/longjmp, so locals
 * stay in registers and the non-throwing path costs one predictable branch.
 * The pending exception is per goroutine (thread-local state).
 */
typedef struct gocpy_method {
    const char *name;                                  /* "Area" */
    const char *sig;                                   /* canonical signature, '1' prefix = @throws */
    void *fn;                                          /* wrapper: R fn(void *self, params...) */
} gocpy_method;

typedef struct gocpy_typeinfo {
    const char *name;                                  /* "main.ParseError" */
    void (*fmt)(gocpy_sb *sb, const void *value);      /* %v / String() / Error() */
    size_t size;
    bool is_ptr;                                       /* pointer-shaped: stored directly in interfaces */
    bool (*eq)(const void *a, const void *b);          /* NULL: not comparable */
    int nmethods;
    const gocpy_method *methods;                       /* method set (for dynamic itab lookup) */
} gocpy_typeinfo;

typedef struct gocpy_exception {
    const gocpy_typeinfo *type;
    void *data;                                        /* copy of the thrown value */
} gocpy_exception;

/* per-goroutine state; allocated as a GC root so a pending exception stays alive */
typedef struct gocpy_gstate {
    gocpy_exception *exc;
} gocpy_gstate;
extern _Thread_local gocpy_gstate *gocpy_g;
#define gocpy_exc (gocpy_g->exc)                       /* pending exception or NULL */

#define GOCPY_PENDING() __builtin_expect(gocpy_exc != NULL, 0)

gocpy_exception *gocpy_exc_basic(gocpy_string message); /* Exception("...") */
gocpy_exception *gocpy_exc_new(const gocpy_typeinfo *ti, const void *value, size_t size);
gocpy_string gocpy_exc_message(gocpy_exception *e);
gocpy_string gocpy_exc_typename(gocpy_exception *e);
gocpy_string gocpy_exc_string(gocpy_exception *e);
void gocpy_uncaught(void);                             /* abort if an exception is pending */

/* ---------------------------------------------------------------- goroutines & channels
 * Goroutines are OS threads (pthreads, registered with the GC). Channels,
 * WaitGroup and Mutex share one runtime lock and condition variable: simple
 * and correct; a scheduler with per-object wait queues is a later optimisation.
 * When every goroutine is blocked on runtime primitives the program stops
 * with "all goroutines are asleep - deadlock!" (exit code 2), as in Go.
 */
void gocpy_go(void (*fn)(void *), void *arg);

typedef struct gocpy_chan gocpy_chan;
gocpy_chan *gocpy_chan_make(size_t elem, int64_t cap);
void gocpy_chan_send(gocpy_chan *c, const void *v);
bool gocpy_chan_recv(gocpy_chan *c, void *out);        /* false: closed and drained */
void gocpy_chan_close(gocpy_chan *c);
int64_t gocpy_chan_len(gocpy_chan *c);
int64_t gocpy_chan_cap(gocpy_chan *c);

typedef struct {
    gocpy_chan *c;
    bool send;
    void *val;                                         /* send: source, recv: destination */
    bool ok;                                           /* recv: false if closed */
} gocpy_selcase;
int gocpy_select(gocpy_selcase *cases, int n, bool has_default); /* index or -1 for default */

typedef struct { int64_t n; } gocpy_waitgroup;
void gocpy_wg_add(gocpy_waitgroup *wg, int64_t d);
void gocpy_wg_done(gocpy_waitgroup *wg);
void gocpy_wg_wait(gocpy_waitgroup *wg);

typedef struct { bool locked; } gocpy_mutex;
void gocpy_mutex_lock(gocpy_mutex *m);
void gocpy_mutex_unlock(gocpy_mutex *m);

void gocpy_sleep(int64_t ns);
int64_t gocpy_now(void);                               /* monotonic nanoseconds */
int64_t gocpy_since(int64_t t);
static inline int64_t gocpy_dur_ms(int64_t d) { return d / 1000000; }
static inline double gocpy_dur_seconds(int64_t d) { return (double)d / 1e9; }

/* ---------------------------------------------------------------- stage 5
 * Function values are {fn, env}: fn takes the environment as its first
 * parameter (NULL for plain functions). Closures capture variables by
 * reference: env is an array of pointers to heap cells.
 */
typedef struct { void *fn; void *env; } gocpy_func;
static inline void *gocpy_func_fn(gocpy_func f) {
    if (f.fn == NULL) gocpy_panic("invalid memory address or nil pointer dereference (call of nil func)");
    return f.fn;
}

/* Interfaces: {itab, data}. data points to a heap copy of the value, or is
 * the pointer itself for pointer-shaped types. itab->fns follows the
 * interface's method order (sorted by name). */
typedef struct gocpy_itab {
    const gocpy_typeinfo *type;
    void *fns[];
} gocpy_itab;
typedef struct { const gocpy_itab *itab; void *data; } gocpy_iface;
typedef struct gocpy_idesc {
    const char *name;
    int n;
    const char *const *names;
    const char *const *sigs;
} gocpy_idesc;

void *gocpy_box(const void *v, size_t n);
const gocpy_itab *gocpy_itab_get(const gocpy_idesc *I, const gocpy_typeinfo *T); /* NULL: not implemented */
gocpy_iface gocpy_iface_conv(gocpy_iface x, const gocpy_idesc *I);  /* statically valid conversion */
bool gocpy_iface_to_iface(gocpy_iface x, const gocpy_idesc *I, gocpy_iface *out); /* x.(I) */
_Noreturn void gocpy_panic_assert(gocpy_iface x, const char *iface, const char *want);
bool gocpy_iface_eq(gocpy_iface a, gocpy_iface b);
void gocpy_sb_iface(gocpy_sb *sb, gocpy_iface x);
static inline void *gocpy_iface_fn(gocpy_iface x, int i) {
    if (x.itab == NULL) gocpy_panic("invalid memory address or nil pointer dereference (method call on nil interface)");
    return x.itab->fns[i];
}
static inline const gocpy_typeinfo *gocpy_iface_type(gocpy_iface x) { return x.itab ? x.itab->type : NULL; }
/* pointer to the dynamic value (for typeinfo fmt/eq) */
#define GOCPY_IFACE_VALP(x) ((x).itab->type->is_ptr ? (const void *)&(x).data : (const void *)(x).data)

/* exceptions <-> interfaces */
gocpy_exception *gocpy_exc_from_iface(gocpy_iface x);  /* throw err */
bool gocpy_exc_as_iface(gocpy_exception *e, const gocpy_idesc *I, gocpy_iface *out); /* catch e: I */

/* std/errors */
gocpy_iface gocpy_errors_new(gocpy_string msg);

/* Maps: hash table with insertion-ordered iteration. */
typedef struct gocpy_map gocpy_map;
typedef uint64_t (*gocpy_hashfn)(const void *);
typedef bool (*gocpy_eqfn)(const void *, const void *);
gocpy_map *gocpy_map_make(size_t ks, size_t vs, gocpy_hashfn h, gocpy_eqfn eq, int64_t hint);
bool gocpy_map_get(gocpy_map *m, const void *k, void *out, size_t vs); /* zero value if missing */
void *gocpy_map_assign(gocpy_map *m, const void *k);  /* slot for m[k] (inserted if missing) */
void gocpy_map_delete(gocpy_map *m, const void *k);
int64_t gocpy_map_len(gocpy_map *m);
bool gocpy_map_next(gocpy_map *m, int64_t *it, void **k, void **v);
uint64_t gocpy_hash_bytes(const void *p, size_t n);
uint64_t gocpy_hash_string(const void *p);
uint64_t gocpy_hash_f64(const void *p);
uint64_t gocpy_hash_f32(const void *p);
static inline uint64_t gocpy_hash_mix(uint64_t h, uint64_t x) {
    h ^= x + 0x9e3779b97f4a7c15ULL + (h << 6) + (h >> 2);
    return h;
}

/* defer: per-call LIFO list run on every exit path */
typedef struct gocpy_defer { void (*fn)(void *); void *arg; struct gocpy_defer *next; } gocpy_defer;
void gocpy_defer_push(gocpy_defer **list, void (*fn)(void *), void *arg);
void gocpy_run_defers(gocpy_defer **list);

#endif
