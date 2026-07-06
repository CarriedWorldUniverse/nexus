package dispatch

import "testing"

func TestFrontierAuthConfig_DefaultsToClaudeOAuthSecret(t *testing.T) {
	f := NewFrontierAuthConfig()
	name, key := f.Get()
	if name != DefaultFrontierAuthSecretName || key != DefaultFrontierAuthSecretKey {
		t.Fatalf("Get() = (%q, %q), want (%q, %q)", name, key, DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey)
	}
}

func TestFrontierAuthConfig_SetOverridesAndReportsChange(t *testing.T) {
	f := NewFrontierAuthConfig()

	if changed := f.Set("almanac-secret", "TOKEN"); !changed {
		t.Fatalf("Set should report a change on first override")
	}
	name, key := f.Get()
	if name != "almanac-secret" || key != "TOKEN" {
		t.Fatalf("Get() after Set = (%q, %q), want (\"almanac-secret\", \"TOKEN\")", name, key)
	}

	if changed := f.Set("almanac-secret", "TOKEN"); changed {
		t.Fatalf("Set with identical values should report no change")
	}
}

func TestFrontierAuthConfig_SetEmptyNameIsNoop(t *testing.T) {
	f := NewFrontierAuthConfig()
	if changed := f.Set("", "whatever"); changed {
		t.Fatalf("Set with empty name should be a no-op (never regress to no secret)")
	}
	name, key := f.Get()
	if name != DefaultFrontierAuthSecretName || key != DefaultFrontierAuthSecretKey {
		t.Fatalf("Get() after empty-name Set = (%q, %q), want defaults unchanged", name, key)
	}
}

func TestFrontierAuthConfig_SetEmptyKeyFallsBackToDefaultKey(t *testing.T) {
	f := NewFrontierAuthConfig()
	f.Set("almanac-secret", "")
	name, key := f.Get()
	if name != "almanac-secret" || key != DefaultFrontierAuthSecretKey {
		t.Fatalf("Get() = (%q, %q), want (\"almanac-secret\", %q)", name, key, DefaultFrontierAuthSecretKey)
	}
}
