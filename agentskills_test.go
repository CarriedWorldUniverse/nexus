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

// TestFilterAllowlist is a table test of the skill-gating primitive (M1
// Unit 3, ROLE-MODEL.md §9 least privilege): an empty allow list is the
// back-compat no-op (every skill passes through); a non-empty one keeps
// only the named skills.
func TestFilterAllowlist(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("need at least 2 embedded skills for this test, got %d", len(all))
	}

	tests := []struct {
		name  string
		allow []string
		want  func([]Skill) bool
	}{
		{
			name:  "nil allowlist passes every skill through",
			allow: nil,
			want:  func(got []Skill) bool { return len(got) == len(all) },
		},
		{
			name:  "empty allowlist passes every skill through",
			allow: []string{},
			want:  func(got []Skill) bool { return len(got) == len(all) },
		},
		{
			name:  "single-name allowlist keeps only that skill",
			allow: []string{"development"},
			want: func(got []Skill) bool {
				if len(got) != 1 {
					return false
				}
				return got[0].Name == "development"
			},
		},
		{
			name:  "unknown name in allowlist yields empty result",
			allow: []string{"no-such-skill"},
			want:  func(got []Skill) bool { return len(got) == 0 },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterAllowlist(all, tc.allow)
			if !tc.want(got) {
				t.Errorf("FilterAllowlist(%v) = %v skills, unexpected result", tc.allow, len(got))
			}
		})
	}
}

// TestAllowedName is the get_skill-side counterpart to TestFilterAllowlist.
func TestAllowedName(t *testing.T) {
	tests := []struct {
		name  string
		skill string
		allow []string
		want  bool
	}{
		{name: "nil allowlist permits everything", skill: "development", allow: nil, want: true},
		{name: "empty allowlist permits everything", skill: "development", allow: []string{}, want: true},
		{name: "named skill permitted", skill: "development", allow: []string{"development", "review"}, want: true},
		{name: "unnamed skill denied", skill: "security", allow: []string{"development", "review"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllowedName(tc.skill, tc.allow); got != tc.want {
				t.Errorf("AllowedName(%q, %v) = %v, want %v", tc.skill, tc.allow, got, tc.want)
			}
		})
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
		"cairn": true,
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
