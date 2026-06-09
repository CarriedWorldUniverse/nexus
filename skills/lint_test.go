package skills

import (
	"regexp"
	"strings"
	"testing"
)

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
