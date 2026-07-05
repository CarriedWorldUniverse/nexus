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

// TestBuilderDecide covers the NEX-477 builder goal-loop decision matrix:
// when to keep pursuing, when to push back for missing acceptance or a
// missing PR, and when to exit. taskDoneHonor is the "acceptance gate not
// applicable or already passed" value passed for every case that predates
// NET-27 (no criteria captured, verifier unavailable, or already met) — see
// TestBuilderDecide_Acceptance below for the acceptance-specific matrix.
func TestBuilderDecide(t *testing.T) {
	cases := []struct {
		name            string
		result          funnel.GoalResult
		acceptance      taskDoneStep
		prVerified      bool
		prRepromptsLeft int
		want            builderStep
	}{
		{"blocked exits", funnel.GoalResult{Blocked: true, Reason: "blocked"}, taskDoneHonor, false, 3, builderExit},
		{"intermediate goal_not_met continues", funnel.GoalResult{Done: false, Reason: "goal_not_met"}, taskDoneHonor, false, 3, builderContinue},
		{"complete with PR exits", funnel.GoalResult{Done: true, Reason: "complete"}, taskDoneHonor, true, 3, builderExit},
		{"complete no PR reprompts", funnel.GoalResult{Done: true, Reason: "complete"}, taskDoneHonor, false, 3, builderRepromptPR},
		{"complete no PR exhausted exits", funnel.GoalResult{Done: true, Reason: "complete"}, taskDoneHonor, false, 0, builderExit},
		{"scratch no PR exits", funnel.GoalResult{Done: true, Reason: "scratch"}, taskDoneHonor, false, 3, builderExit},
		{"loop_cap no PR exits", funnel.GoalResult{Done: true, Reason: "loop_cap"}, taskDoneHonor, false, 3, builderExit},
		{"blocked short-circuits before PR", funnel.GoalResult{Blocked: true, Done: true, Reason: "complete"}, taskDoneHonor, true, 3, builderExit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := builderDecide(tc.result, tc.acceptance, tc.prVerified, tc.prRepromptsLeft); got != tc.want {
				t.Errorf("builderDecide(%+v, acceptance=%d, pr=%v, left=%d) = %d, want %d", tc.result, tc.acceptance, tc.prVerified, tc.prRepromptsLeft, got, tc.want)
			}
		})
	}
}

// TestBuilderDecide_Acceptance covers NET-27: the judge-complete exit path
// must be gated on acceptance criteria exactly like task_done is, and the
// acceptance check takes priority over the PR check (no point opening/
// verifying a PR for output that doesn't meet the DoD yet).
func TestBuilderDecide_Acceptance(t *testing.T) {
	complete := funnel.GoalResult{Done: true, Reason: "complete"}
	cases := []struct {
		name       string
		acceptance taskDoneStep
		prVerified bool
		want       builderStep
	}{
		{"judge-complete + criteria met (honor) + PR verified exits", taskDoneHonor, true, builderExit},
		{"judge-complete + criteria not met, budget remains -> reprompt acceptance (not PR)", taskDoneReprompt, true, builderRepromptAcceptance},
		{"judge-complete + criteria never met, budget exhausted -> blocked (not PR)", taskDoneBlocked, true, builderBlockedAcceptance},
		{"acceptance reprompt wins even when PR already verified", taskDoneReprompt, true, builderRepromptAcceptance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := builderDecide(complete, tc.acceptance, tc.prVerified, 3); got != tc.want {
				t.Errorf("builderDecide(complete, acceptance=%d, pr=%v) = %d, want %d", tc.acceptance, tc.prVerified, got, tc.want)
			}
		})
	}
}

// TestBuilderDecide_AcceptanceGatesNonCompleteReasons is the bounded-residual
// regression (live-reproduced 2026-07-05 08:19): a rejected task_done mid-
// turn is a live completion CLAIM regardless of what that SAME turn's judge
// separately classified the overall reply as. Restricting the acceptance
// gate to reason=="complete" let a scratch/loop_cap/unknown_class-classified
// turn silently override the rejection with an unconditional exit — the
// model's task_done was denied, but the goal-loop still walked away as if
// there were nothing left to verify. acceptance must win regardless of
// result.Reason whenever the caller (builderGoalLoop) decided to run the
// gate at all; only the PR check stays reason=="complete"-scoped (a
// scratch/loop_cap/unknown_class turn never claimed a PR-worthy completion).
func TestBuilderDecide_AcceptanceGatesNonCompleteReasons(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		acceptance taskDoneStep
		want       builderStep
	}{
		{"scratch + rejected task_done -> reprompt acceptance, not a silent exit", "scratch", taskDoneReprompt, builderRepromptAcceptance},
		{"scratch + budget exhausted -> blocked, not a silent exit", "scratch", taskDoneBlocked, builderBlockedAcceptance},
		{"loop_cap + rejected task_done -> reprompt acceptance, not a silent exit", "loop_cap", taskDoneReprompt, builderRepromptAcceptance},
		{"unknown_class + rejected task_done -> reprompt acceptance, not a silent exit", "unknown_class", taskDoneReprompt, builderRepromptAcceptance},
		{"scratch + acceptance honors (no claim this turn) -> exit, no PR gate", "scratch", taskDoneHonor, builderExit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := funnel.GoalResult{Done: true, Reason: tc.reason}
			// prVerified/prRepromptsLeft are irrelevant whenever acceptance
			// isn't Honor, and for the Honor+non-complete case the PR gate
			// must not apply either (want builderExit regardless).
			if got := builderDecide(result, tc.acceptance, false, 3); got != tc.want {
				t.Errorf("builderDecide(%+v, acceptance=%d) = %d, want %d", result, tc.acceptance, got, tc.want)
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

// TestBuilderPRVerifierRepoLessBypassesGate covers Unit B item 3
// (respond-only completion, NET-22): a repo-less brief has no PR to gate
// on, so the verifier must return true unconditionally WITHOUT calling
// prExistsFn (a gh call for an empty repo would only ever error) — this is
// what stops builderGoalLoop from re-prompting the model to open a PR that
// can never exist (the anvil-builder "blocked after success" bug).
func TestBuilderPRVerifierRepoLessBypassesGate(t *testing.T) {
	orig := prExistsFn
	defer func() { prExistsFn = orig }()
	called := false
	prExistsFn = func(string, string) (bool, error) {
		called = true
		return false, errors.New("should never be called for a repo-less brief")
	}
	if ok := builderPRVerifier(slog.Default(), "anvil", "", "NET-22", "")(); !ok {
		t.Fatal("repo-less verifier must return true (PR gate skipped)")
	}
	if called {
		t.Fatal("repo-less verifier must not call prExistsFn at all")
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
