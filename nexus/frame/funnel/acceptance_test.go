package funnel

import (
	"context"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// TestAcceptanceVerifier_FailsOpenOnMisconfigured covers the "verifier not
// configured" branch — Verify itself returns an error (not a met=true
// verdict); builderOnTaskDone (agentfunnel) is the layer that turns a
// verify error into a fail-open honor. Nil receiver must not panic either
// (mirrors the nil-safe pattern elsewhere in the package).
func TestAcceptanceVerifier_FailsOpenOnMisconfigured(t *testing.T) {
	var v *AcceptanceVerifier
	if _, err := v.Verify(context.Background(), "criteria", "output"); err == nil {
		t.Error("nil verifier must error, not silently succeed")
	}
	if _, err := (&AcceptanceVerifier{}).Verify(context.Background(), "criteria", "output"); err == nil {
		t.Error("empty-field verifier must error")
	}
}

func TestAcceptanceVerifier_ParsesJSON(t *testing.T) {
	tests := []struct {
		name      string
		modelText string
		wantMet   bool
		wantErr   bool
	}{
		{"clean met true", `{"met": true, "reason": "token present"}`, true, false},
		{"clean met false", `{"met": false, "reason": "token missing"}`, false, false},
		{"fenced", "```json\n{\"met\": false, \"reason\": \"no artifact\"}\n```", false, false},
		{"prose before json", `Verdict: {"met": true, "reason": "ok"}`, true, false},
		{"unparseable errors", "maybe?", false, true},
		{"missing met field errors", `{"reason": "no verdict"}`, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &scriptedProvider{results: []bridle.ProviderResult{
				{FinalText: tc.modelText, StopReason: bridle.StopReasonModelDone},
			}}
			v := &AcceptanceVerifier{
				Harness:  bridle.NewHarness(prov),
				Provider: "scripted",
				Model:    "judge",
			}
			verdict, err := v.Verify(context.Background(), "must produce token X", "reported: token X present")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("model said %q: expected error, got verdict=%+v", tc.modelText, verdict)
				}
				return
			}
			if err != nil {
				t.Fatalf("model said %q: unexpected error: %v", tc.modelText, err)
			}
			if verdict.Met != tc.wantMet {
				t.Errorf("model said %q: Met=%v, want %v", tc.modelText, verdict.Met, tc.wantMet)
			}
		})
	}
}

func TestAcceptanceVerifier_ErrorsOnHarnessFailure(t *testing.T) {
	v := &AcceptanceVerifier{
		Harness:  bridle.NewHarness(erroringProvider{err: context.DeadlineExceeded}),
		Provider: "erroring",
		Model:    "m",
	}
	if _, err := v.Verify(context.Background(), "criteria", "output"); err == nil {
		t.Error("harness error must surface as an error (caller fails open, not Verify itself)")
	}
}
