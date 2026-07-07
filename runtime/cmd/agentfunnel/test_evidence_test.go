package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

func TestCriteriaMentionsTests(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"passing race-enabled tests", true},
		{"A PR exists with TESTS", true},
		{"implement the feature", false},
		{"", false},
	} {
		if got := criteriaMentionsTests(tc.in); got != tc.want {
			t.Errorf("criteriaMentionsTests(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDiffTouchesTestFile(t *testing.T) {
	withTest := "diff --git a/foo.go b/foo.go\n+++ b/foo.go\n@@\n+code\n" +
		"diff --git a/foo_test.go b/foo_test.go\n+++ b/foo_test.go\n@@\n+func TestX\n"
	withoutTest := "diff --git a/foo.go b/foo.go\n+++ b/foo.go\n@@\n+code\n"
	// a test file mentioned only in body text, not a header, must NOT count
	bodyMention := "+++ b/readme.md\n@@\n+see foo_test.go for details\n"

	if !diffTouchesTestFile(withTest) {
		t.Error("diff adding foo_test.go should count")
	}
	if diffTouchesTestFile(withoutTest) {
		t.Error("diff with no test file should not count")
	}
	if diffTouchesTestFile(bodyMention) {
		t.Error("a _test.go mentioned only in diff body text must not count")
	}
}

func TestTestEvidenceMissing(t *testing.T) {
	origDiff := prDiffFn
	defer func() { prDiffFn = origDiff }()
	log := slog.Default()
	const testCriteria = "implement X with passing tests"
	const noTestCriteria = "implement X"

	diffNoTests := "diff --git a/x.go b/x.go\n+++ b/x.go\n+code\n"
	diffWithTests := "diff --git a/x_test.go b/x_test.go\n+++ b/x_test.go\n+func TestX\n"

	cases := []struct {
		name     string
		enabled  bool
		repo     string
		criteria string
		diffFn   func(string, string, string) (string, bool, error)
		want     bool
	}{
		{"disabled → never fires", false, "org/repo", testCriteria,
			func(string, string, string) (string, bool, error) { return diffNoTests, true, nil }, false},
		{"no repo → skip", true, "", testCriteria,
			func(string, string, string) (string, bool, error) { return diffNoTests, true, nil }, false},
		{"criteria silent on tests → skip", true, "org/repo", noTestCriteria,
			func(string, string, string) (string, bool, error) { return diffNoTests, true, nil }, false},
		{"tests required, diff has none → MISSING", true, "org/repo", testCriteria,
			func(string, string, string) (string, bool, error) { return diffNoTests, true, nil }, true},
		{"tests required, diff has tests → present", true, "org/repo", testCriteria,
			func(string, string, string) (string, bool, error) { return diffWithTests, true, nil }, false},
		{"diff fetch errors → fail-open", true, "org/repo", testCriteria,
			func(string, string, string) (string, bool, error) { return "", false, errors.New("gh boom") }, false},
		{"no diff yet → fail-open", true, "org/repo", testCriteria,
			func(string, string, string) (string, bool, error) { return "", false, nil }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.enabled {
				t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "1")
			} else {
				t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "0")
			}
			prDiffFn = tc.diffFn
			g := &builderAcceptanceGate{repo: tc.repo, ticket: "NET-1", criteria: tc.criteria}
			if got := g.testEvidenceMissing(log); got != tc.want {
				t.Fatalf("testEvidenceMissing = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideUnit3OverridesMet proves the Decide wiring: a judge that returns
// met=true is overridden to not-met (and reprompts) when Unit 3 finds the
// criteria require tests but the PR diff has none.
func TestDecideUnit3OverridesMet(t *testing.T) {
	origDiff := prDiffFn
	defer func() { prDiffFn = origDiff }()
	t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "1")
	t.Setenv("ACCEPTANCE_JUDGE_DIFF", "0") // isolate Unit 3 from Unit 1 augmentation
	prDiffFn = func(string, string, string) (string, bool, error) {
		return "diff --git a/x.go b/x.go\n+++ b/x.go\n+code\n", true, nil // no test file
	}
	verify := func(_ context.Context, _, _ string) (funnel.AcceptanceVerdict, error) {
		return funnel.AcceptanceVerdict{Met: true, Reason: "judge says done"}, nil
	}
	gate := newBuilderAcceptanceGate("implement X with passing tests", verify)
	gate.repo, gate.ticket = "org/repo", "NET-1"

	step, verdict := gate.Decide(context.Background(), "I wrote and passed all tests", slog.Default())
	if verdict.Met {
		t.Fatal("Unit 3 should have overridden met=true to false (no test file in diff)")
	}
	if step != taskDoneReprompt {
		t.Fatalf("step = %v, want taskDoneReprompt", step)
	}
}
