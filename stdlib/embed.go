// Package stdlib embeds the part of the Gocpy standard library that is
// written in Gocpy itself (std/io, std/strings, std/strconv, std/http,
// std/udp). The packages are materialized into the cache directory and
// loaded like ordinary module packages.
package stdlib

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed */*.gocpy
var files embed.FS

var (
	once sync.Once
	pkgs map[string]bool
	hash string
)

func scan() {
	pkgs = map[string]bool{}
	h := sha256.New()
	var names []string
	fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".gocpy") {
			names = append(names, p)
			pkgs["std/"+path.Dir(p)] = true
		}
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		data, _ := files.ReadFile(n)
		fmt.Fprintf(h, "%s %d\n", n, len(data))
		h.Write(data)
	}
	hash = fmt.Sprintf("%x", h.Sum(nil)[:12])
}

// Has reports whether p (for example "std/io") is a source std package.
func Has(p string) bool {
	once.Do(scan)
	return pkgs[p]
}

// Packages returns the import paths of the source std packages, sorted.
func Packages() []string {
	once.Do(scan)
	var ps []string
	for p := range pkgs {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Files returns the embedded file system (one directory per package).
func Files() fs.FS { return files }

var (
	dirMu   sync.Mutex
	dirPath string
)

// Dir writes the packages to the cache and returns the root directory;
// package std/x lives in Dir()/x. The directory is written again when it
// was removed or GOCPY_CACHE changed.
func Dir() (string, error) {
	dirMu.Lock()
	defer dirMu.Unlock()
	if dirPath != "" {
		if _, err := os.Stat(filepath.Join(dirPath, ".complete")); err == nil {
			return dirPath, nil
		}
		dirPath = ""
	}
	var dirErr error
	func() {
		once.Do(scan)
		base := os.Getenv("GOCPY_CACHE")
		if base == "" {
			d, err := os.UserCacheDir()
			if err != nil {
				d = os.TempDir()
			}
			base = filepath.Join(d, "gocpy")
		}
		root := filepath.Join(base, "std", hash)
		if _, err := os.Stat(filepath.Join(root, ".complete")); err == nil {
			dirPath = root
			return
		}
		tmp, err := os.MkdirTemp(filepath.Dir(root), "tmp-")
		if err != nil {
			if err = os.MkdirAll(filepath.Dir(root), 0o755); err == nil {
				tmp, err = os.MkdirTemp(filepath.Dir(root), "tmp-")
			}
			if err != nil {
				dirErr = err
				return
			}
		}
		err = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			dst := filepath.Join(tmp, filepath.FromSlash(p))
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			data, err := files.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0o444)
		})
		if err == nil {
			err = os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644)
		}
		if err != nil {
			os.RemoveAll(tmp)
			dirErr = err
			return
		}
		if err := os.Rename(tmp, root); err != nil {
			os.RemoveAll(tmp) // another process won the race
			if _, serr := os.Stat(filepath.Join(root, ".complete")); serr != nil {
				dirErr = err
				return
			}
		}
		dirPath = root
	}()
	return dirPath, dirErr
}
