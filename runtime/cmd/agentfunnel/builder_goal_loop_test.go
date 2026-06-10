package main

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// TestHarnessOpenPR covers NEX-528: the harness opens the PR when the agent
// pushed its work but never ran `gh pr create`; it does NOT open when nothing
// is pushed (the agent still has work) and reports failure on a create error.
func TestHarnessOpenPR(t *testing.T) {
	origPushed, origList, origExists, origCreate := branchPushedFn, pushedBranchesFn, prExistsFn, prCreateFn
	t.Cleanup(func() {
		branchPushedFn = origPushed
		pushedBranchesFn = origList
		prExistsFn = origExists
		prCreateFn = origCreate
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	branchPushedFn = func(string) (bool, error) { return true, nil }
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	prCreateFn = func(repo, branch, body string) (string, error) { return "https://pr/1", nil }
	if url, ok := harnessOpenPR(log, "o/r", "builder/NEX-1", "NEX-1", "run-1", "DoD"); !ok || url != "https://pr/1" {
		t.Fatalf("pushed+create-ok: url=%q ok=%v, want https://pr/1 true", url, ok)
	}

	branchPushedFn = func(string) (bool, error) { return false, nil }
	pushedBranchesFn = func() ([]string, error) { return nil, nil }
	if url, ok := harnessOpenPR(log, "o/r", "builder/NEX-1", "NEX-1", "run-1", "DoD"); ok || url != "" {
		t.Fatalf("not-pushed must not open (re-prompt instead): url=%q ok=%v", url, ok)
	}

	branchPushedFn = func(string) (bool, error) { return true, nil }
	prCreateFn = func(string, string, string) (string, error) { return "", errors.New("boom") }
	if _, ok := harnessOpenPR(log, "o/r", "builder/NEX-1", "NEX-1", "run-1", "DoD"); ok {
		t.Fatal("create error must not report success")
	}
}

func TestHarnessOpenPRSalvagesUnconventionalBranchByRunOrTicket(t *testing.T) {
	origPushed, origList, origExists, origCreate := branchPushedFn, pushedBranchesFn, prExistsFn, prCreateFn
	t.Cleanup(func() {
		branchPushedFn = origPushed
		pushedBranchesFn = origList
		prExistsFn = origExists
		prCreateFn = origCreate
	})

	branchPushedFn = func(string) (bool, error) { return false, nil }
	pushedBranchesFn = func() ([]string, error) {
		return []string{"scratch/other", "fix/unusual-nex-553-name"}, nil
	}
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	var gotBranch string
	prCreateFn = func(_, branch, _ string) (string, error) {
		gotBranch = branch
		return "https://pr/553", nil
	}

	url, ok := harnessOpenPR(slog.Default(), "o/r", "builder/NEX-553", "NEX-553", "run-abc", "DoD")
	if !ok || url != "https://pr/553" {
		t.Fatalf("harnessOpenPR = %q %v, want PR URL true", url, ok)
	}
	if gotBranch != "fix/unusual-nex-553-name" {
		t.Fatalf("created branch = %q", gotBranch)
	}
}

func TestHarnessOpenPRNoopsWhenPRExists(t *testing.T) {
	origPushed, origList, origExists, origCreate := branchPushedFn, pushedBranchesFn, prExistsFn, prCreateFn
	t.Cleanup(func() {
		branchPushedFn = origPushed
		pushedBranchesFn = origList
		prExistsFn = origExists
		prCreateFn = origCreate
	})

	branchPushedFn = func(string) (bool, error) { return false, nil }
	pushedBranchesFn = func() ([]string, error) { return []string{"feature/run-abc123"}, nil }
	prExistsFn = func(_, branch string) (bool, error) { return branch == "feature/run-abc123", nil }
	prCreateFn = func(string, string, string) (string, error) {
		t.Fatal("prCreateFn must not be called when PR already exists")
		return "", nil
	}

	if url, ok := harnessOpenPR(slog.Default(), "o/r", "builder/NEX-553", "NEX-553", "run-abc123", "DoD"); !ok || url != "" {
		t.Fatalf("existing PR should be treated as handled without creating: url=%q ok=%v", url, ok)
	}
}

func TestHarnessOpenPRUsesDoDBody(t *testing.T) {
	origPushed, origExists, origCreate := branchPushedFn, prExistsFn, prCreateFn
	t.Cleanup(func() {
		branchPushedFn = origPushed
		prExistsFn = origExists
		prCreateFn = origCreate
	})

	branchPushedFn = func(string) (bool, error) { return true, nil }
	prExistsFn = func(string, string) (bool, error) { return false, nil }
	var gotBody string
	prCreateFn = func(_, _, body string) (string, error) {
		gotBody = body
		return "https://pr/553", nil
	}

	dod := "Definition of Done\n- salvage fires\n- body lands"
	if _, ok := harnessOpenPR(slog.Default(), "o/r", "builder/NEX-553", "NEX-553", "run-abc123", dod); !ok {
		t.Fatal("harnessOpenPR returned false")
	}
	if !strings.Contains(gotBody, dod) {
		t.Fatalf("PR body missing DoD:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, "opened this PR at the completion window") {
		t.Fatalf("PR body missing salvage timing justification:\n%s", gotBody)
	}
}

func TestDispatchDoD(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "brief DoD section",
			content: "@anvil [TICKET NEX-553] Fix it\n\nDescription:\ncontext\n\nDefinition of Done:\n- salvage fires\n- body lands",
			want:    "- salvage fires\n- body lands",
		},
		{
			name:    "terminated by next heading",
			content: "Preamble\n## Definition of Done\n- first\n\n## Notes\nignore",
			want:    "- first",
		},
		{
			name:    "no marker falls back to full content",
			content: "Ship the PR",
			want:    "Ship the PR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dispatchDoD(tt.content); got != tt.want {
				t.Fatalf("dispatchDoD() = %q, want %q", got, tt.want)
			}
		})
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
