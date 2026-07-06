package cfgreconcile

import (
	"context"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// TestFrontierAuth_AlmanacOverridesSecretPointer is the §6 build-spec
// acceptance test: a fake almanac carrying a frontier-auth doc redirects
// dispatch.FrontierAuthConfig away from its k8s-secret default.
func TestFrontierAuth_AlmanacOverridesSecretPointer(t *testing.T) {
	cfg := dispatch.NewFrontierAuthConfig()
	r := &fakeReader{vals: map[string]string{
		FrontierAuthPath: `{"secret_name":"almanac-frontier-secret","secret_key":"OAUTH_TOKEN"}`,
	}}
	rc := NewFrontierAuth(r, cfg, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReconcileOnce updated = %d, want 1", n)
	}
	name, key := cfg.Get()
	if name != "almanac-frontier-secret" || key != "OAUTH_TOKEN" {
		t.Fatalf("Get() after reconcile = (%q, %q), want almanac values", name, key)
	}
}

// TestFrontierAuth_AbsentAlmanacFallsBackToK8sSecret is the §6 build-spec
// acceptance test: no frontier-auth doc in almanac (the "almanac absent"
// case for this key — e.g. almanac dark, or the key simply unset) means
// dispatch.FrontierAuthConfig NEVER moves off the M0.3 claude-oauth secret,
// which is the actual k8s-secret delivery mechanism jobspec.BuildJob injects
// into every claude-code dispatch by default.
func TestFrontierAuth_AbsentAlmanacFallsBackToK8sSecret(t *testing.T) {
	cfg := dispatch.NewFrontierAuthConfig()
	r := &fakeReader{vals: map[string]string{}} // no doc at FrontierAuthPath
	rc := NewFrontierAuth(r, cfg, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReconcileOnce updated = %d, want 0 (no-op)", n)
	}
	name, key := cfg.Get()
	if name != dispatch.DefaultFrontierAuthSecretName || key != dispatch.DefaultFrontierAuthSecretKey {
		t.Fatalf("Get() = (%q, %q), want the k8s-secret defaults (%q, %q)",
			name, key, dispatch.DefaultFrontierAuthSecretName, dispatch.DefaultFrontierAuthSecretKey)
	}
}

// TestFrontierAuth_MalformedDocFallsBackToK8sSecret mirrors the malformed-doc
// skip behavior of the other cfgreconcile domains (see NetworkDefaults):
// almanac reachable but the doc unparsable → no-op, k8s-secret default kept.
func TestFrontierAuth_MalformedDocFallsBackToK8sSecret(t *testing.T) {
	cfg := dispatch.NewFrontierAuthConfig()
	r := &fakeReader{vals: map[string]string{FrontierAuthPath: `not-json`}}
	rc := NewFrontierAuth(r, cfg, quietLog())

	n, err := rc.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReconcileOnce updated = %d, want 0", n)
	}
	name, key := cfg.Get()
	if name != dispatch.DefaultFrontierAuthSecretName || key != dispatch.DefaultFrontierAuthSecretKey {
		t.Fatalf("Get() = (%q, %q), want the k8s-secret defaults", name, key)
	}
}
