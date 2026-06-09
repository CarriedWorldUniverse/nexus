package agentskills

import (
	"regexp"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	raw := "---\nname: development\ndescription: how to build\nwhen_to_use: when coding\n---\n# development\n\nbody line one.\n"
	s, err := parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "development" || s.Description != "how to build" || s.WhenToUse != "when coding" {
		t.Fatalf("frontmatter: %+v", s)
	}
	if want := "# development\n\nbody line one.\n"; s.Body != want {
		t.Fatalf("body = %q, want %q", s.Body, want)
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	if _, err := parse([]byte("# no frontmatter\n")); err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

// TestParseCRLF guards the Windows line-ending regression: a SKILL.md checked
// out with CRLF must still parse.
func TestParseCRLF(t *testing.T) {
	raw := "---\r\nname: dev\r\ndescription: d\r\nwhen_to_use: w\r\n---\r\n# body\r\n"
	s, err := parse([]byte(raw))
	if err != nil {
		t.Fatalf("CRLF parse failed: %v", err)
	}
	if s.Name != "dev" || s.Description != "d" {
		t.Fatalf("CRLF frontmatter: %+v", s)
	}
}

func TestLoadSearchGet(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no skills loaded from embed")
	}
	hits := Search("TICKET")
	if len(hits) == 0 {
		t.Fatal("expected the development skill to match 'ticket'")
	}
	found := false
	for _, h := range hits {
		if h.Name == "development" {
			found = true
		}
	}
	if !found {
		t.Fatalf("development not in hits: %+v", hits)
	}
	body, ok := Get("development")
	if !ok || !strings.Contains(body, "# development") {
		t.Fatalf("Get(development) ok=%v body=%q", ok, body)
	}
	if _, ok := Get("nonexistent"); ok {
		t.Fatal("Get(nonexistent) should be false")
	}
}

// loadRef matches a cross-reference like "load the security skill" (case-
// insensitive) and captures the skill name. Every match must name a real skill.
var loadRef = regexp.MustCompile(`(?i)load (?:the )?` + "`?" + `([a-z][a-z-]+)` + "`?" + ` skill`)

func TestSkillsLint(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"workflow-basics": true, "spec": true, "planning": true, "development": true,
		"review": true, "merge": true, "release": true, "house-style": true, "security": true,
	}
	names := map[string]bool{}
	for _, s := range all {
		names[s.Name] = true
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("%s: empty description", s.Name)
		}
		if strings.TrimSpace(s.WhenToUse) == "" {
			t.Errorf("%s: empty when_to_use", s.Name)
		}
		for _, m := range loadRef.FindAllStringSubmatch(s.Body, -1) {
			if ref := strings.ToLower(m[1]); !want[ref] {
				t.Errorf("%s: cross-ref to unknown skill %q", s.Name, ref)
			}
		}
	}
	for n := range want {
		if !names[n] {
			t.Errorf("missing skill: %s", n)
		}
	}
}
