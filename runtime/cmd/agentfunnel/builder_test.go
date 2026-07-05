package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// TestBuilderOnTaskDoneCancels covers the no-criteria back-compat path: a
// dispatch with no captured acceptance criteria (criteria="") honors
// task_done unconditionally, exactly like before Unit B.
func TestBuilderOnTaskDoneCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gate := newBuilderAcceptanceGate("", nil)
	onDone := builderOnTaskDone(cancel, slog.Default(), "anvil", gate, nil,
		func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")

	onDone("PR opened")

	select {
	case <-ctx.Done():
	default:
		t.Fatal("OnTaskDone did not cancel the context")
	}
}

// TestVerificationInput covers the review-finding fix (Unit B fix pass 2):
// the verifier must see the REAL posted turn output, not just a task_done
// self-report — a model that writes "posted CONVERGED-BETA-OK" as its OWN
// summary text without ever having produced it must not pass on the
// self-report alone. When realOutputFor yields non-empty text it is
// included and labeled authoritative; empty degrades to summary-only
// (tracker not wired, or nothing streamed yet at the moment task_done fired).
func TestVerificationInput(t *testing.T) {
	t.Run("empty real output degrades to summary only", func(t *testing.T) {
		got := verificationInput("0 conflicts, 100% memory match", func() string { return "" })
		if got != "0 conflicts, 100% memory match" {
			t.Errorf("got %q, want the bare summary unchanged", got)
		}
	})
	t.Run("nil realOutputFor degrades to summary only", func(t *testing.T) {
		got := verificationInput("summary text", nil)
		if got != "summary text" {
			t.Errorf("got %q, want the bare summary unchanged", got)
		}
	})
	t.Run("non-empty real output is included and labeled authoritative", func(t *testing.T) {
		got := verificationInput("0 conflicts, 100% memory match", func() string { return "hello there" })
		if !strings.Contains(got, "0 conflicts, 100% memory match") {
			t.Errorf("combined input lost the self-report: %q", got)
		}
		if !strings.Contains(got, "hello there") {
			t.Errorf("combined input lost the real output: %q", got)
		}
		if !strings.Contains(got, "authoritative") {
			t.Errorf("combined input does not mark the real output authoritative: %q", got)
		}
	})
}

// TestTaskDoneDecide covers Unit B's verified-task_done decision matrix
// (NET-22/23/24): no criteria / unavailable verifier / verify error all
// fail OPEN (honor); met honors; not-met reprompts while budget remains,
// then blocks once exhausted.
func TestTaskDoneDecide(t *testing.T) {
	cases := []struct {
		name          string
		hasCriteria   bool
		verifierAvail bool
		verifyErr     error
		met           bool
		repromptsLeft int
		want          taskDoneStep
	}{
		{"no criteria honors unconditionally", false, true, nil, false, 3, taskDoneHonor},
		{"verifier unavailable fails open", true, false, nil, false, 3, taskDoneHonor},
		{"verify error fails open", true, true, errors.New("boom"), false, 3, taskDoneHonor},
		{"met honors", true, true, nil, true, 3, taskDoneHonor},
		{"not met reprompts while budget remains", true, true, nil, false, 3, taskDoneReprompt},
		{"not met reprompts with one left", true, true, nil, false, 1, taskDoneReprompt},
		{"not met blocks once budget exhausted", true, true, nil, false, 0, taskDoneBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskDoneDecide(tc.hasCriteria, tc.verifierAvail, tc.verifyErr, tc.met, tc.repromptsLeft); got != tc.want {
				t.Errorf("taskDoneDecide(criteria=%v, avail=%v, err=%v, met=%v, left=%d) = %d, want %d",
					tc.hasCriteria, tc.verifierAvail, tc.verifyErr, tc.met, tc.repromptsLeft, got, tc.want)
			}
		})
	}
}

// TestBuilderOnTaskDoneVerified drives the full I/O wrapper with a fake
// judge: met exits immediately; not-met re-prompts via ReceiveSynthetic and
// keeps the builder running (no stop) until the reprompt budget is
// exhausted, at which point it sends a "failed" dispatch status and stops
// (BLOCKED, never a silent success — NET-24). A nil verify func always
// fails open regardless of criteria (judge unbuildable at startup).
func TestBuilderOnTaskDoneVerified(t *testing.T) {
	t.Run("met exits", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		verify := func(context.Context, string, string) (funnel.AcceptanceVerdict, error) {
			return funnel.AcceptanceVerdict{Met: true, Reason: "token present"}, nil
		}
		gate := newBuilderAcceptanceGate("criteria text", verify)
		onDone := builderOnTaskDone(cancel, slog.Default(), "plumb", gate, nil,
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")
		onDone("CONVERGED-ALPHA-OK")
		if ctx.Err() == nil {
			t.Fatal("met verdict must exit (stop called)")
		}
	})

	t.Run("not met reprompts then blocks after budget exhausted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		verify := func(context.Context, string, string) (funnel.AcceptanceVerdict, error) {
			return funnel.AcceptanceVerdict{Met: false, Reason: "required token missing"}, nil
		}
		gate := newBuilderAcceptanceGate("criteria text", verify)
		onDone := builderOnTaskDone(cancel, slog.Default(), "keel", gate, nil,
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")

		for i := 0; i < builderAcceptanceRepromptCap; i++ {
			onDone("0 conflicts, 100% memory match")
			if ctx.Err() != nil {
				t.Fatalf("call %d: rejected task_done stopped early (budget not yet exhausted)", i)
			}
		}
		// Budget now exhausted — the next rejection must block (exit), not loop forever.
		onDone("0 conflicts, 100% memory match")
		if ctx.Err() == nil {
			t.Fatal("exhausted reprompt budget must exit (BLOCKED), not keep looping")
		}
	})

	t.Run("judge error fails open", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		verify := func(context.Context, string, string) (funnel.AcceptanceVerdict, error) {
			return funnel.AcceptanceVerdict{}, errors.New("judge subprocess crashed")
		}
		gate := newBuilderAcceptanceGate("criteria text", verify)
		onDone := builderOnTaskDone(cancel, slog.Default(), "anvil", gate, nil,
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")
		onDone("summary")
		if ctx.Err() == nil {
			t.Fatal("judge error must fail open (exit), not block completion on the judge")
		}
	})

	t.Run("nil verify func fails open even with criteria", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		gate := newBuilderAcceptanceGate("criteria text", nil)
		onDone := builderOnTaskDone(cancel, slog.Default(), "anvil", gate, nil,
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")
		onDone("summary")
		if ctx.Err() == nil {
			t.Fatal("nil verifier must fail open (exit)")
		}
	})

	t.Run("real output overrides a confabulated self-report", func(t *testing.T) {
		// NET-24 exact shape: the model's task_done summary claims success
		// ("0 conflicts, 100% memory match") but the judge sees the REAL
		// turn output too (via realOutputFor) and that's what actually
		// determines the verdict — here the real output DOES contain the
		// required token even though the summary is generic, so it must honor.
		ctx, cancel := context.WithCancel(context.Background())
		var gotOutput string
		verify := func(_ context.Context, _ string, output string) (funnel.AcceptanceVerdict, error) {
			gotOutput = output
			met := strings.Contains(output, "CONVERGED-ALPHA-OK")
			return funnel.AcceptanceVerdict{Met: met, Reason: "token check"}, nil
		}
		gate := newBuilderAcceptanceGate("must contain CONVERGED-ALPHA-OK", verify)
		onDone := builderOnTaskDone(cancel, slog.Default(), "plumb", gate,
			func() string { return "the real streamed reply: CONVERGED-ALPHA-OK" },
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")
		onDone("0 conflicts, 100% memory match")
		if ctx.Err() == nil {
			t.Fatal("real output contains the token — must honor (exit)")
		}
		if !strings.Contains(gotOutput, "CONVERGED-ALPHA-OK") {
			t.Errorf("verifier did not see the real output: %q", gotOutput)
		}
		if !strings.Contains(gotOutput, "0 conflicts, 100% memory match") {
			t.Errorf("verifier lost the self-report entirely: %q", gotOutput)
		}
	})
}

// TestBuilderAcceptanceGate_SharedBudgetAcrossPaths is the regression test
// for review finding #3: task_done and the goal-loop's judge-complete exit
// must share ONE arbiter with ONE reprompt budget — a builder that races
// both paths (calls task_done mid-turn while the SAME turn's judge is also
// about to rule complete) must not get 2x the intended reprompt budget.
func TestBuilderAcceptanceGate_SharedBudgetAcrossPaths(t *testing.T) {
	calls := 0
	verify := func(context.Context, string, string) (funnel.AcceptanceVerdict, error) {
		calls++
		return funnel.AcceptanceVerdict{Met: false, Reason: "not there yet"}, nil
	}
	gate := newBuilderAcceptanceGate("criteria", verify)

	// Simulate the two call sites interleaving: task_done fires (path 1),
	// then the goal-loop's own judge-complete check fires (path 2) on the
	// SAME gate — as they would when both share one builderAcceptanceGate
	// constructed once in main() and passed to both builderOnTaskDone and
	// builderGoalLoop.
	steps := []taskDoneStep{}
	for i := 0; i < builderAcceptanceRepromptCap+2; i++ {
		var step taskDoneStep
		if i%2 == 0 {
			step, _ = gate.Decide(context.Background(), "task_done path output", slog.Default())
		} else {
			step, _ = gate.Decide(context.Background(), "goal-loop path output", slog.Default())
		}
		steps = append(steps, step)
	}
	if calls != builderAcceptanceRepromptCap+2 {
		t.Fatalf("expected every Decide call to invoke verify (fail-open needs a fresh read each time), got %d calls", calls)
	}
	blockedAt := -1
	for i, s := range steps {
		if s == taskDoneBlocked {
			blockedAt = i
			break
		}
	}
	if blockedAt != builderAcceptanceRepromptCap {
		t.Fatalf("shared budget must exhaust after exactly %d reprompts regardless of which path called Decide; blocked at index %d, steps=%v",
			builderAcceptanceRepromptCap, blockedAt, steps)
	}
}

// TestReadAcceptanceCriteriaFile covers review finding #4: an unreadable
// -acceptance-file (e.g. a ConfigMap-mount race — the volume not yet
// materialized) must WARN and fail open (empty criteria), never crash the
// process the way -role-file/-policy-fragment-file's fail() does.
func TestReadAcceptanceCriteriaFile(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if got := readAcceptanceCriteriaFile("", log); got != "" {
		t.Errorf("empty path: got %q, want empty", got)
	}
	if got := readAcceptanceCriteriaFile("/nonexistent/path/acceptance.md", log); got != "" {
		t.Errorf("unreadable path must fail open (empty), got %q", got)
	}

	dir := t.TempDir()
	path := dir + "/acceptance.md"
	if err := os.WriteFile(path, []byte("  - must contain X  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readAcceptanceCriteriaFile(path, log); got != "- must contain X" {
		t.Errorf("got %q, want trimmed file contents", got)
	}
}

func TestBuilderReplyTopicOnlyAppliesInBuilderMode(t *testing.T) {
	if got := builderReplyTopic(true, "NEX-443"); got != "NEX-443" {
		t.Errorf("builderReplyTopic(true): got %q, want NEX-443", got)
	}
	if got := builderReplyTopic(false, "NEX-443"); got != "" {
		t.Errorf("builderReplyTopic(false): got %q, want empty", got)
	}
}
