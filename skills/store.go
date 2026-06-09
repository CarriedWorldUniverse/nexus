package skills

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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
