package funnel

import (
	"context"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// scriptedFilter returns a predefined FilterDecision for testing.
type scriptedFilter struct {
	decision FilterDecision
}

func (f scriptedFilter) Judge(_ context.Context, _ FilterInput) FilterDecision {
	return f.decision
}

// scriptedRecordingFilter captures each FilterInput it judges and returns the
// next queued decision (defaulting to complete when the queue empties).
// Used to verify GoalLoop threads prior-turn text through the funnel
// path (NEX-249) and to script multi-turn class sequences.
type scriptedRecordingFilter struct {
	next     []FilterDecision
	received []FilterInput
}

func (f *scriptedRecordingFilter) Judge(_ context.Context, in FilterInput) FilterDecision {
	f.received = append(f.received, in)
	if len(f.next) == 0 {
		return FilterDecision{ShouldPost: true, Class: FilterClassComplete}
	}
	d := f.next[0]
	f.next = f.next[1:]
	return d
}

func TestGoalLoop_PursueComplete(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "done", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: true,
		Class:      FilterClassComplete,
		Reason:     "done",
	}}
	f.Receive(bridle.InboxItem{From: "operator", Content: "do the thing", MsgID: 1})

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Implement the goal loop",
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Errorf("expected Done=true on complete, got %+v", result)
	}
	if result.Blocked {
		t.Errorf("expected Blocked=false on complete")
	}
	if result.TurnsRun != 1 {
		t.Errorf("expected TurnsRun=1, got %d", result.TurnsRun)
	}
}

func TestGoalLoop_PursueGoalNotMet(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "still working", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: true,
		Class:      FilterClassGoalNotMet,
		Reason:     "more work needed",
	}}
	f.Receive(bridle.InboxItem{From: "operator", Content: "do the thing", MsgID: 1})

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Implement the goal loop",
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Errorf("expected Done=false on goal_not_met, got %+v", result)
	}
	if result.Reason != "goal_not_met" {
		t.Errorf("expected Reason=goal_not_met, got %q", result.Reason)
	}
	if result.TurnsRun != 1 {
		t.Errorf("expected TurnsRun=1, got %d", result.TurnsRun)
	}

	// Check that a continuation brief was enqueued.
	if f.InboxLen() == 0 {
		t.Fatal("expected a continuation inbox item to be enqueued")
	}
}

func TestGoalLoop_PursueBlocked(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "blocked on external dep", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: false,
		Class:      FilterClassBlocked,
		Reason:     "waiting on operator",
	}}
	f.Receive(bridle.InboxItem{From: "operator", Content: "do the thing", MsgID: 1})

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Implement the goal loop",
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Blocked {
		t.Errorf("expected Blocked=true, got %+v", result)
	}
	if result.TurnsRun != 1 {
		t.Errorf("expected TurnsRun=1, got %d", result.TurnsRun)
	}
}

func TestGoalLoop_PursueLoopCap(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "not done", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: true,
		Class:      FilterClassGoalNotMet,
		Reason:     "more work",
	}}

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Implement the goal loop",
		MaxTurns: 2,
	})

	// Run turn 1: should continue.
	f.Receive(bridle.InboxItem{From: "operator", Content: "do it", MsgID: 1})
	result1, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if result1.Done {
		t.Errorf("turn 1: expected not done")
	}

	// Run turn 2: should hit cap.
	f.Receive(bridle.InboxItem{From: "system", Content: "continue", MsgID: 0})
	result2, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !result2.Done {
		t.Errorf("turn 2: expected Done=true on loop cap")
	}
	if result2.Reason != "loop_cap" {
		t.Errorf("turn 2: expected Reason=loop_cap, got %q", result2.Reason)
	}
	if result2.TurnsRun != 2 {
		t.Errorf("turn 2: expected TurnsRun=2, got %d", result2.TurnsRun)
	}
}

func TestGoalLoop_PursueNoDoD(t *testing.T) {
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "", // empty
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Errorf("expected Done=true when DoD is empty")
	}
	if result.Reason != "no_dod" {
		t.Errorf("expected Reason=no_dod, got %q", result.Reason)
	}
}

func TestGoalLoop_PursueEmptyInbox(t *testing.T) {
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Some DoD",
	})
	// Don't enqueue anything — inbox is empty.
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Errorf("expected Done=true on empty inbox")
	}
	if result.Reason != "empty_inbox" {
		t.Errorf("expected Reason=empty_inbox, got %q", result.Reason)
	}
}

func TestGoalLoop_BuildContinuationBrief(t *testing.T) {
	gl := NewGoalLoop(nil, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Implement the goal loop.\n- Pass tests\n- Ship it",
		MaxTurns: 20,
	})
	gl.turnCount = 3
	brief := gl.buildContinuationBrief("I made progress on the implementation.")
	if !strings.Contains(brief, "[CONTINUATION]") {
		t.Errorf("brief should contain [CONTINUATION] marker: %s", brief)
	}
	if !strings.Contains(brief, "NEX-210") {
		t.Errorf("brief should contain ticket ID: %s", brief)
	}
	if !strings.Contains(brief, "3/20") {
		t.Errorf("brief should contain turn counter: %s", brief)
	}
	if !strings.Contains(brief, "Implement the goal loop") {
		t.Errorf("brief should contain DoD: %s", brief)
	}
	if !strings.Contains(brief, "I made progress") {
		t.Errorf("brief should contain prior output excerpt: %s", brief)
	}
}

func TestGoalLoop_LegacyFilterDerivesClass(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "done", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	// Legacy filter that leaves Class empty.
	f.cfg.Filter = AlwaysPostFilter{}
	f.Receive(bridle.InboxItem{From: "operator", Content: "do it", MsgID: 1})

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-210",
		DoD:      "Test legacy filter",
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Done {
		t.Errorf("legacy filter should result in Done=true (derived complete)")
	}
	if result.Reason != "complete" {
		t.Errorf("expected Reason=complete, got %q", result.Reason)
	}
}

func TestGoalLoop_ThreadRootFlowsToContinuation(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "still working", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: true,
		Class:      FilterClassGoalNotMet,
		Reason:     "more work needed",
	}}
	f.Receive(bridle.InboxItem{From: "operator", Content: "do the thing", MsgID: 1})

	const threadRoot int64 = 42
	gl := NewGoalLoop(f, GoalConfig{
		TicketID:   "NEX-225",
		DoD:        "Test thread root propagation",
		ThreadRoot: threadRoot,
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Errorf("expected Done=false on goal_not_met, got %+v", result)
	}

	if f.InboxLen() == 0 {
		t.Fatal("expected a continuation inbox item to be enqueued")
	}
	item := f.inbox[len(f.inbox)-1]
	if item.ThreadRoot != threadRoot {
		t.Errorf("expected ThreadRoot=%d on continuation item, got %d", threadRoot, item.ThreadRoot)
	}
}

func TestGoalLoop_ZeroThreadRootDefault(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "still working", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: true,
		Class:      FilterClassGoalNotMet,
		Reason:     "more work needed",
	}}
	f.Receive(bridle.InboxItem{From: "operator", Content: "do the thing", MsgID: 1})

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-225",
		DoD:      "Test zero ThreadRoot regression",
	})
	result, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Done {
		t.Errorf("expected Done=false on goal_not_met, got %+v", result)
	}

	if f.InboxLen() == 0 {
		t.Fatal("expected a continuation inbox item to be enqueued")
	}
	item := f.inbox[len(f.inbox)-1]
	if item.ThreadRoot != 0 {
		t.Errorf("expected ThreadRoot=0 on continuation item (ContextGlobal default), got %d", item.ThreadRoot)
	}
}

// NEX-249 fix A: GoalLoop threads each turn's FinalText into the next
// judge's PriorTurnFinalText so the judge can detect repetition. First
// turn has empty prior; second turn carries first turn's reply.
func TestGoalLoop_PriorTurnTextThreadedToFunnel(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "turn one reply", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "turn two reply", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	rec := &scriptedRecordingFilter{
		next: []FilterDecision{
			{ShouldPost: true, Class: FilterClassGoalNotMet},
			{ShouldPost: true, Class: FilterClassComplete},
		},
	}
	f.cfg.Filter = rec
	f.Receive(bridle.InboxItem{From: "operator", Content: "go", MsgID: 1})

	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-249",
		DoD:      "Drive the loop",
		MaxTurns: 5,
	})
	if _, err := gl.Pursue(context.Background()); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := gl.Pursue(context.Background()); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	if len(rec.received) != 2 {
		t.Fatalf("expected 2 judge invocations, got %d", len(rec.received))
	}
	if rec.received[0].PriorTurnFinalText != "" {
		t.Errorf("turn 1 should have empty PriorTurnFinalText (no prior); got %q",
			rec.received[0].PriorTurnFinalText)
	}
	if rec.received[1].PriorTurnFinalText != "turn one reply" {
		t.Errorf("turn 2 should carry turn 1's reply as prior; got %q",
			rec.received[1].PriorTurnFinalText)
	}
}

// NEX-249 fix B: consecutiveGoalNotMetCap escalates a stuck judge
// to Blocked regardless of MaxTurns, with Reason "repeated_goal_not_met".
// Caller (Frame outer for-loop) sees Blocked=true and exits.
func TestGoalLoop_ConsecutiveGoalNotMetEscalatesToBlocked(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "lurking 1", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "lurking 2", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "lurking 3", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	// Judge keeps returning goal_not_met — simulating the bug.
	f.cfg.Filter = scriptedFilter{FilterDecision{
		ShouldPost: true,
		Class:      FilterClassGoalNotMet,
	}}
	f.Receive(bridle.InboxItem{From: "operator", Content: "go", MsgID: 1})

	// MaxTurns higher than the cap so the cap is what stops us.
	gl := NewGoalLoop(f, GoalConfig{
		TicketID: "NEX-249",
		DoD:      "Some DoD",
		MaxTurns: 20,
	})
	var last GoalResult
	for i := 0; i < consecutiveGoalNotMetCap; i++ {
		r, err := gl.Pursue(context.Background())
		if err != nil {
			t.Fatalf("Pursue %d: %v", i+1, err)
		}
		last = r
	}
	if !last.Blocked {
		t.Errorf("expected Blocked=true after %d consecutive goal_not_met; got %+v",
			consecutiveGoalNotMetCap, last)
	}
	if last.Reason != "repeated_goal_not_met" {
		t.Errorf("expected Reason=repeated_goal_not_met; got %q", last.Reason)
	}
	if last.TurnsRun != consecutiveGoalNotMetCap {
		t.Errorf("expected TurnsRun=%d; got %d", consecutiveGoalNotMetCap, last.TurnsRun)
	}
}

// NEX-249 fix B: a non-goal_not_met verdict in the middle of a stuck
// streak resets the consecutive counter — the safety cap only fires
// on TRULY consecutive goal_not_met. Otherwise an aspect that hits
// goal_not_met twice, makes a real complete turn, then hits another
// two would get misclassified as blocked.
func TestGoalLoop_NonGoalNotMetResetsCounter(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "t1", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "t2", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "t3", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "t4", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
		{FinalText: "t5", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, _ := newTestFunnel(t, bridle.ProviderResult{})
	f.cfg.Harness = bridle.NewHarness(prov)
	rec := &scriptedRecordingFilter{
		next: []FilterDecision{
			{ShouldPost: true, Class: FilterClassGoalNotMet},
			{ShouldPost: true, Class: FilterClassGoalNotMet},
			// Counter reset here:
			{ShouldPost: true, Class: FilterClassComplete},
			// New streak begins.
			{ShouldPost: true, Class: FilterClassGoalNotMet},
			{ShouldPost: true, Class: FilterClassGoalNotMet},
		},
	}
	f.cfg.Filter = rec
	f.Receive(bridle.InboxItem{From: "operator", Content: "go", MsgID: 1})

	// Turn 1 + 2 — both goal_not_met, counter at 2.
	gl := NewGoalLoop(f, GoalConfig{TicketID: "NEX-249", DoD: "x", MaxTurns: 20})
	for range 2 {
		if _, err := gl.Pursue(context.Background()); err != nil {
			t.Fatalf("Pursue: %v", err)
		}
	}
	if gl.consecutiveGoalNotMet != 2 {
		t.Errorf("after 2 goal_not_met expected counter=2, got %d", gl.consecutiveGoalNotMet)
	}

	// Turn 3 — complete. Counter resets, loop terminates Done.
	r3, err := gl.Pursue(context.Background())
	if err != nil {
		t.Fatalf("Pursue 3: %v", err)
	}
	if !r3.Done || r3.Reason != "complete" {
		t.Errorf("turn 3 expected Done complete; got %+v", r3)
	}
	if gl.consecutiveGoalNotMet != 0 {
		t.Errorf("complete should reset counter; got %d", gl.consecutiveGoalNotMet)
	}
}
