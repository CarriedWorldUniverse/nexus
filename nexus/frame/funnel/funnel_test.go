package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// scriptedProvider is a minimal bridle.Provider that replays a queue of
// ProviderResult values. Lets tests assert compaction triggers, inbox
// folding, and Usage cumulation without standing up a real model API.
type scriptedProvider struct {
	results []bridle.ProviderResult
	pos     int
	calls   atomic.Int32
	last    bridle.ProviderRequest // last request seen, for prompt assertions
}

func (p *scriptedProvider) Name() bridle.ProviderID { return "scripted" }

func (p *scriptedProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
		SupportsMCP:            true,
	}
}

func (p *scriptedProvider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	p.calls.Add(1)
	p.last = req
	if p.pos >= len(p.results) {
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	r := p.results[p.pos]
	p.pos++
	if r.StopReason == "" {
		r.StopReason = bridle.StopReasonModelDone
	}
	return r, nil
}

// noopRunner satisfies bridle.ToolRunner; never called for these tests
// because scriptedProvider doesn't emit ToolCalls.
type noopRunner struct{}

func (noopRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func newTestFunnel(t *testing.T, results ...bridle.ProviderResult) (*Funnel, *scriptedProvider) {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, prov
}

func TestNew_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"no AspectID", func(c *Config) { c.AspectID = "" }},
		{"no Harness", func(c *Config) { c.Harness = nil }},
		{"no Provider", func(c *Config) { c.Provider = "" }},
		{"no Model", func(c *Config) { c.Model = "" }},
		{"no Runner", func(c *Config) { c.Runner = nil }},
	}
	base := func() Config {
		return Config{
			AspectID: "frame",
			Harness:  bridle.NewHarness(&scriptedProvider{}),
			Provider: "scripted",
			Model:    "m",
			Runner:   noopRunner{},
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(&cfg)
			if _, err := New(cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNew_AppliesDefaultCompaction(t *testing.T) {
	f, _ := newTestFunnel(t)
	if f.cfg.Compaction.ThresholdTokens != 125_000 {
		t.Errorf("default ThresholdTokens=%d want 125000", f.cfg.Compaction.ThresholdTokens)
	}
}

func TestDeliberate_EmptyInbox_Errors(t *testing.T) {
	f, prov := newTestFunnel(t)
	_, err := f.Deliberate(context.Background(), "")
	if !errors.Is(err, ErrEmptyInbox) {
		t.Errorf("got err=%v want ErrEmptyInbox", err)
	}
	if prov.calls.Load() != 0 {
		t.Error("provider should not have been called on empty inbox + empty user msg")
	}
}

func TestDeliberate_HappyPath_FoldsInbox(t *testing.T) {
	f, prov := newTestFunnel(t,
		bridle.ProviderResult{
			FinalText:    "ack",
			Usage:        bridle.Usage{InputTokens: 100, OutputTokens: 20},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "ack"}},
		},
	)

	f.Receive(bridle.InboxItem{From: "operator", Content: "hello frame"})
	if f.InboxLen() != 1 {
		t.Fatalf("inbox len=%d want 1", f.InboxLen())
	}

	res, err := f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatalf("Deliberate: %v", err)
	}
	if res.TurnResult.FinalText != "ack" {
		t.Errorf("FinalText=%q want ack", res.TurnResult.FinalText)
	}
	if prov.calls.Load() != 1 {
		t.Errorf("provider calls=%d want 1", prov.calls.Load())
	}
	// Inbox should be drained.
	if f.InboxLen() != 0 {
		t.Errorf("inbox len=%d want 0 after Deliberate", f.InboxLen())
	}
	// SessionTail should contain the assistant message.
	tail := f.SessionTail()
	if len(tail) != 1 || tail[0].Content != "ack" {
		t.Errorf("session tail wrong: %+v", tail)
	}
	// Cumulative tokens recorded.
	if got := f.CumulativeTokens(); got != 120 {
		t.Errorf("CumulativeTokens=%d want 120", got)
	}
	// Provider should have received messages that include the inbox content.
	// bridle folds inbox into ProviderRequest.Messages.
	gotInboxContent := false
	for _, m := range prov.last.Messages {
		if strings.Contains(m.Content, "hello frame") {
			gotInboxContent = true
			break
		}
	}
	if !gotInboxContent {
		t.Errorf("provider didn't see inbox content in any message: %+v", prov.last.Messages)
	}
}

func TestDeliberate_UserMessageWithoutInbox_Works(t *testing.T) {
	f, _ := newTestFunnel(t,
		bridle.ProviderResult{
			FinalText: "k",
			Usage:     bridle.Usage{InputTokens: 5, OutputTokens: 1},
		},
	)
	_, err := f.Deliberate(context.Background(), "ping")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeliberate_AccumulatesAcrossTurns(t *testing.T) {
	f, _ := newTestFunnel(t,
		bridle.ProviderResult{Usage: bridle.Usage{InputTokens: 1000, OutputTokens: 500}},
		bridle.ProviderResult{Usage: bridle.Usage{InputTokens: 2000, OutputTokens: 800}},
	)
	for i := 0; i < 2; i++ {
		f.Receive(bridle.InboxItem{From: "operator", Content: "hi"})
		if _, err := f.Deliberate(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.CumulativeTokens(); got != 4300 {
		t.Errorf("CumulativeTokens=%d want 4300 (1500+2800)", got)
	}
}

// Compaction triggers a summarize turn when cumulativeTokens crosses
// threshold. The summary becomes the new SessionTail; counter resets.
func TestDeliberate_CompactsAtThreshold(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		// Turn 1 — pushes us past threshold (160k > 150k default).
		{
			FinalText:    "first turn output",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "first turn output"}},
		},
		// Turn 2's compaction summarize call.
		{
			FinalText:    "compact briefing of prior session",
			Usage:        bridle.Usage{InputTokens: 1500, OutputTokens: 200},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "compact briefing of prior session"}},
		},
		// Turn 2 main bridle call — runs against the post-compaction tail.
		{
			FinalText:    "second turn",
			Usage:        bridle.Usage{InputTokens: 500, OutputTokens: 100},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "second turn"}},
		},
	}}
	harness := bridle.NewHarness(prov)
	f, err := New(Config{
		AspectID: "frame", Harness: harness,
		Provider: "scripted", Model: "m", Runner: noopRunner{},
		Compaction: CompactionPolicy{ThresholdTokens: 150_000, MaxSummaryTokens: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: pushes past threshold.
	f.Receive(bridle.InboxItem{From: "operator", Content: "first"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.CumulativeTokens(); got != 160_000 {
		t.Errorf("after turn 1: cumulative=%d want 160000", got)
	}
	preCompactSession := f.SessionID()

	// Turn 2: compaction triggers BEFORE the main turn runs. So this
	// deliberation makes 2 provider calls (summarize + main).
	f.Receive(bridle.InboxItem{From: "operator", Content: "second"})
	preCalls := prov.calls.Load()
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	postCalls := prov.calls.Load()
	if postCalls-preCalls != 2 {
		t.Errorf("turn 2 should have made 2 provider calls (summarize + main), got %d", postCalls-preCalls)
	}

	// Session tail should contain the summary + turn 2's output.
	tail := f.SessionTail()
	if len(tail) != 2 {
		t.Errorf("session tail len=%d want 2 (summary + turn 2 output): %+v", len(tail), tail)
	}
	if !strings.Contains(tail[0].Content, "compact briefing") {
		t.Errorf("session tail[0] should be the summary, got %+v", tail[0])
	}

	// Session id should have rotated.
	if f.SessionID() == preCompactSession {
		t.Error("session id should have rotated on compaction")
	}

	// Cumulative tokens should reflect summary output + turn 2 (counter
	// reset to summary's output, then turn 2's usage added).
	want := 200 + (500 + 100) // summary output + turn 2 in+out
	if got := f.CumulativeTokens(); got != want {
		t.Errorf("post-compact cumulative=%d want %d", got, want)
	}
}

// Compaction with empty tail should not call the provider — there's
// nothing to summarize.
func TestCompact_EmptyTail_NoOp(t *testing.T) {
	f, prov := newTestFunnel(t)
	if err := f.compact(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if prov.calls.Load() != 0 {
		t.Error("compact on empty tail should not call provider")
	}
}

// Compaction failure shouldn't crash the deliberation — log + continue.
func TestDeliberate_CompactFailureContinues(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		// Turn 1 pushes past threshold.
		{
			FinalText:    "big",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "big"}},
		},
		// Compact summarize turn — empty FinalText causes summary error.
		{
			Usage: bridle.Usage{InputTokens: 100, OutputTokens: 0},
		},
		// Main turn 2 — succeeds despite compact failure.
		{
			FinalText:    "fallback",
			Usage:        bridle.Usage{InputTokens: 200, OutputTokens: 50},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "fallback"}},
		},
	}}
	harness := bridle.NewHarness(prov)
	f, _ := New(Config{
		AspectID: "frame", Harness: harness,
		Provider: "scripted", Model: "m", Runner: noopRunner{},
	})

	f.Receive(bridle.InboxItem{From: "operator", Content: "go"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	f.Receive(bridle.InboxItem{From: "operator", Content: "go again"})
	res, err := f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res.TurnResult.FinalText != "fallback" {
		t.Errorf("FinalText=%q want fallback", res.TurnResult.FinalText)
	}
}

// Mid-deliberation Receive calls should accumulate for the NEXT cycle,
// not interfere with the running one.
func TestReceive_DuringDeliberate_GoesToNextCycle(t *testing.T) {
	f, _ := newTestFunnel(t,
		bridle.ProviderResult{
			FinalText: "first",
			Usage:     bridle.Usage{InputTokens: 10, OutputTokens: 1},
		},
	)
	f.Receive(bridle.InboxItem{From: "operator", Content: "first"})

	// Prime an additional comm before the deliberation.
	f.Receive(bridle.InboxItem{From: "operator", Content: "second"})

	if f.InboxLen() != 2 {
		t.Fatalf("inbox=%d want 2", f.InboxLen())
	}

	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if f.InboxLen() != 0 {
		t.Errorf("inbox should be drained, got %d", f.InboxLen())
	}
}

func TestNewSessionID_UniqueAcrossCalls(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id := newSessionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate session id: %s", id)
		}
		seen[id] = struct{}{}
	}
}
