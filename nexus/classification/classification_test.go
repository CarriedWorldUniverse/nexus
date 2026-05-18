package classification

import (
	"os"
	"testing"
)

func TestResolveModel_EnvVar(t *testing.T) {
	os.Setenv("NEXUS_PR_TRIAGE_MODEL", "claude-haiku-4-5")
	defer os.Unsetenv("NEXUS_PR_TRIAGE_MODEL")

	got := ResolveModel("NEXUS_PR_TRIAGE_MODEL", "deepseek-chat", "")
	if got != "claude-haiku-4-5" {
		t.Errorf("ResolveModel = %q, want %q (env var should win)", got, "claude-haiku-4-5")
	}
}

func TestResolveModel_Default(t *testing.T) {
	os.Unsetenv("NEXUS_PR_TRIAGE_MODEL")

	got := ResolveModel("NEXUS_PR_TRIAGE_MODEL", "deepseek-chat", "")
	if got != "deepseek-chat" {
		t.Errorf("ResolveModel = %q, want %q (default when no env var)", got, "deepseek-chat")
	}
}

func TestResolveModel_PerCallOverride(t *testing.T) {
	os.Setenv("NEXUS_PR_TRIAGE_MODEL", "claude-haiku-4-5")
	defer os.Unsetenv("NEXUS_PR_TRIAGE_MODEL")

	got := ResolveModel("NEXUS_PR_TRIAGE_MODEL", "deepseek-chat", "claude-opus-4-7")
	if got != "claude-opus-4-7" {
		t.Errorf("ResolveModel = %q, want %q (per-call override wins over env var)", got, "claude-opus-4-7")
	}
}

func TestResolveModel_EmptyDefault(t *testing.T) {
	os.Unsetenv("NEXUS_PR_TRIAGE_MODEL")

	got := ResolveModel("NEXUS_PR_TRIAGE_MODEL", "", "")
	if got != "" {
		t.Errorf("ResolveModel with empty default and no env = %q, want empty", got)
	}
}
