package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// fakeWorkerStatusSender records every SendWorkerStatus call. Optionally
// returns an error to exercise the best-effort (never-panic/never-block)
// send path.
type fakeWorkerStatusSender struct {
	mu   sync.Mutex
	sent []frames.WorkerStatusPayload
	err  error
}

func (f *fakeWorkerStatusSender) SendWorkerStatus(_ context.Context, p frames.WorkerStatusPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
	return f.err
}

func (f *fakeWorkerStatusSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeWorkerStatusSender) last() frames.WorkerStatusPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

// fakeObsHook is a no-op funnel.ObservabilityHook that records calls it
// received, so tests can confirm turnMetricsTracker forwards everything
// unchanged (transparent decorator).
type fakeObsHook struct {
	mu         sync.Mutex
	begins     int
	events     int
	ends       int
	lastLabel  string
	lastModel  string
	lastVendor string
}

func (f *fakeObsHook) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
	f.lastLabel = label
	f.lastModel = model
	f.lastVendor = provider
}
func (f *fakeObsHook) OnBridleEvent(ev bridle.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events++
}
func (f *fakeObsHook) EndTurn() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends++
}

func turnDoneEvent(input, output int) bridle.TurnDone {
	return bridle.TurnDone{
		Result: bridle.TurnResult{
			Usage: bridle.Usage{InputTokens: input, OutputTokens: output},
		},
	}
}

func TestTurnMetricsTracker_CountsMainTurnsAndTokens(t *testing.T) {
	next := &fakeObsHook{}
	var boundaryFires int
	tr := newTurnMetricsTracker(next, func() { boundaryFires++ })

	tr.BeginTurn("t1", "main", "claude-opus-4-7", "claude-code", 0)
	tr.OnBridleEvent(turnDoneEvent(100, 50))
	tr.EndTurn()

	turns, tokens := tr.Snapshot()
	if turns != 1 {
		t.Errorf("turns = %d, want 1", turns)
	}
	if tokens != 150 {
		t.Errorf("tokensUsed = %d, want 150", tokens)
	}
	if boundaryFires != 1 {
		t.Errorf("onMainTurnEnd fired %d times, want 1", boundaryFires)
	}
	// Forwarding: the wrapped hook must have seen everything too.
	if next.begins != 1 || next.events != 1 || next.ends != 1 {
		t.Errorf("forwarding broken: begins=%d events=%d ends=%d", next.begins, next.events, next.ends)
	}
	if next.lastLabel != "main" || next.lastModel != "claude-opus-4-7" {
		t.Errorf("forwarded BeginTurn args wrong: label=%q model=%q", next.lastLabel, next.lastModel)
	}
}

func TestTurnMetricsTracker_NonMainTurnsAccumulateTokensNotTurns(t *testing.T) {
	tr := newTurnMetricsTracker(nil, nil)

	tr.BeginTurn("t1", "filter-judge", "cheap-model", "claude-code", 0)
	tr.OnBridleEvent(turnDoneEvent(10, 5))
	tr.EndTurn()

	turns, tokens := tr.Snapshot()
	if turns != 0 {
		t.Errorf("turns = %d, want 0 (judge turns don't count as deliberation progress)", turns)
	}
	if tokens != 15 {
		t.Errorf("tokensUsed = %d, want 15 (judge turns still cost tokens)", tokens)
	}
}

func TestTurnMetricsTracker_AccumulatesAcrossMultipleTurns(t *testing.T) {
	tr := newTurnMetricsTracker(nil, nil)

	for i := 0; i < 3; i++ {
		tr.BeginTurn("t", "main", "m", "p", 0)
		tr.OnBridleEvent(turnDoneEvent(10, 10))
		tr.EndTurn()
	}
	turns, tokens := tr.Snapshot()
	if turns != 3 {
		t.Errorf("turns = %d, want 3", turns)
	}
	if tokens != 60 {
		t.Errorf("tokensUsed = %d, want 60", tokens)
	}
}

func TestTurnMetricsTracker_OnMainTurnEndDoesNotFireForNonMain(t *testing.T) {
	var fires int
	tr := newTurnMetricsTracker(nil, func() { fires++ })
	tr.BeginTurn("t", "compact", "m", "p", 0)
	tr.OnBridleEvent(turnDoneEvent(1, 1))
	tr.EndTurn()
	if fires != 0 {
		t.Errorf("onMainTurnEnd fired %d times for a compact turn, want 0", fires)
	}
}

func TestWorkerStatusEmitter_EmitPopulatesFullShape(t *testing.T) {
	sender := &fakeWorkerStatusSender{}
	metrics := newTurnMetricsTracker(nil, nil)
	metrics.BeginTurn("t", "main", "m", "p", 0)
	metrics.OnBridleEvent(turnDoneEvent(20, 30))
	metrics.EndTurn()

	started := time.Now().Add(-5 * time.Minute).UTC()
	expires := time.Now().Add(30 * time.Minute).UTC()
	e := newWorkerStatusEmitter(
		sender, "anvil", "builder", "plumb", "wi-42", "2.1.0", "runner:cli-2.1.0",
		started,
		func() funnel.Binding { return funnel.Binding{Provider: "claude-code", Model: "claude-opus-4-7"} },
		func() (bool, time.Time) { return true, expires },
		metrics, nil,
	)

	e.Emit(context.Background(), "running")

	if sender.count() != 1 {
		t.Fatalf("sent = %d, want 1", sender.count())
	}
	got := sender.last()
	if got.Agent != "anvil" || got.Role != "builder" || got.Personality != "plumb" || got.WorkItemID != "wi-42" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.State != "running" {
		t.Errorf("state = %q, want running", got.State)
	}
	if got.Provider != "claude-code" || got.Model != "claude-opus-4-7" {
		t.Errorf("binding fields wrong: %+v", got)
	}
	if !got.AuthOk || !got.TokenExpiresAt.Equal(expires) {
		t.Errorf("auth fields wrong: %+v", got)
	}
	if got.CLIVersion != "2.1.0" || got.ImageTag != "runner:cli-2.1.0" {
		t.Errorf("version fields wrong: %+v", got)
	}
	if got.Turns != 1 || got.TokensUsed != 50 {
		t.Errorf("metrics fields wrong: %+v", got)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, started)
	}
	if got.LastHeartbeat.IsZero() {
		t.Error("last_heartbeat must be stamped")
	}
}

func TestWorkerStatusEmitter_SendFailureIsBestEffort(t *testing.T) {
	sender := &fakeWorkerStatusSender{err: errFakeSend}
	e := newWorkerStatusEmitter(sender, "anvil", "", "", "", "", "", time.Now(),
		nil, nil, nil, nil)

	// Must not panic and must not return an error (Emit has no return
	// value) even though the sender always fails.
	e.Emit(context.Background(), "running")
	if sender.count() != 1 {
		t.Fatalf("sender should still have been called once, got %d", sender.count())
	}
}

func TestWorkerStatusEmitter_NilSenderIsNoop(t *testing.T) {
	e := newWorkerStatusEmitter(nil, "anvil", "", "", "", "", "", time.Now(), nil, nil, nil, nil)
	// Must not panic with a nil sender.
	e.Emit(context.Background(), "running")
}

var errFakeSend = &fakeSendError{}

type fakeSendError struct{}

func (*fakeSendError) Error() string { return "fake send failure" }

func TestWorkerStatusEmitter_StartHeartbeatFiresOnCadence(t *testing.T) {
	sender := &fakeWorkerStatusSender{}
	e := newWorkerStatusEmitter(sender, "anvil", "", "", "", "", "", time.Now(), nil, nil, nil, nil)
	e.Emit(context.Background(), "running") // boot emit — not counted by the ticker

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Use a short interval directly rather than the real 60s cadence —
	// StartHeartbeat takes the interval as a parameter specifically so
	// tests don't have to wait a full minute.
	e.StartHeartbeat(ctx, 10*time.Millisecond)

	deadline := time.After(2 * time.Second)
	for {
		if sender.count() >= 3 { // 1 boot + at least 2 ticks
			break
		}
		select {
		case <-deadline:
			t.Fatalf("heartbeat ticker did not fire enough times; got %d sends", sender.count())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestWorkerStatusEmitter_StartHeartbeatStopsOnContextCancel(t *testing.T) {
	sender := &fakeWorkerStatusSender{}
	e := newWorkerStatusEmitter(sender, "anvil", "", "", "", "", "", time.Now(), nil, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	e.StartHeartbeat(ctx, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	cancel()
	countAtCancel := sender.count()
	time.Sleep(50 * time.Millisecond)
	if sender.count() > countAtCancel+1 {
		// Allow at most one in-flight tick to land after cancel.
		t.Fatalf("heartbeat kept firing after context cancel: at cancel=%d, after=%d", countAtCancel, sender.count())
	}
}
