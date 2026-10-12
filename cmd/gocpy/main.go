// Command gocpy is the Gocpy toolchain driver.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rusfort/gocpy/internal/ast"
	"github.com/rusfort/gocpy/internal/build"
	"github.com/rusfort/gocpy/internal/lexer"
	"github.com/rusfort/gocpy/internal/lsp"
	"github.com/rusfort/gocpy/internal/parser"
)

// version is set at release time: -ldflags "-X main.version=1.0.0"
var version = "0.4.0"

var usage = `gocpy — Gocpy toolchain ` + version + `

Usage:
  gocpy tokens <file.gocpy>      print tokens
  gocpy parse  <file.gocpy>      print AST
  gocpy check  <dir|file>        parse + semantic checks + type checking
  gocpy build  [-o out] [-emit-c dir] [-v] <dir|file>
                                 compile to a native binary via C
  gocpy run    [-emit-c dir] [-v] <dir|file> [-- args...]
                                 build and run
  gocpy init   <module>          create gocpy.yml (and main.gocpy) in the current directory
  gocpy get    [-go|-gocpy] <module>[@version|@latest] ...
                                 add or update a requirement in gocpy.yml
  gocpy mod    download          download all requirements, update gocpy.sum
  gocpy mod    verify            check downloaded modules against gocpy.sum
  gocpy mod    graph             print the requirement graph and selected versions
  gocpy lsp                      run the language server (stdio), used by editors
  gocpy doctor                   check the C compiler, libgc and the Go toolchain
  gocpy version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "tokens":
		err = cmdTokens(args)
	case "parse":
		err = cmdParse(args)
	case "check":
		err = cmdCheck(args)
	case "build":
		err = cmdBuild(args, false)
	case "run":
		err = cmdBuild(args, true)
	case "init":
		err = cmdInit(args)
	case "get":
		err = cmdGet(args)
	case "mod":
		err = cmdMod(args)
	case "test", "fmt":
		err = fmt.Errorf("gocpy %s: not implemented yet", cmd)
	case "lsp":
		err = lsp.Serve(os.Stdin, os.Stdout, version)
	case "doctor":
		if err := build.Doctor(func(f string, a ...any) { fmt.Printf(f, a...) }); err != nil {
			fmt.Fprintln(os.Stderr, "doctor:", err)
			os.Exit(1)
		}
		fmt.Println("ok: gocpy", version, "is ready")
	case "version":
		fmt.Println("gocpy", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func oneFile(args []string) (string, []byte, error) {
	if len(args) != 1 {
		return "", nil, fmt.Errorf("expected exactly one file")
	}
	src, err := os.ReadFile(args[0])
	return args[0], src, err
}

func cmdTokens(args []string) error {
	name, src, err := oneFile(args)
	if err != nil {
		return err
	}
	toks, errs := lexer.Tokenize(name, src)
	for _, t := range toks {
		fmt.Println(t)
	}
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, e)
		}
		return fmt.Errorf("%d lexical error(s)", len(errs))
	}
	return nil
}

func cmdParse(args []string) error {
	name, src, err := oneFile(args)
	if err != nil {
		return err
	}
	f, err := parser.ParseFile(name, src)
	if err != nil {
		return err
	}
	ast.Fprint(os.Stdout, f)
	return nil
}

func cmdCheck(args []string) error {
	target := "."
	if len(args) > 0 {
		target = args[0]
	}
	prog, _, _, warnings, err := build.Frontend(target, &build.Options{Log: os.Stderr})
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, w)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return fmt.Errorf("check failed")
	}
	fmt.Printf("ok  %s (%d files, %d packages)\n", target, len(prog.Files), len(prog.Units))
	return nil
}

func cmdBuild(args []string, runIt bool) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	out := fs.String("o", "", "output binary")
	emit := fs.String("emit-c", "", "keep generated C code in this directory")
	verbose := fs.Bool("v", false, "print C compiler commands")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	target := "."
	var progArgs []string
	if len(rest) > 0 {
		target = rest[0]
		progArgs = rest[1:]
		if len(progArgs) > 0 && progArgs[0] == "--" {
			progArgs = progArgs[1:]
		}
	}
	opts := build.Options{Out: *out, EmitC: *emit, Verbose: *verbose, Log: os.Stderr}
	var tmpDir string
	if runIt && opts.Out == "" {
		d, err := os.MkdirTemp("", "gocpy-run-")
		if err != nil {
			return err
		}
		tmpDir = d
		defer os.RemoveAll(d)
		opts.Out = filepath.Join(d, "prog")
	}
	res, err := build.Build(target, opts)
	if err != nil {
		return err
	}
	if !runIt {
		return nil
	}
	_ = tmpDir
	cmd := exec.Command(res.Binary, progArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}
