package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// TestHarnessOpenPR covers NEX-528: the harness opens the PR when the agent
// pushed its work but never ran `gh pr create`; it does NOT open when nothing
// is pushed (the agent still has work) and reports failure on a create error.
func TestHarnessOpenPR(t *testing.T) {
	origPushed, origCreate := branchPushedFn, prCreateFn
	t.Cleanup(func() { branchPushedFn = origPushed; prCreateFn = origCreate })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	branchPushedFn = func(string) (bool, error) { return true, nil }
	prCreateFn = func(repo, branch string) (string, error) { return "https://pr/1", nil }
	if url, ok := harnessOpenPR(log, "o/r", "builder/NEX-1"); !ok || url != "https://pr/1" {
		t.Fatalf("pushed+create-ok: url=%q ok=%v, want https://pr/1 true", url, ok)
	}

	branchPushedFn = func(string) (bool, error) { return false, nil }
	if url, ok := harnessOpenPR(log, "o/r", "builder/NEX-1"); ok || url != "" {
		t.Fatalf("not-pushed must not open (re-prompt instead): url=%q ok=%v", url, ok)
	}

	branchPushedFn = func(string) (bool, error) { return true, nil }
	prCreateFn = func(string, string) (string, error) { return "", errors.New("boom") }
	if _, ok := harnessOpenPR(log, "o/r", "builder/NEX-1"); ok {
		t.Fatal("create error must not report success")
	}
}

// TestBuilderDecide covers the NEX-477 builder goal-loop decision matrix: when
// to keep pursuing, when to push back for a missing PR, and when to exit.
func TestBuilderDecide(t *testing.T) {
	cases := []struct {
		name            string
		result          funnel.GoalResult
		prVerified      bool
		prRepromptsLeft int
		want            builderStep
	}{
		{"blocked exits", funnel.GoalResult{Blocked: true, Reason: "blocked"}, false, 3, builderExit},
		{"intermediate goal_not_met continues", funnel.GoalResult{Done: false, Reason: "goal_not_met"}, false, 3, builderContinue},
		{"complete with PR exits", funnel.GoalResult{Done: true, Reason: "complete"}, true, 3, builderExit},
		{"complete no PR reprompts", funnel.GoalResult{Done: true, Reason: "complete"}, false, 3, builderRepromptPR},
		{"complete no PR exhausted exits", funnel.GoalResult{Done: true, Reason: "complete"}, false, 0, builderExit},
		{"scratch no PR exits", funnel.GoalResult{Done: true, Reason: "scratch"}, false, 3, builderExit},
		{"loop_cap no PR exits", funnel.GoalResult{Done: true, Reason: "loop_cap"}, false, 3, builderExit},
		{"blocked short-circuits before PR", funnel.GoalResult{Blocked: true, Done: true, Reason: "complete"}, true, 3, builderExit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := builderDecide(tc.result, tc.prVerified, tc.prRepromptsLeft); got != tc.want {
				t.Errorf("builderDecide(%+v, pr=%v, left=%d) = %d, want %d", tc.result, tc.prVerified, tc.prRepromptsLeft, got, tc.want)
			}
		})
	}
}

// TestBuilderPRVerifier confirms the pure PR check: true only when a PR exists,
// false on miss or error (fail-closed), with no stop side effect.
func TestBuilderPRVerifier(t *testing.T) {
	orig := prExistsFn
	defer func() { prExistsFn = orig }()
	log := slog.Default()
	cases := []struct {
		name string
		fn   func(repo, ticket string) (bool, error)
		want bool
	}{
		{"pr exists", func(_, _ string) (bool, error) { return true, nil }, true},
		{"no pr", func(_, _ string) (bool, error) { return false, nil }, false},
		{"error is false", func(_, _ string) (bool, error) { return false, errors.New("boom") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prExistsFn = tc.fn
			if got := builderPRVerifier(log, "plumb", "org/repo", "NEX-1", "")(); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestBuilderPRVerifierUsesCustomBranch(t *testing.T) {
	orig := prExistsFn
	defer func() { prExistsFn = orig }()
	var gotRepo, gotBranch string
	prExistsFn = func(repo, branch string) (bool, error) {
		gotRepo = repo
		gotBranch = branch
		return true, nil
	}
	if ok := builderPRVerifier(slog.Default(), "plumb", "org/repo", "NEX-1", "feature/custom")(); !ok {
		t.Fatal("verifier returned false")
	}
	if gotRepo != "org/repo" || gotBranch != "feature/custom" {
		t.Fatalf("prExists args repo=%q branch=%q", gotRepo, gotBranch)
	}
}
