package lsp

// Documentation for keywords, builtins and annotations (hover and completion).

type kwDoc struct {
	doc     string
	snippet string // optional snippet for completion
	label   string // snippet label
}

var keywordDocs = map[string]kwDoc{
	"package":     {doc: "Package clause. All `.gocpy` files of a directory form one package.", snippet: "package ${1:main}"},
	"import":      {doc: "Imports packages: `std/...` is the standard library, module paths from `gocpy.yml` are Gocpy packages, modules from `require.go` are Go packages, and `\"./file.go\"` is a Go file of the package directory. Import name conflicts are errors.", snippet: "import (\n\t\"${1:std/fmt}\"\n)", label: "import (…)"},
	"func":        {doc: "Declares a function, a method (`func (r T) M()`), an operator (`func (a T) +(b T) T`) or a function literal.", snippet: "func ${1:name}(${2}) ${3}{\n\t$0\n}", label: "func name() {…}"},
	"type":        {doc: "Declares a type: a struct, an interface, an alias or a generic type such as `type List[T any] struct {…}`.", snippet: "type ${1:Name} struct {\n\t$0\n}", label: "type Name struct {…}"},
	"var":         {doc: "Declares a variable. Shadowing is forbidden, and unused variables are errors.", snippet: "var ${1:name} ${2:int}"},
	"const":       {doc: "Declares a constant, evaluated at compile time. `iota` is supported.", snippet: "const ${1:Name} = ${2:0}"},
	"struct":      {doc: "Struct type: a sequence of named fields."},
	"interface":   {doc: "Interface type: a set of methods, or a generic constraint (`~int | float64`, `comparable`)."},
	"map":         {doc: "Map type `map[K]V`. Iteration follows insertion order."},
	"chan":        {doc: "Channel type `chan T` for communication between goroutines (`go`)."},
	"if":          {doc: "Conditional statement. Accepts an init statement: `if x := f(); x > 0 {…}`.", snippet: "if ${1:cond} {\n\t$0\n}", label: "if cond {…}"},
	"else":        {doc: "The `else` branch of an `if` statement.", snippet: "else {\n\t$0\n}", label: "else {…}"},
	"for":         {doc: "Loop: `for {…}`, `for cond {…}`, `for i := 0; i < n; i++ {…}`, `for x in xs {…}`, `for i, x in xs {…}`.", snippet: "for ${1:x} in ${2:xs} {\n\t$0\n}", label: "for x in xs {…}"},
	"in":          {doc: "Iterates over a collection in `for … in` loops and list comprehensions such as `[x * 2 for x in xs if x > 0]`."},
	"switch":      {doc: "Multi-way branch. A `case` does not fall through without `fallthrough`. Type switches: `switch x := v.(type)`.", snippet: "switch ${1:x} {\ncase ${2:value}:\n\t$0\ndefault:\n}", label: "switch x {…}"},
	"case":        {doc: "A branch of `switch` or `select`."},
	"default":     {doc: "The default branch of `switch` or `select`."},
	"break":       {doc: "Exits a loop, `switch` or `select`."},
	"continue":    {doc: "Continues with the next iteration of a loop."},
	"fallthrough": {doc: "Transfers control to the next `case` without checking its condition."},
	"return":      {doc: "Returns from a function."},
	"go":          {doc: "Starts a function call in a new goroutine (a pthread in this version).", snippet: "go func() {\n\t$0\n}()", label: "go func() {…}()"},
	"defer":       {doc: "Defers a call until the function returns. Deferred calls run in reverse order, also during an exception.", snippet: "defer ${1:f}()"},
	"select":      {doc: "Waits on several channel operations.", snippet: "select {\ncase ${1:v} := <-${2:ch}:\n\t$0\n}", label: "select {…}"},
	"try":         {doc: "Block with exception handlers. Functions annotated with `@throws` can be called only inside `try` or from other `@throws` functions.", snippet: "try {\n\t$1\n} catch ${2:e} {\n\t$0\n}", label: "try {…} catch e {…}"},
	"catch":       {doc: "Exception handler: `catch e {…}` catches everything (`e` has type `Exception`), `catch e: MyError {…}` catches by type.", snippet: "catch ${1:e} {\n\t$0\n}", label: "catch e {…}"},
	"finally":     {doc: "Block that always runs after `try` / `catch`, on every exit path.", snippet: "finally {\n\t$0\n}", label: "finally {…}"},
	"throw":       {doc: "Throws an exception. The enclosing function must be annotated with `@throws`, or the statement must be inside `try`.", snippet: "throw ${1:err}"},
	"unsafe":      {doc: "Unsafe block: enables `malloc`, `calloc`, `realloc`, `free`, `memcpy`, `memset`, `slice`, pointer arithmetic and pointer casts `(*T)(p)`.", snippet: "unsafe {\n\t$0\n}", label: "unsafe {…}"},
}

var builtinDocs = map[string]string{
	"len":     "`len(x) int` returns the length of a string, slice, array, map or channel.",
	"cap":     "`cap(x) int` returns the capacity of a slice, array or channel.",
	"append":  "`append(s []T, xs ...T) []T` appends elements to a slice.",
	"make":    "`make(T, args...)` creates a slice, map or channel.",
	"new":     "`new(T) *T` allocates a zero value of type T on the garbage-collected heap.",
	"sizeof":  "`sizeof(T) uintptr` returns the size of a type in bytes.",
	"close":   "`close(ch)` closes a channel.",
	"delete":  "`delete(m, key)` removes a key from a map.",
	"malloc":  "`malloc(n uintptr) *byte` allocates memory outside the GC (only in `unsafe`). Release it with `free`.",
	"calloc":  "`calloc(n, size uintptr) *byte` allocates zeroed memory outside the GC (only in `unsafe`).",
	"realloc": "`realloc(p *T, n uintptr) *T` resizes a memory block (only in `unsafe`).",
	"free":    "`free(p)` releases memory allocated by `malloc`, `calloc` or `realloc` (only in `unsafe`).",
	"memcpy":  "`memcpy(dst, src, n)` copies n bytes; overlapping regions are allowed (only in `unsafe`).",
	"memset":  "`memset(p, value, n)` fills n bytes with a value (only in `unsafe`).",
	"slice":   "`slice(p *T, n int) []T` makes a slice over raw memory (only in `unsafe`).",

	"bool": "Boolean type: `true` or `false`.", "string": "Immutable UTF-8 string.",
	"int": "Signed 64-bit integer.", "int8": "Signed 8-bit integer.", "int16": "Signed 16-bit integer.",
	"int32": "Signed 32-bit integer.", "int64": "Signed 64-bit integer.",
	"uint": "Unsigned 64-bit integer.", "uint8": "Unsigned 8-bit integer.", "uint16": "Unsigned 16-bit integer.",
	"uint32": "Unsigned 32-bit integer.", "uint64": "Unsigned 64-bit integer.", "uintptr": "Unsigned integer large enough to hold a pointer.",
	"float32": "32-bit floating-point number.", "float64": "64-bit floating-point number.",
	"byte": "Alias for `uint8`.", "rune": "Alias for `int32`: a Unicode code point.",
	"any":        "The empty interface: a value of any type.",
	"error":      "The error interface: `interface { Error() string }`.",
	"Exception":  "Built-in exception type, similar to `Exception` in Java. `throw Exception(\"message\")` throws it, and `catch e {…}` gives `e` this type. Methods: `Message()`, `TypeName()`, `String()`.",
	"comparable": "Generic constraint: types that support `==` and `!=`.",
	"thread":     "Thread handle.",
	"true":       "The boolean constant true.", "false": "The boolean constant false.",
	"nil":  "The zero value of pointers, slices, maps, channels, interfaces and functions.",
	"iota": "Counter in a `const` group: 0, 1, 2, …",
}

type annotDoc struct {
	doc     string
	snippet string
}

var annotDocs = map[string]annotDoc{
	"throws":     {doc: "The function may let an exception escape. It can be called only inside `try` or from another `@throws` function."},
	"overload":   {doc: "Overloading: required on every function, method or operator that shares a name with a different signature."},
	"inline":     {doc: "Asks the C compiler to inline the function."},
	"noinline":   {doc: "Prevents inlining of the function."},
	"hot":        {doc: "Marks a frequently called function for more aggressive optimization."},
	"cold":       {doc: "Marks a rarely called function."},
	"nogc":       {doc: "Marks a function that should not allocate on the garbage-collected heap (not enforced yet)."},
	"packed":     {doc: "Struct without padding between fields."},
	"align":      {doc: "Struct alignment: `@align(16)`.", snippet: "align(${1:16})"},
	"export":     {doc: "Exports the function to C under a fixed name: `@export(\"name\")`.", snippet: "export(\"${1:name}\")"},
	"target":     {doc: "Restricts the declaration to the given platforms: `@target(\"linux\")` (reserved, not enforced yet).", snippet: "target(\"${1:linux}\")"},
	"deprecated": {doc: "Marks a deprecated declaration with a hint for users: `@deprecated(\"use X\")`.", snippet: "deprecated(\"${1:use X}\")"},
	"extern":     {doc: "Declares a C function without a body: `@extern(\"puts\")`.", snippet: "extern(\"${1:name}\")"},
	"be_to_le":   {doc: "Intrinsic: converts byte order from big-endian to little-endian.", snippet: "be_to_le(${1:x})"},
	"le_to_be":   {doc: "Intrinsic: converts byte order from little-endian to big-endian.", snippet: "le_to_be(${1:x})"},
}

var directiveDocs = map[string]string{
	"gocpy:ignore:unused": "Suppresses the unused declaration error for the declaration on the same line.",
}
