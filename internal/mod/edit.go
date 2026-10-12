package mod

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetRequirement adds or updates path@version in root/gocpy.yml under the
// section of kind, keeping the rest of the file (comments included).
// A flat 0.1-style require: map is converted to the sectioned form.
func SetRequirement(root string, req Requirement) error {
	p := filepath.Join(root, "gocpy.yml")
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level must be a mapping", p)
	}
	reqNode := mapGet(top, "require")
	if reqNode == nil || reqNode.Kind != yaml.MappingNode {
		reqNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapSet(top, "require", reqNode)
	}
	// convert legacy entries (path: version) into the gocpy section
	var legacy []string
	var kept []*yaml.Node
	for i := 0; i+1 < len(reqNode.Content); i += 2 {
		k, v := reqNode.Content[i], reqNode.Content[i+1]
		if k.Value != "gocpy" && k.Value != "go" && v.Kind == yaml.ScalarNode {
			legacy = append(legacy, k.Value+"@"+v.Value)
			continue
		}
		kept = append(kept, k, v)
	}
	reqNode.Content = kept
	for _, l := range legacy {
		appendSeq(section(reqNode, "gocpy"), l)
	}
	// remove the path from both sections, then add it to its own
	for _, name := range []string{"gocpy", "go"} {
		if s := mapGet(reqNode, name); s != nil && s.Kind == yaml.SequenceNode {
			var out []*yaml.Node
			for _, e := range s.Content {
				if path, _, _ := strings.Cut(e.Value, "@"); path != req.Path {
					out = append(out, e)
				}
			}
			s.Content = out
		}
	}
	appendSeq(section(reqNode, req.Kind.String()), req.String())
	// drop empty sections
	var final []*yaml.Node
	for i := 0; i+1 < len(reqNode.Content); i += 2 {
		if v := reqNode.Content[i+1]; v.Kind == yaml.SequenceNode && len(v.Content) == 0 {
			continue
		}
		final = append(final, reqNode.Content[i], reqNode.Content[i+1])
	}
	reqNode.Content = final

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	enc.Close()
	if _, err := ParseConfig(p, buf.Bytes()); err != nil {
		return err
	}
	return os.WriteFile(p, buf.Bytes(), 0o644)
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

func section(req *yaml.Node, name string) *yaml.Node {
	s := mapGet(req, name)
	if s == nil || s.Kind != yaml.SequenceNode {
		s = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		mapSet(req, name, s)
	}
	return s
}

func appendSeq(s *yaml.Node, v string) {
	s.Content = append(s.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
}

// LatestTag returns the highest semver tag of a Gocpy module's repository.
func LatestTag(path string) (string, error) {
	repo, sub := RepoRoot(path)
	out, err := gitOutput("ls-remote", "--tags", "--refs", RepoURL(repo))
	if err != nil {
		return "", fmt.Errorf("%s: cannot list tags: %v", path, err)
	}
	prefix := "refs/tags/"
	if sub != "" {
		prefix += sub + "/"
	}
	best := ""
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || !strings.HasPrefix(f[1], prefix) {
			continue
		}
		v := strings.TrimPrefix(f[1], prefix)
		if !IsValidVersion(v) || parseSemver(v).pre != "" {
			continue
		}
		if best == "" || CompareVersions(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s: no release tags (vX.Y.Z) found", path)
	}
	return best, nil
}
