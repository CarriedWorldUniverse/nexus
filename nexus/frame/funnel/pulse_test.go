package funnel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexus-cw/bridle"
)

// recordingPulser captures every Fire call. Goroutine-safe: pulses
// fire from the deliberation goroutine, and tests assert from the
// test goroutine.
type recordingPulser struct {
	mu     sync.Mutex
	pulses []StatusPulse
}

func (p *recordingPulser) Fire(_ context.Context, sp StatusPulse) {
	p.mu.Lock()
	p.pulses = append(p.pulses, sp)
	p.mu.Unlock()
}

func (p *recordingPulser) snapshot() []StatusPulse {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]StatusPulse, len(p.pulses))
	copy(out, p.pulses)
	return out
}

func TestNoopPulser_NeverPanics(t *testing.T) {
	NoopPulser{}.Fire(context.Background(), StatusPulse{Kind: PulseKindCompact})
}

func TestPulse_FiresBeforeCompactStartEvent(t *testing.T) {
	pulser := &recordingPulser{}
	sink := &recordingSink{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		// Turn 1: cumulative goes over threshold.
		{
			FinalText:    "first",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first"}},
		},
		// Compaction's summarize call.
		{FinalText: "summary", Usage: bridle.Usage{InputTokens: 1_000, OutputTokens: 500}},
		// Turn 2 proper.
		{FinalText: "post", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50}},
	}}
	f, err := New(Config{
		AspectID:   "frame",
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Events:     sink,
		Pulser:     pulser,
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Deliberate(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Deliberate(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	pulses := pulser.snapshot()
	if len(pulses) != 1 {
		t.Fatalf("expected exactly 1 pulse for compaction, got %d", len(pulses))
	}
	if pulses[0].Kind != PulseKindCompact {
		t.Errorf("pulse kind: got %q, want %q", pulses[0].Kind, PulseKindCompact)
	}
	if pulses[0].AspectID != "frame" {
		t.Errorf("pulse aspect_id: got %q, want frame", pulses[0].AspectID)
	}
	if pulses[0].Reason == "" {
		t.Error("pulse reason should be human-readable, not empty")
	}
	if pulses[0].EstimatedDuration <= 0 {
		t.Errorf("pulse estimated duration should be positive: got %v", pulses[0].EstimatedDuration)
	}
}

func TestPulse_DoesNotFireWhenNoCompaction(t *testing.T) {
	pulser := &recordingPulser{}
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "small", Usage: bridle.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
	f, _ := New(Config{
		AspectID:   "frame",
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Pulser:     pulser,
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	if got := len(pulser.snapshot()); got != 0 {
		t.Errorf("no compaction expected; got %d pulses", got)
	}
}

func TestPulse_NilPulserIsSafe(t *testing.T) {
	// Construct a Funnel with nil Pulser explicitly via Config —
	// New should default it. Verify defaults work and Deliberate
	// runs cleanly.
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "ok", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		// Pulser intentionally unset
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("deliberate with nil pulser: %v", err)
	}
}

// panickyPulser verifies a misbehaving pulser cannot break the
// deliberation. The funnel's pulse() recovers; the deliberation
// must complete.
type panickyPulser struct{}

func (panickyPulser) Fire(_ context.Context, _ StatusPulse) { panic("boom") }

func TestPulse_PanickingPulserDoesNotBreakCompaction(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{
			FinalText:    "first",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first"}},
		},
		{FinalText: "summary", Usage: bridle.Usage{InputTokens: 1_000, OutputTokens: 500}},
		{FinalText: "post", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50}},
	}}
	f, _ := New(Config{
		AspectID:   "frame",
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Pulser:     panickyPulser{},
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	_, _ = f.Deliberate(context.Background(), "a")
	res, err := f.Deliberate(context.Background(), "b")
	if err != nil {
		t.Fatalf("deliberation should succeed despite panicking pulser: %v", err)
	}
	if res.TurnResult.FinalText != "post" {
		t.Errorf("expected post-compact turn to complete: got %q", res.TurnResult.FinalText)
	}
}

// blockingPulser wedges Fire forever. Used to verify the funnel's
// pulseTimeout abandons the call rather than stalling the long op
// the pulse is supposed to be announcing.
type blockingPulser struct {
	release chan struct{}
}

func (b *blockingPulser) Fire(_ context.Context, _ StatusPulse) {
	<-b.release
}

func TestPulse_BlockingPulserDoesNotStallCompaction(t *testing.T) {
	pulser := &blockingPulser{release: make(chan struct{})}
	defer close(pulser.release)

	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{
			FinalText:    "first",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first"}},
		},
		{FinalText: "summary", Usage: bridle.Usage{InputTokens: 1_000, OutputTokens: 500}},
		{FinalText: "post", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50}},
	}}
	f, _ := New(Config{
		AspectID:   "frame",
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Pulser:     pulser,
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	_, _ = f.Deliberate(context.Background(), "a")

	done := make(chan struct{})
	go func() {
		_, _ = f.Deliberate(context.Background(), "b")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deliberation stalled by blocking pulser — pulseTimeout not enforced")
	}
}

func TestPulse_FiresBeforeCompactStartLifecycleEvent(t *testing.T) {
	// Ordering invariant: human-visible pulse precedes machine-readable
	// compact.start event. Operators see "compacting context" in chat
	// before the dashboard activity strip flips.
	pulser := &orderingPulser{}
	sink := &recordingSink{}

	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{
			FinalText:    "first",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first"}},
		},
		{FinalText: "summary", Usage: bridle.Usage{InputTokens: 1_000, OutputTokens: 500}},
		{FinalText: "post", Usage: bridle.Usage{InputTokens: 100, OutputTokens: 50}},
	}}
	pulser.eventSink = sink
	f, _ := New(Config{
		AspectID:   "frame",
		Harness:    bridle.NewHarness(prov),
		Provider:   "scripted",
		Model:      "m",
		Runner:     noopRunner{},
		Events:     sink,
		Pulser:     pulser,
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 1024},
	})
	_, _ = f.Deliberate(context.Background(), "a")
	_, _ = f.Deliberate(context.Background(), "b")

	if !pulser.sawNoCompactStartAtPulseTime {
		t.Error("pulse fired AFTER compact.start event — ordering invariant violated")
	}
}

// orderingPulser checks at Fire-time whether compact.start has
// already been emitted to the sink. If so, the ordering invariant
// is broken.
type orderingPulser struct {
	eventSink                    *recordingSink
	sawNoCompactStartAtPulseTime bool
}

func (p *orderingPulser) Fire(_ context.Context, _ StatusPulse) {
	if p.eventSink == nil {
		p.sawNoCompactStartAtPulseTime = true
		return
	}
	p.sawNoCompactStartAtPulseTime = true
	for _, e := range p.eventSink.snapshot() {
		if e.Type == EventCompactStart {
			p.sawNoCompactStartAtPulseTime = false
			return
		}
	}
}
