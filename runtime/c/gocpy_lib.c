/* Gocpy runtime library: fmt (Printf and Scan families), errors (Is, As,
 * Unwrap, Join) and the system calls behind std/io, std/http and std/udp. */
#define _GNU_SOURCE
#include "gocpy_rt.h"

#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <math.h>
#include <netdb.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <dirent.h>
#include <unistd.h>
#include <arpa/inet.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <time.h>
#include <sys/types.h>

#ifdef GOCPY_GC_BOEHM
#include <gc.h>
#endif

#define GOCPY_ETIMEOUT 99999

#ifndef SOCK_CLOEXEC
#define SOCK_CLOEXEC 0 /* macOS: no atomic close-on-exec for sockets */
#endif

/* ---------------------------------------------------------------- helpers */

static gocpy_string str_copy(const char *p, int64_t n) {
    uint8_t *b = (uint8_t *)gocpy_alloc((size_t)(n > 0 ? n : 1));
    if (n > 0) memcpy(b, p, (size_t)n);
    return (gocpy_string){ b, n };
}

static gocpy_string str_c(const char *p) { return str_copy(p, (int64_t)strlen(p)); }

/* NUL-terminated copy (malloc'd; free with free) */
static char *cstr(gocpy_string s) {
    char *p = (char *)malloc((size_t)s.len + 1);
    if (p == NULL) gocpy_panic("out of memory");
    memcpy(p, s.ptr, (size_t)s.len);
    p[s.len] = 0;
    return p;
}

static const gocpy_method *find_method(const gocpy_typeinfo *t, const char *name, const char *sig) {
    if (t == NULL) return NULL;
    for (int i = 0; i < t->nmethods; i++)
        if (strcmp(t->methods[i].name, name) == 0 && strcmp(t->methods[i].sig, sig) == 0) return &t->methods[i];
    return NULL;
}

/* self argument of a method wrapper for a value stored at valp */
static void *method_self(const gocpy_typeinfo *t, const void *valp) {
    return t->is_ptr ? *(void *const *)valp : (void *)valp;
}

static gocpy_tup_Tint_error tup(int64_t n, gocpy_iface err) { return (gocpy_tup_Tint_error){ n, err }; }
static const gocpy_iface nil_iface = { NULL, NULL };

/* calls Write or Read (both func([]byte) (int, error)) of an interface value */
typedef gocpy_tup_Tint_error (*rw_fn)(void *, gocpy_slice);
static gocpy_tup_Tint_error call_rw(gocpy_iface x, gocpy_slice p) {
    return ((rw_fn)gocpy_iface_fn(x, 0))(x.data, p);
}

/* ---------------------------------------------------------------- io.EOF */

static pthread_once_t eof_once = PTHREAD_ONCE_INIT;
static gocpy_iface eof_err, unexpected_eof_err;
static void eof_init(void) {
    eof_err = gocpy_errors_new(GOCPY_STR("EOF"));
    unexpected_eof_err = gocpy_errors_new(GOCPY_STR("unexpected EOF"));
}
gocpy_iface gocpy_io_eof(void) { pthread_once(&eof_once, eof_init); return eof_err; }
gocpy_iface gocpy_io_unexpected_eof(void) { pthread_once(&eof_once, eof_init); return unexpected_eof_err; }

/* ---------------------------------------------------------------- formatting */

typedef struct {
    bool minus, plus, sharp, space, zero;
    bool has_wid, has_prec;
    int wid, prec;
} fflags;

static int64_t rune_count(const uint8_t *p, int64_t n) {
    int64_t c = 0;
    for (int64_t i = 0; i < n; i++) if ((p[i] & 0xC0) != 0x80) c++;
    return c;
}

/* appends s padded to the width (spaces, or zeros when zero is set) */
static void put_padded(gocpy_sb *sb, const uint8_t *s, int64_t n, const fflags *f, bool zero) {
    int64_t w = f->has_wid ? f->wid : 0, rc = rune_count(s, n);
    if (w <= rc) { gocpy_sb_bytes(sb, s, n); return; }
    int64_t pad = w - rc;
    if (f->minus) {
        gocpy_sb_bytes(sb, s, n);
        for (int64_t i = 0; i < pad; i++) gocpy_sb_byte(sb, ' ');
        return;
    }
    if (zero) {
        /* keep a leading sign before the zeros */
        int64_t k = 0;
        if (n > 0 && (s[0] == '-' || s[0] == '+' || s[0] == ' ')) { gocpy_sb_byte(sb, s[0]); k = 1; }
        for (int64_t i = 0; i < pad; i++) gocpy_sb_byte(sb, '0');
        gocpy_sb_bytes(sb, s + k, n - k);
        return;
    }
    for (int64_t i = 0; i < pad; i++) gocpy_sb_byte(sb, ' ');
    gocpy_sb_bytes(sb, s, n);
}

static void put_padded_sb(gocpy_sb *sb, gocpy_sb *tmp, const fflags *f, bool zero) {
    put_padded(sb, tmp->buf, tmp->len, f, zero);
}

static void fmt_integer(gocpy_sb *sb, uint64_t mag, bool neg, int base, bool upper, const fflags *f, char verb) {
    char digits[80];
    int nd = 0;
    const char *dg = upper ? "0123456789ABCDEF" : "0123456789abcdef";
    if (mag == 0) digits[nd++] = '0';
    while (mag > 0) { digits[nd++] = dg[mag % (uint64_t)base]; mag /= (uint64_t)base; }
    if (f->has_prec && f->prec == 0 && nd == 1 && digits[0] == '0') nd = 0;
    gocpy_sb tmp = gocpy_sb_new();
    if (neg) gocpy_sb_byte(&tmp, '-');
    else if (f->plus) gocpy_sb_byte(&tmp, '+');
    else if (f->space) gocpy_sb_byte(&tmp, ' ');
    if (verb == 'O') gocpy_sb_cstr(&tmp, "0o");
    else if (f->sharp) {
        if (base == 16) gocpy_sb_cstr(&tmp, upper ? "0X" : "0x");
        else if (base == 8 && !(nd > 0 && digits[nd - 1] == '0')) gocpy_sb_byte(&tmp, '0');
        else if (base == 2) gocpy_sb_cstr(&tmp, "0b");
    }
    int64_t prefix = tmp.len;
    if (f->has_prec) for (int i = nd; i < f->prec; i++) gocpy_sb_byte(&tmp, '0');
    for (int i = nd - 1; i >= 0; i--) gocpy_sb_byte(&tmp, (uint8_t)digits[i]);
    bool zero = f->zero && !f->minus && !f->has_prec;
    if (zero && f->has_wid && tmp.len < f->wid) {
        /* zeros go after the sign and prefix */
        gocpy_sb out = gocpy_sb_new();
        gocpy_sb_bytes(&out, tmp.buf, prefix);
        for (int64_t i = tmp.len; i < f->wid; i++) gocpy_sb_byte(&out, '0');
        gocpy_sb_bytes(&out, tmp.buf + prefix, tmp.len - prefix);
        gocpy_sb_bytes(sb, out.buf, out.len);
        return;
    }
    put_padded_sb(sb, &tmp, f, false);
}

static void quote_rune_into(gocpy_sb *sb, int32_t r, char q) {
    switch (r) {
    case '\a': gocpy_sb_cstr(sb, "\\a"); return;
    case '\b': gocpy_sb_cstr(sb, "\\b"); return;
    case '\f': gocpy_sb_cstr(sb, "\\f"); return;
    case '\n': gocpy_sb_cstr(sb, "\\n"); return;
    case '\r': gocpy_sb_cstr(sb, "\\r"); return;
    case '\t': gocpy_sb_cstr(sb, "\\t"); return;
    case '\v': gocpy_sb_cstr(sb, "\\v"); return;
    case '\\': gocpy_sb_cstr(sb, "\\\\"); return;
    }
    if (r == q) { gocpy_sb_byte(sb, '\\'); gocpy_sb_byte(sb, (uint8_t)q); return; }
    if (r < 0x20 || r == 0x7f) {
        char buf[8];
        snprintf(buf, sizeof buf, "\\x%02x", (unsigned)r);
        gocpy_sb_cstr(sb, buf);
        return;
    }
    if (r > 0x10FFFF || (r >= 0xD800 && r <= 0xDFFF)) { gocpy_sb_cstr(sb, "\\ufffd"); return; }
    gocpy_string s = gocpy_string_from_rune(r);
    gocpy_sb_str(sb, s);
}

static void quote_string(gocpy_sb *sb, const uint8_t *p, int64_t n, bool backquote) {
    if (backquote) {
        bool ok = true;
        for (int64_t i = 0; i < n; i++) if (p[i] == '`' || (p[i] < 0x20 && p[i] != '\t') || p[i] == 0x7f) { ok = false; break; }
        if (ok) { gocpy_sb_byte(sb, '`'); gocpy_sb_bytes(sb, p, n); gocpy_sb_byte(sb, '`'); return; }
    }
    gocpy_sb_byte(sb, '"');
    gocpy_string s = { p, n };
    for (int64_t i = 0; i < n;) {
        int32_t r;
        int64_t next = i + gocpy_decode_rune(s, i, &r);
        if (r == 0xFFFD && next == i + 1) {
            char buf[8];
            snprintf(buf, sizeof buf, "\\x%02x", p[i]);
            gocpy_sb_cstr(sb, buf);
        } else {
            quote_rune_into(sb, r, '"');
        }
        i = next;
    }
    gocpy_sb_byte(sb, '"');
}

static void fmt_hex_bytes(gocpy_sb *sb, const uint8_t *p, int64_t n, bool upper, const fflags *f) {
    const char *dg = upper ? "0123456789ABCDEF" : "0123456789abcdef";
    gocpy_sb tmp = gocpy_sb_new();
    if (f->has_prec && f->prec < n) n = f->prec;
    for (int64_t i = 0; i < n; i++) {
        if (f->space && i > 0) gocpy_sb_byte(&tmp, ' ');
        if (f->sharp && (i == 0 || f->space)) gocpy_sb_cstr(&tmp, upper ? "0X" : "0x");
        gocpy_sb_byte(&tmp, (uint8_t)dg[p[i] >> 4]);
        gocpy_sb_byte(&tmp, (uint8_t)dg[p[i] & 15]);
    }
    put_padded_sb(sb, &tmp, f, false);
}

static void fmt_text(gocpy_sb *sb, const uint8_t *p, int64_t n, char verb, const fflags *f) {
    switch (verb) {
    case 'x': case 'X':
        fmt_hex_bytes(sb, p, n, verb == 'X', f);
        return;
    case 'q': {
        gocpy_sb tmp = gocpy_sb_new();
        quote_string(&tmp, p, n, f->sharp);
        put_padded_sb(sb, &tmp, f, false);
        return;
    }
    }
    if (f->has_prec) {
        /* precision truncates to runes */
        int64_t i = 0, c = 0;
        while (i < n && c < f->prec) { i++; while (i < n && (p[i] & 0xC0) == 0x80) i++; c++; }
        n = i;
    }
    put_padded(sb, p, n, f, f->zero && !f->minus);
}

static void fmt_float(gocpy_sb *sb, double v, int bits, char verb, const fflags *f) {
    gocpy_sb tmp = gocpy_sb_new();
    if (isnan(v) || isinf(v)) {
        if (isnan(v)) gocpy_sb_cstr(&tmp, f->plus ? "+NaN" : f->space ? " NaN" : "NaN");
        else if (v > 0) gocpy_sb_cstr(&tmp, f->plus ? "+Inf" : f->space ? " Inf" : "+Inf");
        else gocpy_sb_cstr(&tmp, "-Inf");
        put_padded_sb(sb, &tmp, f, false);
        return;
    }
    if ((verb == 'v' || verb == 'g' || verb == 'G') && !f->has_prec && !f->sharp) {
        if (f->plus && !signbit(v)) gocpy_sb_byte(&tmp, '+');
        else if (f->space && !signbit(v)) gocpy_sb_byte(&tmp, ' ');
        gocpy_sb_f64(&tmp, v, bits);
        if (verb == 'G') for (int64_t i = 0; i < tmp.len; i++) if (tmp.buf[i] == 'e') tmp.buf[i] = 'E';
        put_padded_sb(sb, &tmp, f, f->zero && !f->minus);
        return;
    }
    char cf[32], *q = cf;
    *q++ = '%';
    if (f->plus) *q++ = '+';
    else if (f->space) *q++ = ' ';
    if (f->sharp) *q++ = '#';
    *q++ = '.';
    *q++ = '*';
    *q++ = verb == 'v' ? 'g' : verb == 'F' ? 'f' : verb;
    *q = 0;
    /* a negative precision means "omitted" for printf */
    int prec = f->has_prec ? f->prec : (verb == 'g' || verb == 'G' || verb == 'v') ? -1 : 6;
    char buf[512];
    int n = snprintf(buf, sizeof buf, cf, prec, v);
    if (n >= (int)sizeof buf) n = (int)sizeof buf - 1;
    gocpy_sb_bytes(&tmp, buf, n);
    put_padded_sb(sb, &tmp, f, f->zero && !f->minus);
}

static int64_t load_int(const void *p, size_t size) {
    switch (size) {
    case 1: return *(const int8_t *)p;
    case 2: return *(const int16_t *)p;
    case 4: return *(const int32_t *)p;
    default: return *(const int64_t *)p;
    }
}
static uint64_t load_uint(const void *p, size_t size) {
    switch (size) {
    case 1: return *(const uint8_t *)p;
    case 2: return *(const uint16_t *)p;
    case 4: return *(const uint32_t *)p;
    default: return *(const uint64_t *)p;
    }
}

static bool is_byte_slice(const gocpy_typeinfo *t) {
    return t != NULL && t->kind == GOCPY_K_SLICE && t->elem != NULL && t->elem->kind == GOCPY_K_UINT && t->elem->size == 1;
}

static void fmt_value(gocpy_sb *sb, const gocpy_typeinfo *t, const void *p, char verb, const fflags *f, int depth);

static void bad_verb(gocpy_sb *sb, const gocpy_typeinfo *t, const void *p, char verb) {
    gocpy_sb_cstr(sb, "%!");
    gocpy_sb_byte(sb, (uint8_t)verb);
    gocpy_sb_byte(sb, '(');
    if (t == NULL) gocpy_sb_cstr(sb, "<nil>");
    else {
        gocpy_sb_cstr(sb, t->name);
        gocpy_sb_byte(sb, '=');
        t->fmt(sb, p);
    }
    gocpy_sb_byte(sb, ')');
}

static void fmt_elems(gocpy_sb *sb, const gocpy_typeinfo *et, const uint8_t *data, int64_t n, char verb, const fflags *f, int depth) {
    gocpy_sb_byte(sb, '[');
    for (int64_t i = 0; i < n; i++) {
        if (i > 0) gocpy_sb_byte(sb, ' ');
        fmt_value(sb, et, data + (size_t)i * et->size, verb, f, depth + 1);
    }
    gocpy_sb_byte(sb, ']');
}

/* formats the value at p (of type t) with one verb */
static void fmt_value(gocpy_sb *sb, const gocpy_typeinfo *t, const void *p, char verb, const fflags *f, int depth) {
    if (t == NULL) {
        if (verb == 'v' || verb == 's') { put_padded(sb, (const uint8_t *)"<nil>", 5, f, false); return; }
        bad_verb(sb, NULL, NULL, verb);
        return;
    }
    if (t->kind == GOCPY_K_IFACE) {
        const gocpy_iface *x = (const gocpy_iface *)p;
        if (x->itab == NULL) { fmt_value(sb, NULL, NULL, verb, f, depth); return; }
        fmt_value(sb, x->itab->type, GOCPY_IFACE_VALP(*x), verb, f, depth);
        return;
    }
    if (verb == 'T') {
        put_padded(sb, (const uint8_t *)t->name, (int64_t)strlen(t->name), f, false);
        return;
    }
    if (verb == 'p') {
        if (t->kind == GOCPY_K_PTR || t->is_ptr || t->kind == GOCPY_K_SLICE) {
            const void *ptr = t->kind == GOCPY_K_SLICE ? ((const gocpy_slice *)p)->data : *(void *const *)p;
            fmt_integer(sb, (uint64_t)(uintptr_t)ptr, false, 16, false, &(fflags){ .sharp = !f->sharp, .has_wid = f->has_wid, .wid = f->wid, .minus = f->minus }, 'x');
            return;
        }
        bad_verb(sb, t, p, verb);
        return;
    }
    /* Error() / String() for the text verbs */
    if (verb == 'v' || verb == 's' || verb == 'q' || verb == 'x' || verb == 'X') {
        const gocpy_method *m = find_method(t, "Error", "0()(string)");
        if (m == NULL) m = find_method(t, "String", "0()(string)");
        if (m != NULL) {
            if (t->is_ptr && *(void *const *)p == NULL && t->kind == GOCPY_K_PTR) {
                put_padded(sb, (const uint8_t *)"<nil>", 5, f, false);
                return;
            }
            gocpy_string s = ((gocpy_string (*)(void *))m->fn)(method_self(t, p));
            fmt_text(sb, s.ptr, s.len, verb == 'v' ? 's' : verb, f);
            return;
        }
    }
    switch (t->kind) {
    case GOCPY_K_BOOL:
        if (verb == 'v' || verb == 't') {
            bool b = *(const bool *)p;
            put_padded(sb, (const uint8_t *)(b ? "true" : "false"), b ? 4 : 5, f, false);
            return;
        }
        break;
    case GOCPY_K_INT: case GOCPY_K_UINT: {
        bool sign = t->kind == GOCPY_K_INT;
        int64_t sv = sign ? load_int(p, t->size) : 0;
        uint64_t uv = sign ? (sv < 0 ? (uint64_t)0 - (uint64_t)sv : (uint64_t)sv) : load_uint(p, t->size);
        bool neg = sign && sv < 0;
        switch (verb) {
        case 'v': case 'd': fmt_integer(sb, uv, neg, 10, false, f, verb); return;
        case 'b': fmt_integer(sb, uv, neg, 2, false, f, verb); return;
        case 'o': case 'O': fmt_integer(sb, uv, neg, 8, false, f, verb); return;
        case 'x': fmt_integer(sb, uv, neg, 16, false, f, verb); return;
        case 'X': fmt_integer(sb, uv, neg, 16, true, f, verb); return;
        case 'c': {
            gocpy_string s = gocpy_string_from_rune(sign ? sv : (int64_t)uv);
            put_padded(sb, s.ptr, s.len, f, false);
            return;
        }
        case 'q': {
            gocpy_sb tmp = gocpy_sb_new();
            gocpy_sb_byte(&tmp, '\'');
            quote_rune_into(&tmp, (int32_t)(sign ? sv : (int64_t)uv), '\'');
            gocpy_sb_byte(&tmp, '\'');
            put_padded_sb(sb, &tmp, f, false);
            return;
        }
        case 'U': {
            char buf[32];
            int64_t r = sign ? sv : (int64_t)uv;
            int n = snprintf(buf, sizeof buf, "U+%04llX", (unsigned long long)r);
            gocpy_sb tmp = gocpy_sb_new();
            gocpy_sb_bytes(&tmp, buf, n);
            if (f->sharp) {
                gocpy_sb_cstr(&tmp, " '");
                gocpy_sb_str(&tmp, gocpy_string_from_rune(r));
                gocpy_sb_byte(&tmp, '\'');
            }
            put_padded_sb(sb, &tmp, f, false);
            return;
        }
        case 'e': case 'E': case 'f': case 'F': case 'g': case 'G':
            break;
        }
        break;
    }
    case GOCPY_K_FLOAT: {
        double v = t->size == 4 ? (double)*(const float *)p : *(const double *)p;
        switch (verb) {
        case 'v': case 'e': case 'E': case 'f': case 'F': case 'g': case 'G':
            fmt_float(sb, v, t->size == 4 ? 32 : 64, verb, f);
            return;
        }
        break;
    }
    case GOCPY_K_STRING: {
        gocpy_string s = *(const gocpy_string *)p;
        if (verb == 'v' || verb == 's' || verb == 'q' || verb == 'x' || verb == 'X') {
            fmt_text(sb, s.ptr, s.len, verb == 'v' ? 's' : verb, f);
            return;
        }
        break;
    }
    case GOCPY_K_SLICE: case GOCPY_K_ARRAY: {
        if (t->elem == NULL) break;
        const uint8_t *data;
        int64_t n;
        if (t->kind == GOCPY_K_SLICE) {
            const gocpy_slice *s = (const gocpy_slice *)p;
            data = (const uint8_t *)s->data;
            n = s->len;
        } else {
            data = (const uint8_t *)p;
            n = t->elem->size ? (int64_t)(t->size / t->elem->size) : 0;
        }
        if (t->elem->kind == GOCPY_K_UINT && t->elem->size == 1 && (verb == 's' || verb == 'q' || verb == 'x' || verb == 'X')) {
            fmt_text(sb, data, n, verb, f);
            return;
        }
        if (verb == 'v') break;
        fmt_elems(sb, t->elem, data, n, verb, f, depth);
        return;
    }
    default:
        break;
    }
    if (verb == 'v' || (verb == 's' && t->kind != GOCPY_K_INT && t->kind != GOCPY_K_UINT && t->kind != GOCPY_K_FLOAT && t->kind != GOCPY_K_BOOL)) {
        gocpy_sb tmp = gocpy_sb_new();
        t->fmt(&tmp, p);
        put_padded_sb(sb, &tmp, f, false);
        return;
    }
    bad_verb(sb, t, p, verb);
}

static void fmt_arg(gocpy_sb *sb, gocpy_iface a, char verb, const fflags *f) {
    if (a.itab == NULL) { fmt_value(sb, NULL, NULL, verb, f, 0); return; }
    fmt_value(sb, a.itab->type, GOCPY_IFACE_VALP(a), verb, f, 0);
}

static bool arg_int(gocpy_iface a, int64_t *out) {
    if (a.itab == NULL) return false;
    const gocpy_typeinfo *t = a.itab->type;
    const void *p = GOCPY_IFACE_VALP(a);
    if (t->kind == GOCPY_K_INT) { *out = load_int(p, t->size); return true; }
    if (t->kind == GOCPY_K_UINT) { *out = (int64_t)load_uint(p, t->size); return true; }
    return false;
}

/* the Printf engine; wrapped receives the operand of the first %w */
static void do_printf(gocpy_sb *sb, gocpy_string format, gocpy_slice args, gocpy_iface *wrapped) {
    const uint8_t *s = format.ptr;
    int64_t n = format.len, argi = 0, nargs = args.len;
    gocpy_iface *av = (gocpy_iface *)args.data;
    for (int64_t i = 0; i < n;) {
        if (s[i] != '%') {
            int64_t j = i;
            while (j < n && s[j] != '%') j++;
            gocpy_sb_bytes(sb, s + i, j - i);
            i = j;
            continue;
        }
        i++;
        fflags f = {0};
        for (; i < n; i++) {
            switch (s[i]) {
            case '-': f.minus = true; f.zero = false; continue;
            case '+': f.plus = true; continue;
            case '#': f.sharp = true; continue;
            case ' ': f.space = true; continue;
            case '0': f.zero = !f.minus; continue;
            }
            break;
        }
        if (i < n && s[i] == '*') {
            i++;
            int64_t w;
            if (argi < nargs && arg_int(av[argi], &w)) {
                f.has_wid = true;
                f.wid = (int)(w < 0 ? -w : w);
                if (w < 0) { f.minus = true; f.zero = false; }
            } else {
                gocpy_sb_cstr(sb, "%!(BADWIDTH)");
            }
            argi++;
        } else {
            while (i < n && s[i] >= '0' && s[i] <= '9') { f.has_wid = true; f.wid = f.wid * 10 + (s[i] - '0'); i++; }
        }
        if (i < n && s[i] == '.') {
            i++;
            f.has_prec = true;
            if (i < n && s[i] == '*') {
                i++;
                int64_t pr;
                if (argi < nargs && arg_int(av[argi], &pr) && pr >= 0) f.prec = (int)pr;
                else { gocpy_sb_cstr(sb, "%!(BADPREC)"); f.has_prec = false; }
                argi++;
            } else {
                while (i < n && s[i] >= '0' && s[i] <= '9') { f.prec = f.prec * 10 + (s[i] - '0'); i++; }
            }
        }
        if (i >= n) { gocpy_sb_cstr(sb, "%!(NOVERB)"); break; }
        /* the verb is one rune */
        int32_t r;
        i += gocpy_decode_rune(format, i, &r);
        if (r == '%') { gocpy_sb_byte(sb, '%'); continue; }
        char verb = r < 128 ? (char)r : '?';
        if (argi >= nargs) {
            gocpy_sb_cstr(sb, "%!");
            gocpy_sb_str(sb, gocpy_string_from_rune(r));
            gocpy_sb_cstr(sb, "(MISSING)");
            continue;
        }
        gocpy_iface a = av[argi++];
        if (verb == 'w') {
            if (wrapped != NULL && wrapped->itab == NULL && a.itab != NULL && find_method(a.itab->type, "Error", "0()(string)") != NULL) {
                *wrapped = a;
                fmt_arg(sb, a, 'v', &f);
            } else if (wrapped != NULL && a.itab != NULL && find_method(a.itab->type, "Error", "0()(string)") != NULL) {
                fmt_arg(sb, a, 'v', &f);
            } else {
                gocpy_sb_cstr(sb, "%!w(");
                if (a.itab == NULL) gocpy_sb_cstr(sb, "<nil>");
                else { gocpy_sb_cstr(sb, a.itab->type->name); gocpy_sb_byte(sb, '='); gocpy_sb_iface(sb, a); }
                gocpy_sb_byte(sb, ')');
            }
            continue;
        }
        if (r >= 128) { bad_verb(sb, a.itab ? a.itab->type : NULL, a.itab ? GOCPY_IFACE_VALP(a) : NULL, '?'); continue; }
        fmt_arg(sb, a, verb, &f);
    }
    if (argi < nargs) {
        gocpy_sb_cstr(sb, "%!(EXTRA ");
        for (int64_t k = argi; k < nargs; k++) {
            if (k > argi) gocpy_sb_cstr(sb, ", ");
            if (av[k].itab == NULL) gocpy_sb_cstr(sb, "<nil>");
            else {
                gocpy_sb_cstr(sb, av[k].itab->type->name);
                gocpy_sb_byte(sb, '=');
                gocpy_sb_iface(sb, av[k]);
            }
        }
        gocpy_sb_byte(sb, ')');
    }
}

static bool is_string_arg(gocpy_iface a) { return a.itab != NULL && a.itab->type->kind == GOCPY_K_STRING; }

static void do_print(gocpy_sb *sb, gocpy_slice args, bool ln) {
    gocpy_iface *av = (gocpy_iface *)args.data;
    fflags f = {0};
    for (int64_t i = 0; i < args.len; i++) {
        if (i > 0 && (ln || (!is_string_arg(av[i]) && !is_string_arg(av[i - 1])))) gocpy_sb_byte(sb, ' ');
        fmt_arg(sb, av[i], 'v', &f);
    }
    if (ln) gocpy_sb_byte(sb, '\n');
}

gocpy_string gocpy_fmt_sprintf(gocpy_string format, gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_printf(&sb, format, args, NULL);
    return gocpy_sb_string(&sb);
}

gocpy_string gocpy_fmt_sprint(gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_print(&sb, args, false);
    return gocpy_sb_string(&sb);
}

gocpy_string gocpy_fmt_sprintln(gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_print(&sb, args, true);
    return gocpy_sb_string(&sb);
}

gocpy_tup_Tint_error gocpy_fmt_printf(gocpy_string format, gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_printf(&sb, format, args, NULL);
    gocpy_sb_write_stdout(&sb);
    return tup(sb.len, nil_iface);
}

static gocpy_tup_Tint_error write_sb(gocpy_iface w, gocpy_sb *sb) {
    if (w.itab == NULL) gocpy_panic("invalid memory address or nil pointer dereference (nil io.Writer)");
    gocpy_slice p = { sb->buf, sb->len, sb->cap };
    return call_rw(w, p);
}

gocpy_tup_Tint_error gocpy_fmt_fprint(gocpy_iface w, gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_print(&sb, args, false);
    return write_sb(w, &sb);
}

gocpy_tup_Tint_error gocpy_fmt_fprintln(gocpy_iface w, gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_print(&sb, args, true);
    return write_sb(w, &sb);
}

gocpy_tup_Tint_error gocpy_fmt_fprintf(gocpy_iface w, gocpy_string format, gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    do_printf(&sb, format, args, NULL);
    return write_sb(w, &sb);
}

/* ---------------------------------------------------------------- errors */

/* fmt.Errorf with %w: *fmt.wrapError */
typedef struct { gocpy_string msg; gocpy_iface err; } wrap_error;
static void wrap_fmt(gocpy_sb *sb, const void *p) { gocpy_sb_str(sb, (*(wrap_error *const *)p)->msg); }
static gocpy_string wrap_error_m(void *self) { return ((wrap_error *)self)->msg; }
static gocpy_iface wrap_unwrap(void *self) { return ((wrap_error *)self)->err; }
static bool wrap_eq(const void *a, const void *b) { return *(void *const *)a == *(void *const *)b; }
static const gocpy_method wrap_methods[] = {
    { "Error", "0()(string)", (void *)wrap_error_m },
    { "Unwrap", "0()(error)", (void *)wrap_unwrap },
};
static const gocpy_typeinfo wrap_ti = { "*fmt.wrapError", wrap_fmt, sizeof(void *), true, wrap_eq, 2, wrap_methods, GOCPY_K_PTR, NULL, NULL };
static const gocpy_itab wrap_itab = { &wrap_ti, { (void *)wrap_error_m } };

gocpy_iface gocpy_fmt_errorf(gocpy_string format, gocpy_slice args) {
    gocpy_sb sb = gocpy_sb_new();
    gocpy_iface wrapped = nil_iface;
    do_printf(&sb, format, args, &wrapped);
    gocpy_string msg = gocpy_sb_string(&sb);
    if (wrapped.itab == NULL) return gocpy_errors_new(msg);
    wrap_error *w = (wrap_error *)gocpy_alloc(sizeof(wrap_error));
    w->msg = msg;
    w->err = wrapped;
    return (gocpy_iface){ &wrap_itab, w };
}

/* errors.Join: *errors.joinError */
typedef struct { gocpy_slice errs; } join_error;
static gocpy_string join_error_m(void *self) {
    join_error *j = (join_error *)self;
    gocpy_sb sb = gocpy_sb_new();
    gocpy_iface *ev = (gocpy_iface *)j->errs.data;
    for (int64_t i = 0; i < j->errs.len; i++) {
        if (i > 0) gocpy_sb_byte(&sb, '\n');
        gocpy_sb_iface(&sb, ev[i]);
    }
    return gocpy_sb_string(&sb);
}
static void join_fmt(gocpy_sb *sb, const void *p) { gocpy_sb_str(sb, join_error_m(*(void *const *)p)); }
static gocpy_slice join_unwrap(void *self) { return ((join_error *)self)->errs; }
static const gocpy_method join_methods[] = {
    { "Error", "0()(string)", (void *)join_error_m },
    { "Unwrap", "0()(serror)", (void *)join_unwrap },
};
static const gocpy_typeinfo join_ti = { "*errors.joinError", join_fmt, sizeof(void *), true, wrap_eq, 2, join_methods, GOCPY_K_PTR, NULL, NULL };
static const gocpy_itab join_itab = { &join_ti, { (void *)join_error_m } };

gocpy_iface gocpy_errors_join(gocpy_slice errs) {
    gocpy_iface *ev = (gocpy_iface *)errs.data;
    int64_t n = 0;
    for (int64_t i = 0; i < errs.len; i++) if (ev[i].itab != NULL) n++;
    if (n == 0) return nil_iface;
    join_error *j = (join_error *)gocpy_alloc(sizeof(join_error));
    j->errs = gocpy_make_slice(n, n, sizeof(gocpy_iface));
    gocpy_iface *out = (gocpy_iface *)j->errs.data;
    for (int64_t i = 0, k = 0; i < errs.len; i++) if (ev[i].itab != NULL) out[k++] = ev[i];
    return (gocpy_iface){ &join_itab, j };
}

gocpy_iface gocpy_errors_unwrap(gocpy_iface err) {
    if (err.itab == NULL) return nil_iface;
    const gocpy_method *m = find_method(err.itab->type, "Unwrap", "0()(error)");
    if (m == NULL) return nil_iface;
    return ((gocpy_iface (*)(void *))m->fn)(err.data);
}

static bool same_error(gocpy_iface a, gocpy_iface b) {
    if (a.itab == NULL || b.itab == NULL) return a.itab == b.itab;
    const gocpy_typeinfo *t = a.itab->type;
    if (t != b.itab->type || t->eq == NULL) return false;
    return t->eq(GOCPY_IFACE_VALP(a), GOCPY_IFACE_VALP(b));
}

static bool errors_is(gocpy_iface err, gocpy_iface target, int depth) {
    for (; err.itab != NULL && depth < 100; depth++) {
        if (same_error(err, target)) return true;
        const gocpy_method *m = find_method(err.itab->type, "Is", "0(error)(bool)");
        if (m != NULL && ((bool (*)(void *, gocpy_iface))m->fn)(err.data, target)) return true;
        const gocpy_method *ms = find_method(err.itab->type, "Unwrap", "0()(serror)");
        if (ms != NULL) {
            gocpy_slice es = ((gocpy_slice (*)(void *))ms->fn)(err.data);
            for (int64_t i = 0; i < es.len; i++)
                if (errors_is(((gocpy_iface *)es.data)[i], target, depth + 1)) return true;
            return false;
        }
        err = gocpy_errors_unwrap(err);
    }
    return false;
}

bool gocpy_errors_is(gocpy_iface err, gocpy_iface target) {
    if (target.itab == NULL) return err.itab == NULL;
    return errors_is(err, target, 0);
}

static bool errors_as(gocpy_iface err, const gocpy_typeinfo *want, void *dst, int depth) {
    for (; err.itab != NULL && depth < 100; depth++) {
        if (want->kind == GOCPY_K_IFACE && want->iface != NULL) {
            gocpy_iface out;
            if (gocpy_iface_to_iface(err, want->iface, &out)) { *(gocpy_iface *)dst = out; return true; }
        } else if (err.itab->type == want) {
            memcpy(dst, GOCPY_IFACE_VALP(err), want->size);
            return true;
        }
        const gocpy_method *ms = find_method(err.itab->type, "Unwrap", "0()(serror)");
        if (ms != NULL) {
            gocpy_slice es = ((gocpy_slice (*)(void *))ms->fn)(err.data);
            for (int64_t i = 0; i < es.len; i++)
                if (errors_as(((gocpy_iface *)es.data)[i], want, dst, depth + 1)) return true;
            return false;
        }
        err = gocpy_errors_unwrap(err);
    }
    return false;
}

bool gocpy_errors_as(gocpy_iface err, gocpy_iface target) {
    if (target.itab == NULL || target.itab->type->kind != GOCPY_K_PTR || target.itab->type->elem == NULL || target.data == NULL)
        gocpy_panic("errors.As: target must be a non-nil pointer");
    return errors_as(err, target.itab->type->elem, target.data, 0);
}

/* ---------------------------------------------------------------- scanning */

typedef struct {
    int kind;                       /* 0 string, 1 stdin, 2 io.Reader */
    gocpy_string s;
    int64_t pos;
    gocpy_iface r;
    int peek;                       /* pushed-back byte or -1 */
    gocpy_iface err;                /* read error from the reader */
    uint8_t *one;                   /* 1-byte buffer for io.Reader sources */
} scansrc;

static int src_get(scansrc *src) {
    if (src->peek >= 0) { int c = src->peek; src->peek = -1; return c; }
    switch (src->kind) {
    case 0:
        if (src->pos >= src->s.len) return -1;
        return src->s.ptr[src->pos++];
    case 1: {
        uint8_t c;
        fflush(stdout);
        for (;;) {
            ssize_t n = read(0, &c, 1);
            if (n == 1) return c;
            if (n < 0 && errno == EINTR) continue;
            return -1;
        }
    }
    default: {
        if (src->one == NULL) src->one = (uint8_t *)gocpy_alloc(1);
        for (int tries = 0; tries < 100; tries++) {
            gocpy_tup_Tint_error res = call_rw(src->r, (gocpy_slice){ src->one, 1, 1 });
            if (res.r0 == 1) return src->one[0];
            if (res.r1.itab != NULL) {
                if (!same_error(res.r1, gocpy_io_eof())) src->err = res.r1;
                return -1;
            }
        }
        return -1;
    }
    }
}
static void src_unget(scansrc *src, int c) { if (c >= 0) src->peek = c; }

static bool is_space(int c) { return c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f'; }

static gocpy_iface scan_error(const char *msg) { return gocpy_errors_new(str_c(msg)); }

static gocpy_iface scan_error_tok(const char *msg, gocpy_string tok) {
    gocpy_sb sb = gocpy_sb_new();
    gocpy_sb_cstr(&sb, msg);
    gocpy_sb_str(&sb, tok);
    return gocpy_errors_new(gocpy_sb_string(&sb));
}

/* skips blanks; newlines too unless stop_nl. Returns the next byte (not consumed) */
static int skip_space(scansrc *src, bool stop_nl) {
    for (;;) {
        int c = src_get(src);
        if (c < 0) return -1;
        if (is_space(c) || (c == '\n' && !stop_nl)) continue;
        src_unget(src, c);
        return c;
    }
}

static gocpy_string read_token(scansrc *src) {
    gocpy_sb sb = gocpy_sb_new();
    for (;;) {
        int c = src_get(src);
        if (c < 0) break;
        if (is_space(c) || c == '\n') { src_unget(src, c); break; }
        gocpy_sb_byte(&sb, (uint8_t)c);
    }
    return gocpy_sb_string(&sb);
}

static bool store_int(void *dst, size_t size, int64_t v) {
    switch (size) {
    case 1: if (v < INT8_MIN || v > INT8_MAX) return false; *(int8_t *)dst = (int8_t)v; return true;
    case 2: if (v < INT16_MIN || v > INT16_MAX) return false; *(int16_t *)dst = (int16_t)v; return true;
    case 4: if (v < INT32_MIN || v > INT32_MAX) return false; *(int32_t *)dst = (int32_t)v; return true;
    default: *(int64_t *)dst = v; return true;
    }
}
static bool store_uint(void *dst, size_t size, uint64_t v) {
    switch (size) {
    case 1: if (v > UINT8_MAX) return false; *(uint8_t *)dst = (uint8_t)v; return true;
    case 2: if (v > UINT16_MAX) return false; *(uint16_t *)dst = (uint16_t)v; return true;
    case 4: if (v > UINT32_MAX) return false; *(uint32_t *)dst = (uint32_t)v; return true;
    default: *(uint64_t *)dst = v; return true;
    }
}

/* converts a token into *dst (type et) for verb ('v' for Scan) */
static gocpy_iface scan_store(const gocpy_typeinfo *et, void *dst, gocpy_string tok, char verb) {
    char *c = cstr(tok);
    gocpy_iface err = nil_iface;
    char *end = NULL;
    errno = 0;
    switch (et->kind) {
    case GOCPY_K_INT: {
        int base = verb == 'x' || verb == 'X' ? 16 : verb == 'o' ? 8 : verb == 'b' ? 2 : verb == 'd' ? 10 : 0;
        long long v = strtoll(c, &end, base);
        if (tok.len == 0 || *end != 0) err = scan_error("expected integer");
        else if (errno == ERANGE || !store_int(dst, et->size, v)) err = scan_error_tok("integer overflow on token ", tok);
        break;
    }
    case GOCPY_K_UINT: {
        int base = verb == 'x' || verb == 'X' ? 16 : verb == 'o' ? 8 : verb == 'b' ? 2 : verb == 'd' ? 10 : 0;
        unsigned long long v = strtoull(c, &end, base);
        if (tok.len == 0 || *end != 0 || c[0] == '-') err = scan_error("expected integer");
        else if (errno == ERANGE || !store_uint(dst, et->size, v)) err = scan_error_tok("unsigned integer overflow on token ", tok);
        break;
    }
    case GOCPY_K_FLOAT: {
        double v = strtod(c, &end);
        if (tok.len == 0 || *end != 0) err = scan_error("expected float");
        else if (et->size == 4) *(float *)dst = (float)v;
        else *(double *)dst = v;
        break;
    }
    case GOCPY_K_BOOL:
        if (!strcmp(c, "true") || !strcmp(c, "TRUE") || !strcmp(c, "True") || !strcmp(c, "t") || !strcmp(c, "T") || !strcmp(c, "1")) *(bool *)dst = true;
        else if (!strcmp(c, "false") || !strcmp(c, "FALSE") || !strcmp(c, "False") || !strcmp(c, "f") || !strcmp(c, "F") || !strcmp(c, "0")) *(bool *)dst = false;
        else err = scan_error("syntax error scanning boolean");
        break;
    case GOCPY_K_STRING:
        *(gocpy_string *)dst = str_copy((const char *)tok.ptr, tok.len);
        break;
    case GOCPY_K_SLICE:
        if (is_byte_slice(et)) {
            gocpy_string s = str_copy((const char *)tok.ptr, tok.len);
            *(gocpy_slice *)dst = (gocpy_slice){ (void *)s.ptr, s.len, s.len };
            break;
        }
        /* fallthrough */
    default: {
        gocpy_sb sb = gocpy_sb_new();
        gocpy_sb_cstr(&sb, "can't scan type: ");
        gocpy_sb_cstr(&sb, et->name);
        err = gocpy_errors_new(gocpy_sb_string(&sb));
    }
    }
    free(c);
    return err;
}

static gocpy_iface check_target(gocpy_iface a, const gocpy_typeinfo **et) {
    if (a.itab == NULL || a.itab->type->kind != GOCPY_K_PTR || a.itab->type->elem == NULL || a.data == NULL) {
        gocpy_sb sb = gocpy_sb_new();
        gocpy_sb_cstr(&sb, "can't scan type: ");
        gocpy_sb_cstr(&sb, a.itab ? a.itab->type->name : "<nil>");
        return gocpy_errors_new(gocpy_sb_string(&sb));
    }
    *et = a.itab->type->elem;
    return nil_iface;
}

static gocpy_tup_Tint_error do_scan(scansrc *src, gocpy_slice args, bool ln) {
    gocpy_iface *av = (gocpy_iface *)args.data;
    int64_t n = 0;
    for (int64_t i = 0; i < args.len; i++) {
        const gocpy_typeinfo *et;
        gocpy_iface err = check_target(av[i], &et);
        if (err.itab) return tup(n, err);
        int c = skip_space(src, ln);
        if (c < 0) {
            if (src->err.itab) return tup(n, src->err);
            return tup(n, n == 0 ? gocpy_io_eof() : gocpy_io_unexpected_eof());
        }
        if (c == '\n') return tup(n, scan_error("unexpected newline"));
        gocpy_string tok = read_token(src);
        err = scan_store(et, av[i].data, tok, 'v');
        if (err.itab) return tup(n, err);
        n++;
    }
    if (ln) {
        for (;;) {
            int c = src_get(src);
            if (c < 0 || c == '\n') break;
            if (!is_space(c)) return tup(n, scan_error("expected newline"));
        }
    }
    return tup(n, nil_iface);
}

static gocpy_tup_Tint_error do_scanf(scansrc *src, gocpy_string format, gocpy_slice args) {
    gocpy_iface *av = (gocpy_iface *)args.data;
    int64_t n = 0, argi = 0;
    const uint8_t *f = format.ptr;
    for (int64_t i = 0; i < format.len;) {
        uint8_t fc = f[i];
        if (fc == '%' && i + 1 < format.len && f[i + 1] != '%') {
            i++;
            int width = 0;
            while (i < format.len && f[i] >= '0' && f[i] <= '9') { width = width * 10 + (f[i] - '0'); i++; }
            if (i >= format.len) return tup(n, scan_error("too few operands for format '%'"));
            char verb = (char)f[i++];
            if (argi >= args.len) return tup(n, scan_error("too few operands for format"));
            const gocpy_typeinfo *et;
            gocpy_iface err = check_target(av[argi], &et);
            if (err.itab) return tup(n, err);
            if (verb == 'c') {
                int c = src_get(src);
                if (c < 0) return tup(n, n == 0 ? gocpy_io_eof() : gocpy_io_unexpected_eof());
                if (et->kind == GOCPY_K_INT) store_int(av[argi].data, et->size, c);
                else if (et->kind == GOCPY_K_UINT) store_uint(av[argi].data, et->size, (uint64_t)c);
                else return tup(n, scan_error("bad verb '%c' for non-integer"));
                argi++;
                n++;
                continue;
            }
            int c = skip_space(src, true);
            if (c < 0) {
                if (src->err.itab) return tup(n, src->err);
                return tup(n, n == 0 ? gocpy_io_eof() : gocpy_io_unexpected_eof());
            }
            if (c == '\n') return tup(n, scan_error("unexpected newline"));
            gocpy_sb sb = gocpy_sb_new();
            for (int k = 0; width == 0 || k < width; k++) {
                int ch = src_get(src);
                if (ch < 0) break;
                if (is_space(ch) || ch == '\n') { src_unget(src, ch); break; }
                /* stop at the next literal of the format */
                if (i < format.len && f[i] != '%' && !is_space(f[i]) && f[i] != '\n' && ch == f[i]) { src_unget(src, ch); break; }
                gocpy_sb_byte(&sb, (uint8_t)ch);
            }
            err = scan_store(et, av[argi].data, gocpy_sb_string(&sb), verb == 's' || verb == 'q' ? 'v' : verb);
            if (err.itab) return tup(n, err);
            argi++;
            n++;
            continue;
        }
        if (fc == '%') i++; /* %% matches a literal % */
        if (is_space(fc)) {
            while (i < format.len && is_space(f[i])) i++;
            for (;;) {
                int c = src_get(src);
                if (c < 0) break;
                if (!is_space(c)) { src_unget(src, c); break; }
            }
            continue;
        }
        int c = src_get(src);
        if (fc == '\n') {
            while (c >= 0 && is_space(c)) c = src_get(src);
            if (c != '\n' && c >= 0) return tup(n, scan_error("newline in format does not match input"));
            i++;
            continue;
        }
        if (c != fc) {
            if (c < 0) return tup(n, gocpy_io_unexpected_eof());
            return tup(n, scan_error("input does not match format"));
        }
        i++;
    }
    if (argi < args.len) return tup(n, scan_error("too many operands"));
    return tup(n, nil_iface);
}

static scansrc src_string(gocpy_string s) { return (scansrc){ 0, s, 0, nil_iface, -1, nil_iface, NULL }; }
static scansrc src_stdin(void) { return (scansrc){ 1, { NULL, 0 }, 0, nil_iface, -1, nil_iface, NULL }; }
static scansrc src_reader(gocpy_iface r) {
    if (r.itab == NULL) gocpy_panic("invalid memory address or nil pointer dereference (nil io.Reader)");
    return (scansrc){ 2, { NULL, 0 }, 0, r, -1, nil_iface, NULL };
}

/* Scan on stdin keeps the byte after a token for the next call */
static gocpy_tup_Tint_error stdin_scan(int mode, gocpy_string format, gocpy_slice args) {
    /* Like Go with os.Stdin, stdin is read byte by byte without buffering,
     * and the one byte read past a token is consumed, so fmt.Scan and
     * io.Stdin readers can be mixed. */
    scansrc src = src_stdin();
    return mode == 0 ? do_scan(&src, args, false) : mode == 1 ? do_scan(&src, args, true) : do_scanf(&src, format, args);
}

gocpy_tup_Tint_error gocpy_fmt_scan(gocpy_slice args) { return stdin_scan(0, (gocpy_string){ NULL, 0 }, args); }
gocpy_tup_Tint_error gocpy_fmt_scanln(gocpy_slice args) { return stdin_scan(1, (gocpy_string){ NULL, 0 }, args); }
gocpy_tup_Tint_error gocpy_fmt_scanf(gocpy_string format, gocpy_slice args) { return stdin_scan(2, format, args); }
gocpy_tup_Tint_error gocpy_fmt_sscan(gocpy_string str, gocpy_slice args) { scansrc s = src_string(str); return do_scan(&s, args, false); }
gocpy_tup_Tint_error gocpy_fmt_sscanln(gocpy_string str, gocpy_slice args) { scansrc s = src_string(str); return do_scan(&s, args, true); }
gocpy_tup_Tint_error gocpy_fmt_sscanf(gocpy_string str, gocpy_string format, gocpy_slice args) { scansrc s = src_string(str); return do_scanf(&s, format, args); }
gocpy_tup_Tint_error gocpy_fmt_fscan(gocpy_iface r, gocpy_slice args) { scansrc s = src_reader(r); return do_scan(&s, args, false); }
gocpy_tup_Tint_error gocpy_fmt_fscanln(gocpy_iface r, gocpy_slice args) { scansrc s = src_reader(r); return do_scan(&s, args, true); }
gocpy_tup_Tint_error gocpy_fmt_fscanf(gocpy_iface r, gocpy_string format, gocpy_slice args) { scansrc s = src_reader(r); return do_scanf(&s, format, args); }

/* ---------------------------------------------------------------- system calls */

#define RETRY(expr) ({ long _r; do { _r = (long)(expr); } while (_r < 0 && errno == EINTR); _r; })

gocpy_string gocpy_sys_strerror(int64_t code) {
    if (code < 0) code = -code; /* accepts both conventions */
    if (code == 0) return GOCPY_STR("");
    if (code == GOCPY_ETIMEOUT) return GOCPY_STR("i/o timeout");
    if (code > GOCPY_EGAI) {
        int g = (int)(code - GOCPY_EGAI);
        if (EAI_NONAME < 0) g = -g; /* glibc and musl use negative EAI_* codes */
        if (g == EAI_NONAME
#ifdef EAI_NODATA
            || g == EAI_NODATA
#endif
        ) return GOCPY_STR("no such host");
        return str_c(gai_strerror(g));
    }
    char buf[256];
    const char *m = buf;
#if defined(__GLIBC__) && defined(_GNU_SOURCE)
    m = strerror_r((int)code, buf, sizeof buf);
#else
    if (strerror_r((int)code, buf, sizeof buf) != 0) snprintf(buf, sizeof buf, "errno %d", (int)code);
#endif
    gocpy_string s = str_c(m);
    /* Go style: lower-case first letter */
    if (s.len > 1 && s.ptr[0] >= 'A' && s.ptr[0] <= 'Z' && !(s.ptr[1] >= 'A' && s.ptr[1] <= 'Z')) ((uint8_t *)s.ptr)[0] += 32;
    return s;
}

static int64_t neg_errno(void) {
    if (errno == EAGAIN || errno == EWOULDBLOCK) return -GOCPY_ETIMEOUT;
    return -(int64_t)errno;
}

int64_t gocpy_sys_open(gocpy_string path, int64_t flags, int64_t perm) {
    /* abstract flags of std/io: RDONLY 0, WRONLY 1, RDWR 2, APPEND 8, CREATE 16, EXCL 32, TRUNC 64, SYNC 128 */
    int f = O_CLOEXEC;
    switch (flags & 3) { case 1: f |= O_WRONLY; break; case 2: f |= O_RDWR; break; default: f |= O_RDONLY; }
    if (flags & 8) f |= O_APPEND;
    if (flags & 16) f |= O_CREAT;
    if (flags & 32) f |= O_EXCL;
    if (flags & 64) f |= O_TRUNC;
    if (flags & 128) f |= O_SYNC;
    char *p = cstr(path);
    int fd = (int)RETRY(open(p, f, (mode_t)perm));
    int64_t r = fd < 0 ? neg_errno() : fd;
    free(p);
    if (r >= 0) {
        struct stat st;
        if (fstat((int)r, &st) == 0 && S_ISDIR(st.st_mode) && (flags & 3) != 0) { close((int)r); return -EISDIR; }
    }
    return r;
}

int64_t gocpy_sys_read(int64_t fd, gocpy_slice p) {
    if (fd == 0) fflush(stdout);
    if (p.len == 0) return 0;
    long n = RETRY(read((int)fd, p.data, (size_t)p.len));
    return n < 0 ? neg_errno() : n;
}

int64_t gocpy_sys_write(int64_t fd, gocpy_slice p) {
    if (fd == 1) {
        /* share the stdio buffer with fmt.Println to keep the output order */
        size_t n = fwrite(p.data, 1, (size_t)p.len, stdout);
        if ((int64_t)n < p.len) return -(int64_t)(errno ? errno : EIO);
        return (int64_t)n;
    }
    if (fd == 2) fflush(stdout);
    int64_t done = 0;
    while (done < p.len) {
        long n = RETRY(write((int)fd, (uint8_t *)p.data + done, (size_t)(p.len - done)));
        if (n < 0) return done > 0 ? done : neg_errno();
        done += n;
    }
    return done;
}

int64_t gocpy_sys_close(int64_t fd) {
    if (fd <= 2) { fflush(stdout); return 0; }
    shutdown((int)fd, SHUT_RDWR); /* wakes up accept/recv blocked in other threads (sockets only) */
    return close((int)fd) < 0 ? neg_errno() : 0;
}

int64_t gocpy_sys_seek(int64_t fd, int64_t off, int64_t whence) {
    off_t r = lseek((int)fd, (off_t)off, whence == 1 ? SEEK_CUR : whence == 2 ? SEEK_END : SEEK_SET);
    return r < 0 ? neg_errno() : (int64_t)r;
}

int64_t gocpy_sys_fsync(int64_t fd) {
    if (fd == 1) { fflush(stdout); return 0; }
    return fsync((int)fd) < 0 ? neg_errno() : 0;
}

static void fill_stat(const struct stat *st, gocpy_slice out) {
    int64_t *o = (int64_t *)out.data;
    if (out.len < 4) return;
    o[0] = (int64_t)st->st_size;
    o[1] = S_ISDIR(st->st_mode) ? 1 : 0;
#if defined(__APPLE__)
    o[2] = (int64_t)st->st_mtimespec.tv_sec * 1000000000LL + st->st_mtimespec.tv_nsec;
#else
    o[2] = (int64_t)st->st_mtim.tv_sec * 1000000000LL + st->st_mtim.tv_nsec;
#endif
    o[3] = (int64_t)(st->st_mode & 07777);
}

int64_t gocpy_sys_stat(gocpy_string path, gocpy_slice out) {
    char *p = cstr(path);
    struct stat st;
    int r = stat(p, &st);
    int64_t res = r < 0 ? neg_errno() : 0;
    free(p);
    if (r == 0) fill_stat(&st, out);
    return res;
}

int64_t gocpy_sys_fstat(int64_t fd, gocpy_slice out) {
    struct stat st;
    if (fstat((int)fd, &st) < 0) return neg_errno();
    fill_stat(&st, out);
    return 0;
}

int64_t gocpy_sys_mkdir(gocpy_string path, int64_t perm) {
    char *p = cstr(path);
    int r = mkdir(p, (mode_t)perm);
    int64_t res = r < 0 ? neg_errno() : 0;
    free(p);
    return res;
}

int64_t gocpy_sys_remove(gocpy_string path) {
    char *p = cstr(path);
    int64_t res = 0;
    if (unlink(p) < 0) {
        int e1 = errno;
        res = -(int64_t)e1;
        if (e1 == EISDIR || e1 == EPERM) { /* a directory: Linux says EISDIR, macOS EPERM */
            if (rmdir(p) == 0) res = 0;
            else res = (errno == EEXIST) ? -ENOTEMPTY : -(int64_t)errno;
        }
    }
    free(p);
    return res;
}

int64_t gocpy_sys_rename(gocpy_string from, gocpy_string to) {
    char *a = cstr(from), *b = cstr(to);
    int r = rename(a, b);
    int64_t res = r < 0 ? neg_errno() : 0;
    free(a);
    free(b);
    return res;
}

static int cmp_names(const void *a, const void *b) { return strcmp(*(char *const *)a, *(char *const *)b); }

gocpy_string gocpy_sys_readdir(gocpy_string path, gocpy_slice err) {
    char *p = cstr(path);
    DIR *d = opendir(p);
    free(p);
    if (d == NULL) { if (err.len > 0) ((int64_t *)err.data)[0] = -neg_errno(); return GOCPY_STR(""); }
    size_t cap = 16, n = 0;
    char **names = (char **)malloc(cap * sizeof(char *));
    struct dirent *e;
    while ((e = readdir(d)) != NULL) {
        if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
        if (n == cap) { cap *= 2; names = (char **)realloc(names, cap * sizeof(char *)); }
        names[n++] = strdup(e->d_name);
    }
    closedir(d);
    qsort(names, n, sizeof(char *), cmp_names);
    gocpy_sb sb = gocpy_sb_new();
    for (size_t i = 0; i < n; i++) {
        gocpy_sb_cstr(&sb, names[i]);
        gocpy_sb_byte(&sb, 0); /* every name ends with NUL */
        free(names[i]);
    }
    free(names);
    if (err.len > 0) ((int64_t *)err.data)[0] = 0;
    return gocpy_sb_string(&sb);
}

gocpy_string gocpy_sys_tempdir(void) {
    const char *t = getenv("TMPDIR");
    if (t == NULL || *t == 0) t = "/tmp";
    size_t n = strlen(t);
    while (n > 1 && t[n - 1] == '/') n--;
    return str_copy(t, (int64_t)n);
}

/* ---------------------------------------------------------------- sockets */

static pthread_once_t sigpipe_once = PTHREAD_ONCE_INIT;
static void ignore_sigpipe(void) { signal(SIGPIPE, SIG_IGN); }

static int64_t gai_code(int g) {
    if (g == EAI_SYSTEM) return -(int64_t)errno;
    return -(int64_t)(GOCPY_EGAI + (g < 0 ? -g : g));
}

/* resolves host:port; host "" means any address (passive) */
static int resolve(gocpy_string host, int64_t port, int socktype, bool passive, struct addrinfo **res) {
    struct addrinfo hints;
    memset(&hints, 0, sizeof hints);
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = socktype;
    if (passive) hints.ai_flags = AI_PASSIVE;
    char portbuf[16];
    snprintf(portbuf, sizeof portbuf, "%lld", (long long)port);
    char *h = host.len > 0 ? cstr(host) : NULL;
    if (h != NULL && h[0] == '[') { /* [::1] */
        size_t n = strlen(h);
        if (n > 1 && h[n - 1] == ']') { memmove(h, h + 1, n - 2); h[n - 2] = 0; }
    }
    int g = getaddrinfo(h, portbuf, &hints, res);
    free(h);
    return g;
}

/* a socket bound to all interfaces: dual-stack IPv6 when available */
static int64_t bind_any(int socktype, int64_t port) {
    int fd = socket(AF_INET6, socktype | SOCK_CLOEXEC, 0);
    if (fd >= 0) {
        int zero = 0, one = 1;
        setsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &zero, sizeof zero);
        if (socktype == SOCK_STREAM) setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
        struct sockaddr_in6 a;
        memset(&a, 0, sizeof a);
        a.sin6_family = AF_INET6;
        a.sin6_port = htons((uint16_t)port);
        a.sin6_addr = in6addr_any;
        if (bind(fd, (struct sockaddr *)&a, sizeof a) == 0) return fd;
        int e = errno;
        close(fd);
        if (e != EAFNOSUPPORT && e != EADDRNOTAVAIL) return -(int64_t)e;
    }
    fd = socket(AF_INET, socktype | SOCK_CLOEXEC, 0);
    if (fd < 0) return neg_errno();
    int one = 1;
    if (socktype == SOCK_STREAM) setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    struct sockaddr_in a;
    memset(&a, 0, sizeof a);
    a.sin_family = AF_INET;
    a.sin_port = htons((uint16_t)port);
    a.sin_addr.s_addr = htonl(INADDR_ANY);
    if (bind(fd, (struct sockaddr *)&a, sizeof a) < 0) { int64_t r = neg_errno(); close(fd); return r; }
    return fd;
}

static int64_t bind_host(gocpy_string host, int64_t port, int socktype) {
    if (host.len == 0) return bind_any(socktype, port);
    struct addrinfo *res;
    int g = resolve(host, port, socktype, true, &res);
    if (g != 0) return gai_code(g);
    int64_t last = -EADDRNOTAVAIL;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        int fd = socket(ai->ai_family, ai->ai_socktype | SOCK_CLOEXEC, ai->ai_protocol);
        if (fd < 0) { last = neg_errno(); continue; }
        int one = 1;
        if (socktype == SOCK_STREAM) setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
        if (bind(fd, ai->ai_addr, ai->ai_addrlen) == 0) { freeaddrinfo(res); return fd; }
        last = neg_errno();
        close(fd);
    }
    freeaddrinfo(res);
    return last;
}

int64_t gocpy_sys_tcp_listen(gocpy_string host, int64_t port) {
    pthread_once(&sigpipe_once, ignore_sigpipe);
    int64_t fd = bind_host(host, port, SOCK_STREAM);
    if (fd < 0) return fd;
    if (listen((int)fd, 128) < 0) { int64_t r = neg_errno(); close((int)fd); return r; }
    return fd;
}

int64_t gocpy_sys_tcp_accept(int64_t fd) {
    for (;;) {
        int c = accept((int)fd, NULL, NULL);
        if (c >= 0) {
            fcntl(c, F_SETFD, FD_CLOEXEC);
            int one = 1;
            setsockopt(c, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
            return c;
        }
        if (errno == EINTR || errno == ECONNABORTED) continue;
        return neg_errno();
    }
}

static int connect_timeout(int fd, const struct sockaddr *addr, socklen_t len, int64_t timeout_ms) {
    if (timeout_ms <= 0) return (int)RETRY(connect(fd, addr, len));
    int fl = fcntl(fd, F_GETFL, 0);
    fcntl(fd, F_SETFL, fl | O_NONBLOCK);
    int r = connect(fd, addr, len);
    if (r < 0 && errno == EINPROGRESS) {
        struct pollfd p = { fd, POLLOUT, 0 };
        int64_t deadline = gocpy_now() + timeout_ms * 1000000LL;
        for (;;) {
            int64_t left = (deadline - gocpy_now()) / 1000000LL;
            if (left <= 0) { errno = ETIMEDOUT; r = -1; break; }
            int pr = poll(&p, 1, (int)left);
            if (pr < 0 && errno == EINTR) continue;
            if (pr == 0) { errno = ETIMEDOUT; r = -1; break; }
            int err = 0;
            socklen_t el = sizeof err;
            getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &el);
            if (err != 0) { errno = err; r = -1; } else r = 0;
            break;
        }
    }
    fcntl(fd, F_SETFL, fl);
    return r;
}

int64_t gocpy_sys_tcp_dial(gocpy_string host, int64_t port, int64_t timeout_ms) {
    pthread_once(&sigpipe_once, ignore_sigpipe);
    struct addrinfo *res;
    int g = resolve(host.len ? host : GOCPY_STR("localhost"), port, SOCK_STREAM, false, &res);
    if (g != 0) return gai_code(g);
    int64_t last = -ECONNREFUSED;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        int fd = socket(ai->ai_family, ai->ai_socktype | SOCK_CLOEXEC, ai->ai_protocol);
        if (fd < 0) { last = neg_errno(); continue; }
        if (connect_timeout(fd, ai->ai_addr, ai->ai_addrlen, timeout_ms) == 0) {
            int one = 1;
            setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
            freeaddrinfo(res);
            return fd;
        }
        last = errno == ETIMEDOUT ? -GOCPY_ETIMEOUT : neg_errno();
        close(fd);
    }
    freeaddrinfo(res);
    return last;
}

int64_t gocpy_sys_udp_open(gocpy_string host, int64_t port, bool conn) {
    pthread_once(&sigpipe_once, ignore_sigpipe);
    if (!conn) return bind_host(host, port, SOCK_DGRAM);
    struct addrinfo *res;
    int g = resolve(host.len ? host : GOCPY_STR("localhost"), port, SOCK_DGRAM, false, &res);
    if (g != 0) return gai_code(g);
    int64_t last = -EADDRNOTAVAIL;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        int fd = socket(ai->ai_family, ai->ai_socktype | SOCK_CLOEXEC, ai->ai_protocol);
        if (fd < 0) { last = neg_errno(); continue; }
        if (connect(fd, ai->ai_addr, ai->ai_addrlen) == 0) { freeaddrinfo(res); return fd; }
        last = neg_errno();
        close(fd);
    }
    freeaddrinfo(res);
    return last;
}

static int sock_family(int fd) {
    struct sockaddr_storage ss;
    socklen_t len = sizeof ss;
    if (getsockname(fd, (struct sockaddr *)&ss, &len) < 0) return AF_UNSPEC;
    return ss.ss_family;
}

int64_t gocpy_sys_sendto(int64_t fd, gocpy_slice p, gocpy_string host, int64_t port) {
    struct addrinfo *res;
    int g = resolve(host.len ? host : GOCPY_STR("localhost"), port, SOCK_DGRAM, false, &res);
    if (g != 0) return gai_code(g);
    int fam = sock_family((int)fd);
    int64_t r = -EAFNOSUPPORT;
    for (struct addrinfo *ai = res; ai != NULL; ai = ai->ai_next) {
        struct sockaddr_storage dst;
        socklen_t dl = ai->ai_addrlen;
        memcpy(&dst, ai->ai_addr, ai->ai_addrlen);
        if (fam == AF_INET6 && ai->ai_family == AF_INET) {
            /* IPv4 destination on a dual-stack socket: v4-mapped address */
            struct sockaddr_in *v4 = (struct sockaddr_in *)ai->ai_addr;
            struct sockaddr_in6 *v6 = (struct sockaddr_in6 *)&dst;
            memset(v6, 0, sizeof *v6);
            v6->sin6_family = AF_INET6;
            v6->sin6_port = v4->sin_port;
            v6->sin6_addr.s6_addr[10] = 0xff;
            v6->sin6_addr.s6_addr[11] = 0xff;
            memcpy(&v6->sin6_addr.s6_addr[12], &v4->sin_addr, 4);
            dl = sizeof *v6;
        } else if (fam != AF_UNSPEC && fam != ai->ai_family) {
            continue;
        }
        long n = RETRY(sendto((int)fd, p.data, (size_t)p.len, 0, (struct sockaddr *)&dst, dl));
        r = n < 0 ? neg_errno() : n;
        break;
    }
    freeaddrinfo(res);
    return r;
}

static _Thread_local char peer_ip[INET6_ADDRSTRLEN];
static _Thread_local int peer_port;

static void addr_string(const struct sockaddr *sa, char *ip, size_t iplen, int *port) {
    ip[0] = 0;
    *port = 0;
    if (sa->sa_family == AF_INET) {
        const struct sockaddr_in *a = (const struct sockaddr_in *)sa;
        inet_ntop(AF_INET, &a->sin_addr, ip, (socklen_t)iplen);
        *port = ntohs(a->sin_port);
    } else if (sa->sa_family == AF_INET6) {
        const struct sockaddr_in6 *a = (const struct sockaddr_in6 *)sa;
        if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr)) inet_ntop(AF_INET, &a->sin6_addr.s6_addr[12], ip, (socklen_t)iplen);
        else inet_ntop(AF_INET6, &a->sin6_addr, ip, (socklen_t)iplen);
        *port = ntohs(a->sin6_port);
    }
}

int64_t gocpy_sys_recvfrom(int64_t fd, gocpy_slice p) {
    struct sockaddr_storage ss;
    socklen_t len = sizeof ss;
    long n = RETRY(recvfrom((int)fd, p.data, (size_t)p.len, 0, (struct sockaddr *)&ss, &len));
    if (n < 0) return neg_errno();
    addr_string((struct sockaddr *)&ss, peer_ip, sizeof peer_ip, &peer_port);
    return n;
}

gocpy_string gocpy_sys_last_peer_ip(void) { return str_c(peer_ip); }
int64_t gocpy_sys_last_peer_port(void) { return peer_port; }

gocpy_string gocpy_sys_sock_ip(int64_t fd, bool peer) {
    struct sockaddr_storage ss;
    socklen_t len = sizeof ss;
    int r = peer ? getpeername((int)fd, (struct sockaddr *)&ss, &len) : getsockname((int)fd, (struct sockaddr *)&ss, &len);
    if (r < 0) return GOCPY_STR("");
    char ip[INET6_ADDRSTRLEN];
    int port;
    addr_string((struct sockaddr *)&ss, ip, sizeof ip, &port);
    return str_c(ip);
}

int64_t gocpy_sys_sock_port(int64_t fd, bool peer) {
    struct sockaddr_storage ss;
    socklen_t len = sizeof ss;
    int r = peer ? getpeername((int)fd, (struct sockaddr *)&ss, &len) : getsockname((int)fd, (struct sockaddr *)&ss, &len);
    if (r < 0) return 0;
    char ip[INET6_ADDRSTRLEN];
    int port;
    addr_string((struct sockaddr *)&ss, ip, sizeof ip, &port);
    return port;
}

int64_t gocpy_sys_set_timeout(int64_t fd, int64_t read_ms, int64_t write_ms) {
    struct timeval tv;
    if (read_ms >= 0) {
        tv.tv_sec = read_ms / 1000;
        tv.tv_usec = (read_ms % 1000) * 1000;
        if (setsockopt((int)fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv) < 0) return neg_errno();
    }
    if (write_ms >= 0) {
        tv.tv_sec = write_ms / 1000;
        tv.tv_usec = (write_ms % 1000) * 1000;
        if (setsockopt((int)fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv) < 0) return neg_errno();
    }
    return 0;
}

int64_t gocpy_sys_shutdown_write(int64_t fd) {
    return shutdown((int)fd, SHUT_WR) < 0 ? neg_errno() : 0;
}

gocpy_string gocpy_sys_resolve(gocpy_string host, gocpy_slice err) {
    struct addrinfo *res;
    int g = resolve(host.len ? host : GOCPY_STR("localhost"), 0, SOCK_DGRAM, false, &res);
    if (g != 0) { if (err.len > 0) ((int64_t *)err.data)[0] = -gai_code(g); return GOCPY_STR(""); }
    char ip[INET6_ADDRSTRLEN];
    int port;
    addr_string(res->ai_addr, ip, sizeof ip, &port);
    freeaddrinfo(res);
    if (err.len > 0) ((int64_t *)err.data)[0] = 0;
    return str_c(ip);
}

/* ---------------------------------------------------------------- strconv */

/* gocpy_strconv_parse_float parses s like Go's strconv.ParseFloat;
 * err[0] = 0 (ok), 1 (syntax) or 2 (out of range). */
double gocpy_strconv_parse_float(gocpy_string s, int64_t bits, gocpy_slice err) {
    int64_t *e = (int64_t *)err.data;
    e[0] = 1;
    if (s.len == 0 || s.len > 1000) return 0;
    char buf[1024];
    int64_t n = 0;
    bool underscore_ok = false;
    for (int64_t i = 0; i < s.len; i++) {
        char c = (char)s.ptr[i];
        if (c == '_') {
            if (!underscore_ok) return 0;
            continue;
        }
        if (c == 0 || isspace((unsigned char)c)) return 0;
        if (n == 0 || (n == 1 && (buf[0] == '+' || buf[0] == '-'))) {
            /* underscores are allowed only with a base prefix, as in Go */
            if (c == '0' && i + 1 < s.len && (s.ptr[i + 1] == 'x' || s.ptr[i + 1] == 'X')) underscore_ok = true;
        }
        buf[n++] = c;
    }
    buf[n] = 0;
    const char *p = buf;
    if (*p == '+' || *p == '-') p++;
    /* strtod accepts "nan(...)" and "infinity"; Go accepts inf, infinity and nan only */
    if (strncasecmp(p, "nan", 3) == 0 && p[3] != 0) return 0;
    errno = 0;
    char *end;
    double v = strtod(buf, &end);
    if (end != buf + n) return 0;
    e[0] = 0;
    if (errno == ERANGE && isinf(v)) e[0] = 2;
    if (bits == 32) {
        float f = (float)v;
        if (isinf(f) && !isinf(v)) e[0] = 2;
        v = f;
    }
    return v;
}

/* shortest_digits finds the shortest decimal digits that round-trip v;
 * returns the number of digits, the digits in d and the decimal exponent. */
static int shortest_digits(double v, int bits, char *d, int *exp10) {
    char buf[64];
    for (int p = 1; p <= 17; p++) {
        snprintf(buf, sizeof buf, "%.*e", p - 1, v);
        double back = strtod(buf, NULL);
        bool ok = bits == 32 ? (float)back == (float)v : back == v;
        if (ok || p == 17) {
            const char *q = buf;
            if (*q == '-') q++;
            int nd = 0;
            for (; *q && *q != 'e'; q++) if (*q != '.') d[nd++] = *q;
            *exp10 = atoi(q + 1);
            while (nd > 1 && d[nd - 1] == '0') nd--;
            d[nd] = 0;
            return nd;
        }
    }
    return 0;
}

/* gocpy_strconv_format_float is strconv.FormatFloat for the formats
 * 'e', 'E', 'f', 'g' and 'G'. */
gocpy_string gocpy_strconv_format_float(double v, int64_t fmtc, int64_t prec, int64_t bits) {
    char buf[2048];
    if (isnan(v)) return GOCPY_STR("NaN");
    if (isinf(v)) return v > 0 ? GOCPY_STR("+Inf") : GOCPY_STR("-Inf");
    char verb = (char)fmtc;
    if (verb != 'e' && verb != 'E' && verb != 'f' && verb != 'g' && verb != 'G') verb = 'g';
    if (bits == 32) v = (float)v;
    if (prec >= 0) {
        if (prec > 1000) prec = 1000;
        int n = snprintf(buf, sizeof buf, (char[]){ '%', '.', '*', verb, 0 }, (int)prec, v);
        if (n >= (int)sizeof buf) n = (int)sizeof buf - 1;
        return str_copy(buf, n);
    }
    char d[32];
    int e10;
    int nd = shortest_digits(v, (int)bits, d, &e10);
    bool neg = signbit(v);
    bool expo = verb == 'e' || verb == 'E';
    if (verb == 'g' || verb == 'G') expo = e10 < -4 || e10 >= 6; /* Go uses precision 6 for shortest %g */
    gocpy_sb sb = gocpy_sb_new();
    if (neg) gocpy_sb_byte(&sb, '-');
    if (expo) {
        gocpy_sb_byte(&sb, d[0]);
        if (nd > 1) {
            gocpy_sb_byte(&sb, '.');
            gocpy_sb_bytes(&sb, d + 1, nd - 1);
        }
        gocpy_sb_byte(&sb, (verb == 'E' || verb == 'G') ? 'E' : 'e');
        int x = e10;
        gocpy_sb_byte(&sb, x < 0 ? '-' : '+');
        if (x < 0) x = -x;
        char xb[16];
        int xn = snprintf(xb, sizeof xb, "%02d", x);
        gocpy_sb_bytes(&sb, xb, xn);
    } else {
        int dp = e10 + 1; /* digits before the decimal point */
        if (dp <= 0) {
            gocpy_sb_cstr(&sb, "0");
            gocpy_sb_byte(&sb, '.');
            for (int i = 0; i < -dp; i++) gocpy_sb_byte(&sb, '0');
            gocpy_sb_bytes(&sb, d, nd);
        } else {
            for (int i = 0; i < dp; i++) gocpy_sb_byte(&sb, i < nd ? d[i] : '0');
            if (nd > dp) {
                gocpy_sb_byte(&sb, '.');
                gocpy_sb_bytes(&sb, d + dp, nd - dp);
            }
        }
    }
    return gocpy_sb_string(&sb);
}

/* ---------------------------------------------------------------- helpers for std sources */

/* gocpy_sys_copy copies min(len(dst), len(src)) bytes (overlap allowed). */
int64_t gocpy_sys_copy(gocpy_slice dst, gocpy_slice src) {
    int64_t n = dst.len < src.len ? dst.len : src.len;
    if (n > 0) memmove(dst.data, src.data, (size_t)n);
    return n;
}

/* gocpy_sys_errkind classifies an error code: 1 not exist, 2 exist,
 * 3 permission, 4 timeout, 5 is a directory, 6 not a directory,
 * 7 connection refused, 8 connection reset or broken pipe, 0 other. */
int64_t gocpy_sys_errkind(int64_t code) {
    switch (code) {
    case ENOENT: return 1;
    case EEXIST: case ENOTEMPTY: return 2;
    case EACCES: case EPERM: return 3;
    case GOCPY_ETIMEOUT: case ETIMEDOUT: return 4;
    case EISDIR: return 5;
    case ENOTDIR: return 6;
    case ECONNREFUSED: return 7;
    case ECONNRESET: case EPIPE: return 8;
    }
    return 0;
}

/* gocpy_unix_nano is time.UnixNano: wall-clock nanoseconds since 1970. */
int64_t gocpy_unix_nano(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}
