// Package agentskills is the canonical store of nexus dev-lifecycle skills.
// The SKILL.md files live under .agents/skills/ - the cross-platform Agent
// Skills location that codex-cli and claude-code discover natively. This
// package (at the repo root, the only place a go:embed can reach the dot-dir)
// embeds them for the nexus-skills-mcp server, which serves the API aspects.
package agentskills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed all:.agents/skills
var files embed.FS

// Skill is one parsed SKILL.md: frontmatter fields plus the markdown body.
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	WhenToUse   string `yaml:"when_to_use"`
	Body        string `yaml:"-"`
}

// parse splits leading "---"-fenced YAML frontmatter from the markdown body.
func parse(raw []byte) (Skill, error) {
	var s Skill
	// Normalize CRLF to LF so SKILL.md files checked out with Windows line
	// endings still parse (the fence checks are LF-based).
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return s, fmt.Errorf("missing frontmatter fence")
	}
	rest := raw[len("---\n"):]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return s, fmt.Errorf("unterminated frontmatter")
	}
	front := rest[:end]
	body := rest[end+len("\n---\n"):]
	if err := yaml.Unmarshal(front, &s); err != nil {
		return s, fmt.Errorf("frontmatter yaml: %w", err)
	}
	if strings.TrimSpace(s.Name) == "" {
		return s, fmt.Errorf("frontmatter: name required")
	}
	s.Body = string(body)
	return s, nil
}

// Load parses every SKILL.md embedded in the store.
func Load() ([]Skill, error) {
	var out []Skill
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Base(p) != "SKILL.md" {
			return nil
		}
		raw, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		s, err := parse(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Search returns skills whose name/description/when_to_use contain query.
// Matching is case-insensitive. Empty query returns all skills.
func Search(query string) []Skill {
	all, err := Load()
	if err != nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all
	}
	var hits []Skill
	for _, s := range all {
		hay := strings.ToLower(s.Name + " " + s.Description + " " + s.WhenToUse)
		if strings.Contains(hay, q) {
			hits = append(hits, s)
		}
	}
	return hits
}

// Get returns the full SKILL.md body for an exact skill name.
func Get(name string) (string, bool) {
	all, err := Load()
	if err != nil {
		return "", false
	}
	for _, s := range all {
		if s.Name == name {
			return s.Body, true
		}
	}
	return "", false
}

// FilterAllowlist scopes skills to a role's SkillAllowlist — the
// skill-gating primitive for role-at-spawn (M1 Unit 3, ROLE-MODEL.md §9
// "least privilege"). An empty allow list is the back-compat no-op: every
// skill passes through unfiltered (today's ungated behavior). A non-empty
// allow list keeps only skills whose Name is in it, in the input order.
func FilterAllowlist(skills []Skill, allow []string) []Skill {
	if len(allow) == 0 {
		return skills
	}
	want := make(map[string]bool, len(allow))
	for _, name := range allow {
		want[name] = true
	}
	out := make([]Skill, 0, len(skills))
	for _, s := range skills {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// AllowedName reports whether name is permitted under a role's
// SkillAllowlist — the get_skill-side counterpart to FilterAllowlist. An
// empty allow list permits every name (back-compat: all skills).
func AllowedName(name string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if a == name {
			return true
		}
	}
	return false
}
