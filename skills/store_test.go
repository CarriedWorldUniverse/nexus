package skills

import (
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
