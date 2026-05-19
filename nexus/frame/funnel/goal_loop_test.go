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
