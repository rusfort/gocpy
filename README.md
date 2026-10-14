<p align="center">
  <img src="assets/brand/gocpy-icon.svg" width="140" alt="Gocpy logo">
</p>

<h1 align="center">Gocpy (Go-C-Py)</h1>

<p align="center">
  The simplicity of Go, the control of C, the brevity of Python.<br>
  A statically typed language that compiles to native code through C.
</p>

<p align="center">
  <a href="https://github.com/rusfort/gocpy/actions/workflows/ci.yml"><img src="https://github.com/rusfort/gocpy/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/rusfort/gocpy/releases/latest"><img src="https://img.shields.io/github/v/release/rusfort/gocpy" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT license"></a>
  <a href="https://pkg.go.dev/github.com/rusfort/gocpy"><img src="https://img.shields.io/static/v1?label=godev&message=reference&color=00add8" alt="godev"></a>
  <a href="https://t.me/rusfort"><img src="https://img.shields.io/static/v1?label=telegram&message=rusfort&color=00add8" alt="Telegram"></a>
</p>

<p align="center">
  <a href="https://rusfort.github.io/gocpy/">Website</a> ·
  <a href="docs/grammar.ebnf">Grammar</a> ·
  <a href="docs/MODULES.md">Modules</a> ·
  <a href="editors/vscode">VS Code extension</a> ·
  <a href="https://github.com/rusfort/gocpy/releases">Releases</a>
</p>

---

## Introduction

Gocpy is a general-purpose, statically typed programming language. It keeps
Go's readable syntax and Go's model of packages, interfaces, generics and
goroutines. From C it takes predictable native code and explicit, opt-in
manual memory management. From Python it takes concise expressions: f-strings,
`for x in xs` loops and list comprehensions.

The design goals are:

1. **Readable by default.** Braces and no semicolons, one obvious way to write
   a loop, no hidden control flow. Code reads like Go and is about as short as
   Python.
2. **Safe by default, unsafe on request.** Memory is garbage collected, slices
   and indexes are bounds-checked, and runtime faults stop the program with a
   clear message. Raw memory (`malloc`/`free`), pointer arithmetic and
   pointer casts exist only inside an explicit `unsafe { }` block.
3. **Errors are visible in signatures.** Errors are values, as in Go
   (`?` returns early). Exceptions exist too, but a function that may throw
   must say so with `@throws`, and the compiler checks this exactly.
4. **Strictness that prevents bugs.** Shadowing is forbidden everywhere,
   unused variables and imports are errors, and overloading must be declared
   with `@overload`. Import name conflicts are errors too.
5. **Native speed, simple interop.** The compiler emits portable C11, so
   programs are ordinary native binaries. C functions are called through
   `@extern`. Go packages, both module dependencies and Go files placed next
   to the Gocpy code, are linked through a built-in bridge.
6. **One tool.** `gocpy` builds, runs, checks, manages dependencies
   (`gocpy.yml`, similar to `go.mod`) and serves the editor through the
   Language Server Protocol.

The compiler is written in Go. Its backend generates C, the runtime is written
in C, and garbage collection uses the Boehm GC.

```gocpy
package main

import "std/fmt"

type User struct {
    ID   int
    Name string
    Tags []string
}

func (u *User) Greeting() string {
    return f"Hello, {u.Name}! You have {len(u.Tags)} tags"
}

func main() {
    users := []User{{1, "Ann", nil}, {2, "Bob", []string{"admin"}}}
    names := [u.Name for u in users if u.ID > 1]
    for i, u in users {
        fmt.Println(i, u.Greeting())
    }
    fmt.Println(names)
}
```

## Contents

- [Installation](#installation)
- [Quick start](#quick-start)
- [Language tour](#language-tour)
  - [Source files and packages](#source-files-and-packages)
  - [Types, functions and methods](#types-functions-and-methods)
  - [Python-style expressions](#python-style-expressions)
  - [Errors and exceptions](#errors-and-exceptions)
  - [Strictness rules](#strictness-rules)
  - [Interfaces and generics](#interfaces-and-generics)
  - [Closures, defer and maps](#closures-defer-and-maps)
  - [Goroutines and channels](#goroutines-and-channels)
  - [Memory: GC and unsafe blocks](#memory-gc-and-unsafe-blocks)
  - [Annotations](#annotations)
- [Modules and dependencies](#modules-and-dependencies)
- [Interoperability](#interoperability)
  - [Calling C](#calling-c)
  - [Using Go modules](#using-go-modules)
  - [Go files next to Gocpy code](#go-files-next-to-gocpy-code)
- [Standard library](#standard-library)
- [Command-line tool](#command-line-tool)
- [Editor support](#editor-support)
- [How the compiler works](#how-the-compiler-works)
- [Current limitations](#current-limitations)
- [Repository layout](#repository-layout)
- [License](#license)

## Installation

Prebuilt binaries for Linux and macOS (amd64 and arm64) are published on
[GitHub Releases](https://github.com/rusfort/gocpy/releases).

| Method | Command |
|---|---|
| Homebrew (macOS, Linux) | `brew install --cask rusfort/tap/gocpy` |
| Install script | `curl -fsSL https://raw.githubusercontent.com/rusfort/gocpy/HEAD/install.sh \| sh` |
| Linux packages | `.deb`, `.rpm` and `.apk` from the release page |
| From source (Go 1.21+) | `go install github.com/rusfort/gocpy/cmd/gocpy@latest` |

Requirements:

- **Building programs:** a C compiler (gcc or clang) and the Boehm GC (`libgc`).
- **Gocpy dependencies:** `git`.
- **Go dependencies and Go files:** the Go toolchain 1.21 or newer.

`gocpy doctor` checks the environment and explains what is missing.

## Quick start

```sh
mkdir hello && cd hello
gocpy init example.com/hello     # creates gocpy.yml and main.gocpy
gocpy run .                      # build and run
gocpy build -o hello .           # produce a native binary
```

A single file works without `gocpy.yml`: `gocpy run main.gocpy`.

## Language tour

### Source files and packages

- Source files use the `.gocpy` extension. There are no header files.
- A **directory is a compilation unit**: all `.gocpy` files of a directory form
  one package, as in Go.
- `gocpy.yml` describes a module: its path, build settings and dependencies.
  An optional `<file>.yml` next to `<file>.gocpy` overrides settings for that
  file only (`opt`, extra C `flags`).
- Blocks use braces; semicolons are inserted by the lexer, as in Go.
- Packages are imported by path: `std/...` for the standard library, the module
  path for your own packages, and module paths from `gocpy.yml` for
  dependencies.

```yaml
# gocpy.yml
module: github.com/you/app
version: 0.1.0
build:
  opt: 2        # C optimization level
  gc: boehm     # boehm | none
```

### Types, functions and methods

- Numeric types are the same as in Go: `int`, `int8` … `int64`, `uint` …
  `uint64`, `uintptr`, `float32`, `float64`, `byte`, `rune`. The other basic
  types are `bool`, `string` and `any`.
- Composite types: arrays, slices, maps, structs, pointers, channels and
  function types.
- Constants have arbitrary precision and support `iota`.
- Functions return multiple results. Methods have value or pointer receivers.
  Method values (`x.M`) capture the receiver.
- Operators can be overloaded for your types:

```gocpy
type Money struct {
    cents int64
}

@overload
func (a Money) +(b Money) Money {
    return Money{cents: a.cents + b.cents}
}
```

### Python-style expressions

```gocpy
name := "Ann"
fmt.Println(f"Hello, {name}! {3.14159:.2f} {42:>6}")   // f-strings with formats
squares := [x * x for x in xs if x > 0]                 // list comprehensions
for i, x in xs {                                        // for … in
    fmt.Println(i, x)
}
for v in ch {                                           // works for channels too
    fmt.Println(v)
}
```

`in` is a reserved keyword.

### Errors and exceptions

Gocpy has both models, and each is visible to the reader.

**Errors are values.** A function returns `error` as its last result, and the
`?` operator returns it early:

```gocpy
func load(path string) (Config, error) {
    data := read(path)?          // returns the error to the caller
    return parse(data)
}
```

**Exceptions are checked.** Any value of a named type can be thrown. A function
that may let an exception escape must be annotated with `@throws`, and the
compiler verifies this precisely (overloads and methods included):

```gocpy
type ParseError struct {
    msg string
}

@throws
func parse(s string) int {
    if s == "" {
        throw ParseError{msg: "empty string"}
    }
    return len(s)
}

func main() {
    try {
        fmt.Println(parse(""))
    } catch e: ParseError {        // catch by type
        fmt.Println("parse error:", e.msg)
    } catch e {                    // catch everything; e has type Exception
        fmt.Println(e.Message())
    } finally {
        fmt.Println("done")
    }
}
```

The rules:

- `throw Exception("message")` throws the built-in Java-style exception type.
  `Exception` provides `Message()`, `TypeName()` and `String()`.
- `catch e: error` catches every exception whose type implements `error`.
  `throw err` throws an error value.
- `throw e` inside `catch` rethrows. `finally` runs on every exit path: normal
  completion, an exception, `return`, `break` or `continue`.
- `main` and goroutine entry points cannot be `@throws`. Calling a `@throws`
  function from a package initializer is an error. `@throws` on a function
  that cannot throw is also an error.
- Exceptions are implemented without `setjmp`/`longjmp`: a throw jumps to the
  local handler, and a call to a `@throws` function is followed by a single
  pending-exception check.

Runtime faults are not exceptions. An index out of range, a nil map write, a
send on a closed channel or a deadlock stop the program with a message and
exit code 2, as Go panics do.

### Strictness rules

- **No shadowing at all**, not even of predeclared names such as `len` or
  `int`. Redeclaring a name with `:=` in the same scope is an error.
- **Unused local variables and imports are errors.** A deliberate exception is
  written on the same line: `x := f() //gocpy:ignore:unused`.
- **Overloading is explicit:** every variant of an overloaded function, method
  or operator carries `@overload`.
- **Import conflicts are errors:** two packages with the same name in one file
  need an alias. An import name may not collide with a package-level name.

### Interfaces and generics

- Interfaces are satisfied structurally, as in Go. Embedding, `any`, `error`,
  type assertions `x.(T)` (also with `ok`) and type switches are supported.
- Generic functions and generic types with methods:

```gocpy
type Number interface {
    ~int | ~float64
}

func Max[T Number](a T, b T) T {
    if a > b {
        return a
    }
    return b
}

type Stack[T any] struct {
    items []T
}

func (s *Stack[T]) Push(x T) {
    s.items = append(s.items, x)
}
```

- Constraints can be `any`, `comparable`, interfaces with methods, and unions
  (`int | float64`, `~int`).
- Type arguments are inferred from arguments structurally (`[]T`, `map[K]V`,
  `*T`, `func(T) U`, `List[T]`). For untyped constants the wider kind wins,
  so `Max(2.5, 1)` is `float64`. Explicit instantiation also works:
  `Max[float64](1, 2)`.
- Generics are implemented by monomorphization. Errors inside a generic body
  name the instance, for example `(in instance Bad[string])`.

### Closures, defer and maps

- Function values, function literals and closures. Captured variables move to
  the heap, so a closure sees later changes.
- `defer` works with functions, methods, function values, interface methods
  and `close`. Arguments are evaluated at the `defer` statement, calls run in
  LIFO order (also during an exception), and deferred functions may change
  named results.
- `map[K]V` supports literals, `make`, `delete`, `len` and `v, ok := m[k]`.
  Keys can be structs and arrays. **Iteration follows insertion order.**

### Goroutines and channels

- `go f(args)` and `go obj.Method(args)`. Arguments are evaluated in the
  calling goroutine.
- `chan T`, buffered and unbuffered; `ch <- v`, `<-ch`, `v, ok := <-ch`,
  `close`, `len`, `cap`, `for v in ch`.
- `select` with send, receive and `default` cases.
- `std/sync` provides `WaitGroup` and `Mutex`, whose zero values are ready to
  use. `std/time` provides `Duration`, `Sleep`, `Now` and `Since`.
- Runtime errors match Go: `send on closed channel`, `close of closed channel`,
  `all goroutines are asleep - deadlock!`.
- In this version a goroutine is a pthread registered with the GC. The runtime
  is tested with ThreadSanitizer and AddressSanitizer.

### Memory: GC and unsafe blocks

Ordinary code allocates on the garbage-collected heap. Manual memory, pointer
arithmetic and pointer casts are available only inside `unsafe { }`. Outside
the block these names do not exist (`malloc is only available inside an
unsafe block`).

```gocpy
unsafe {
    buf := malloc(16)            // *byte
    defer free(buf)
    memset(buf, 0, 16)
    p := (*int32)(buf)
    p[0] = 42                    // no bounds check
    q := p + 1                   // pointer arithmetic in elements
    q[0] = 7
    xs := slice(p, 4)            // []int32 over raw memory
    fmt.Println(xs)              // [42 7 0 0]
}
```

- **Memory:** `malloc`, `calloc` (zeroed), `realloc`, `free` (`free(nil)` is
  allowed), `memcpy` (overlap-safe) and `memset`.
- **Pointers:** `p + n`, `p - n`, `p++`, `p - q`, comparisons and `p[i]`.
- **Casts:** `*T ↔ *U` and `*T ↔ uintptr`. A cast to `*T` is allowed only if
  `T` contains no GC pointers, which enforces the rule "never store GC
  pointers in malloc memory".
- `@be_to_le(x)` and `@le_to_be(x)` swap byte order of integers, floats,
  arrays and structs, `@packed` ones included. These intrinsics also work
  outside `unsafe`.
- The runtime exits with code 2 on out-of-memory, negative sizes, and
  `free`/`realloc` of GC memory. Double free and use-after-free are undefined
  behavior, as in C; use AddressSanitizer to catch them.

### Annotations

| Annotation | Meaning |
|---|---|
| `@throws` | The function may let an exception escape. |
| `@overload` | Marks every variant of an overloaded function or operator. |
| `@inline`, `@noinline`, `@hot`, `@cold` | Code generation hints passed to the C compiler. |
| `@packed`, `@align(16)` | Struct layout. |
| `@extern("c_name")` | Declares a function implemented in C. |
| `@export("c_name")` | Exports a Gocpy function to C under a fixed name. |
| `@be_to_le(x)`, `@le_to_be(x)` | Byte-order intrinsics. |

Directives in comments start with `//gocpy:`, for example
`//gocpy:ignore:unused`.

## Modules and dependencies

Imports work as in Go, and versions are pinned in `gocpy.yml`. Gocpy and Go
dependencies are listed in separate sections:

```yaml
module: github.com/rusfort/gocpy-first-app
version: 0.1.0
require:
  gocpy:
    - github.com/john/gocpy-repo@v0.1.0
  go:
    - github.com/google/uuid@v1.6.0
replace:                                   # optional, main module only
  github.com/john/gocpy-repo: ../gocpy-repo
```

- Packages of your own module are imported by module path:
  `github.com/rusfort/gocpy-first-app/util` is the `util/` directory.
- Gocpy modules are fetched with git by tag. Versions are selected with
  minimal version selection (MVS), and hashes are recorded in `gocpy.sum`,
  which should be committed.
- Types, methods, generics, interfaces, exceptions, constants and globals all
  work across packages. Only capitalized names, fields and methods are visible
  from other packages.
- Packages are initialized in dependency order. Import cycles are errors.

Commands: `gocpy init`, `gocpy get`, `gocpy mod download | verify | graph`.
The full reference is in [docs/MODULES.md](docs/MODULES.md), and
a working example is in [examples/modules](examples/modules).

## Interoperability

### Calling C

Declare a function with `@extern` and list the C sources in `gocpy.yml`:

```yaml
native:
  c: ["native/*.c"]
  include: ["native"]
  link: ["-lm"]
```

```gocpy
@extern("c_mix")
func mix(a int, b int) int

func main() {
    fmt.Println(mix(2, 5))
}
```

Native code of dependencies is built together with the program.

### Using Go modules

Packages of the Go standard library and Go packages from `require.go` are
imported like any other package. A Go standard library package is imported by
its Go path; when a Gocpy `std/` package has the same name, one of them needs
an alias:

```gocpy
import (
    "std/fmt"
    gofmt "fmt"
    "net/http"
    "io"
    "github.com/google/uuid"
)

func main() {
    fmt.Println(uuid.NewString(), gofmt.Sprint("any", 1, []int{2, 3}))
    resp, err := http.Get("https://example.com")
    if err != nil {
        fmt.Println(err)
        return
    }
    body, _ := io.ReadAll(resp.Body)
    resp.Body.Close()
    fmt.Println(resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
}
```

The bridge creates a small Go module with your `require` and `replace`
entries, reads the API of the imported packages and generates a table of Go
calls plus Gocpy stubs. It builds everything with
`go build -buildmode=c-archive`, links the archive into the program and
caches the result.

Go values and types in Gocpy:

| Go | Gocpy |
|---|---|
| `bool`, integers, floats, `string`, `[]byte` | the same types, copied |
| named basic types (`time.Duration`, `http.Dir`) | aliases of the basic type (`type Duration = int64`) |
| `any` / `interface{}` | `any`: Gocpy values (structs, slices, maps, pointers) are copied into Go values; Go values come back as Gocpy values |
| unnamed slices, arrays, maps (basic keys) | the same Gocpy types, copied |
| `error` | `error`; `errors.Is` and `==` work with Go errors such as `io.EOF` |
| other named types (`*http.Response`, `url.URL`, `bytes.Buffer`) | Gocpy types with the same names, methods and fields; they refer to the Go value |
| interfaces (`io.Reader`) | Gocpy interfaces implemented by Go values |

Fields of Go values are read and written like fields: `resp.StatusCode`,
`u.Path = "/a"`, `c.MaxAge++`. Composite literals (`&url.URL{Host: "x"}`)
and `new(T)` create Go values. Variadic functions work; package variables are
read once at startup.

Limits: Go functions with func or channel parameters (callbacks), generic Go
functions and types, unnamed struct types and pointers to basic types are not
available. Gocpy values cannot implement Go interfaces, and Go does not see
later changes of a copied Gocpy value (out-parameters such as
`json.Unmarshal(data, &v)` do not work; use `json.NewDecoder` or Go types).
The address of a field of a Go value cannot be taken. Methods of named basic
types (`time.Duration.String`) are not available.

Go calls run on a thread pool unknown to the GC while the calling goroutine
waits; a call costs a few microseconds. Go values are released when the Gocpy
values that refer to them are collected. A Go panic stops the program with
exit code 2. Using an unsupported name produces an explanation, for example
`strings.Map: parameter 1: func values (callbacks) are not supported`.

### Go files next to Gocpy code

A Go file placed in the directory of a Gocpy package can be imported by its
file name:

```
app/
├── gocpy.yml
├── main.gocpy
├── textstat.go     // package textstat
└── letters.go      // package textstat (unexported helpers)
```

```gocpy
package main

import (
    "std/fmt"
    "./textstat.go"            // with an alias: ts "./textstat.go"
)

func main() {
    fmt.Println(textstat.Words("hello from go"), textstat.MaxWords)
}
```

- The path must be `./<file>.go` in the importing package's own directory.
  `../x.go` and `./sub/x.go` are rejected.
- All non-test `.go` files of the directory form one Go package, as Go
  requires. The import name is the name from their `package` clause, which
  cannot be `main`.
- The Go files may import the Go standard library and modules listed in
  `require.go`. An import of any other module is an error, so versions stay
  pinned in `gocpy.yml` and `gocpy.sum`.
- The same API as for Go modules is supported. The bridge is rebuilt
  when the Go files change. In the editor, go-to-definition and hover lead
  straight into the `.go` files.

See [examples/golocal](examples/golocal).

## Standard library

| Package | Contents |
|---|---|
| `std/fmt` | `Print`/`Println`/`Printf`, `Sprint*`, `Fprint*`, `Errorf` (with `%w`), `Scan`/`Scanln`/`Scanf`, `Sscan*`, `Fscan*`, `Stringer`; verbs `%v %d %s %q %x %X %o %b %c %U %e %f %g %t %p %T`, width, precision and flags |
| `std/errors` | `New`, `Is`, `As`, `Unwrap`, `Join` |
| `std/strings` | `Contains`, `Index`, `Split`, `Fields`, `Join`, `Replace`, `Trim*`, `Cut*`, `ToUpper`/`ToLower`, `EqualFold`, `Map`, `Count`, `Builder`, `Reader` and more |
| `std/strconv` | `Itoa`, `Atoi`, `ParseInt`/`ParseUint`/`ParseFloat`/`ParseBool`, `Format*`, `Quote`, `Unquote`, `NumError` |
| `std/io` | Go's `io` interfaces and helpers (`Reader`, `Writer`, `Copy`, `ReadAll`, `ReadFull`, `MultiReader`, ...), files (`Open`, `Create`, `OpenFile`, `ReadFile`, `WriteFile`, `Stat`, `ReadDir`, `Mkdir*`, `Remove*`, `Rename`, `CreateTemp`, `Stdin`/`Stdout`/`Stderr`), the `bufio` part (`NewReader`, `NewWriter`, `NewScanner`) and `Buffer` |
| `std/http` | HTTP/1.1 client (`Get`, `Head`, `Post`, `PostForm`, `Client.Do`, redirects, timeouts) and server (`ServeMux` with Go 1.22 patterns such as `"GET /users/{id}"`, `ListenAndServe`, `Server`, `FileServer`, `StripPrefix`, `Redirect`, keep-alive), `URL`, `Values`, `Header` |
| `std/udp` | `Listen`, `Dial`, `Conn` (`ReadFrom`, `WriteTo`, `Read`, `Write`, timeouts), `ResolveAddr` |
| `std/sync` | `WaitGroup` (`Add`, `Done`, `Wait`), `Mutex` (`Lock`, `Unlock`) |
| `std/time` | `Duration`, `Nanosecond` … `Hour`, `Sleep`, `Now`, `Since`, `UnixNano`, `Milliseconds()`, `Seconds()` |

`strings`, `strconv`, `io`, `http` and `udp` are written in Gocpy itself
(`stdlib/`) and ship inside the compiler, so go-to-definition in the editor
opens their sources. Go modules fill the remaining gaps through the bridge.

```gocpy
package main

import (
    "std/fmt"
    "std/http"
    "std/io"
)

func main() {
    http.HandleFunc("GET /hello/{name}", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintf(w, "hello, %s\n", r.PathValue("name"))
    })
    http.Handle("/files/", http.StripPrefix("/files", http.FileServer("./www")))
    io.WriteFile("started.txt", []byte("ok\n"), 0o644)
    fmt.Println(http.ListenAndServe(":8080", nil))
}
```

## Command-line tool

```
gocpy build  [-o out] [-emit-c dir] [-v] <dir|file>   compile to a native binary
gocpy run    [-emit-c dir] [-v] <dir|file> [-- args]  build and run
gocpy check  <dir|file>                               parse and type-check only
gocpy init   <module>                                 create gocpy.yml and main.gocpy
gocpy get    [-go|-gocpy] <module>[@version|@latest]  add or update a requirement
gocpy mod    download | verify | graph                manage dependencies
gocpy lsp                                             language server (stdio)
gocpy doctor                                          check the C compiler, libgc and Go
gocpy tokens <file> | parse <file>                    debugging: tokens and AST
gocpy version
```

`-emit-c dir` keeps the generated C code for inspection.

## Editor support

`gocpy lsp` is a Language Server Protocol server. The
[VS Code extension](editors/vscode) uses it and provides:

- semantic highlighting and the **Gocpy Dark** theme, matching the colors of
  the Gocpy IDE;
- diagnostics while typing, with all errors shown rather than only the first;
- go to definition, find references, rename and call hierarchy across
  packages. Definitions also lead into the sources of Go packages;
- hover documentation, completion of keywords, annotations, package and type
  members and import paths, and signature help with overloads.

Analysis is error-tolerant: a syntax error in one place does not break
highlighting and navigation in the rest of the file.

## How the compiler works

```
.gocpy ─▶ lexer ─▶ parser ─▶ loader ─▶ checks ─▶ type checker ─▶ C generator ─▶ cc ─▶ binary
                              │                                                  ▲
                              └── Go bridge (go build -buildmode=c-archive) ─────┘
```

| Stage | Package | Role |
|---|---|---|
| Lexer | `internal/lexer` | Tokens, automatic semicolons, f-strings |
| Parser | `internal/parser` | AST for the grammar in [docs/grammar.ebnf](docs/grammar.ebnf) |
| Loader | `internal/build` | Packages, imports, modules, name mangling, the Go bridge |
| Checks | `internal/check` | Annotations, directives, structural rules |
| Types | `internal/types` | Type checking, overloads, generics, `@throws` analysis |
| Codegen | `internal/codegen/c` | GNU C11 output |
| Runtime | `runtime/c` | Strings, slices, maps, channels, exceptions, formatting, GC glue |
| Modules | `internal/mod` | `gocpy.yml`, MVS, `gocpy.sum`, git fetching |
| Editor | `internal/lsp` | Language server |

## Current limitations

- `defer fmt.Println(...)` is not supported; wrap the call in `func() { ... }()`.
- Interfaces cannot be map keys.
- The variable of a C-style `for` loop is shared between iterations.
- A generic body is checked only when instantiated. Methods cannot have their
  own type parameters, partial explicit instantiation is not supported, and
  there are no method expressions (`T.M`).
- The Go bridge does not support callbacks (func and channel parameters),
  generic Go functions and types, or Gocpy values implementing Go
  interfaces; Gocpy values passed to Go are copies.
- `std/http` has no HTTPS/TLS (use a Go module through the bridge). Server
  responses are buffered and sent when the handler returns (no streaming or
  `Flush`), client bodies are read into memory, and `Client.Timeout` applies
  to each network operation rather than to the whole request.
- `fmt`: `%+v` and `%#v` print like `%v`. `strings.ToUpper`/`ToLower` cover
  Latin, Greek and Cyrillic only. There is no `[]rune(s)` conversion and no
  `time.After`.
- No cross-compilation yet: `build.targets` other than the host are ignored
  with a warning.

## Repository layout

```
cmd/gocpy/         command-line tool
internal/          compiler, loader, modules, Go bridge, language server
runtime/c/         C runtime
stdlib/            standard library packages written in Gocpy
docs/              grammar and module reference
examples/          example programs (stage2 … stage7, modules, golocal, feed)
editors/vscode/    VS Code extension
site/              project website
packaging/         release packaging
```

## License

[MIT](LICENSE) © 2026 rusfort (Nikolai Kozakov / Box Tech)
