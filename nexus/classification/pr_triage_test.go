package classification

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
	bridlefake "github.com/CarriedWorldUniverse/bridle/fake"
)

func TestPRTriage_NeedsReview(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "needs-review", "reason": "new handler in auth package"}`,
	})

	classifier := &PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	diff := `diff --git a/auth/middleware.go b/auth/middleware.go
+func ValidateSession(token string) (*Session, error) {
+    claims, err := parseJWT(token)
+    if err != nil {
+        return nil, err
+    }
+    return lookupSession(claims.Subject)
+}`

	verdict, err := classifier.Classify(context.Background(), PRTriageInput{Diff: diff})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if verdict.Class != ClassNeedsReview {
		t.Errorf("Class = %q, want %q", verdict.Class, ClassNeedsReview)
	}
}

func TestPRTriage_Trivial(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "trivial", "reason": "typo fix"}`,
	})

	classifier := &PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	diff := `diff --git a/README.md b/README.md
-- instalation
+- installation`

	verdict, err := classifier.Classify(context.Background(), PRTriageInput{Diff: diff})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if verdict.Class != ClassTrivial {
		t.Errorf("Class = %q, want %q", verdict.Class, ClassTrivial)
	}
}

func TestPRTriage_EmptyDiff(t *testing.T) {
	classifier := &PRTriage{
		Harness:  bridle.NewHarness(bridlefake.NewProvider()),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	_, err := classifier.Classify(context.Background(), PRTriageInput{Diff: ""})
	if err == nil {
		t.Fatal("expected error for empty diff")
	}
}

func TestPRTriage_FailOpenOnParseError(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: "not valid JSON at all, model went off the rails",
	})

	classifier := &PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	verdict, err := classifier.Classify(context.Background(), PRTriageInput{Diff: "some diff"})
	if err != nil {
		t.Fatalf("Classify should fail open, not return error: %v", err)
	}
	if verdict.Class != ClassNeedsReview {
		t.Errorf("fail-open class = %q, want %q", verdict.Class, ClassNeedsReview)
	}
}

func TestPRTriage_HarnessErrorFailsOpen(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Err: errors.New("provider error"),
	})

	classifier := &PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	verdict, err := classifier.Classify(context.Background(), PRTriageInput{Diff: "diff content"})
	if err != nil {
		t.Fatalf("Classify should fail open, not return error: %v", err)
	}
	if verdict.Class != ClassNeedsReview {
		t.Errorf("fail-open class = %q, want %q", verdict.Class, ClassNeedsReview)
	}
	if verdict.Reason != "classifier_error" {
		t.Errorf("fail-open reason = %q, want %q", verdict.Reason, "classifier_error")
	}
}

func TestPRTriage_TruncatesLongDiff(t *testing.T) {
	prov := bridlefake.NewProvider(bridlefake.Step{
		Text: `{"class": "needs-review", "reason": "big diff"}`,
	})

	classifier := &PRTriage{
		Harness:  bridle.NewHarness(prov),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	longDiff := strings.Repeat("+added line\n", 500)

	verdict, err := classifier.Classify(context.Background(), PRTriageInput{Diff: longDiff})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if verdict.Class != ClassNeedsReview {
		t.Errorf("Class = %q, want %q", verdict.Class, ClassNeedsReview)
	}
}

func TestPRTriage_ModelOverrideEnvVar(t *testing.T) {
	t.Setenv("NEXUS_PR_TRIAGE_MODEL", "claude-haiku-4-5")

	classifier := &PRTriage{
		Harness:  bridle.NewHarness(bridlefake.NewProvider(bridlefake.Step{
			Text: `{"class": "trivial", "reason": "test"}`,
		})),
		Provider: "claude-api",
		Model:    "deepseek-chat",
	}

	verdict, err := classifier.Classify(context.Background(), PRTriageInput{
		Diff:          "test diff",
		ModelOverride: "custom-model",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if verdict.Class != ClassTrivial {
		t.Errorf("Class = %q, want %q", verdict.Class, ClassTrivial)
	}
}
