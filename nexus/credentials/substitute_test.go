package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// setProvider seeds a kind='provider' credential under the test store
// with the given key + base_url + api_shape. Returns nothing; helper
// fails the test on error.
func setProvider(t *testing.T, s *Store, name, key, baseURL, shape string) {
	t.Helper()
	err := s.Set(context.Background(), UpsertParams{
		Name:           name,
		Kind:           KindProvider,
		Bundle:         map[string]any{"api_shape": shape, "base_url": baseURL, "key": key},
		AllowedAspects: []string{"*"},
		Mode:           ModeFetch,
	})
	if err != nil {
		t.Fatalf("seed provider %q: %v", name, err)
	}
}

func TestSubstitute_SingleReference(t *testing.T) {
	s, _ := newTestStore(t)
	setProvider(t, s, "gh-pat", "ghp_secretvalue", "https://api.github.com", "openai")

	in := `{"env":{"TOKEN":"${credential:gh-pat.key}"}}`
	out, err := s.Substitute(context.Background(), "forge", in)
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	want := `{"env":{"TOKEN":"ghp_secretvalue"}}`
	if out != want {
		t.Errorf("substitute:\n got  %q\n want %q", out, want)
	}
}

func TestSubstitute_MultipleReferences(t *testing.T) {
	s, _ := newTestStore(t)
	setProvider(t, s, "gh", "ghp_x", "https://api.github.com", "openai")
	setProvider(t, s, "oai", "sk-oai-y", "https://api.openai.com/v1", "openai")

	in := `{"a":"${credential:gh.key}","b":"${credential:oai.key}","c":"${credential:gh.base_url}"}`
	out, err := s.Substitute(context.Background(), "forge", in)
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	want := `{"a":"ghp_x","b":"sk-oai-y","c":"https://api.github.com"}`
	if out != want {
		t.Errorf("multi:\n got  %q\n want %q", out, want)
	}
}

func TestSubstitute_DottedCredentialName(t *testing.T) {
	s, _ := newTestStore(t)
	// Credential names can contain dots (keyfile pattern). Last-dot-wins
	// split means this resolves as (name="github.nexus-anvil", field="key").
	setProvider(t, s, "github.nexus-anvil", "pat_value", "https://api.github.com", "openai")

	in := `{"token":"${credential:github.nexus-anvil.key}"}`
	out, err := s.Substitute(context.Background(), "forge", in)
	if err != nil {
		t.Fatalf("Substitute dotted: %v", err)
	}
	want := `{"token":"pat_value"}`
	if out != want {
		t.Errorf("dotted:\n got  %q\n want %q", out, want)
	}
}

func TestSubstitute_UnknownCredential(t *testing.T) {
	s, _ := newTestStore(t)
	in := `{"x":"${credential:missing.key}"}`
	_, err := s.Substitute(context.Background(), "forge", in)
	if err == nil {
		t.Fatal("expected error for unknown credential, got nil")
	}
	if !errors.Is(err, ErrSubstituteUnknownCredential) {
		t.Errorf("wrong sentinel: got %v want ErrSubstituteUnknownCredential", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the bad reference, got %q", err.Error())
	}
}

func TestSubstitute_UnknownField(t *testing.T) {
	s, _ := newTestStore(t)
	setProvider(t, s, "gh", "x", "u", "openai")

	in := `{"x":"${credential:gh.nonexistent}"}`
	_, err := s.Substitute(context.Background(), "forge", in)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !errors.Is(err, ErrSubstituteUnknownField) {
		t.Errorf("wrong sentinel: got %v want ErrSubstituteUnknownField", err)
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should name the bad field, got %q", err.Error())
	}
}

func TestSubstitute_MalformedPlaceholders(t *testing.T) {
	s, _ := newTestStore(t)
	setProvider(t, s, "gh", "x", "u", "openai")

	cases := []struct {
		name string
		in   string
	}{
		{"missing dot (no field)", `{"x":"${credential:gh}"}`},
		{"empty body", `{"x":"${credential:}"}`},
		{"empty field after dot", `{"x":"${credential:gh.}"}`},
		{"empty name before dot", `{"x":"${credential:.key}"}`},
		{"unclosed brace", `{"x":"${credential:gh.key"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Substitute(context.Background(), "forge", tc.in)
			if err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
			if err != nil && !errors.Is(err, ErrSubstituteMalformed) {
				t.Errorf("wrong sentinel for %s: got %v", tc.name, err)
			}
		})
	}
}

func TestSubstitute_NoPlaceholdersIsPassThrough(t *testing.T) {
	s, _ := newTestStore(t)
	in := `{"plain":"value","nested":{"x":1}}`
	out, err := s.Substitute(context.Background(), "forge", in)
	if err != nil {
		t.Fatalf("Substitute pass-through: %v", err)
	}
	if out != in {
		t.Errorf("pass-through changed input:\n got  %q\n want %q", out, in)
	}
}

func TestSubstitute_WritesAuditRowPerReference(t *testing.T) {
	s, db := newTestStore(t)
	setProvider(t, s, "gh", "x", "u", "openai")
	setProvider(t, s, "oai", "y", "u", "openai")

	in := `{"a":"${credential:gh.key}","b":"${credential:oai.key}","c":"${credential:gh.base_url}"}`
	if _, err := s.Substitute(context.Background(), "forge", in); err != nil {
		t.Fatalf("Substitute: %v", err)
	}

	// 3 references → 3 audit rows.
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM credential_audit`).Scan(&total); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if total != 3 {
		t.Errorf("audit rows: got %d want 3", total)
	}

	// Every row should be action=fetch with via=mcp_profile_substitute and
	// the right profile_aspect.
	rows, err := db.Query(`SELECT credential_name, aspect, action, details FROM credential_audit ORDER BY id`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	seen := map[string]int{}
	for rows.Next() {
		var name, aspect, action, details string
		if err := rows.Scan(&name, &aspect, &action, &details); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if action != "fetch" {
			t.Errorf("action = %q; want fetch", action)
		}
		if aspect != "forge" {
			t.Errorf("aspect = %q; want forge", aspect)
		}
		if !strings.Contains(details, `"via":"mcp_profile_substitute"`) {
			t.Errorf("details missing via tag: %s", details)
		}
		if !strings.Contains(details, `"profile_aspect":"forge"`) {
			t.Errorf("details missing profile_aspect: %s", details)
		}
		if !strings.Contains(details, `"credential":"`+name+`"`) {
			t.Errorf("details missing credential field: %s", details)
		}
		seen[name]++
	}
	if seen["gh"] != 2 || seen["oai"] != 1 {
		t.Errorf("per-credential audit counts: got %v want gh=2 oai=1", seen)
	}
}

func TestSubstitute_UnknownCredentialWritesNoAudit(t *testing.T) {
	s, db := newTestStore(t)
	setProvider(t, s, "gh", "x", "u", "openai")

	// First placeholder resolves, second doesn't. The whole call must
	// fail; partial-success substitutions are not allowed (would yield a
	// half-rendered profile). No audit row should land for either —
	// failing the call rolls forward as if it never ran.
	//
	// Implementation note: this means the substitution must validate
	// every reference before writing any audit row, OR write inside a
	// transaction that rolls back on failure. Tests pin the observable
	// behaviour; impl picks the mechanism.
	in := `{"a":"${credential:gh.key}","b":"${credential:missing.key}"}`
	if _, err := s.Substitute(context.Background(), "forge", in); err == nil {
		t.Fatal("expected error, got nil")
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM credential_audit`).Scan(&total); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if total != 0 {
		t.Errorf("audit rows after failed Substitute: got %d want 0", total)
	}
}
