package mod

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CacheDir is the module cache: $GOCPY_MODCACHE or <user cache>/gocpy/mod.
func CacheDir() (string, error) {
	if d := os.Getenv("GOCPY_MODCACHE"); d != "" {
		return filepath.Abs(d)
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the module cache (set GOCPY_MODCACHE): %v", err)
	}
	return filepath.Join(d, "gocpy", "mod"), nil
}

// escapePath makes a path safe on case-insensitive file systems (as Go does).
func escapePath(p string) string {
	var b strings.Builder
	for _, c := range p {
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(c + 'a' - 'A')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

var knownHosts = map[string]bool{
	"github.com": true, "gitlab.com": true, "bitbucket.org": true, "codeberg.org": true, "gitee.com": true,
}

// RepoRoot splits a module path into the repository root and a subdirectory.
func RepoRoot(path string) (repo, sub string) {
	elems := strings.Split(path, "/")
	if knownHosts[elems[0]] && len(elems) >= 3 {
		return strings.Join(elems[:3], "/"), strings.Join(elems[3:], "/")
	}
	for i, e := range elems {
		if strings.HasSuffix(e, ".git") {
			return strings.Join(elems[:i+1], "/"), strings.Join(elems[i+1:], "/")
		}
	}
	return path, ""
}

// RepoURL maps a repository root to a git URL. Tests replace it.
var RepoURL = func(repo string) string {
	if strings.HasSuffix(repo, ".git") {
		return "https://" + repo
	}
	return "https://" + repo + ".git"
}

// Download makes path@version available in the cache and returns its directory
// and h1: hash. The tag is the version, or <subdir>/<version> for modules
// that live in a subdirectory of a repository (as in Go).
func Download(path, version string, logf func(string, ...any)) (dir, hash string, err error) {
	cache, err := CacheDir()
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(cache, escapePath(path)+"@"+version)
	hashFile := dir + ".h1"
	if h, err := os.ReadFile(hashFile); err == nil {
		if _, err := os.Stat(dir); err == nil {
			return dir, strings.TrimSpace(string(h)), nil
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", "", fmt.Errorf("downloading %s@%s requires git in PATH", path, version)
	}
	repo, sub := RepoRoot(path)
	tag := version
	if sub != "" {
		tag = sub + "/" + version
	}
	if logf != nil {
		logf("gocpy: downloading %s %s\n", path, version)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(cache, ".tmp-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	clone := filepath.Join(tmp, "repo")
	cmd := exec.Command("git", "-c", "advice.detachedHead=false", "clone", "--quiet", "--depth", "1",
		"--branch", tag, RepoURL(repo), clone)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("%s@%s: git clone %s (tag %s) failed: %v\n%s", path, version, RepoURL(repo), tag,
			err, strings.TrimSpace(out.String()))
	}
	src := filepath.Join(clone, filepath.FromSlash(sub))
	cfg, err := LoadConfig(src)
	if err != nil {
		return "", "", ErrNotGocpy{fmt.Errorf("%s@%s: not a Gocpy module (no gocpy.yml at tag %s)", path, version, tag)}
	}
	if cfg.Module != path {
		return "", "", fmt.Errorf("%s@%s: gocpy.yml declares module %q, but it was required as %q", path, version, cfg.Module, path)
	}
	stage := filepath.Join(tmp, "mod")
	if err := copyTree(src, stage); err != nil {
		return "", "", err
	}
	hash, err = HashDir(stage)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", err
	}
	os.RemoveAll(dir)
	if err := os.Rename(stage, dir); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(hashFile, []byte(hash+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return dir, hash, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".github") && rel != "." {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and special files are not part of a module
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		o, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(o, in); err != nil {
			o.Close()
			return err
		}
		return o.Close()
	})
}

// HashDir computes the h1: hash of a directory tree (the dirhash algorithm Go
// uses for go.sum: sha256 over sorted "sha256  name" lines).
func HashDir(dir string) (string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(n)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%x  %s\n", sha256.Sum256(data), n)
	}
	return "h1:" + base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// ErrNotGocpy reports a repository tag without gocpy.yml.
type ErrNotGocpy struct{ Err error }

func (e ErrNotGocpy) Error() string { return e.Err.Error() }
