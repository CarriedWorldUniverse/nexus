package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// TestLoadToolPolicyEmptyPathPermissive verifies that an unconfigured
// -policy flag preserves the historical permissive behaviour
// (DefaultAllow=true) so existing aspects keep working unchanged.
func TestLoadToolPolicyEmptyPath(t *testing.T) {
	p, err := loadToolPolicy("", nil)
	if err != nil {
		t.Fatalf("empty path should not error: %v", err)
	}
	if !p.DefaultAllow {
		t.Error("empty path must yield DefaultAllow=true (permissive default)")
	}
	if v, _ := p.Decide(bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"id"}`)}); v != 0 {
		t.Errorf("permissive default should allow bash, verdict=%v", v)
	}
}

// TestLoadToolPolicyValidFile parses a real JSON file and confirms the
// decoded policy enforces as written.
func TestLoadToolPolicyValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	const src = `{"default_allow":true,"tools":{"bash":false},"escalate":{"write":true},"bash_deny":["rm -rf"],"write_path_allow":["work/"]}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := loadToolPolicy(path, nil)
	if err != nil {
		t.Fatalf("valid file should not error: %v", err)
	}
	if !p.DefaultAllow {
		t.Error("default_allow should parse to true")
	}
	if p.Tools["bash"] {
		t.Error("tools.bash should parse to false")
	}
	if !p.Escalate["write"] {
		t.Error("escalate.write should parse to true")
	}
	if len(p.BashDeny) != 1 || p.BashDeny[0] != "rm -rf" {
		t.Errorf("bash_deny=%v want [rm -rf]", p.BashDeny)
	}
	if len(p.WritePathAllow) != 1 || p.WritePathAllow[0] != "work/" {
		t.Errorf("write_path_allow=%v want [work/]", p.WritePathAllow)
	}
}

// TestLoadToolPolicyMissingFile confirms a non-existent path fails fast
// rather than silently falling back to permissive.
func TestLoadToolPolicyMissingFile(t *testing.T) {
	_, err := loadToolPolicy(filepath.Join(t.TempDir(), "does-not-exist.json"), nil)
	if err == nil {
		t.Fatal("missing policy file must return an error, not a permissive fallback")
	}
}

// TestLoadToolPolicyMalformedFile confirms invalid JSON is an error.
func TestLoadToolPolicyMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"default_allow": tru`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToolPolicy(path, nil); err == nil {
		t.Fatal("malformed policy JSON must return an error")
	}
}

// TestApplyPolicyFragment is a table test of the role-at-spawn overlay
// precedence (M1 Unit 3): a nil fragment is a total no-op; a non-nil
// fragment replaces exactly the fields it sets (including an explicit
// empty slice/map), leaving base fields it omits untouched, and always
// takes DefaultAllow from the fragment.
func TestApplyPolicyFragment(t *testing.T) {
	base := funnel.ToolPolicy{
		DefaultAllow:   true,
		Tools:          map[string]bool{"bash": true},
		Escalate:       map[string]bool{"write": true},
		BashDeny:       []string{"rm -rf"},
		WritePathAllow: []string{"work/"},
	}

	tests := []struct {
		name string
		frag *funnel.ToolPolicy
		want funnel.ToolPolicy
	}{
		{
			name: "nil fragment is a no-op",
			frag: nil,
			want: base,
		},
		{
			name: "empty fragment still forces DefaultAllow and leaves omitted fields",
			frag: &funnel.ToolPolicy{DefaultAllow: false},
			want: funnel.ToolPolicy{
				DefaultAllow:   false,
				Tools:          base.Tools,
				Escalate:       base.Escalate,
				BashDeny:       base.BashDeny,
				WritePathAllow: base.WritePathAllow,
			},
		},
		{
			name: "fragment tools override, other fields untouched",
			frag: &funnel.ToolPolicy{DefaultAllow: true, Tools: map[string]bool{"write": false, "edit": false}},
			want: funnel.ToolPolicy{
				DefaultAllow:   true,
				Tools:          map[string]bool{"write": false, "edit": false},
				Escalate:       base.Escalate,
				BashDeny:       base.BashDeny,
				WritePathAllow: base.WritePathAllow,
			},
		},
		{
			name: "fragment write_path_allow explicit empty replaces (read-only role)",
			frag: &funnel.ToolPolicy{DefaultAllow: true, WritePathAllow: []string{}},
			want: funnel.ToolPolicy{
				DefaultAllow:   true,
				Tools:          base.Tools,
				Escalate:       base.Escalate,
				BashDeny:       base.BashDeny,
				WritePathAllow: []string{},
			},
		},
		{
			name: "fragment replaces every field when it sets all of them",
			frag: &funnel.ToolPolicy{
				DefaultAllow:   false,
				Tools:          map[string]bool{"bash": false},
				Escalate:       map[string]bool{},
				BashDeny:       []string{"mkfs"},
				WritePathAllow: []string{"tests/"},
			},
			want: funnel.ToolPolicy{
				DefaultAllow:   false,
				Tools:          map[string]bool{"bash": false},
				Escalate:       map[string]bool{},
				BashDeny:       []string{"mkfs"},
				WritePathAllow: []string{"tests/"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPolicyFragment(base, tc.frag)
			if got.DefaultAllow != tc.want.DefaultAllow {
				t.Errorf("DefaultAllow = %v, want %v", got.DefaultAllow, tc.want.DefaultAllow)
			}
			if !reflect.DeepEqual(got.Tools, tc.want.Tools) {
				t.Errorf("Tools = %v, want %v", got.Tools, tc.want.Tools)
			}
			if !reflect.DeepEqual(got.Escalate, tc.want.Escalate) {
				t.Errorf("Escalate = %v, want %v", got.Escalate, tc.want.Escalate)
			}
			if !reflect.DeepEqual(got.BashDeny, tc.want.BashDeny) {
				t.Errorf("BashDeny = %v, want %v", got.BashDeny, tc.want.BashDeny)
			}
			if !reflect.DeepEqual(got.WritePathAllow, tc.want.WritePathAllow) {
				t.Errorf("WritePathAllow = %v, want %v", got.WritePathAllow, tc.want.WritePathAllow)
			}
		})
	}
}

// TestLoadToolPolicyWithFragment confirms loadToolPolicy threads a spawn
// PolicyFragment over the static file (or the permissive default when
// -policy is empty) — the loadToolPolicy-level integration of
// applyPolicyFragment.
func TestLoadToolPolicyWithFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	const src = `{"default_allow":true,"tools":{"bash":true}}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	frag := &funnel.ToolPolicy{DefaultAllow: false, Tools: map[string]bool{"bash": false}}

	// Static file + fragment: fragment wins.
	p, err := loadToolPolicy(path, frag)
	if err != nil {
		t.Fatalf("loadToolPolicy with fragment: %v", err)
	}
	if p.DefaultAllow {
		t.Error("fragment DefaultAllow=false should win over the static file's true")
	}
	if p.Tools["bash"] {
		t.Error("fragment tools.bash=false should win over the static file's true")
	}

	// Empty -policy + fragment: fragment overlays the permissive default.
	p2, err := loadToolPolicy("", frag)
	if err != nil {
		t.Fatalf("loadToolPolicy(\"\", fragment): %v", err)
	}
	if p2.DefaultAllow {
		t.Error("fragment should override the permissive default too")
	}
}
