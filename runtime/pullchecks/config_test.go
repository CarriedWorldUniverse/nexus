package pullchecks

import (
	"log/slog"
	"testing"
)

// clearPullEnv unsets every CW_PULL_* var for the duration of a subtest and
// restores the previous values on cleanup — env tests must never leak state
// into siblings or later packages under `go test ./...`.
func clearPullEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"CW_PULL_SERVER_ADDR", "CW_PULL_ORG", "CW_PULL_SLUG", "CW_PULL_PROJECT",
		"CW_PULL_TLS_CERT", "CW_PULL_TLS_KEY", "CW_PULL_TLS_CA", "CW_PULL_DEV_INSECURE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

// TestNewRecorderFromEnvDarkByDefault is the package-level half of the DARK
// DEFAULT proof: with every CW_PULL_* var unset, NewRecorderFromEnv must
// return nil — no dial attempt, no PullService client constructed. The
// agentfunnel-side half (zero PullService calls reach a live fake server
// when the caller correctly treats nil as "off") lives in the agentfunnel
// package's own wiring tests.
func TestNewRecorderFromEnvDarkByDefault(t *testing.T) {
	clearPullEnv(t)
	if rec := NewRecorderFromEnv(slog.Default()); rec != nil {
		t.Fatalf("NewRecorderFromEnv with no CW_PULL_* env = %+v, want nil (dark default)", rec)
	}
}

func TestNewRecorderFromEnvMissingOrgOrSlugStaysDark(t *testing.T) {
	clearPullEnv(t)
	t.Setenv("CW_PULL_SERVER_ADDR", "cairn.example:443")
	t.Setenv("CW_PULL_DEV_INSECURE", "1")
	// org/slug both still unset.
	if rec := NewRecorderFromEnv(slog.Default()); rec != nil {
		t.Fatalf("NewRecorderFromEnv with addr set but org/slug unset = %+v, want nil", rec)
	}

	t.Setenv("CW_PULL_ORG", "org-1")
	// slug still unset.
	if rec := NewRecorderFromEnv(slog.Default()); rec != nil {
		t.Fatalf("NewRecorderFromEnv with org set but slug unset = %+v, want nil", rec)
	}
}

func TestNewRecorderFromEnvMissingTLSStaysDark(t *testing.T) {
	clearPullEnv(t)
	t.Setenv("CW_PULL_SERVER_ADDR", "cairn.example:443")
	t.Setenv("CW_PULL_ORG", "org-1")
	t.Setenv("CW_PULL_SLUG", "widgets")
	// No TLS material and no CW_PULL_DEV_INSECURE=1 opt-in.
	if rec := NewRecorderFromEnv(slog.Default()); rec != nil {
		t.Fatalf("NewRecorderFromEnv with no TLS config and no dev-insecure opt-in = %+v, want nil", rec)
	}
}

func TestNewRecorderFromEnvEnabledWithDevInsecure(t *testing.T) {
	clearPullEnv(t)
	t.Setenv("CW_PULL_SERVER_ADDR", "cairn.example:443")
	t.Setenv("CW_PULL_ORG", "org-1")
	t.Setenv("CW_PULL_SLUG", "widgets")
	t.Setenv("CW_PULL_PROJECT", "PROJ")
	t.Setenv("CW_PULL_DEV_INSECURE", "1")
	// grpc.NewClient does not dial eagerly, so this succeeds without a live
	// server — it only proves the config path resolves to a non-nil,
	// correctly-scoped Recorder, not that the address is reachable.
	rec := NewRecorderFromEnv(slog.Default())
	if rec == nil {
		t.Fatal("NewRecorderFromEnv with full config + dev-insecure = nil, want a Recorder")
	}
	if rec.Org != "org-1" || rec.Slug != "widgets" || rec.Project != "PROJ" {
		t.Fatalf("Recorder = %+v, unexpected fields", rec)
	}
}
