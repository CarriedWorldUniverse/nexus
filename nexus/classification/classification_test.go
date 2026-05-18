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

func TestParseVerdict_NeedsReview(t *testing.T) {
	raw := `{"class": "needs-review", "reason": "touches auth middleware"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Class != ClassNeedsReview {
		t.Errorf("Class = %q, want %q", v.Class, ClassNeedsReview)
	}
	if v.Reason != "touches auth middleware" {
		t.Errorf("Reason = %q, want %q", v.Reason, "touches auth middleware")
	}
}

func TestParseVerdict_Trivial(t *testing.T) {
	raw := `{"class": "trivial", "reason": "whitespace only"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Class != ClassTrivial {
		t.Errorf("Class = %q, want %q", v.Class, ClassTrivial)
	}
}

func TestParseVerdict_Suspicious(t *testing.T) {
	raw := `{"class": "suspicious", "reason": "large diff in credential code"}`
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Class != ClassSuspicious {
		t.Errorf("Class = %q, want %q", v.Class, ClassSuspicious)
	}
}

func TestParseVerdict_CodeFence(t *testing.T) {
	raw := "```json\n{\"class\": \"needs-review\", \"reason\": \"new endpoint\"}\n```"
	v, err := ParseVerdict(raw)
	if err != nil {
		t.Fatalf("ParseVerdict: %v", err)
	}
	if v.Class != ClassNeedsReview {
		t.Errorf("Class = %q, want %q", v.Class, ClassNeedsReview)
	}
}

func TestParseVerdict_InvalidClass(t *testing.T) {
	raw := `{"class": "fantastic", "reason": "not a real class"}`
	_, err := ParseVerdict(raw)
	if err == nil {
		t.Fatal("expected error for invalid class, got nil")
	}
}

func TestParseVerdict_NotJSON(t *testing.T) {
	_, err := ParseVerdict("just some text, no JSON here")
	if err == nil {
		t.Fatal("expected error for non-JSON, got nil")
	}
}
