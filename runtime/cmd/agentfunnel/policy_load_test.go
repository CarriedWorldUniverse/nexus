package main

import (
	"os"
	"path/filepath"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// TestLoadToolPolicyEmptyPathPermissive verifies that an unconfigured
// -policy flag preserves the historical permissive behaviour
// (DefaultAllow=true) so existing aspects keep working unchanged.
func TestLoadToolPolicyEmptyPath(t *testing.T) {
	p, err := loadToolPolicy("")
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
	p, err := loadToolPolicy(path)
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
	_, err := loadToolPolicy(filepath.Join(t.TempDir(), "does-not-exist.json"))
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
	if _, err := loadToolPolicy(path); err == nil {
		t.Fatal("malformed policy JSON must return an error")
	}
}
