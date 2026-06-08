package hooks

import "testing"

func TestMergeDecisionsDefaultContinueTrue(t *testing.T) {
	got := MergeDecisions(Decision{Decision: "allow", AdditionalContext: "ok"})
	if !got.Continue {
		t.Fatal("Continue = false, want default true")
	}
}

func TestMergeDecisionsDenyWinsContextConcatAndContinue(t *testing.T) {
	got := MergeDecisions(
		Decision{AdditionalContext: "first", Continue: true, Decision: "allow"},
		Decision{AdditionalContext: "second", Continue: true, Decision: "deny"},
		Decision{AdditionalContext: "third", Decision: "allow"}.WithContinue(false),
	)

	if got.Decision != "deny" {
		t.Fatalf("Decision = %q, want deny", got.Decision)
	}
	if got.PermissionDecision != "deny" {
		t.Fatalf("PermissionDecision = %q, want deny", got.PermissionDecision)
	}
	if got.AdditionalContext != "first\nsecond\nthird" {
		t.Fatalf("AdditionalContext = %q", got.AdditionalContext)
	}
	if got.Continue {
		t.Fatal("Continue = true, want false")
	}
}

func TestMergeDecisionsBlockWinsOverDeny(t *testing.T) {
	got := MergeDecisions(
		Decision{Decision: "deny"},
		Decision{Decision: "block"},
	)

	if got.Decision != "block" {
		t.Fatalf("Decision = %q, want block", got.Decision)
	}
	if got.PermissionDecision != "block" {
		t.Fatalf("PermissionDecision = %q, want block", got.PermissionDecision)
	}
}
