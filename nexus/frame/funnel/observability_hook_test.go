package funnel

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// recordingHook is a minimal ObservabilityHook for tests. Captures
// BeginTurn / OnBridleEvent / EndTurn calls so assertions can verify
// ordering, labels, and that bridle events fan out through MultiSink.
type recordingHook struct {
	mu     sync.Mutex
	begins []recordedBegin
	events []bridle.Event
	ends   int
}

type recordedBegin struct {
	turnID, label, model, provider string
	trigger                        int64
}

func (h *recordingHook) BeginTurn(turnID, label, model, provider string, trigger int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.begins = append(h.begins, recordedBegin{turnID, label, model, provider, trigger})
}

func (h *recordingHook) OnBridleEvent(ev bridle.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
}

func (h *recordingHook) EndTurn() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ends++
}

// emittingProvider extends the scripted-provider pattern by emitting
// ModelChunk events for every result so MultiSink fanout can be
// observed. Distinct from scriptedProvider in funnel_test.go to keep
// changes additive.
type emittingProvider struct {
	results []bridle.ProviderResult
	pos     int
	calls   atomic.Int32
}

func (p *emittingProvider) Name() bridle.ProviderID { return "emitting" }

func (p *emittingProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}

func (p *emittingProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	p.calls.Add(1)
	if p.pos >= len(p.results) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	r := p.results[p.pos]
	p.pos++
	if r.StopReason == "" {
		r.StopReason = bridle.StopReasonModelDone
	}
	// Emit a single ModelChunk so MultiSink wiring can be observed.
	if r.FinalText != "" {
		sink.Emit(bridle.ModelChunk{Text: r.FinalText})
	}
	return r, nil
}

func TestDeliberate_FiresObservabilityHook(t *testing.T) {
	hook := &recordingHook{}
	prov := &emittingProvider{results: []bridle.ProviderResult{{
		FinalText:    "hello",
		Usage:        bridle.Usage{InputTokens: 10, OutputTokens: 5},
		SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "hello"}},
	}}}
	f, err := New(Config{
		AspectID:          "frame",
		SystemPrompt:      "test",
		Harness:           bridle.NewHarness(prov),
		Provider:          "emitting",
		Model:             "test-model",
		Runner:            noopRunner{},
		ObservabilityHook: hook,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("Deliberate: %v", err)
	}

	hook.mu.Lock()
	defer hook.mu.Unlock()

	// Two BeginTurn calls: the real "main" turn, then the synthetic
	// "filter-decision" turn the funnel emits after runFilter so
	// every filter outcome surfaces as a frame.
	if len(hook.begins) != 2 {
		t.Fatalf("BeginTurn calls=%d want 2 (main + filter-decision)", len(hook.begins))
	}
	b := hook.begins[0]
	if b.label != "main" {
		t.Errorf("BeginTurn[0] label=%q want main", b.label)
	}
	if hook.begins[1].label != "filter-decision" {
		t.Errorf("BeginTurn[1] label=%q want filter-decision", hook.begins[1].label)
	}
	if b.model != "test-model" || b.provider != "emitting" {
		t.Errorf("BeginTurn model/provider=%q/%q want test-model/emitting", b.model, b.provider)
	}
	if b.turnID == "" {
		t.Errorf("BeginTurn turnID empty")
	}
	// Provider emits a ModelChunk; bridle's harness emits a TurnDone
	// (or similar) after RunTurn returns. Both fan through MultiSink
	// to the hook.
	if len(hook.events) < 1 {
		t.Fatalf("OnBridleEvent calls=%d want >=1", len(hook.events))
	}
	sawChunk := false
	for _, ev := range hook.events {
		if c, ok := ev.(bridle.ModelChunk); ok && c.Text == "hello" {
			sawChunk = true
			break
		}
	}
	if !sawChunk {
		t.Errorf("hook never saw ModelChunk{Text:hello}; events=%+v", hook.events)
	}
	if hook.ends != 2 {
		t.Errorf("EndTurn calls=%d want 2 (main + filter-decision)", hook.ends)
	}
}

func TestDeliberate_NilHookKeepsCollectSinkPath(t *testing.T) {
	prov := &emittingProvider{results: []bridle.ProviderResult{{FinalText: "x"}}}
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(prov),
		Provider:     "emitting",
		Model:        "test-model",
		Runner:       noopRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic; absence of hook is the documented no-op path.
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("Deliberate: %v", err)
	}
	if prov.calls.Load() != 1 {
		t.Errorf("provider calls=%d want 1", prov.calls.Load())
	}
}

func TestMultiSink_FansOut(t *testing.T) {
	var a, b []bridle.Event
	sinkA := bridle.EventSink(eventSinkFunc(func(ev bridle.Event) { a = append(a, ev) }))
	sinkB := bridle.EventSink(eventSinkFunc(func(ev bridle.Event) { b = append(b, ev) }))

	ms := multiSink{sinkA, nil, sinkB}
	ms.Emit(bridle.ModelChunk{Text: "alpha"})
	ms.Emit(bridle.StepBoundary{Step: 1})

	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("len(a)=%d len(b)=%d want 2 each", len(a), len(b))
	}
}

// eventSinkFunc adapts a closure to bridle.EventSink so the MultiSink
// test stays a single self-contained file.
type eventSinkFunc func(ev bridle.Event)

func (f eventSinkFunc) Emit(ev bridle.Event) { f(ev) }

var _ json.RawMessage // keep encoding/json import minimal in case of future expansion
