#include "gocpy_rt.h"
#include <stdio.h>
#include <stdlib.h>
#include <math.h>

#ifdef GOCPY_GC_BOEHM
#define GC_THREADS
#include <gc.h>
#endif
#include <pthread.h>
#include <time.h>
#include <errno.h>

_Thread_local gocpy_gstate *gocpy_g;

static gocpy_gstate *gstate_new(void) {
#ifdef GOCPY_GC_BOEHM
    return (gocpy_gstate *)GC_MALLOC_UNCOLLECTABLE(sizeof(gocpy_gstate));
#else
    return (gocpy_gstate *)calloc(1, sizeof(gocpy_gstate));
#endif
}

static void gstate_free(gocpy_gstate *g) {
#ifdef GOCPY_GC_BOEHM
    GC_FREE(g);
#else
    free(g);
#endif
}

/* ---------------------------------------------------------------- startup */

void gocpy_init(int argc, char **argv) {
    (void)argc; (void)argv;
#ifdef GOCPY_GC_BOEHM
    GC_INIT();
#endif
    gocpy_g = gstate_new();
}

_Noreturn void gocpy_panic(const char *msg) {
    fflush(stdout);
    fprintf(stderr, "runtime error: %s\n", msg);
    exit(2);
}

_Noreturn void gocpy_panic_index(int64_t i, int64_t len) {
    char buf[128];
    snprintf(buf, sizeof buf, "index out of range [%lld] with length %lld", (long long)i, (long long)len);
    gocpy_panic(buf);
}

_Noreturn void gocpy_panic_slice(int64_t lo, int64_t hi, int64_t cap) {
    char buf[160];
    snprintf(buf, sizeof buf, "slice bounds out of range [%lld:%lld] with capacity %lld",
             (long long)lo, (long long)hi, (long long)cap);
    gocpy_panic(buf);
}

/* ---------------------------------------------------------------- memory */

void *gocpy_alloc(size_t n) {
    if (n == 0) n = 1;
#ifdef GOCPY_GC_BOEHM
    void *p = GC_MALLOC(n);
#else
    void *p = calloc(1, n);
#endif
    if (!p) gocpy_panic("out of memory");
    return p;
}

static void *gocpy_alloc_atomic(size_t n) { /* memory without pointers */
    if (n == 0) n = 1;
#ifdef GOCPY_GC_BOEHM
    void *p = GC_MALLOC_ATOMIC(n);
    if (p) memset(p, 0, n);
#else
    void *p = calloc(1, n);
#endif
    if (!p) gocpy_panic("out of memory");
    return p;
}

void *gocpy_alloc_atomic_bytes(size_t n) { return gocpy_alloc_atomic(n); }

/* ---------------------------------------------------------------- manual memory */

uint8_t *gocpy_raw_malloc(int64_t n, bool zero) {
    if (n < 0) gocpy_panic("malloc: negative size");
    size_t sz = n ? (size_t)n : 1;
    void *p = zero ? calloc(1, sz) : malloc(sz);
    if (!p) gocpy_panic("out of memory");
    return (uint8_t *)p;
}

uint8_t *gocpy_raw_realloc(void *p, int64_t n) {
    if (n < 0) gocpy_panic("realloc: negative size");
#ifdef GOCPY_GC_BOEHM
    if (p && GC_base(p)) gocpy_panic("realloc of GC-managed memory");
#endif
    void *q = realloc(p, n ? (size_t)n : 1);
    if (!q) gocpy_panic("out of memory");
    return (uint8_t *)q;
}

void gocpy_raw_free(void *p) {
    if (!p) return;
#ifdef GOCPY_GC_BOEHM
    if (GC_base(p)) gocpy_panic("free of GC-managed memory");
#endif
    free(p);
}

void gocpy_raw_memcpy(void *dst, const void *src, int64_t n) {
    if (n < 0) gocpy_panic("memcpy: negative size");
    if (n) memmove(dst, src, (size_t)n);
}

void gocpy_raw_memset(void *p, uint8_t b, int64_t n) {
    if (n < 0) gocpy_panic("memset: negative size");
    if (n) memset(p, b, (size_t)n);
}

/* ---------------------------------------------------------------- strings */

gocpy_string gocpy_string_concat(gocpy_string a, gocpy_string b) {
    if (a.len == 0) return b;
    if (b.len == 0) return a;
    uint8_t *p = gocpy_alloc_atomic((size_t)(a.len + b.len));
    memcpy(p, a.ptr, (size_t)a.len);
    memcpy(p + a.len, b.ptr, (size_t)b.len);
    return (gocpy_string){ p, a.len + b.len };
}

bool gocpy_string_eq(gocpy_string a, gocpy_string b) {
    return a.len == b.len && (a.len == 0 || memcmp(a.ptr, b.ptr, (size_t)a.len) == 0);
}

int gocpy_string_cmp(gocpy_string a, gocpy_string b) {
    int64_t n = a.len < b.len ? a.len : b.len;
    int r = n ? memcmp(a.ptr, b.ptr, (size_t)n) : 0;
    if (r != 0) return r < 0 ? -1 : 1;
    return a.len < b.len ? -1 : (a.len > b.len ? 1 : 0);
}

uint8_t gocpy_string_at(gocpy_string s, int64_t i) {
    return s.ptr[gocpy_bounds(i, s.len)];
}

gocpy_string gocpy_string_slice(gocpy_string s, int64_t lo, int64_t hi) {
    if (lo == GOCPY_ABSENT) lo = 0;
    if (hi == GOCPY_ABSENT) hi = s.len;
    if (lo < 0 || hi < lo || hi > s.len) gocpy_panic_slice(lo, hi, s.len);
    return (gocpy_string){ s.ptr + lo, hi - lo };
}

gocpy_string gocpy_string_from_bytes(gocpy_slice b) {
    uint8_t *p = gocpy_alloc_atomic((size_t)b.len);
    if (b.len) memcpy(p, b.data, (size_t)b.len);
    return (gocpy_string){ p, b.len };
}

gocpy_slice gocpy_bytes_from_string(gocpy_string s) {
    uint8_t *p = gocpy_alloc_atomic((size_t)s.len);
    if (s.len) memcpy(p, s.ptr, (size_t)s.len);
    return (gocpy_slice){ p, s.len, s.len };
}

gocpy_string gocpy_string_from_rune(int64_t r) {
    uint8_t *p = gocpy_alloc_atomic(4);
    int n;
    if (r < 0 || r > 0x10FFFF || (r >= 0xD800 && r <= 0xDFFF)) r = 0xFFFD;
    if (r < 0x80) { p[0] = (uint8_t)r; n = 1; }
    else if (r < 0x800) { p[0] = 0xC0 | (r >> 6); p[1] = 0x80 | (r & 0x3F); n = 2; }
    else if (r < 0x10000) { p[0] = 0xE0 | (r >> 12); p[1] = 0x80 | ((r >> 6) & 0x3F); p[2] = 0x80 | (r & 0x3F); n = 3; }
    else { p[0] = 0xF0 | (r >> 18); p[1] = 0x80 | ((r >> 12) & 0x3F); p[2] = 0x80 | ((r >> 6) & 0x3F); p[3] = 0x80 | (r & 0x3F); n = 4; }
    return (gocpy_string){ p, n };
}

/* decodes the rune at byte offset i; returns its width */
int64_t gocpy_decode_rune(gocpy_string s, int64_t i, int32_t *r) {
    const uint8_t *p = s.ptr + i;
    int64_t left = s.len - i;
    uint8_t c = p[0];
    if (c < 0x80) { *r = c; return 1; }
    int n = (c >> 5) == 6 ? 2 : (c >> 4) == 14 ? 3 : (c >> 3) == 30 ? 4 : 0;
    if (n == 0 || left < n) { *r = 0xFFFD; return 1; }
    int32_t v = c & (0x7F >> n);
    for (int k = 1; k < n; k++) {
        if ((p[k] & 0xC0) != 0x80) { *r = 0xFFFD; return 1; }
        v = (v << 6) | (p[k] & 0x3F);
    }
    *r = v;
    return n;
}

/* ---------------------------------------------------------------- slices */

gocpy_slice gocpy_make_slice(int64_t len, int64_t cap, size_t es) {
    if (cap == GOCPY_ABSENT) cap = len;
    if (len < 0 || cap < len) gocpy_panic("makeslice: len out of range");
    return (gocpy_slice){ gocpy_alloc((size_t)cap * es), len, cap };
}

gocpy_slice gocpy_slice_lit(int64_t n, size_t es, const void *src) {
    void *p = gocpy_alloc((size_t)n * es);
    if (n) memcpy(p, src, (size_t)n * es);
    return (gocpy_slice){ p, n, n };
}

gocpy_slice gocpy_slice_slice(gocpy_slice s, int64_t lo, int64_t hi, int64_t max, size_t es) {
    if (lo == GOCPY_ABSENT) lo = 0;
    if (hi == GOCPY_ABSENT) hi = s.len;
    if (max == GOCPY_ABSENT) max = s.cap;
    if (lo < 0 || hi < lo || max < hi || max > s.cap) gocpy_panic_slice(lo, hi, s.cap);
    return (gocpy_slice){ (uint8_t *)s.data + (size_t)lo * es, hi - lo, max - lo };
}

gocpy_slice gocpy_slice_from_array(void *data, int64_t len) {
    return (gocpy_slice){ data, len, len };
}

static void gocpy_slice_grow(gocpy_slice *s, int64_t need, size_t es) {
    if (need <= s->cap) return;
    int64_t ncap = s->cap < 4 ? 4 : s->cap;
    while (ncap < need) ncap = ncap < 1024 ? ncap * 2 : ncap + ncap / 4;
    void *p = gocpy_alloc((size_t)ncap * es);
    if (s->len) memcpy(p, s->data, (size_t)s->len * es);
    s->data = p;
    s->cap = ncap;
}

void gocpy_slice_append(gocpy_slice *s, const void *elem, size_t es) {
    gocpy_slice_grow(s, s->len + 1, es);
    memcpy((uint8_t *)s->data + (size_t)s->len * es, elem, es);
    s->len++;
}

void gocpy_slice_append_slice(gocpy_slice *s, gocpy_slice o, size_t es) {
    if (o.len == 0) return;
    gocpy_slice_grow(s, s->len + o.len, es);
    memmove((uint8_t *)s->data + (size_t)s->len * es, o.data, (size_t)o.len * es);
    s->len += o.len;
}

/* ---------------------------------------------------------------- string builder */

gocpy_sb gocpy_sb_new(void) { return (gocpy_sb){ NULL, 0, 0 }; }

void gocpy_sb_bytes(gocpy_sb *sb, const void *p, int64_t n) {
    if (n <= 0) return;
    if (sb->len + n > sb->cap) {
        int64_t nc = sb->cap ? sb->cap * 2 : 64;
        while (nc < sb->len + n) nc *= 2;
        uint8_t *nb = gocpy_alloc_atomic((size_t)nc);
        if (sb->len) memcpy(nb, sb->buf, (size_t)sb->len);
        sb->buf = nb;
        sb->cap = nc;
    }
    memcpy(sb->buf + sb->len, p, (size_t)n);
    sb->len += n;
}

void gocpy_sb_byte(gocpy_sb *sb, uint8_t c) { gocpy_sb_bytes(sb, &c, 1); }
void gocpy_sb_cstr(gocpy_sb *sb, const char *s) { gocpy_sb_bytes(sb, s, (int64_t)strlen(s)); }
void gocpy_sb_str(gocpy_sb *sb, gocpy_string s) { gocpy_sb_bytes(sb, s.ptr, s.len); }

void gocpy_sb_i64(gocpy_sb *sb, int64_t v) {
    char buf[24];
    int n = snprintf(buf, sizeof buf, "%lld", (long long)v);
    gocpy_sb_bytes(sb, buf, n);
}

void gocpy_sb_u64(gocpy_sb *sb, uint64_t v) {
    char buf[24];
    int n = snprintf(buf, sizeof buf, "%llu", (unsigned long long)v);
    gocpy_sb_bytes(sb, buf, n);
}

void gocpy_sb_hex(gocpy_sb *sb, uint64_t v, bool upper) {
    char buf[24];
    int n = snprintf(buf, sizeof buf, upper ? "%llX" : "%llx", (unsigned long long)v);
    gocpy_sb_bytes(sb, buf, n);
}

void gocpy_sb_bool(gocpy_sb *sb, bool v) { gocpy_sb_cstr(sb, v ? "true" : "false"); }

void gocpy_sb_ptr(gocpy_sb *sb, const void *p) {
    if (!p) { gocpy_sb_cstr(sb, "<nil>"); return; }
    char buf[32];
    int n = snprintf(buf, sizeof buf, "0x%llx", (unsigned long long)(uintptr_t)p);
    gocpy_sb_bytes(sb, buf, n);
}

/* Shortest round-trip formatting identical to Go's strconv.FormatFloat(v, 'g', -1, bits). */
void gocpy_sb_f64(gocpy_sb *sb, double v, int bits) {
    if (isnan(v)) { gocpy_sb_cstr(sb, "NaN"); return; }
    if (isinf(v)) { gocpy_sb_cstr(sb, v > 0 ? "+Inf" : "-Inf"); return; }
    if (v == 0) { gocpy_sb_cstr(sb, signbit(v) ? "-0" : "0"); return; }
    char buf[40];
    int prec;
    for (prec = 0; prec < 17; prec++) {
        snprintf(buf, sizeof buf, "%.*e", prec, v);
        if (bits == 32 ? (float)strtod(buf, NULL) == (float)v : strtod(buf, NULL) == v) break;
    }
    /* buf = [-]d.ddddde[+-]XX ; extract digits and exponent */
    char digits[24];
    int nd = 0, neg = 0;
    const char *p = buf;
    if (*p == '-') { neg = 1; p++; }
    for (; *p && *p != 'e'; p++) if (*p != '.') digits[nd++] = *p;
    digits[nd] = 0;
    int exp = atoi(p + 1);
    while (nd > 1 && digits[nd - 1] == '0') digits[--nd] = 0;
    if (neg) gocpy_sb_byte(sb, '-');
    if (exp < -4 || exp >= 6) { /* same rule as Go strconv shortest %g */
        gocpy_sb_byte(sb, (uint8_t)digits[0]);
        if (nd > 1) { gocpy_sb_byte(sb, '.'); gocpy_sb_bytes(sb, digits + 1, nd - 1); }
        char e[8];
        int n = snprintf(e, sizeof e, "e%c%02d", exp < 0 ? '-' : '+', exp < 0 ? -exp : exp);
        gocpy_sb_bytes(sb, e, n);
        return;
    }
    if (exp < 0) {
        gocpy_sb_cstr(sb, "0.");
        for (int i = 0; i < -exp - 1; i++) gocpy_sb_byte(sb, '0');
        gocpy_sb_bytes(sb, digits, nd);
        return;
    }
    if (nd <= exp + 1) {
        gocpy_sb_bytes(sb, digits, nd);
        for (int i = nd; i <= exp; i++) gocpy_sb_byte(sb, '0');
        return;
    }
    gocpy_sb_bytes(sb, digits, exp + 1);
    gocpy_sb_byte(sb, '.');
    gocpy_sb_bytes(sb, digits + exp + 1, nd - exp - 1);
}

void gocpy_sb_fprec(gocpy_sb *sb, double v, int prec, char verb) {
    char buf[512];
    int n = snprintf(buf, sizeof buf, verb == 'e' ? "%.*e" : "%.*f", prec < 0 ? 6 : prec, v);
    if (n >= (int)sizeof buf) n = sizeof buf - 1;
    gocpy_sb_bytes(sb, buf, n);
}

/* pads the text written since mark to width (align: '<' '>' '^' '0') */
void gocpy_sb_pad(gocpy_sb *sb, int64_t mark, int width, char align) {
    int64_t n = sb->len - mark;
    if (n >= width) return;
    int64_t pad = width - n;
    int64_t left = 0;
    switch (align) {
    case '<': left = 0; break;
    case '^': left = pad / 2; break;
    default:  left = pad; break; /* '>' and '0' */
    }
    int64_t right = pad - left;
    uint8_t *tmp = gocpy_alloc_atomic((size_t)n);
    memcpy(tmp, sb->buf + mark, (size_t)n);
    sb->len = mark;
    char fill = align == '0' ? '0' : ' ';
    int64_t start = 0;
    if (align == '0' && n > 0 && (tmp[0] == '-' || tmp[0] == '+')) { gocpy_sb_byte(sb, tmp[0]); start = 1; }
    for (int64_t i = 0; i < left; i++) gocpy_sb_byte(sb, (uint8_t)fill);
    gocpy_sb_bytes(sb, tmp + start, n - start);
    for (int64_t i = 0; i < right; i++) gocpy_sb_byte(sb, ' ');
}

gocpy_string gocpy_sb_string(gocpy_sb *sb) {
    return (gocpy_string){ sb->buf, sb->len };
}

void gocpy_sb_write_stdout(gocpy_sb *sb) {
    if (sb->len) fwrite(sb->buf, 1, (size_t)sb->len, stdout);
}

/* ---------------------------------------------------------------- exceptions */

static void basic_fmt(gocpy_sb *sb, const void *p) { gocpy_sb_str(sb, *(const gocpy_string *)p); }
static gocpy_string basic_error(void *self) { return *(gocpy_string *)self; }
static const gocpy_method basic_methods[] = { { "Error", "0()(string)", (void *)basic_error } };
static const gocpy_typeinfo basic_ti = { "Exception", basic_fmt, sizeof(gocpy_string), false, NULL, 1, basic_methods };

gocpy_exception *gocpy_exc_basic(gocpy_string message) {
    return gocpy_exc_new(&basic_ti, &message, sizeof message);
}

gocpy_exception *gocpy_exc_new(const gocpy_typeinfo *ti, const void *value, size_t size) {
    gocpy_exception *e = (gocpy_exception *)gocpy_alloc(sizeof(gocpy_exception));
    e->type = ti;
    e->data = gocpy_alloc(size ? size : 1);
    if (size) memcpy(e->data, value, size);
    return e;
}

gocpy_string gocpy_exc_message(gocpy_exception *e) {
    gocpy_sb sb = gocpy_sb_new();
    if (e && e->type->fmt) e->type->fmt(&sb, e->data);
    return gocpy_sb_string(&sb);
}

gocpy_string gocpy_exc_typename(gocpy_exception *e) {
    const char *n = e ? e->type->name : "<nil>";
    return (gocpy_string){ (const uint8_t *)n, (int64_t)strlen(n) };
}

gocpy_string gocpy_exc_string(gocpy_exception *e) {
    gocpy_sb sb = gocpy_sb_new();
    gocpy_sb_str(&sb, gocpy_exc_typename(e));
    gocpy_sb_cstr(&sb, ": ");
    gocpy_sb_str(&sb, gocpy_exc_message(e));
    return gocpy_sb_string(&sb);
}

void gocpy_uncaught(void) {
    if (!gocpy_exc) return;
    gocpy_string s = gocpy_exc_string(gocpy_exc);
    fprintf(stderr, "uncaught exception: %.*s\n", (int)s.len, (const char *)s.ptr);
    exit(2);
}

/* ---------------------------------------------------------------- goroutines */

static pthread_mutex_t rt_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t rt_cond = PTHREAD_COND_INITIALIZER;
static int64_t rt_alive = 1;   /* goroutines, including main */
static int64_t rt_blocked = 0; /* goroutines waiting on runtime primitives */

static _Noreturn void deadlock(void) {
    fflush(stdout);
    fprintf(stderr, "fatal error: all goroutines are asleep - deadlock!\n");
    exit(2);
}

static uint64_t rt_gen = 0;    /* incremented by every broadcast */

/* rt_wait: block on the runtime condition; rt_lock must be held.
   A broadcast resets rt_blocked: every waiter is going to re-check its
   condition, so none of them may be counted as asleep any more. */
static void rt_wait(void) {
    rt_blocked++;
    if (rt_blocked >= rt_alive) deadlock();
    uint64_t gen = rt_gen;
    pthread_cond_wait(&rt_cond, &rt_lock);
    if (rt_gen == gen) rt_blocked--; /* spurious wakeup: still counted */
}

static void rt_wake(void) {
    rt_gen++;
    rt_blocked = 0;
    pthread_cond_broadcast(&rt_cond);
}

static _Noreturn void rt_panic(const char *msg) {
    pthread_mutex_unlock(&rt_lock);
    gocpy_panic(msg);
}

typedef struct { void (*fn)(void *); void *arg; } go_start;

static void *go_main(void *p) {
    go_start s = *(go_start *)p;
    free(p);
    gocpy_g = gstate_new();
    s.fn(s.arg);
    gstate_free(gocpy_g);
    gocpy_g = NULL;
    pthread_mutex_lock(&rt_lock);
    rt_alive--;
    if (rt_alive > 0 && rt_blocked >= rt_alive) deadlock();
    pthread_mutex_unlock(&rt_lock);
    return NULL;
}

void gocpy_go(void (*fn)(void *), void *arg) {
    go_start *s = (go_start *)malloc(sizeof *s);
    if (!s) gocpy_panic("out of memory");
    s->fn = fn;
    s->arg = arg;
    pthread_mutex_lock(&rt_lock);
    rt_alive++;
    pthread_mutex_unlock(&rt_lock);
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    pthread_t t;
    int err = pthread_create(&t, &attr, go_main, s);
    pthread_attr_destroy(&attr);
    if (err) gocpy_panic("cannot start goroutine (pthread_create failed)");
}

/* ---------------------------------------------------------------- channels */

struct gocpy_chan {
    size_t elem;
    int64_t cap;         /* 0 = unbuffered */
    int64_t slots;       /* max(cap, 1) */
    uint8_t *buf;
    int64_t head, count;
    bool closed;
    int64_t recv_waiting; /* receivers blocked on this channel (incl. select) */
    uint64_t put_seq, take_seq;
};

gocpy_chan *gocpy_chan_make(size_t elem, int64_t cap) {
    if (cap < 0) gocpy_panic("makechan: size out of range");
    gocpy_chan *c = (gocpy_chan *)gocpy_alloc(sizeof(gocpy_chan));
    c->elem = elem;
    c->cap = cap;
    c->slots = cap > 0 ? cap : 1;
    c->buf = (uint8_t *)gocpy_alloc((size_t)c->slots * (elem ? elem : 1));
    return c;
}

/* value movement; rt_lock held */
static uint64_t chan_put(gocpy_chan *c, const void *v) {
    int64_t i = (c->head + c->count) % c->slots;
    if (c->elem) memcpy(c->buf + (size_t)i * c->elem, v, c->elem);
    c->count++;
    rt_wake();
    return ++c->put_seq;
}

static void chan_take(gocpy_chan *c, void *out) {
    if (c->elem) {
        memcpy(out, c->buf + (size_t)c->head * c->elem, c->elem);
        memset(c->buf + (size_t)c->head * c->elem, 0, c->elem); /* drop GC reference */
    }
    c->head = (c->head + 1) % c->slots;
    c->count--;
    c->take_seq++;
    rt_wake();
}

/* An unbuffered send parks the value in the single slot and waits until a
   receiver takes it; a buffered send only needs free space. */
static bool can_put(gocpy_chan *c) { return c->count < c->slots; }

void gocpy_chan_send(gocpy_chan *c, const void *v) {
    pthread_mutex_lock(&rt_lock);
    if (!c) for (;;) rt_wait(); /* send on nil channel blocks forever */
    for (;;) {
        if (c->closed) rt_panic("send on closed channel");
        if (can_put(c)) break;
        rt_wait();
    }
    uint64_t id = chan_put(c, v);
    if (c->cap == 0) {
        while (c->take_seq < id) rt_wait();
    }
    pthread_mutex_unlock(&rt_lock);
}

bool gocpy_chan_recv(gocpy_chan *c, void *out) {
    pthread_mutex_lock(&rt_lock);
    if (!c) for (;;) rt_wait();
    c->recv_waiting++;
    rt_wake();
    bool ok = true;
    for (;;) {
        if (c->count > 0) { chan_take(c, out); break; }
        if (c->closed) { if (c->elem) memset(out, 0, c->elem); ok = false; break; }
        rt_wait();
    }
    c->recv_waiting--;
    pthread_mutex_unlock(&rt_lock);
    return ok;
}

void gocpy_chan_close(gocpy_chan *c) {
    pthread_mutex_lock(&rt_lock);
    if (!c) rt_panic("close of nil channel");
    if (c->closed) rt_panic("close of closed channel");
    c->closed = true;
    rt_wake();
    pthread_mutex_unlock(&rt_lock);
}

int64_t gocpy_chan_len(gocpy_chan *c) {
    if (!c) return 0;
    pthread_mutex_lock(&rt_lock);
    int64_t n = c->cap == 0 ? 0 : c->count;
    pthread_mutex_unlock(&rt_lock);
    return n;
}

int64_t gocpy_chan_cap(gocpy_chan *c) { return c ? c->cap : 0; }

static uint32_t sel_rand = 2463534242u; /* xorshift; guarded by rt_lock */

int gocpy_select(gocpy_selcase *cs, int n, bool has_default) {
    pthread_mutex_lock(&rt_lock);
    bool registered = false;
    int chosen = -1;
    for (;;) {
        sel_rand ^= sel_rand << 13; sel_rand ^= sel_rand >> 17; sel_rand ^= sel_rand << 5;
        int start = n ? (int)(sel_rand % (uint32_t)n) : 0;
        for (int k = 0; k < n && chosen < 0; k++) {
            int i = (start + k) % n;
            gocpy_chan *c = cs[i].c;
            if (!c) continue;
            if (cs[i].send) {
                if (c->closed) rt_panic("send on closed channel");
                /* unbuffered: only ready if a receiver is waiting */
                bool ready = c->cap > 0 ? can_put(c) : (c->count == 0 && c->recv_waiting > 0);
                if (ready) { chan_put(c, cs[i].val); chosen = i; }
            } else if (c->count > 0) {
                chan_take(c, cs[i].val); cs[i].ok = true; chosen = i;
            } else if (c->closed) {
                if (c->elem) memset(cs[i].val, 0, c->elem);
                cs[i].ok = false; chosen = i;
            }
        }
        if (chosen >= 0 || has_default) break;
        if (!registered) {
            for (int i = 0; i < n; i++) if (cs[i].c && !cs[i].send) cs[i].c->recv_waiting++;
            registered = true;
            rt_wake();
            continue; /* re-check: a parked sender may now be able to proceed */
        }
        rt_wait();
    }
    if (registered)
        for (int i = 0; i < n; i++) if (cs[i].c && !cs[i].send) cs[i].c->recv_waiting--;
    pthread_mutex_unlock(&rt_lock);
    return chosen;
}

/* ---------------------------------------------------------------- sync */

void gocpy_wg_add(gocpy_waitgroup *wg, int64_t d) {
    pthread_mutex_lock(&rt_lock);
    wg->n += d;
    if (wg->n < 0) rt_panic("sync: negative WaitGroup counter");
    if (wg->n == 0) rt_wake();
    pthread_mutex_unlock(&rt_lock);
}

void gocpy_wg_done(gocpy_waitgroup *wg) { gocpy_wg_add(wg, -1); }

void gocpy_wg_wait(gocpy_waitgroup *wg) {
    pthread_mutex_lock(&rt_lock);
    while (wg->n > 0) rt_wait();
    pthread_mutex_unlock(&rt_lock);
}

void gocpy_mutex_lock(gocpy_mutex *m) {
    pthread_mutex_lock(&rt_lock);
    while (m->locked) rt_wait();
    m->locked = true;
    pthread_mutex_unlock(&rt_lock);
}

void gocpy_mutex_unlock(gocpy_mutex *m) {
    pthread_mutex_lock(&rt_lock);
    if (!m->locked) rt_panic("sync: unlock of unlocked mutex");
    m->locked = false;
    rt_wake();
    pthread_mutex_unlock(&rt_lock);
}

/* ---------------------------------------------------------------- time */

void gocpy_sleep(int64_t ns) {
    if (ns <= 0) return;
    struct timespec ts = { (time_t)(ns / 1000000000), (long)(ns % 1000000000) };
    while (nanosleep(&ts, &ts) == -1 && errno == EINTR) {}
}

int64_t gocpy_now(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

int64_t gocpy_since(int64_t t) { return gocpy_now() - t; }

/* ---------------------------------------------------------------- stage 5: interfaces */

void *gocpy_box(const void *v, size_t n) {
    void *p = gocpy_alloc(n ? n : 1);
    if (n) memcpy(p, v, n);
    return p;
}

/* signature match: equal, or a non-throwing method for a @throws interface method */
static bool sig_ok(const char *have, const char *want) {
    if (strcmp(have + 1, want + 1) != 0) return false;
    return have[0] == '0' || want[0] == '1';
}

typedef struct itab_entry {
    const gocpy_idesc *I;
    const gocpy_typeinfo *T;
    const gocpy_itab *itab;            /* NULL: T does not implement I */
    struct itab_entry *next;
} itab_entry;
static itab_entry *itab_cache;
static pthread_mutex_t itab_lock = PTHREAD_MUTEX_INITIALIZER;

const gocpy_itab *gocpy_itab_get(const gocpy_idesc *I, const gocpy_typeinfo *T) {
    pthread_mutex_lock(&itab_lock);
    for (itab_entry *e = itab_cache; e; e = e->next) {
        if (e->I == I && e->T == T) {
            pthread_mutex_unlock(&itab_lock);
            return e->itab;
        }
    }
    gocpy_itab *it = (gocpy_itab *)gocpy_alloc(sizeof(gocpy_itab) + sizeof(void *) * (size_t)(I->n ? I->n : 1));
    it->type = T;
    bool ok = true;
    for (int i = 0; i < I->n && ok; i++) {
        ok = false;
        for (int j = 0; j < T->nmethods; j++) {
            if (strcmp(T->methods[j].name, I->names[i]) == 0 && sig_ok(T->methods[j].sig, I->sigs[i])) {
                it->fns[i] = T->methods[j].fn;
                ok = true;
                break;
            }
        }
    }
    itab_entry *e = (itab_entry *)gocpy_alloc(sizeof(itab_entry));
    e->I = I; e->T = T; e->itab = ok ? it : NULL; e->next = itab_cache;
    itab_cache = e;
    pthread_mutex_unlock(&itab_lock);
    return e->itab;
}

gocpy_iface gocpy_iface_conv(gocpy_iface x, const gocpy_idesc *I) {
    if (x.itab == NULL) return x;
    const gocpy_itab *it = gocpy_itab_get(I, x.itab->type);
    if (it == NULL) gocpy_panic("internal: interface conversion failed");
    return (gocpy_iface){ it, x.data };
}

bool gocpy_iface_to_iface(gocpy_iface x, const gocpy_idesc *I, gocpy_iface *out) {
    if (x.itab == NULL) { *out = (gocpy_iface){0}; return false; }
    const gocpy_itab *it = gocpy_itab_get(I, x.itab->type);
    if (it == NULL) { *out = (gocpy_iface){0}; return false; }
    *out = (gocpy_iface){ it, x.data };
    return true;
}

_Noreturn void gocpy_panic_assert(gocpy_iface x, const char *iface, const char *want) {
    char buf[512];
    if (x.itab == NULL)
        snprintf(buf, sizeof buf, "interface conversion: %s is nil, not %s", iface, want);
    else
        snprintf(buf, sizeof buf, "interface conversion: %s is %s, not %s", iface, x.itab->type->name, want);
    gocpy_panic(buf);
}

bool gocpy_iface_eq(gocpy_iface a, gocpy_iface b) {
    if (a.itab == NULL || b.itab == NULL) return a.itab == b.itab;
    if (a.itab->type != b.itab->type) return false;
    const gocpy_typeinfo *t = a.itab->type;
    if (t->eq == NULL) {
        char buf[256];
        snprintf(buf, sizeof buf, "comparing uncomparable type %s", t->name);
        gocpy_panic(buf);
    }
    return t->eq(GOCPY_IFACE_VALP(a), GOCPY_IFACE_VALP(b));
}

void gocpy_sb_iface(gocpy_sb *sb, gocpy_iface x) {
    if (x.itab == NULL) { gocpy_sb_cstr(sb, "<nil>"); return; }
    x.itab->type->fmt(sb, GOCPY_IFACE_VALP(x));
}

gocpy_exception *gocpy_exc_from_iface(gocpy_iface x) {
    if (x.itab == NULL) gocpy_panic("throw of nil interface value");
    const gocpy_typeinfo *t = x.itab->type;
    gocpy_exception *e = (gocpy_exception *)gocpy_alloc(sizeof(gocpy_exception));
    e->type = t;
    e->data = t->is_ptr ? gocpy_box(&x.data, sizeof(void *)) : x.data;
    return e;
}

bool gocpy_exc_as_iface(gocpy_exception *e, const gocpy_idesc *I, gocpy_iface *out) {
    const gocpy_itab *it = gocpy_itab_get(I, e->type);
    if (it == NULL) return false;
    *out = (gocpy_iface){ it, e->type->is_ptr ? *(void **)e->data : e->data };
    return true;
}

/* std/errors: errors.New returns an error whose dynamic type is *errors.errorString */
static void errstr_fmt(gocpy_sb *sb, const void *p) { gocpy_sb_str(sb, **(gocpy_string *const *)p); }
static gocpy_string errstr_error(void *self) { return *(gocpy_string *)self; }
static bool errstr_eq(const void *a, const void *b) { return *(void *const *)a == *(void *const *)b; }
static const gocpy_method errstr_methods[] = { { "Error", "0()(string)", (void *)errstr_error } };
static const gocpy_typeinfo errstr_ti = { "*errors.errorString", errstr_fmt, sizeof(void *), true, errstr_eq, 1, errstr_methods };
static const gocpy_itab errstr_itab = { &errstr_ti, { (void *)errstr_error } };

gocpy_iface gocpy_errors_new(gocpy_string msg) {
    return (gocpy_iface){ &errstr_itab, gocpy_box(&msg, sizeof msg) };
}

/* ---------------------------------------------------------------- stage 5: maps
 * Compact layout (like CPython dicts): entries are appended to a dense array
 * (iteration order = insertion order); an open-addressing index maps hashes
 * to entry numbers. Deleted entries stay as tombstones until the next resize.
 */
struct gocpy_map {
    size_t ks, vs, es;                 /* key size, value size, entry stride */
    gocpy_hashfn hash;
    gocpy_eqfn eq;
    int64_t count;                     /* live entries */
    int64_t used;                      /* entries[0..used) incl. tombstones */
    int64_t ecap;
    uint8_t *entries;                  /* [hash u64][live u8 pad][key][value] */
    int64_t *index;                    /* -1 empty, else entry number */
    int64_t mask;
};

#define ENT_HDR 16
static size_t align8(size_t n) { return (n + 7) & ~(size_t)7; }
static uint8_t *ent(gocpy_map *m, int64_t i) { return m->entries + (size_t)i * m->es; }
static void *ent_key(gocpy_map *m, uint8_t *e) { (void)m; return e + ENT_HDR; }
static void *ent_val(gocpy_map *m, uint8_t *e) { return e + ENT_HDR + align8(m->ks); }

static void map_reindex(gocpy_map *m, int64_t slots) {
    /* compact tombstones, rebuild the index */
    int64_t j = 0;
    for (int64_t i = 0; i < m->used; i++) {
        uint8_t *e = ent(m, i);
        if (!e[8]) continue;
        if (i != j) memmove(ent(m, j), e, m->es);
        j++;
    }
    m->used = j;
    m->index = (int64_t *)gocpy_alloc(sizeof(int64_t) * (size_t)slots);
    memset(m->index, 0xff, sizeof(int64_t) * (size_t)slots);
    m->mask = slots - 1;
    for (int64_t i = 0; i < m->used; i++) {
        uint64_t h = *(uint64_t *)ent(m, i);
        int64_t s = (int64_t)(h & (uint64_t)m->mask);
        while (m->index[s] >= 0) s = (s + 1) & m->mask;
        m->index[s] = i;
    }
}

gocpy_map *gocpy_map_make(size_t ks, size_t vs, gocpy_hashfn h, gocpy_eqfn eq, int64_t hint) {
    if (hint < 0) gocpy_panic("makemap: size out of range");
    gocpy_map *m = (gocpy_map *)gocpy_alloc(sizeof(gocpy_map));
    m->ks = ks; m->vs = vs; m->hash = h; m->eq = eq;
    m->es = ENT_HDR + align8(ks) + align8(vs);
    int64_t slots = 8;
    while (slots * 2 < hint * 3) slots *= 2;
    m->ecap = slots;
    m->entries = (uint8_t *)gocpy_alloc(m->es * (size_t)m->ecap);
    map_reindex(m, slots);
    return m;
}

static int64_t map_find(gocpy_map *m, const void *k, uint64_t h, int64_t *slot) {
    int64_t s = (int64_t)(h & (uint64_t)m->mask);
    for (;;) {
        int64_t i = m->index[s];
        if (i < 0) { if (slot) *slot = s; return -1; }
        uint8_t *e = ent(m, i);
        if (e[8] && *(uint64_t *)e == h && m->eq(ent_key(m, e), k)) { if (slot) *slot = s; return i; }
        s = (s + 1) & m->mask;
    }
}

bool gocpy_map_get(gocpy_map *m, const void *k, void *out, size_t vs) {
    if (m != NULL && m->count > 0) {
        int64_t i = map_find(m, k, m->hash(k), NULL);
        if (i >= 0) { memcpy(out, ent_val(m, ent(m, i)), vs); return true; }
    }
    memset(out, 0, vs);
    return false;
}

void *gocpy_map_assign(gocpy_map *m, const void *k) {
    if (m == NULL) gocpy_panic("assignment to entry in nil map");
    uint64_t h = m->hash(k);
    int64_t slot;
    int64_t i = map_find(m, k, h, &slot);
    if (i >= 0) return ent_val(m, ent(m, i));
    if ((m->used + 1) * 3 > (m->mask + 1) * 2 || m->used == m->ecap) {
        int64_t slots = m->mask + 1;
        while ((m->count + 1) * 3 > slots * 2) slots *= 2;
        if (m->count + 1 > m->ecap / 2) slots *= 2;
        if (slots > m->ecap) {
            uint8_t *ne = (uint8_t *)gocpy_alloc(m->es * (size_t)slots);
            memcpy(ne, m->entries, m->es * (size_t)m->used);
            m->entries = ne;
            m->ecap = slots;
        }
        map_reindex(m, slots);
        map_find(m, k, h, &slot);
    }
    i = m->used++;
    uint8_t *e = ent(m, i);
    memset(e, 0, m->es);
    *(uint64_t *)e = h;
    e[8] = 1;
    memcpy(ent_key(m, e), k, m->ks);
    m->index[slot] = i;
    m->count++;
    return ent_val(m, e);
}

void gocpy_map_delete(gocpy_map *m, const void *k) {
    if (m == NULL || m->count == 0) return;
    int64_t i = map_find(m, k, m->hash(k), NULL);
    if (i < 0) return;
    ent(m, i)[8] = 0;                  /* tombstone: the index slot keeps probing chains intact */
    m->count--;
}

int64_t gocpy_map_len(gocpy_map *m) { return m ? m->count : 0; }

bool gocpy_map_next(gocpy_map *m, int64_t *it, void **k, void **v) {
    if (m == NULL) return false;
    while (*it < m->used) {
        uint8_t *e = ent(m, (*it)++);
        if (e[8]) { *k = ent_key(m, e); *v = ent_val(m, e); return true; }
    }
    return false;
}

uint64_t gocpy_hash_bytes(const void *p, size_t n) {
    const uint8_t *b = (const uint8_t *)p;
    uint64_t h = 1469598103934665603ULL;
    for (size_t i = 0; i < n; i++) { h ^= b[i]; h *= 1099511628211ULL; }
    h ^= h >> 33; h *= 0xff51afd7ed558ccdULL; h ^= h >> 33;
    return h;
}
uint64_t gocpy_hash_string(const void *p) {
    const gocpy_string *s = (const gocpy_string *)p;
    return gocpy_hash_bytes(s->ptr, (size_t)s->len);
}
uint64_t gocpy_hash_f64(const void *p) {
    double d = *(const double *)p;
    if (d == 0) d = 0;                 /* +0 and -0 are equal keys */
    return gocpy_hash_bytes(&d, sizeof d);
}
uint64_t gocpy_hash_f32(const void *p) {
    float f = *(const float *)p;
    if (f == 0) f = 0;
    return gocpy_hash_bytes(&f, sizeof f);
}

/* ---------------------------------------------------------------- stage 5: defer */

void gocpy_defer_push(gocpy_defer **list, void (*fn)(void *), void *arg) {
    gocpy_defer *d = (gocpy_defer *)gocpy_alloc(sizeof(gocpy_defer));
    d->fn = fn; d->arg = arg; d->next = *list;
    *list = d;
}

void gocpy_run_defers(gocpy_defer **list) {
    while (*list) {
        gocpy_defer *d = *list;
        *list = d->next;
        d->fn(d->arg);
    }
}
