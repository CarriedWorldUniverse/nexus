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
	origPushed, origCreate, origExists := pushedBranchesFn, prCreateFn, prExistsFn
	t.Cleanup(func() { pushedBranchesFn = origPushed; prCreateFn = origCreate; prExistsFn = origExists })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushedBranchesFn = func() ([]string, error) { return []string{"builder/NEX-1"}, nil }
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	prCreateFn = func(repo, branch, body string) (string, error) { return "https://pr/1", nil }
	if url, ok := harnessOpenPR(log, "o/r", "NEX-1", "", "builder/NEX-1", "DoD"); !ok || url != "https://pr/1" {
		t.Fatalf("pushed+create-ok: url=%q ok=%v, want https://pr/1 true", url, ok)
	}

	pushedBranchesFn = func() ([]string, error) { return nil, nil }
	if url, ok := harnessOpenPR(log, "o/r", "NEX-1", "", "builder/NEX-1", "DoD"); ok || url != "" {
		t.Fatalf("not-pushed must not open (re-prompt instead): url=%q ok=%v", url, ok)
	}

	pushedBranchesFn = func() ([]string, error) { return []string{"builder/NEX-1"}, nil }
	prCreateFn = func(string, string, string) (string, error) { return "", errors.New("boom") }
	if _, ok := harnessOpenPR(log, "o/r", "NEX-1", "", "builder/NEX-1", "DoD"); ok {
		t.Fatal("create error must not report success")
	}
}

func TestHarnessOpenPRFindsUnconventionalBranchByTicket(t *testing.T) {
	origPushed, origCreate, origExists := pushedBranchesFn, prCreateFn, prExistsFn
	t.Cleanup(func() { pushedBranchesFn = origPushed; prCreateFn = origCreate; prExistsFn = origExists })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushedBranchesFn = func() ([]string, error) {
		return []string{"scratch/unrelated", "fix/nex-553-open-pr-salvage"}, nil
	}
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	var gotBranch string
	prCreateFn = func(repo, branch, body string) (string, error) {
		gotBranch = branch
		return "https://pr/553", nil
	}

	if url, ok := harnessOpenPR(log, "o/r", "NEX-553", "run-abc", "builder/NEX-553", "DoD"); !ok || url != "https://pr/553" {
		t.Fatalf("url=%q ok=%v, want https://pr/553 true", url, ok)
	}
	if gotBranch != "fix/nex-553-open-pr-salvage" {
		t.Fatalf("opened branch %q, want unconventional ticket branch", gotBranch)
	}
}

func TestHarnessOpenPRPrefersRunIDOverTicketBranch(t *testing.T) {
	origPushed, origCreate, origExists := pushedBranchesFn, prCreateFn, prExistsFn
	t.Cleanup(func() { pushedBranchesFn = origPushed; prCreateFn = origCreate; prExistsFn = origExists })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushedBranchesFn = func() ([]string, error) {
		return []string{"fix/nex-553-old-attempt", "runner/run-123"}, nil
	}
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	var gotBranch string
	prCreateFn = func(repo, branch, body string) (string, error) {
		gotBranch = branch
		return "https://pr/553", nil
	}

	if url, ok := harnessOpenPR(log, "o/r", "NEX-553", "run-123", "builder/NEX-553", "DoD"); !ok || url != "https://pr/553" {
		t.Fatalf("url=%q ok=%v, want https://pr/553 true", url, ok)
	}
	if gotBranch != "runner/run-123" {
		t.Fatalf("opened branch %q, want run-id branch", gotBranch)
	}
}

func TestHarnessOpenPRDoesNotMatchTicketPrefix(t *testing.T) {
	origPushed, origCreate, origExists := pushedBranchesFn, prCreateFn, prExistsFn
	t.Cleanup(func() { pushedBranchesFn = origPushed; prCreateFn = origCreate; prExistsFn = origExists })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushedBranchesFn = func() ([]string, error) { return []string{"fix/nex-553-open-pr-salvage"}, nil }
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	prCreateFn = func(string, string, string) (string, error) {
		t.Fatal("prCreateFn must not be called for another ticket's branch")
		return "", nil
	}

	if url, ok := harnessOpenPR(log, "o/r", "NEX-55", "", "builder/NEX-55", "DoD"); ok || url != "" {
		t.Fatalf("ticket prefix must not match: url=%q ok=%v", url, ok)
	}
}

func TestHarnessOpenPRNoopsWhenPRExists(t *testing.T) {
	origPushed, origCreate, origExists := pushedBranchesFn, prCreateFn, prExistsFn
	t.Cleanup(func() { pushedBranchesFn = origPushed; prCreateFn = origCreate; prExistsFn = origExists })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushedBranchesFn = func() ([]string, error) { return []string{"fix/nex-553-open-pr-salvage"}, nil }
	prExistsFn = func(string, string) (bool, error) { return true, nil }
	prCreateFn = func(string, string, string) (string, error) {
		t.Fatal("prCreateFn must not be called when a PR already exists")
		return "", nil
	}

	if url, ok := harnessOpenPR(log, "o/r", "NEX-553", "", "builder/NEX-553", "DoD"); ok || url != "" {
		t.Fatalf("existing PR must no-op: url=%q ok=%v", url, ok)
	}
}

func TestHarnessOpenPRUsesDoDBody(t *testing.T) {
	origPushed, origCreate, origExists := pushedBranchesFn, prCreateFn, prExistsFn
	t.Cleanup(func() { pushedBranchesFn = origPushed; prCreateFn = origCreate; prExistsFn = origExists })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushedBranchesFn = func() ([]string, error) { return []string{"runner/run-123"}, nil }
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	var gotBody string
	prCreateFn = func(repo, branch, body string) (string, error) {
		gotBody = body
		return "https://pr/553", nil
	}

	dod := "Definition of Done:\n- salvage fires\n- body lands"
	if _, ok := harnessOpenPR(log, "o/r", "NEX-553", "run-123", "builder/NEX-553", dod); !ok {
		t.Fatal("harnessOpenPR did not open")
	}
	if gotBody != dod {
		t.Fatalf("body = %q, want DoD body", gotBody)
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
