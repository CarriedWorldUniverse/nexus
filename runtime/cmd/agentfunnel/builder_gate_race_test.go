package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	bridlefake "github.com/CarriedWorldUniverse/bridle/fake"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// nopChatGateway is the minimal funnel.ChatGateway CommsRunner needs
// (it hard-requires a non-nil Gateway even for the task_done tool, which
// never actually posts chat) — every method is a no-op, this test only
// cares about the task_done/goal-loop race, not chat delivery.
type nopChatGateway struct{}

func (nopChatGateway) SendChat(context.Context, string, int64, string) (int64, error) { return 1, nil }
func (nopChatGateway) ReactTo(context.Context, int64, string) error                   { return nil }
func (nopChatGateway) ReadThread(context.Context, int64, int64) ([]funnel.ChatMessage, error) {
	return nil, nil
}
func (nopChatGateway) AnnounceFile(context.Context, string, string) (int64, error) { return 1, nil }
func (nopChatGateway) ShareFile(context.Context, string, []string) (int64, error)  { return 1, nil }
func (nopChatGateway) ReadMessage(context.Context, int64) (funnel.ChatMessage, error) {
	return funnel.ChatMessage{}, nil
}
func (nopChatGateway) ListShared(context.Context, int) ([]funnel.SharedFileRef, error) {
	return nil, nil
}
func (nopChatGateway) GetShared(context.Context, int64) (funnel.SharedFileRef, error) {
	return funnel.SharedFileRef{}, nil
}

// alwaysCompleteFilter is a funnel.OutputFilter stub that always classifies
// the turn FilterClassComplete — this test drives the goal-loop's OTHER exit
// path (the judge, not task_done), so the judge verdict itself is fixed and
// uninteresting; what's under test is what happens when task_done ALSO fires
// in the same turn.
type alwaysCompleteFilter struct{}

func (alwaysCompleteFilter) Judge(context.Context, funnel.FilterInput) funnel.FilterDecision {
	return funnel.FilterDecision{ShouldPost: true, Class: funnel.FilterClassComplete, GoalDone: true}
}

func quietTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuilderGoalLoop_MidFlightTaskDoneSharesGate is the regression test for
// review finding #3 (Unit B fix pass 2): a builder can call the task_done
// tool AND have the goal-loop's own judge classify that SAME turn
// FilterClassComplete — two independent completion signals racing in one
// turn. task_done fires first (synchronously, mid-turn, during the tool
// round) and its verdict is MET, so it calls stop() immediately — ctx is
// already canceled by the time Deliberate finishes the turn and the judge
// (alwaysCompleteFilter, standing in for a real judge that has no idea the
// turn is already being torn down) ALSO rates it complete. Pre-fix,
// builderGoalLoop would then call gate.Decide a SECOND time for output
// already judged — this test asserts the shared gate is consulted exactly
// ONCE for the whole turn (the ctx.Err() guard added alongside this test
// stops the goal-loop's own redundant check).
func TestBuilderGoalLoop_MidFlightTaskDoneSharesGate(t *testing.T) {
	calls := 0
	verify := func(context.Context, string, string) (funnel.AcceptanceVerdict, error) {
		calls++
		return funnel.AcceptanceVerdict{Met: true, Reason: "token present"}, nil
	}
	gate := newBuilderAcceptanceGate("must contain CONVERGED-OK", verify)

	// Step 1: the model calls task_done mid-turn. Step 2: the harness's
	// tool-loop re-invokes the (fake) provider for the next round, which
	// emits the final assistant text — the turn then completes normally
	// (Deliberate returns) even though task_done already called stop().
	fakeProvider := bridlefake.NewProvider(
		bridlefake.Step{
			ToolCalls: []bridle.ToolInvocation{{ID: "1", Name: "task_done", Args: []byte(`{"summary":"0 conflicts, 100% memory match"}`)}},
		},
		bridlefake.Step{Text: "hello there, all done"},
	)
	toolRunner := bridlefake.NewToolRunner(nil) // task_done is intercepted by CommsRunner before reaching this

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	onDone := builderOnTaskDone(cancel, quietTestLogger(), "keel", gate, nil,
		func() *funnel.Funnel { return nil }, // funnelFor: nil is fine, reprompt path just no-ops on ReceiveSynthetic
		func() int64 { return 0 }, nil, "")

	commsRunner := funnel.CommsRunner{
		Gateway:    nopChatGateway{},
		AspectID:   "keel",
		OnTaskDone: onDone,
	}
	// Fan out to toolRunner for any non-comms tool (none expected here).
	runner := funnel.ComposeRunner(commsRunner, toolRunner)

	f, err := funnel.New(funnel.Config{
		AspectID:     "keel",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(fakeProvider),
		Provider:     "fake",
		Model:        "fake-model",
		Runner:       runner,
		Filter:       alwaysCompleteFilter{},
		ChatGateway:  nopChatGateway{},
	})
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	f.Receive(bridle.InboxItem{From: "dispatch", Content: "do the thing"})

	// Drive the REAL builderGoalLoop (not a manual Pursue call) so the
	// production ctx.Err() guard is what's under test: task_done fires
	// INSIDE the first Deliberate call (via the tool round) and cancels
	// ctx; alwaysCompleteFilter still classifies that SAME turn "complete"
	// (it ignores ctx, standing in for a real judge that doesn't know the
	// turn is already being torn down) — pre-fix, builderGoalLoop would
	// then call gate.Decide a SECOND time for the same turn's output.
	goalCfg := funnel.GoalConfig{TicketID: "NET-1", DoD: "must contain CONVERGED-OK"}
	verifyPR := func() bool { return true } // repo-less-style bypass; irrelevant to this test
	openPR := func() (string, bool) { return "", false }

	done := make(chan struct{})
	go func() {
		builderGoalLoop(ctx, f, quietTestLogger(), goalCfg, verifyPR, openPR, cancel, gate, nil, "")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("builderGoalLoop did not return — should terminate once ctx is canceled by task_done")
	}

	if ctx.Err() == nil {
		t.Fatal("task_done must have canceled ctx during the turn (mid-flight completion signal)")
	}
	if calls != 1 {
		t.Errorf("expected exactly ONE verify call (shared gate; the ctx.Err() guard must stop the goal-loop's own acceptance check from re-deciding the same turn), got %d", calls)
	}
	// Met verdicts never spend reprompt budget (only taskDoneReprompt does) —
	// confirms the goal-loop's own gate.Decide truly never ran a second time,
	// rather than having coincidentally also returned Honor without touching
	// the counter.
	if gate.repromptsLeft != builderAcceptanceRepromptCap {
		t.Errorf("shared gate budget = %d, want %d (a met verdict spends no budget)", gate.repromptsLeft, builderAcceptanceRepromptCap)
	}
}
