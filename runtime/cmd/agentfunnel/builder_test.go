package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// TestBuilderOnTaskDoneCancels covers the no-criteria back-compat path: a
// dispatch with no captured acceptance criteria (criteria="") honors
// task_done unconditionally, exactly like before Unit B.
func TestBuilderOnTaskDoneCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	onDone := builderOnTaskDone(cancel, slog.Default(), "anvil", "", nil,
		func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")

	onDone("PR opened")

	select {
	case <-ctx.Done():
	default:
		t.Fatal("OnTaskDone did not cancel the context")
	}
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
		onDone := builderOnTaskDone(cancel, slog.Default(), "plumb", "criteria text", verify,
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
		onDone := builderOnTaskDone(cancel, slog.Default(), "keel", "criteria text", verify,
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
		onDone := builderOnTaskDone(cancel, slog.Default(), "anvil", "criteria text", verify,
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")
		onDone("summary")
		if ctx.Err() == nil {
			t.Fatal("judge error must fail open (exit), not block completion on the judge")
		}
	})

	t.Run("nil verify func fails open even with criteria", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		onDone := builderOnTaskDone(cancel, slog.Default(), "anvil", "criteria text", nil,
			func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")
		onDone("summary")
		if ctx.Err() == nil {
			t.Fatal("nil verifier must fail open (exit)")
		}
	})
}

func TestBuilderReplyTopicOnlyAppliesInBuilderMode(t *testing.T) {
	if got := builderReplyTopic(true, "NEX-443"); got != "NEX-443" {
		t.Errorf("builderReplyTopic(true): got %q, want NEX-443", got)
	}
	if got := builderReplyTopic(false, "NEX-443"); got != "" {
		t.Errorf("builderReplyTopic(false): got %q, want empty", got)
	}
}
