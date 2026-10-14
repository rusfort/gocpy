# Modules and dependencies

Imports in `.gocpy` files work as in Go, and dependency versions are pinned in
`gocpy.yml`, which plays the role of `go.mod`. A program can depend on two
kinds of modules: Gocpy modules and Go modules. Go files can also be placed
directly next to Gocpy code.

- [gocpy.yml](#gocpyyml)
- [Imports](#imports)
- [Gocpy dependencies](#gocpy-dependencies)
- [Go dependencies (the Go bridge)](#go-dependencies-the-go-bridge)
- [Go files next to Gocpy code](#go-files-next-to-gocpy-code)
- [Commands](#commands)
- [Publishing a Gocpy library](#publishing-a-gocpy-library)

## gocpy.yml

```yaml
module: github.com/rusfort/gocpy-first-app
version: 0.1.0
build:
  opt: 2
  gc: boehm
require:
  gocpy:
    - github.com/john/gocpy-repo@v0.1.0
  go:
    - github.com/bob/go-repo@v0.2.0
replace:                                   # optional, applies to the main module only
  github.com/john/gocpy-repo: ../gocpy-repo    # a local copy
  github.com/bob/go-repo: ../go-repo           # a local copy of a Go module
```

- `module` is the module path. It is also the prefix of the import paths of
  its packages: `github.com/rusfort/gocpy-first-app/util` is the `util/`
  directory.
- `require.gocpy` lists modules with Gocpy sources. Each must have a
  `gocpy.yml` at its root.
- `require.go` lists ordinary Go modules. They are linked through the Go
  bridge described below.
- Versions use semver with a `v` prefix: `v0.1.0`, `v1.2.3-rc.1`.
- A module cannot be listed twice or in both sections.
- The flat form from 0.1 (`require: {path: version}`) is still read as
  `require.gocpy`.
- `replace` substitutes a module with a local directory. For Go modules it can
  also name `another/path@version`.

## Imports

```gocpy
import (
    "std/fmt"                                       // the standard library
    "github.com/rusfort/gocpy-first-app/util"       // a package of your module
    "github.com/john/gocpy-repo/geo"                // a package of a Gocpy dependency
    gr "github.com/bob/go-repo"                     // a Go package, with an alias
    "./mathx.go"                                    // Go files of this directory
)
```

Rules:

- A package is a directory of `.gocpy` files, one package per directory. A
  `main` package cannot be imported.
- The default import name is the package name declared in its sources, not the
  last element of the path.
- Only capitalized names are visible from other packages. This applies to
  fields and methods too: accessing `p.tag` from another package is an error.
- Import name conflicts are errors: two packages named `geo` in one file need
  an alias. Shadowing an import name with a local variable is forbidden as
  well.
- An unused import is an error.
- Import cycles and relative paths such as `./util` are rejected. The only
  exception is a Go file of the package's own directory:
  `import "./mathx.go"` (see below).

Everything works across packages: types, methods, generics (instantiated at the
point of use), interfaces, exceptions (`catch e: geo.ParseError`), constants
and global variables. Packages are initialized in dependency order.

## Gocpy dependencies

- The module `github.com/john/gocpy-repo@v0.1.0` is fetched with
  `git clone --depth 1 --branch v0.1.0`.
- A module in a subdirectory of a repository (`github.com/john/mono/lib`) is
  fetched by the tag `lib/v0.1.0`, as in Go.
- The cache lives in `$GOCPY_MODCACHE`, by default `~/.cache/gocpy/mod`
  (`~/Library/Caches/gocpy/mod` on macOS).
- Private repositories work through the usual git configuration: SSH keys, a
  credential helper, or `url.<base>.insteadOf`.
- Versions are selected with minimal version selection (MVS), as in Go: if
  `other` requires `gocpy-repo@v0.2.0` and the main module requires `v0.1.0`,
  `v0.2.0` is selected.
- Native C code of dependencies (`native.c`, `native.include` and
  `native.link` in their `gocpy.yml`) is built together with the program.

### gocpy.sum

- On the first download, the module hash (`h1:`, the same algorithm as in
  `go.sum`) is recorded in `gocpy.sum` next to `gocpy.yml`.
- Later builds verify the hash. If the contents of a tag change, the build
  stops with `SECURITY ERROR: checksum mismatch`.
- The same file stores the lines for Go modules (in `go.sum` format), which
  the Go toolchain verifies.
- Commit `gocpy.sum` together with `gocpy.yml`.

## Go dependencies (the Go bridge)

```gocpy
import (
    "std/fmt"
    "github.com/google/uuid"
)

func main() {
    fmt.Println(uuid.NewString())
    if err := uuid.Validate("nope"); err != nil {
        fmt.Println(err)
    }
}
```

The Go toolchain 1.21 or newer is required. Its path can be set with
`GOCPY_GO`, and `gocpy doctor` checks the environment. A build works like
this:

1. A helper Go module is created for the imported Go packages, with the
   `require` and `replace` entries from `gocpy.yml`.
2. The API of these packages is read with the user's Go toolchain.
3. A table of the Go functions, methods, field accessors and variables is
   generated, together with a Gocpy stub package per Go package.
4. Everything is built with `go build -buildmode=c-archive` and linked into
   the program.
5. The result is cached in `$GOCPY_CACHE/gobridge`, by default
   `~/.cache/gocpy/gobridge`.

Packages of the Go standard library (`net/http`, `encoding/json`, ...) need
no `require` entry: they are imported by their Go path. A Gocpy `std/`
package with the same name needs an alias (`gofmt "fmt"`).

Supported Go API:

- functions, including variadic ones, methods, constants and package
  variables (read once at startup);
- `bool`, integer and float types, `string` and `[]byte`, copied;
- named basic types such as `time.Duration`: aliases of the Gocpy basic type,
  so `time.Duration(n)` and `var d time.Duration` work (their methods are not
  available);
- `any`: Gocpy values are copied into Go values (structs become Go structs
  with the exported fields, slices, maps and pointers become Go slices, maps
  and pointers; a field that refers back to its struct type becomes `any`).
  Go values come back as Gocpy values: basic values, `[]T` and `map[K]V` for
  common element types, otherwise `[]any` and `map[string]any`, and Gocpy
  types for Go types;
- unnamed slices, arrays and maps with basic keys, copied;
- `error`: Go errors become Gocpy errors. `errors.Is` uses Go's `errors.Is`,
  and errors from package variables (`io.EOF`, `os.ErrNotExist`) can be
  compared with `==`;
- other named types: a Gocpy type with the same name, the exported methods
  and the exported fields (also promoted ones). The Gocpy value refers to the
  Go value; copies of it refer to the same Go value. Struct values are kept
  as pointers to a copy, so methods with pointer receivers work;
- Go interfaces: Gocpy interfaces with the same methods. Any Go value that
  implements a Go interface can be passed where the interface is expected
  (`io.ReadAll(strings.NewReader("x"))`). Gocpy values cannot implement Go
  interfaces;
- types of other Go packages used by the imported ones (`*url.URL` in
  `http.Request.URL`) are available with their methods and fields.

Fields of Go values:

```gocpy
u := &url.URL{Scheme: "https", Host: "example.com"}   // a new Go value
u.Path = "/a"                                          // field assignment
u.Path += "/b"
c := http.Cookie{Name: "n", Value: "v"}
c.MaxAge++
b := new(strings.Builder)
```

A field can be read, assigned alone (`=`, `op=`, `++`, `--`) or set in a
composite literal; `&x.F` and assignments with several left-hand sides are
errors. Fields of struct types alias the Go value: `req.URL.Path = "/x"`
changes the request. Fields of slice and map types are copies.

How calls work:

- Arguments and results are encoded into one byte buffer (the format is
  documented in `std/gobridge`). Go values are passed by handle; a handle is
  released when the Gocpy values that refer to it are collected (with
  `gc: boehm`).
- Go calls run on a separate thread pool unknown to the Boehm GC, while the
  calling goroutine waits in `GC_do_blocking`. A call costs a few
  microseconds.
- A panic in Go stops the program with exit code 2:
  `runtime error: panic in Go code: ...`.

Not supported:

- callbacks: Go functions with func or channel parameters or results;
- generic Go functions and types, unnamed struct types, pointers to basic
  types, `complex` and `unsafe.Pointer`;
- Gocpy values that implement Go interfaces;
- changes made by Go to a copied Gocpy value: out-parameters such as
  `json.Unmarshal(data, &v)` do not change `v`.

When code uses such a name, the compiler explains why, for example:
`strings.Map: parameter 1: func values (callbacks) are not supported`.

## Go files next to Gocpy code

Since 0.4.0, a Go file placed in the directory of a Gocpy package can be
imported by its file name:

```
app/
├── gocpy.yml
├── main.gocpy
├── mathx.go        // package mathx
└── helper.go       // package mathx
```

```gocpy
import (
    "std/fmt"
    "./mathx.go"            // or with an alias: m "./mathx.go"
)

func main() {
    fmt.Println(mathx.Add(2, 3), mathx.Scale)
}
```

Rules:

- The path must be `./<file>.go` in the directory of the importing package.
  Paths such as `../x.go` and `./sub/x.go` are rejected.
- All `.go` files of the directory except `_test.go` form one Go package, as
  in Go. Unexported functions in `helper.go` are available to the code in
  `mathx.go`, while Gocpy sees only exported names.
- The import name is the name from the `package` clause of the Go files, not
  the file name. `package main` is rejected because such a package cannot be
  imported.
- Two files of the same Go package cannot be imported in one `.gocpy` file;
  the compiler explains that they are one package.
- The Go files may import the Go standard library and modules from
  `require.go` (with `replace` applied). Importing a module that is not in
  `require.go` is an error, so versions always stay pinned in `gocpy.yml` and
  `gocpy.sum`.
- The supported API is the same as for Go dependencies, and Go 1.21+ is
  required.
- The bridge is rebuilt when the contents of the `.go` files change. The
  language server's go-to-definition and hover lead into these files.

See [examples/golocal](../examples/golocal).

## Commands

```sh
gocpy init github.com/you/app          # gocpy.yml and main.gocpy
gocpy get github.com/john/gocpy-repo@v0.1.0
gocpy get github.com/google/uuid        # without a version: @latest; the module kind is detected
gocpy get -go github.com/bob/go-repo@v0.2.0
gocpy mod download                      # download everything, update gocpy.sum
gocpy mod verify                        # check the cache against gocpy.sum
gocpy mod graph                         # the requirement graph and selected versions
```

`gocpy get` preserves comments and formatting in `gocpy.yml`. If the module is
already listed, it updates the version, and it converts the flat 0.1 form into
the `gocpy:` and `go:` sections.

## Publishing a Gocpy library

1. Create `gocpy.yml` at the repository root with
   `module: github.com/you/lib`. The path must match the repository address.
2. Put packages into directories (`geo/geo.gocpy` is imported as
   `github.com/you/lib/geo`).
3. Tag a version and push the tag: `git tag v0.1.0 && git push origin v0.1.0`.
4. Users add the library with `gocpy get github.com/you/lib@v0.1.0`.
