package mod

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersions(t *testing.T) {
	for _, v := range []string{"v0.1.0", "v1.2.3", "v1.0.0-rc.1", "v2.0.0+meta"} {
		if !IsValidVersion(v) {
			t.Errorf("%s should be valid", v)
		}
	}
	for _, v := range []string{"0.1.0", "v1.2", "v01.2.3", "v1.2.3-", "latest", ""} {
		if IsValidVersion(v) {
			t.Errorf("%s should be invalid", v)
		}
	}
	order := []string{"v0.1.0", "v0.2.0-alpha", "v0.2.0-alpha.1", "v0.2.0-beta", "v0.2.0", "v0.10.0", "v1.0.0"}
	for i := 0; i+1 < len(order); i++ {
		if CompareVersions(order[i], order[i+1]) != -1 || CompareVersions(order[i+1], order[i]) != 1 {
			t.Errorf("%s < %s expected", order[i], order[i+1])
		}
	}
	if CompareVersions("v1.0.0+a", "v1.0.0+b") != 0 {
		t.Error("build metadata must be ignored")
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig("gocpy.yml", []byte(`module: github.com/rusfort/gocpy-first-app
version: 0.1.0
build:
  opt: 2
  gc: boehm
require:
  gocpy:
    - github.com/john/gocpy-repo@v0.1.0
  go:
    - github.com/bob/go-repo@v0.2.0
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Require.Gocpy) != 1 || cfg.Require.Gocpy[0].Path != "github.com/john/gocpy-repo" ||
		cfg.Require.Gocpy[0].Version != "v0.1.0" || cfg.Require.Gocpy[0].Kind != KindGocpy {
		t.Errorf("gocpy: %+v", cfg.Require.Gocpy)
	}
	if len(cfg.Require.Go) != 1 || cfg.Require.Go[0].Path != "github.com/bob/go-repo" || cfg.Require.Go[0].Kind != KindGo {
		t.Errorf("go: %+v", cfg.Require.Go)
	}
	legacy, err := ParseConfig("gocpy.yml", []byte("module: a.b/c\nrequire:\n  github.com/x/y: v1.0.0\n"))
	if err != nil || len(legacy.Require.Gocpy) != 1 || legacy.Require.Gocpy[0].String() != "github.com/x/y@v1.0.0" {
		t.Errorf("legacy: %v %+v", err, legacy)
	}
	bad := map[string]string{
		"require:\n  gocpy:\n    - github.com/x/y\n":                          "missing @version",
		"require:\n  gocpy:\n    - github.com/x/y@1.0\n":                      "invalid version",
		"require:\n  gocpy:\n    - xy/z@v1.0.0\n":                             "must start with a domain",
		"require:\n  gocpy:\n    - a.b/c@v1.0.0\n  go:\n    - a.b/c@v1.0.0\n": "both under gocpy and go",
		"require:\n  gocpy:\n    - a.b/c@v1.0.0\n    - a.b/c@v1.1.0\n":        "listed twice",
		"require:\n  rust:\n    - a.b/c@v1.0.0\n":                             "unknown require section",
		"require:\n  gocpy:\n    - std/fmt@v1.0.0\n":                          "reserved",
	}
	for src, want := range bad {
		if _, err := ParseConfig("gocpy.yml", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error %q, got %v", src, want, err)
		}
	}
}

func TestSetRequirement(t *testing.T) {
	dir := t.TempDir()
	orig := "# my app\nmodule: a.b/app # path\nrequire:\n  github.com/old/lib: v0.1.0\n"
	os.WriteFile(filepath.Join(dir, "gocpy.yml"), []byte(orig), 0o644)
	if err := SetRequirement(dir, Requirement{"github.com/bob/go-repo", "v0.2.0", KindGo}); err != nil {
		t.Fatal(err)
	}
	if err := SetRequirement(dir, Requirement{"github.com/old/lib", "v0.3.0", KindGocpy}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "gocpy.yml"))
	s := string(data)
	for _, want := range []string{"# my app", "# path", "gocpy:\n    - github.com/old/lib@v0.3.0", "go:\n    - github.com/bob/go-repo@v0.2.0"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "v0.1.0") {
		t.Errorf("old version kept:\n%s", s)
	}
	// moving a module between sections
	if err := SetRequirement(dir, Requirement{"github.com/old/lib", "v0.3.0", KindGo}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil || len(cfg.Require.Gocpy) != 0 || len(cfg.Require.Go) != 2 {
		t.Errorf("after move: %v %+v", err, cfg.Require)
	}
}

func TestSumFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := ReadSum(dir)
	if err := s.Check("a.b/c", "v1.0.0", "h1:x"); err != nil {
		t.Fatal(err)
	}
	s.Merge("golang.org/x/text v0.14.0/go.mod h1:y\n")
	if err := s.Write(); err != nil {
		t.Fatal(err)
	}
	s2, err := ReadSum(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Check("a.b/c", "v1.0.0", "h1:x"); err != nil {
		t.Error(err)
	}
	if err := s2.Check("a.b/c", "v1.0.0", "h1:z"); err == nil || !strings.Contains(err.Error(), "SECURITY ERROR") {
		t.Errorf("mismatch not detected: %v", err)
	}
	if !strings.Contains(s2.Text(), "golang.org/x/text v0.14.0/go.mod h1:y") {
		t.Error("go lines lost")
	}
}

// gitRepo creates a bare repository with the given tagged versions.
func gitRepo(t *testing.T, base, name string, versions map[string]map[string]string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), name)
	os.MkdirAll(work, 0o755)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t",
			"-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	var tags []string
	for v := range versions {
		tags = append(tags, v)
	}
	// apply in version order
	for i := 0; i < len(tags); i++ {
		for j := i + 1; j < len(tags); j++ {
			if CompareVersions(tags[j], tags[i]) < 0 {
				tags[i], tags[j] = tags[j], tags[i]
			}
		}
	}
	for _, v := range tags {
		for f, content := range versions[v] {
			p := filepath.Join(work, f)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(content), 0o644)
		}
		git("add", "-A")
		git("commit", "-qm", v)
		git("tag", v)
	}
	dst := filepath.Join(base, name+".git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", work, dst).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestResolveMVS(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	base := t.TempDir()
	t.Setenv("GOCPY_MODCACHE", filepath.Join(t.TempDir(), "cache"))
	old := RepoURL
	RepoURL = func(repo string) string { return "file://" + filepath.Join(base, filepath.Base(repo)+".git") }
	defer func() { RepoURL = old }()

	gitRepo(t, base, "lib", map[string]map[string]string{
		"v0.1.0": {"gocpy.yml": "module: example.com/x/lib\n", "lib.gocpy": "package lib\n"},
		"v0.2.0": {"lib.gocpy": "package lib\n// v2\n"},
	})
	gitRepo(t, base, "other", map[string]map[string]string{
		"v1.0.0": {"gocpy.yml": "module: example.com/x/other\nrequire:\n  gocpy:\n    - example.com/x/lib@v0.2.0\n  go:\n    - golang.org/x/text@v0.14.0\n"},
	})
	root := t.TempDir()
	cfg, err := ParseConfig("gocpy.yml", []byte("module: example.com/app\nrequire:\n  gocpy:\n    - example.com/x/lib@v0.1.0\n    - example.com/x/other@v1.0.0\n  go:\n    - golang.org/x/text@v0.13.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := Resolve(root, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := g.Gocpy["example.com/x/lib"].Version; v != "v0.2.0" {
		t.Errorf("MVS selected lib %s, want v0.2.0", v)
	}
	if v := g.Go["golang.org/x/text"]; v != "v0.14.0" {
		t.Errorf("go requirement %s, want v0.14.0", v)
	}
	if m := g.Lookup("example.com/x/lib/sub/pkg"); m == nil || m.Path != "example.com/x/lib" {
		t.Errorf("lookup: %+v", m)
	}
	if err := g.WriteSum(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "gocpy.sum"))
	if strings.Count(string(data), "\n") != 3 {
		t.Errorf("gocpy.sum:\n%s", data)
	}
	if errs := g.Verify(); len(errs) > 0 {
		t.Error(errs)
	}
	if v, err := LatestTag("example.com/x/lib"); err != nil || v != "v0.2.0" {
		t.Errorf("latest: %s %v", v, err)
	}
	// wrong module path in gocpy.yml
	gitRepo(t, base, "liar", map[string]map[string]string{"v1.0.0": {"gocpy.yml": "module: example.com/x/someone-else\n"}})
	cfg2, _ := ParseConfig("gocpy.yml", []byte("module: example.com/app\nrequire:\n  gocpy:\n    - example.com/x/liar@v1.0.0\n"))
	if _, err := Resolve(t.TempDir(), cfg2, nil); err == nil || !strings.Contains(err.Error(), "declares module") {
		t.Errorf("want module path error, got %v", err)
	}
	// missing tag
	cfg3, _ := ParseConfig("gocpy.yml", []byte("module: example.com/app\nrequire:\n  gocpy:\n    - example.com/x/lib@v9.9.9\n"))
	if _, err := Resolve(t.TempDir(), cfg3, nil); err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Errorf("want clone error, got %v", err)
	}
}
