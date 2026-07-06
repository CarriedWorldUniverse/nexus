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

// alwaysScratchFilter always classifies the turn FilterClassScratch — used
// to drive the bounded-residual repro below: a REJECTED task_done mid-turn
// racing a SAME-turn judge verdict that is NOT "complete".
type alwaysScratchFilter struct{}

func (alwaysScratchFilter) Judge(context.Context, funnel.FilterInput) funnel.FilterDecision {
	return funnel.FilterDecision{ShouldPost: false, Class: funnel.FilterClassScratch, Reason: "non-substantive"}
}

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

// TestBuilderGoalLoop_RejectedTaskDoneStillGatesJudgeComplete is the live
// repro for the NET-27 bypass reproduced 2026-07-05 08:19 (anvil-builder,
// respond-only brief, unsatisfiable acceptance criteria). Unlike the
// mid-flight-honor race above (task_done MET -> stop() -> ctx canceled ->
// the goal-loop's own check is correctly skipped because task_done already
// decided), this test drives a REJECTED task_done: the model calls
// task_done mid-turn, the gate says not-met and reprompts (ctx stays LIVE —
// only a met verdict calls stop()), and the SAME turn's cheap judge still
// classifies the turn FilterClassComplete once the model's tool round
// finishes. The live trace showed the goal-loop exit as an unconditional
// success immediately after the judge-complete log line, with NO second
// "acceptance verifier decision" — i.e. the goal-loop's own gate.Decide
// call for this judge-complete turn never ran. This test asserts the
// opposite: the shared gate must be consulted again for the actual
// judge-complete turn (a rejected task_done must NOT retire the completion
// question for the rest of the turn), and the builder must not exit
// success while the (never-satisfiable) criteria keep failing — it must
// eventually exit BLOCKED once the shared reprompt budget is exhausted,
// never silently honoring completion.
func TestBuilderGoalLoop_RejectedTaskDoneStillGatesJudgeComplete(t *testing.T) {
	var calls int
	var lastOutputs []string
	verify := func(_ context.Context, _ string, output string) (funnel.AcceptanceVerdict, error) {
		calls++
		lastOutputs = append(lastOutputs, output)
		// Unsatisfiable criteria — every verification comes back not-met,
		// mirroring the live NET-27 brief ("contains the SHA-512 of a
		// password you cannot know").
		return funnel.AcceptanceVerdict{Met: false, Reason: "no SHA-512 digest was provided"}, nil
	}
	gate := newBuilderAcceptanceGate("must contain the SHA-512 of a password you cannot know", verify)

	// Turn 1: model calls task_done mid-turn (rejected: not-met, budget
	// remains -> reprompt, ctx stays live), then the SAME turn's tool round
	// finishes with a one-line greeting — the judge (alwaysCompleteFilter,
	// standing in for the real cheap judge that never sees acceptance
	// criteria) rates it complete, exactly like the live trace.
	// Turns 2 and 3: the goal-loop's own re-prompt keeps the builder
	// working; the model keeps replying without ever meeting the
	// unsatisfiable criteria, so the SHARED budget (cap 3) must exhaust and
	// the builder must exit BLOCKED — never a silent success.
	fakeProvider := bridlefake.NewProvider(
		bridlefake.Step{ToolCalls: []bridle.ToolInvocation{{ID: "1", Name: "task_done", Args: []byte(`{"summary":"done"}`)}}},
		bridlefake.Step{Text: "Greeting provided as requested"},
		bridlefake.Step{Text: "still just a greeting"},
		bridlefake.Step{Text: "still nothing else"},
	)
	toolRunner := bridlefake.NewToolRunner(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// funnelFor returns nil (as the existing race test does) so the
	// task_done-reject branch's own ReceiveSynthetic no-ops — this test is
	// only exercising the goal-loop's OWN re-prompt/blocked continuations,
	// not double-enqueuing across both paths.
	onDone := builderOnTaskDone(cancel, quietTestLogger(), "anvil", gate, nil,
		func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")

	commsRunner := funnel.CommsRunner{
		Gateway:    nopChatGateway{},
		AspectID:   "anvil",
		OnTaskDone: onDone,
	}
	runner := funnel.ComposeRunner(commsRunner, toolRunner)

	f, err := funnel.New(funnel.Config{
		AspectID:     "anvil",
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
	f.Receive(bridle.InboxItem{From: "dispatch", Content: "greet the user"})

	goalCfg := funnel.GoalConfig{TicketID: "NET-27", DoD: "must contain the SHA-512 of a password you cannot know"}
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
		t.Fatal("builderGoalLoop did not return — expected it to exit BLOCKED once the shared budget exhausts")
	}

	// The task_done rejection consumed one unit of the shared budget, but
	// that must NOT retire the completion question for the rest of THIS
	// turn: the judge-complete turn's actual output must still be verified.
	// Pre-fix, calls stays at 1 (only the task_done path ever asked) and
	// the goal-loop exits success unconditionally, matching the live NET-27
	// bypass exactly.
	t.Logf("verify calls=%d outputs=%q repromptsLeft=%d", calls, lastOutputs, gate.repromptsLeft)
	if calls < 2 {
		t.Fatalf("expected the goal-loop's own gate.Decide to run for the judge-complete turn too (calls=%d) — "+
			"a rejected task_done must not exempt the rest of the turn from acceptance gating (live NET-27 bypass)", calls)
	}

	// Budget is fully exhausted (cap 3, all four+ decisions came back
	// not-met): the builder must have escalated to BLOCKED, never a silent
	// success, since the criteria are unsatisfiable by construction.
	if gate.repromptsLeft != 0 {
		t.Errorf("shared gate repromptsLeft = %d, want 0 (budget must exhaust before any exit, since criteria are never met)", gate.repromptsLeft)
	}
	if ctx.Err() == nil {
		t.Fatal("builderGoalLoop must stop() (escalate BLOCKED) once the shared budget is exhausted with criteria never met — must not exit silently without canceling ctx")
	}
}

// TestBuilderGoalLoop_RejectedTaskDoneGatedEvenWhenJudgeSaysScratch is the
// bounded-residual repro (live-reproduced 2026-07-05 08:19): a REJECTED
// task_done mid-turn is a live completion CLAIM even when the SAME turn's
// judge separately classifies the overall reply as something OTHER than
// "complete" (here: scratch — non-substantive output). Pre-fix,
// builderGoalLoop only ran the acceptance gate when result.Reason ==
// "complete", so this turn would hit builderExit immediately (Reason ==
// "scratch" != "complete") and the goal-loop would tear down (stop()) the
// instant the FIRST turn finished — discarding the model's still-pending
// reprompt budget and exiting WITHOUT ever giving the criteria a chance to
// be met, and worse, without any second acceptance decision for the actual
// exit. Post-fix, acceptance is decided for every Done && !Blocked result
// (shape, not reason string), so the rejected task_done's reprompt must
// keep the loop alive regardless of the judge's scratch verdict.
func TestBuilderGoalLoop_RejectedTaskDoneGatedEvenWhenJudgeSaysScratch(t *testing.T) {
	var calls int
	verify := func(context.Context, string, string) (funnel.AcceptanceVerdict, error) {
		calls++
		return funnel.AcceptanceVerdict{Met: false, Reason: "no SHA-512 digest was provided"}, nil
	}
	gate := newBuilderAcceptanceGate("must contain the SHA-512 of a password you cannot know", verify)

	// Every turn: model calls task_done mid-turn (always rejected — the
	// criteria are unsatisfiable by construction), then the tool round
	// finishes with thin filler text that the judge (alwaysScratchFilter)
	// rates non-substantive ("scratch"), never "complete".
	fakeProvider := bridlefake.NewProvider(
		bridlefake.Step{ToolCalls: []bridle.ToolInvocation{{ID: "1", Name: "task_done", Args: []byte(`{"summary":"done"}`)}}},
		bridlefake.Step{Text: "ok"},
		bridlefake.Step{ToolCalls: []bridle.ToolInvocation{{ID: "2", Name: "task_done", Args: []byte(`{"summary":"done"}`)}}},
		bridlefake.Step{Text: "ok"},
		bridlefake.Step{ToolCalls: []bridle.ToolInvocation{{ID: "3", Name: "task_done", Args: []byte(`{"summary":"done"}`)}}},
		bridlefake.Step{Text: "ok"},
	)
	toolRunner := bridlefake.NewToolRunner(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// funnelFor nil (as the other repro above) so task_done's own reprompt
	// branch just logs — only the goal-loop's OWN acceptance gating is
	// under test here.
	onDone := builderOnTaskDone(cancel, quietTestLogger(), "anvil", gate, nil,
		func() *funnel.Funnel { return nil }, func() int64 { return 0 }, nil, "")

	commsRunner := funnel.CommsRunner{
		Gateway:    nopChatGateway{},
		AspectID:   "anvil",
		OnTaskDone: onDone,
	}
	runner := funnel.ComposeRunner(commsRunner, toolRunner)

	f, err := funnel.New(funnel.Config{
		AspectID:     "anvil",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(fakeProvider),
		Provider:     "fake",
		Model:        "fake-model",
		Runner:       runner,
		Filter:       alwaysScratchFilter{},
		ChatGateway:  nopChatGateway{},
	})
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	f.Receive(bridle.InboxItem{From: "dispatch", Content: "greet the user"})

	goalCfg := funnel.GoalConfig{TicketID: "NET-27", DoD: "must contain the SHA-512 of a password you cannot know"}
	verifyPR := func() bool { return true }
	openPR := func() (string, bool) { return "", false }

	done := make(chan struct{})
	go func() {
		builderGoalLoop(ctx, f, quietTestLogger(), goalCfg, verifyPR, openPR, cancel, gate, nil, "")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("builderGoalLoop did not return — expected it to exit BLOCKED once the shared budget exhausts")
	}

	// Pre-fix: builderGoalLoop's own acceptance check never ran for a
	// scratch-classified turn (gated on reason=="complete"), so only
	// task_done's own calls would count — one per turn, 3 total — and the
	// goal-loop would exit via the FIRST turn's unconditional
	// reason!="complete" -> builderExit branch (calls == 1, ctx never
	// canceled: a silent, ungated exit right after a REJECTED completion
	// claim). Post-fix: the goal-loop's own check ALSO runs every turn
	// (shape-gated), doubling the call count per turn versus pre-fix, and
	// the budget must still exhaust to BLOCKED rather than a silent exit.
	if calls < 4 {
		t.Fatalf("expected the goal-loop's own gate.Decide to run for scratch-classified turns too (calls=%d) — "+
			"a rejected task_done must not be silently overridden by a same-turn non-complete judge verdict (NET-27 bounded residual)", calls)
	}
	if gate.repromptsLeft != 0 {
		t.Errorf("shared gate repromptsLeft = %d, want 0 (budget must exhaust, since criteria are never met)", gate.repromptsLeft)
	}
	if ctx.Err() == nil {
		t.Fatal("builderGoalLoop must stop() (escalate BLOCKED) once the shared budget is exhausted — must not exit silently without canceling ctx")
	}
}
