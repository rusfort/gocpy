package mod

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SumFile is gocpy.sum: one line per module version, "path version h1:hash".
// Gocpy and Go modules share the file; Go lines use go.sum syntax
// (including "path version/go.mod h1:..." entries) and are passed to the Go
// toolchain unchanged.
type SumFile struct {
	path  string
	lines map[string]string // "path version" -> hash
	dirty bool
}

// ReadSum loads root/gocpy.sum (a missing file is empty).
func ReadSum(root string) (*SumFile, error) {
	s := &SumFile{path: filepath.Join(root, "gocpy.sum"), lines: map[string]string{}}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for i, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 || !strings.HasPrefix(f[2], "h1:") {
			return nil, fmt.Errorf("%s:%d: malformed line", s.path, i+1)
		}
		s.lines[f[0]+" "+f[1]] = f[2]
	}
	return s, nil
}

// Check verifies (or records) the hash of path@version.
func (s *SumFile) Check(path, version, hash string) error {
	k := path + " " + version
	if want, ok := s.lines[k]; ok {
		if want != hash {
			return fmt.Errorf("SECURITY ERROR: checksum mismatch for %s@%s\n\tdownloaded: %s\n\tgocpy.sum:  %s\n"+
				"the module content changed since it was first recorded; if this is expected, remove the line from gocpy.sum",
				path, version, hash, want)
		}
		return nil
	}
	s.lines[k] = hash
	s.dirty = true
	return nil
}

// Text returns the file contents (sorted).
func (s *SumFile) Text() string {
	keys := sortedKeys(s.lines)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", k, s.lines[k])
	}
	return b.String()
}

// Merge adds lines in go.sum syntax (used after the Go toolchain verified them).
func (s *SumFile) Merge(text string) {
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		k := f[0] + " " + f[1]
		if s.lines[k] != f[2] {
			s.lines[k] = f[2]
			s.dirty = true
		}
	}
}

// Drop removes lines whose module is not in keep (used by mod tidy).
func (s *SumFile) Drop(keep func(path string) bool) {
	for k := range s.lines {
		p, _, _ := strings.Cut(k, " ")
		if !keep(p) {
			delete(s.lines, k)
			s.dirty = true
		}
	}
}

// Write saves the file if it changed.
func (s *SumFile) Write() error {
	if !s.dirty {
		return nil
	}
	if len(s.lines) == 0 {
		os.Remove(s.path)
		return nil
	}
	s.dirty = false
	return os.WriteFile(s.path, []byte(s.Text()), 0o644)
}

// Entries lists "path version hash" lines for path (sorted).
func (s *SumFile) Entries(path string) []string {
	var out []string
	for k, h := range s.lines {
		if p, _, _ := strings.Cut(k, " "); p == path {
			out = append(out, k+" "+h)
		}
	}
	sort.Strings(out)
	return out
}
