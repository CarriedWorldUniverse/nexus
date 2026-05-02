package funnel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexus-cw/bridle"
)

// recordingSink captures every emission for assertion. Goroutine-safe:
// the funnel may emit from multiple goroutines in the future, and tests
// shouldn't be the thing that finds the race.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingSink) Emit(_ context.Context, e Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *recordingSink) snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *recordingSink) types() []EventType {
	out := []EventType{}
	for _, e := range s.snapshot() {
		out = append(out, e.Type)
	}
	return out
}

// newTestFunnelWithSink mirrors newTestFunnel but wires a recordingSink.
func newTestFunnelWithSink(t *testing.T, sink EventSink, results ...bridle.ProviderResult) (*Funnel, *scriptedProvider) {
	t.Helper()
	prov := &scriptedProvider{results: results}
	harness := bridle.NewHarness(prov)
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test system prompt",
		Harness:      harness,
		Provider:     "scripted",
		Model:        "test-model",
		Runner:       noopRunner{},
		Events:       sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, prov
}

func TestNew_DefaultsToNoopSink(t *testing.T) {
	f, _ := newTestFunnel(t, bridle.ProviderResult{FinalText: "ok"})
	// No panic + Deliberate succeeds = NoopSink wired correctly.
	if _, err := f.Deliberate(context.Background(), "hello"); err != nil {
		t.Fatalf("deliberate with default sink: %v", err)
	}
}

func TestEmit_TurnStartAndEndFireAroundDeliberate(t *testing.T) {
	sink := &recordingSink{}
	f, _ := newTestFunnelWithSink(t, sink,
		bridle.ProviderResult{
			FinalText:  "hello operator",
			Usage:      bridle.Usage{InputTokens: 100, OutputTokens: 20},
			StopReason: bridle.StopReasonModelDone,
		},
	)

	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("deliberate: %v", err)
	}

	got := sink.types()
	want := []EventType{EventTurnStart, EventTurnEnd, EventFilterJudging}
	if len(got) != len(want) {
		t.Fatalf("event count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

func TestEmit_TurnStartCarriesContextEstimate(t *testing.T) {
	sink := &recordingSink{}
	f, _ := newTestFunnelWithSink(t, sink,
		bridle.ProviderResult{FinalText: "k", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 5}},
	)

	const userMsg = "this is a message that should produce a non-zero context estimate when divided by four"
	if _, err := f.Deliberate(context.Background(), userMsg); err != nil {
		t.Fatalf("deliberate: %v", err)
	}

	events := sink.snapshot()
	if len(events) == 0 {
		t.Fatal("no events captured")
	}
	start := events[0]
	if start.Type != EventTurnStart {
		t.Fatalf("first event: got %q, want %q", start.Type, EventTurnStart)
	}
	payload, ok := start.Payload.(TurnStartPayload)
	if !ok {
		t.Fatalf("payload type: got %T, want TurnStartPayload", start.Payload)
	}
	if payload.TurnID == "" {
		t.Error("turn id empty")
	}
	if payload.Round != 1 {
		t.Errorf("round: got %d, want 1", payload.Round)
	}
	if payload.ContextTokens <= 0 {
		t.Errorf("context tokens estimate non-positive for non-empty user msg: got %d", payload.ContextTokens)
	}
}

func TestEmit_TurnEndCarriesUsageAndDuration(t *testing.T) {
	sink := &recordingSink{}
	f, _ := newTestFunnelWithSink(t, sink,
		bridle.ProviderResult{
			FinalText:  "done",
			Usage:      bridle.Usage{InputTokens: 100, OutputTokens: 50},
			StopReason: bridle.StopReasonModelDone,
		},
	)

	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("deliberate: %v", err)
	}

	events := sink.snapshot()
	// turn.end is the second event (after turn.start, before filter.judging).
	end := events[1]
	if end.Type != EventTurnEnd {
		t.Fatalf("event[1]: got %q, want %q", end.Type, EventTurnEnd)
	}
	payload, ok := end.Payload.(TurnEndPayload)
	if !ok {
		t.Fatalf("payload type: got %T", end.Payload)
	}
	if payload.Usage.InputTokens != 100 || payload.Usage.OutputTokens != 50 {
		t.Errorf("usage: got %+v, want input=100 output=50", payload.Usage)
	}
	if payload.Duration < 0 {
		t.Errorf("duration negative: %v", payload.Duration)
	}
	if payload.StopReason != bridle.StopReasonModelDone {
		t.Errorf("stop reason: got %q, want %q", payload.StopReason, bridle.StopReasonModelDone)
	}
}

func TestEmit_StartAndEndShareTurnID(t *testing.T) {
	sink := &recordingSink{}
	f, _ := newTestFunnelWithSink(t, sink,
		bridle.ProviderResult{FinalText: "k", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	)
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot()
	startID := events[0].Payload.(TurnStartPayload).TurnID
	endID := events[1].Payload.(TurnEndPayload).TurnID
	if startID != endID {
		t.Errorf("turn ids differ: start=%q end=%q", startID, endID)
	}
}

func TestEmit_AspectIDAndTimestampStamped(t *testing.T) {
	sink := &recordingSink{}
	f, _ := newTestFunnelWithSink(t, sink,
		bridle.ProviderResult{FinalText: "k", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	)
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	for _, e := range sink.snapshot() {
		if e.AspectID != "frame" {
			t.Errorf("aspect id: got %q, want frame", e.AspectID)
		}
		if e.EmittedAt.IsZero() {
			t.Errorf("emitted_at unset for %q", e.Type)
		}
	}
}

// panickySink verifies a misbehaving sink can't break the deliberation
// loop. The funnel's emit() recovers; Deliberate must still succeed and
// return the bridle result.
type panickySink struct{}

func (panickySink) Emit(_ context.Context, _ Event) { panic("boom") }

func TestEmit_PanickingSinkDoesNotBreakDeliberation(t *testing.T) {
	f, _ := newTestFunnelWithSink(t, panickySink{},
		bridle.ProviderResult{FinalText: "ok", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	)
	result, err := f.Deliberate(context.Background(), "ping")
	if err != nil {
		t.Fatalf("deliberation should succeed despite panicking sink: %v", err)
	}
	if result.TurnResult.FinalText != "ok" {
		t.Errorf("result text: got %q, want ok", result.TurnResult.FinalText)
	}
}

func TestEmit_CompactStartAndEndFireWhenThresholdCrossed(t *testing.T) {
	sink := &recordingSink{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		// First turn: large output that pushes cumulative over threshold.
		// SessionDelta non-empty so compact() has something to summarize.
		{
			FinalText:    "first",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first"}},
		},
		// Compaction's summarize turn — runs on next Deliberate when
		// cumulativeTokens crosses threshold before the turn.
		{FinalText: "summary briefing", Usage: bridle.Usage{InputTokens: 1_000, OutputTokens: 500}},
		// Post-compact normal turn.
		{FinalText: "post-compact", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50}},
	}}
	harness := bridle.NewHarness(prov)
	f, err := New(Config{
		AspectID:   "frame",
		Harness:    harness,
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Events:     sink,
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: cumulative goes to 160k, crosses threshold.
	if _, err := f.Deliberate(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	// Turn 2: pre-turn compaction triggers, then normal turn runs.
	if _, err := f.Deliberate(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	got := sink.types()
	// Expected order:
	//   turn 1: turn.start turn.end filter.judging
	//   turn 2 entry triggers compaction: compact.start compact.end
	//   turn 2 proper:  turn.start turn.end filter.judging
	want := []EventType{
		EventTurnStart, EventTurnEnd, EventFilterJudging,
		EventCompactStart, EventCompactEnd,
		EventTurnStart, EventTurnEnd, EventFilterJudging,
	}
	if len(got) != len(want) {
		t.Fatalf("event count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

func TestEmit_CompactPayloadCarriesBeforeAndAfter(t *testing.T) {
	sink := &recordingSink{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{
			FinalText:    "first",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first"}},
		},
		{FinalText: "summary", Usage: bridle.Usage{InputTokens: 1_000, OutputTokens: 800}},
		{FinalText: "post", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50}},
	}}
	harness := bridle.NewHarness(prov)
	f, _ := New(Config{
		AspectID:   "frame",
		Harness:    harness,
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Events:     sink,
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	_, _ = f.Deliberate(context.Background(), "a")
	_, _ = f.Deliberate(context.Background(), "b")

	var start *CompactStartPayload
	var end *CompactEndPayload
	for _, e := range sink.snapshot() {
		switch p := e.Payload.(type) {
		case CompactStartPayload:
			start = &p
		case CompactEndPayload:
			end = &p
		}
	}
	if start == nil {
		t.Fatal("no compact.start event")
	}
	if end == nil {
		t.Fatal("no compact.end event")
	}
	if start.Reason != CompactReasonSoft {
		t.Errorf("compact reason: got %q, want %q", start.Reason, CompactReasonSoft)
	}
	if start.TokensBefore != 160_000 {
		t.Errorf("tokens before: got %d, want 160000", start.TokensBefore)
	}
	if end.TokensBefore != 160_000 {
		t.Errorf("compact.end tokens_before: got %d, want 160000", end.TokensBefore)
	}
	if end.TokensAfter != 800 {
		t.Errorf("compact.end tokens_after: got %d, want 800 (summary output)", end.TokensAfter)
	}
	if end.Duration < 0 {
		t.Errorf("compact.end duration negative: %v", end.Duration)
	}
}

func TestNoopSink_NeverPanics(t *testing.T) {
	NoopSink{}.Emit(context.Background(), Event{Type: EventTurnStart})
}

// erroringProvider returns the configured error from RunTurn. Used to
// verify turn.end still fires on the error path — Lock 5's pairing
// invariant says every turn.start has a matching turn.end.
type erroringProvider struct {
	err error
}

func (erroringProvider) Name() bridle.ProviderID { return "erroring" }

func (erroringProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
		SupportsMCP:            true,
	}
}

func (p erroringProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	return bridle.ProviderResult{}, p.err
}

func TestEmit_TurnEndFiresOnProviderError(t *testing.T) {
	sink := &recordingSink{}
	prov := erroringProvider{err: context.DeadlineExceeded}
	harness := bridle.NewHarness(prov)
	f, err := New(Config{
		AspectID: "frame",
		Harness:  harness,
		Provider: "erroring",
		Model:    "m",
		Runner:   noopRunner{},
		Events:   sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, derr := f.Deliberate(context.Background(), "ping"); derr == nil {
		t.Fatal("expected error from erroring provider")
	}
	got := sink.types()
	want := []EventType{EventTurnStart, EventTurnEnd}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("event sequence: got %v, want %v", got, want)
	}
}

// blockingSink wedges Emit until released. Verifies emit() abandons a
// slow sink instead of stalling deliberation.
type blockingSink struct {
	release chan struct{}
}

func (b *blockingSink) Emit(_ context.Context, _ Event) {
	<-b.release
}

func TestEmit_BlockingSinkDoesNotStallDeliberation(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	defer close(sink.release) // unblock background goroutines at test end

	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "ok", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	harness := bridle.NewHarness(prov)
	f, err := New(Config{
		AspectID: "frame",
		Harness:  harness,
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		Events:   sink,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Deliberate must complete despite the sink wedging on every emit.
	// emit() bounds wall time to emitTimeout; for this test, a turn
	// that calls Emit twice (start + end) should still finish well
	// under a second.
	done := make(chan error, 1)
	go func() {
		_, err := f.Deliberate(context.Background(), "ping")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deliberate: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliberate stalled by blocking sink — emit() timeout not enforced")
	}
}
