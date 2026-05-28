package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
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

// TestNew_NormalizesClaudecodeAlias pins the alias-fold added with
// the Deliberate toolkit-blurb fix. Before the fix, Deliberate
// compared `f.cfg.Provider == "claudecode"` (no hyphen) but bridle's
// canonical constant is "claude-code" — so aspects configured with
// the canonical form silently missed the toolkit blurb. After the
// fix, New() folds "claudecode" → bridle.ProviderClaudeCode and the
// downstream comparison uses the typed constant.
func TestNew_NormalizesClaudecodeAlias(t *testing.T) {
	cases := []struct {
		name  string
		input bridle.ProviderID
		want  bridle.ProviderID
	}{
		{"alias spelling", "claudecode", bridle.ProviderClaudeCode},
		{"canonical hyphenated", bridle.ProviderClaudeCode, bridle.ProviderClaudeCode},
		{"unrelated provider not normalized", "scripted", "scripted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				AspectID: "frame",
				Harness:  bridle.NewHarness(&scriptedProvider{}),
				Provider: tc.input,
				Model:    "m",
				Runner:   noopRunner{},
			}
			f, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if f.cfg.Provider != tc.want {
				t.Errorf("Provider = %q; want %q", f.cfg.Provider, tc.want)
			}
		})
	}
}

// TestNew_AutoDerivesFilterObservabilityHook verifies the auto-derive
// added to eliminate the wire-one-forget-the-other footgun: when the
// funnel has an ObservabilityHook but a wrapped/direct CheapModelFilter
// has nil ObservabilityHook, New() populates it from the funnel's.
// Honors an explicitly-set hook on the filter.
func TestNew_AutoDerivesFilterObservabilityHook(t *testing.T) {
	funnelHook := &nopObsHook{}
	otherHook := &nopObsHook{}

	cases := []struct {
		name    string
		filter  OutputFilter
		want    ObservabilityHook
		isInner bool // assert inner CheapModelFilter, not top-level
	}{
		{
			name:   "direct CheapModelFilter — auto-derive",
			filter: &CheapModelFilter{},
			want:   funnelHook,
		},
		{
			name:   "direct CheapModelFilter — explicit hook preserved",
			filter: &CheapModelFilter{ObservabilityHook: otherHook},
			want:   otherHook,
		},
		{
			name:    "HardRulesFilter wrapping CheapModelFilter — auto-derive inner",
			filter:  HardRulesFilter{Inner: &CheapModelFilter{}},
			want:    funnelHook,
			isInner: true,
		},
		{
			name:    "HardRulesFilter wrapping explicit — inner preserved",
			filter:  HardRulesFilter{Inner: &CheapModelFilter{ObservabilityHook: otherHook}},
			want:    otherHook,
			isInner: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := New(Config{
				AspectID:          "frame",
				Harness:           bridle.NewHarness(&scriptedProvider{}),
				Provider:          "scripted",
				Model:             "m",
				Runner:            noopRunner{},
				Filter:            tc.filter,
				ObservabilityHook: funnelHook,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got ObservabilityHook
			if tc.isInner {
				got = f.cfg.Filter.(HardRulesFilter).Inner.(*CheapModelFilter).ObservabilityHook
			} else {
				got = f.cfg.Filter.(*CheapModelFilter).ObservabilityHook
			}
			if got != tc.want {
				t.Errorf("CheapModelFilter.ObservabilityHook = %p; want %p", got, tc.want)
			}
		})
	}
}

// nopObsHook is a noop ObservabilityHook used as a sentinel for
// pointer-identity assertions.
type nopObsHook struct{}

func (*nopObsHook) BeginTurn(_, _, _, _ string, _ int64) {}
func (*nopObsHook) OnBridleEvent(_ bridle.Event)         {}
func (*nopObsHook) EndTurn()                             {}

func TestNew_AppliesDefaultCompaction(t *testing.T) {
	f, _ := newTestFunnel(t)
	if f.cfg.Compaction.ThresholdTokens != 125_000 {
		t.Errorf("default ThresholdTokens=%d want 125000", f.cfg.Compaction.ThresholdTokens)
	}
}

// blockingProvider holds RunTurn open on a release channel so a test
// can fire a second Deliberate while the first is mid-flight.
type blockingProvider struct {
	release chan struct{}
	entered chan struct{} // closed when RunTurn is actually inside the call
}

func (p *blockingProvider) Name() bridle.ProviderID { return "blocking" }
func (p *blockingProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}
func (p *blockingProvider) RunTurn(ctx context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return bridle.ProviderResult{FinalText: "released", StopReason: bridle.StopReasonModelDone}, nil
}

// TestDeliberate_ConcurrentCallReturnsSentinel verifies the single-
// caller guard fires when a second Deliberate is launched while the
// first is still inside the provider call. Pre-guard, the second call
// would silently race on sessionHandle/sessionTail/cumulativeTokens
// across the function's many short mutex sections.
func TestDeliberate_ConcurrentCallReturnsSentinel(t *testing.T) {
	prov := &blockingProvider{release: make(chan struct{}), entered: make(chan struct{})}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "blocking",
		Model:    "m",
		Runner:   noopRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Receive(bridle.InboxItem{MsgID: 1, From: "op", Content: "first"})
	f.Receive(bridle.InboxItem{MsgID: 2, From: "op", Content: "second"})

	// Fire the first call in the background; it'll block inside RunTurn
	// until we close release.
	firstDone := make(chan error, 1)
	go func() {
		_, err := f.Deliberate(context.Background(), "")
		firstDone <- err
	}()

	// Wait for the first call to actually be inside Deliberate's
	// provider invocation, then fire a second call from this goroutine.
	<-prov.entered

	_, err = f.Deliberate(context.Background(), "")
	if !errors.Is(err, ErrConcurrentDeliberate) {
		t.Errorf("second Deliberate err = %v; want ErrConcurrentDeliberate", err)
	}

	// Release the first call and let it complete cleanly. The guard
	// clears via deferred f.deliberating.Store(false) — verified by
	// the separate TestDeliberate_GuardClearsAfterCompletion below.
	close(prov.release)
	if err := <-firstDone; err != nil {
		t.Errorf("first Deliberate err = %v; want nil", err)
	}
}

// TestDeliberate_GuardClearsAfterCompletion verifies the single-caller
// guard releases on the deferred path so a subsequent Deliberate
// succeeds. Uses a vanilla scriptedProvider so the second call has
// its own happy-path replay queue.
func TestDeliberate_GuardClearsAfterCompletion(t *testing.T) {
	f, _ := newTestFunnel(t,
		bridle.ProviderResult{FinalText: "first", StopReason: bridle.StopReasonModelDone},
		bridle.ProviderResult{FinalText: "second", StopReason: bridle.StopReasonModelDone},
	)
	f.Receive(bridle.InboxItem{MsgID: 1, From: "op", Content: "a"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatalf("first Deliberate: %v", err)
	}

	f.Receive(bridle.InboxItem{MsgID: 2, From: "op", Content: "b"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Errorf("second Deliberate err = %v; want nil (guard should have cleared after first)", err)
	}
}

// panickingReturnHandler crashes on OnTurnStart. Used by the
// NEX-204 regression test to exercise the top-level recover in
// Deliberate.
type panickingReturnHandler struct{}

func (panickingReturnHandler) OnTurnStart(context.Context, TurnTrigger) error {
	panic("synthetic OnTurnStart panic")
}
func (panickingReturnHandler) Handle(context.Context, DeliberateResult, TurnTrigger) error {
	return nil
}

// NEX-204: a panic inside any phase of Deliberate (here: the Return
// handler's OnTurnStart) must NOT propagate out and crash the aspect
// goroutine. The top-level recover catches it, logs at error, returns
// a clean error to the caller, and the deliberating guard clears so
// the next Deliberate call can proceed.
//
// Pre-fix the original 2026-05-17 incident: plumb crashed on a chat
// message with markdown + emoji content. The exact trigger was opaque
// (no logs preserved), but the symptom is "any panic in Deliberate
// kills the aspect goroutine". This guard closes the symptom
// regardless of which code path produces the panic.
func TestDeliberate_TopLevelRecoverKeepsAspectAlive(t *testing.T) {
	f, _ := newTestFunnel(t,
		bridle.ProviderResult{FinalText: "post-recover", StopReason: bridle.StopReasonModelDone},
	)
	// Swap the default Return handler for one that panics on every
	// OnTurnStart.
	f.cfg.Return = panickingReturnHandler{}
	f.Receive(bridle.InboxItem{MsgID: 1, From: "op", Content: "edge case"})

	// Call must NOT panic out — recover catches and returns error.
	_, err := f.Deliberate(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on panicking Return handler; got nil (recover not catching?)")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("error message should mention the panic; got %q", err.Error())
	}

	// Crucially: the deliberating guard must have cleared. Verify by
	// swapping in a clean Return handler and calling Deliberate again.
	f.cfg.Return = NoopReturnHandler{}
	f.Receive(bridle.InboxItem{MsgID: 2, From: "op", Content: "next msg"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Errorf("second Deliberate after recovered panic: %v (guard should have cleared)", err)
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

// TestDeliberate_CompactionFailureLimit_RotatesSession drives compact()
// to fail three times in a row and verifies the funnel force-rotates
// the session on the third failure (mirroring the PostTurn rewriter's
// sustained-failure recovery). Prevents the "spam compact attempts
// forever against a deterministic failure" mode.
func TestDeliberate_CompactionFailureLimit_RotatesSession(t *testing.T) {
	// Scripted sequence: turn 1 pushes past threshold; turns 2-4 each
	// pair a failing compact attempt (empty FinalText) with a small
	// main turn. Turn 4's compact attempt should trip the limit and
	// force-rotate the session.
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		// Turn 1: high-token main turn that crosses threshold.
		{
			FinalText:    "big",
			Usage:        bridle.Usage{InputTokens: 100_000, OutputTokens: 60_000},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "big"}},
		},
		// Turn 2: compact fails (empty), main runs.
		{Usage: bridle.Usage{InputTokens: 100}}, // empty FinalText → compact error
		{
			FinalText:    "t2",
			Usage:        bridle.Usage{InputTokens: 200, OutputTokens: 50},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "t2"}},
		},
		// Turn 3: compact fails, main runs.
		{Usage: bridle.Usage{InputTokens: 100}},
		{
			FinalText:    "t3",
			Usage:        bridle.Usage{InputTokens: 200, OutputTokens: 50},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "t3"}},
		},
		// Turn 4: compact fails (3rd time) → force-rotate, main runs in
		// fresh session.
		{Usage: bridle.Usage{InputTokens: 100}},
		{
			FinalText:    "t4-fresh",
			Usage:        bridle.Usage{InputTokens: 200, OutputTokens: 50},
			SessionDelta: []bridle.SessionEvent{{Role: bridle.RoleAssistant, Content: "t4-fresh"}},
		},
	}}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1: prime the funnel past the compaction threshold.
	f.Receive(bridle.InboxItem{From: "op", Content: "go"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	preRotateSessionID := f.SessionID()
	if f.CumulativeTokens() < 125_000 {
		t.Fatalf("turn 1 didn't cross threshold; cumulative=%d", f.CumulativeTokens())
	}

	// Turns 2 and 3: compact fails but no rotation yet.
	for i, label := range []string{"turn 2", "turn 3"} {
		f.Receive(bridle.InboxItem{From: "op", Content: label})
		if _, err := f.Deliberate(context.Background(), ""); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if f.compactionFailures != i+1 {
			t.Errorf("after %s: compactionFailures=%d want %d", label, f.compactionFailures, i+1)
		}
		if f.SessionID() != preRotateSessionID {
			t.Errorf("after %s: session rotated prematurely (was %q, now %q)", label, preRotateSessionID, f.SessionID())
		}
	}

	// Turn 4: third consecutive failure trips the limit → rotation.
	f.Receive(bridle.InboxItem{From: "op", Content: "turn 4"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatalf("turn 4: %v", err)
	}
	if f.compactionFailures != 0 {
		t.Errorf("after rotation: compactionFailures=%d want 0 (reset)", f.compactionFailures)
	}
	if f.SessionID() == preRotateSessionID {
		t.Error("session did NOT rotate after compactionFailureLimit failures")
	}
	// Cumulative should be just turn-4's contribution (250 = 200 in + 50 out)
	// — sessionTail and counter were cleared by the rotation.
	if got := f.CumulativeTokens(); got != 250 {
		t.Errorf("post-rotation cumulative=%d want 250 (turn-4 only)", got)
	}
}

// panickyPostTurn panics in every method. Verifies the funnel's
// PostTurn call sites are panic-safe — a misbehaving rewriter
// implementation must not crash deliberation.
type panickyPostTurn struct {
	// requestReset, when true, signals ShouldResetSession should
	// (try to) return true. The panic still fires first; the funnel's
	// recovery returns false regardless.
	requestReset bool
}

func (p *panickyPostTurn) AfterTurn(context.Context) { panic("AfterTurn boom") }
func (p *panickyPostTurn) ShouldResetSession() bool {
	if p.requestReset {
		panic("ShouldResetSession boom (asked for true)")
	}
	panic("ShouldResetSession boom")
}
func (p *panickyPostTurn) AcknowledgeReset() { panic("AcknowledgeReset boom") }

// TestDeliberate_PostTurnPanicSafe verifies that a PostTurnHook
// panicking inside any of its three methods does not crash
// deliberation. The emit() and runFilter() paths have had panic
// guards since v1; PostTurn was the missing one.
func TestDeliberate_PostTurnPanicSafe(t *testing.T) {
	prov := &scriptedProvider{results: []bridle.ProviderResult{
		{FinalText: "ok", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		PostTurn: &panickyPostTurn{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Receive(bridle.InboxItem{From: "op", Content: "go"})

	result, err := f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatalf("deliberation should succeed despite panicking PostTurn: %v", err)
	}
	if result.TurnResult.FinalText != "ok" {
		t.Errorf("FinalText = %q; want ok", result.TurnResult.FinalText)
	}
}

// TestDeliberate_TurnEndCarriesResolvedModel verifies the
// EventTurnEnd payload surfaces bridle.TurnResult.ResolvedModel. This
// is what downstream observability (plumb activity log, etc.) reads
// to display the model id the upstream API actually answered with —
// rather than the configured cfg.Model, which can diverge when per-
// turn ProviderEnv routes the call to a different backend.
func TestDeliberate_TurnEndCarriesResolvedModel(t *testing.T) {
	// scriptedProvider doesn't surface ResolvedModel; for this test we
	// need a provider that does. Inline a tiny variant.
	prov := &resolvedModelProvider{
		result: bridle.ProviderResult{
			FinalText:     "ok",
			Usage:         bridle.Usage{InputTokens: 1, OutputTokens: 1},
			ResolvedModel: "deepseek-chat-v3-via-anthropic-shape",
		},
	}
	sink := &recordingSink{}
	f, err := New(Config{
		AspectID: "frame",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "claude-3-5-sonnet-20241022", // what was configured
		Runner:   noopRunner{},
		Events:   sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Receive(bridle.InboxItem{From: "op", Content: "go"})
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatalf("deliberate: %v", err)
	}

	// Find the TurnEnd event and check its ResolvedModel.
	var found bool
	for _, ev := range sink.snapshot() {
		if ev.Type != EventTurnEnd {
			continue
		}
		p, ok := ev.Payload.(TurnEndPayload)
		if !ok {
			t.Fatalf("TurnEnd payload type = %T; want TurnEndPayload", ev.Payload)
		}
		if p.ResolvedModel != "deepseek-chat-v3-via-anthropic-shape" {
			t.Errorf("TurnEnd ResolvedModel = %q; want deepseek-...", p.ResolvedModel)
		}
		found = true
	}
	if !found {
		t.Fatal("no TurnEnd event emitted")
	}
}

// resolvedModelProvider returns a fixed ProviderResult with
// ResolvedModel set — tests that the funnel propagates it onto
// EventTurnEnd.
type resolvedModelProvider struct {
	result bridle.ProviderResult
}

func (p *resolvedModelProvider) Name() bridle.ProviderID { return "resolved-model-provider" }
func (p *resolvedModelProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}
func (p *resolvedModelProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	r := p.result
	if r.StopReason == "" {
		r.StopReason = bridle.StopReasonModelDone
	}
	return r, nil
}

// recordingLogHandler collects every slog Record emitted at WARN or
// above so tests can assert the funnel's pressure warnings fire
// once-per-event under hysteresis.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingLogHandler) inboxPressureCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, r := range h.records {
		if strings.Contains(r.Message, "inbox under pressure") {
			count++
		}
	}
	return count
}

// TestReceive_InboxPressureWarnsOnceWithHysteresis verifies the
// inbox-pressure WARN fires once per breach (not on every Receive
// above the threshold) and re-arms after the inbox drains to half.
func TestReceive_InboxPressureWarnsOnceWithHysteresis(t *testing.T) {
	logH := &recordingLogHandler{}
	f, err := New(Config{
		AspectID:           "frame",
		Harness:            bridle.NewHarness(&scriptedProvider{}),
		Provider:           "scripted",
		Model:              "m",
		Runner:             noopRunner{},
		InboxWarnThreshold: 4, // small for test
		Logger:             slog.New(logH),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Push 3 items — below threshold. No warning.
	for i := 0; i < 3; i++ {
		f.Receive(bridle.InboxItem{MsgID: int64(i + 1), From: "op", Content: "x"})
	}
	if got := logH.inboxPressureCount(); got != 0 {
		t.Errorf("after 3 items: pressure warnings = %d; want 0", got)
	}

	// Push 4th — crosses threshold. One warning.
	f.Receive(bridle.InboxItem{MsgID: 4, From: "op", Content: "x"})
	if got := logH.inboxPressureCount(); got != 1 {
		t.Errorf("after crossing threshold: pressure warnings = %d; want 1", got)
	}

	// Push more items above threshold — should NOT re-warn (latched).
	for i := 5; i <= 10; i++ {
		f.Receive(bridle.InboxItem{MsgID: int64(i), From: "op", Content: "x"})
	}
	if got := logH.inboxPressureCount(); got != 1 {
		t.Errorf("after additional items while latched: pressure warnings = %d; want 1", got)
	}

	// Drain the inbox via Deliberate. Each call pops one. Threshold is
	// 4; half is 2. After we've popped 9 items (10 → 1), we're below
	// half and the latch clears.
	for i := 0; i < 9; i++ {
		if _, err := f.Deliberate(context.Background(), ""); err != nil {
			t.Fatalf("deliberate %d: %v", i, err)
		}
	}

	// One item left in inbox. Push more — should re-warn once we
	// cross threshold again. Push 4 more (1 + 4 = 5, > 4).
	for i := 11; i <= 14; i++ {
		f.Receive(bridle.InboxItem{MsgID: int64(i), From: "op", Content: "x"})
	}
	if got := logH.inboxPressureCount(); got != 2 {
		t.Errorf("after second breach: pressure warnings = %d; want 2 (re-armed after drain)", got)
	}
}

// Compaction with empty tail should not call the provider — there's
// nothing to summarize.
func TestCompact_EmptyTail_NoOp(t *testing.T) {
	f, prov := newTestFunnel(t)
	if err := f.compact(context.Background(), nil, f.resolveBinding()); err != nil {
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
// TestReceive_FIFOOneMessagePerDeliberate pins #224: each Deliberate
// pops exactly ONE item from the FIFO inbox. Remaining items stay
// queued for subsequent Deliberate calls. Prior behavior was
// drain-all-into-one-prompt which collapsed cross-thread context;
// see #224 for the principle.
func TestReceive_FIFOOneMessagePerDeliberate(t *testing.T) {
	f, _ := newTestFunnel(t,
		bridle.ProviderResult{
			FinalText: "first reply",
			Usage:     bridle.Usage{InputTokens: 10, OutputTokens: 1},
		},
		bridle.ProviderResult{
			FinalText: "second reply",
			Usage:     bridle.Usage{InputTokens: 10, OutputTokens: 1},
		},
	)
	f.Receive(bridle.InboxItem{From: "operator", Content: "first"})
	f.Receive(bridle.InboxItem{From: "operator", Content: "second"})

	if f.InboxLen() != 2 {
		t.Fatalf("inbox=%d want 2", f.InboxLen())
	}

	// First Deliberate pops the head, leaving one behind.
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if f.InboxLen() != 1 {
		t.Errorf("after first Deliberate inbox=%d want 1", f.InboxLen())
	}

	// Second Deliberate pops the remaining item.
	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if f.InboxLen() != 0 {
		t.Errorf("after second Deliberate inbox=%d want 0", f.InboxLen())
	}

	// Third Deliberate on empty inbox returns ErrEmptyInbox.
	if _, err := f.Deliberate(context.Background(), ""); err != ErrEmptyInbox {
		t.Errorf("third Deliberate err=%v want ErrEmptyInbox", err)
	}
}

func TestMCPConfigPropagatesToProviderRequest(t *testing.T) {
	// When Config.MCP is non-nil, it must flow through to
	// bridle.ProviderRequest.MCP so the claude-code provider gate
	// (skip --allowedTools when MCP is configured) fires.
	prov := &scriptedProvider{}
	harness := bridle.NewHarness(prov)

	// Empty config is the marker pattern — non-nil pointer, no servers.
	// The claude-code provider gate only checks req.MCP != nil.
	mcpCfg := &bridle.MCPClientConfig{}

	f, err := New(Config{
		AspectID:     "test",
		SystemPrompt: "s",
		Harness:      harness,
		Provider:     "scripted",
		Model:        "m",
		Runner:       noopRunner{},
		MCP:          mcpCfg,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Push one inbox item so Deliberate actually runs a turn.
	f.Receive(bridle.InboxItem{From: "operator", Content: "hello"})

	_, err = f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if prov.last.MCP == nil {
		t.Fatal("MCP was nil in ProviderRequest; want non-nil")
	}
	if prov.last.MCP != mcpCfg {
		t.Errorf("MCP pointer mismatch: config=%p request=%p", mcpCfg, prov.last.MCP)
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

// partialThenErrorProvider returns text + a dummy tool call on invocation 1,
// then errors on invocation 2. This simulates a multi-step harness turn where
// the first provider call produces text and the second fails — the harness
// accumulates the text from step 1 and returns it alongside the error from
// step 2. The funnel should surface that partial text via Return.Handle
// rather than dropping it (NEX-239).
type partialThenErrorProvider struct {
	calls int
}

func (p *partialThenErrorProvider) Name() bridle.ProviderID { return "partial-then-error" }

func (p *partialThenErrorProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI, SupportsCustomTools: true}
}

func (p *partialThenErrorProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	p.calls++
	if p.calls == 1 {
		return bridle.ProviderResult{
			FinalText: "partial analysis from dying subprocess",
			ToolCalls: []bridle.ToolInvocation{{ID: "t1", Name: "noop", Args: json.RawMessage(`{}`)}},
		}, nil
	}
	return bridle.ProviderResult{}, errors.New("claude-code: subprocess exited 1")
}

func TestDeliberate_ErrorPath_SurfacesPartialText(t *testing.T) {
	prov := &partialThenErrorProvider{}
	rr := &recordingReturnHandler{}

	f, err := New(Config{
		AspectID: "test",
		Harness:  bridle.NewHarness(prov),
		Provider: "partial-then-error",
		Model:    "test-model",
		Runner:   noopRunner{},
		Return:   rr,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.Receive(bridle.InboxItem{MsgID: 4242, From: "operator", Content: "diagnose this"})

	_, err = f.Deliberate(context.Background(), "")
	if err == nil {
		t.Fatal("expected error from Deliberate, got nil")
	}

	rr.mu.Lock()
	defer rr.mu.Unlock()

	if len(rr.handles) == 0 {
		t.Fatal("Return.Handle was not called on error path — partial text was dropped")
	}

	h := rr.handles[0]
	if !h.Result.Filter.ShouldPost {
		t.Error("partial result should have ShouldPost=true so text surfaces")
	}
	if h.Result.TurnResult.FinalText != "partial analysis from dying subprocess" {
		t.Errorf("FinalText = %q, want %q", h.Result.TurnResult.FinalText, "partial analysis from dying subprocess")
	}
	if h.Trigger.MsgID != 4242 {
		t.Errorf("trigger MsgID = %d, want 4242", h.Trigger.MsgID)
	}
	if h.Trigger.From != "operator" {
		t.Errorf("trigger From = %q, want operator", h.Trigger.From)
	}
}

// recordingFilter captures every FilterInput it was asked to judge
// and returns ShouldPost=true. Used to verify the funnel populates
// FilterInput correctly (tool names, partial flag, etc.).
type recordingFilter struct {
	mu     sync.Mutex
	inputs []FilterInput
}

func (r *recordingFilter) Judge(_ context.Context, in FilterInput) FilterDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, in)
	return FilterDecision{ShouldPost: true}
}

// TestDeliberate_ErrorPath_RoutesPartialThroughFilter verifies the
// error-path partial-result flow runs through runFilter (not a
// hardcoded ShouldPost=true) so scratch suppression still works on
// partials. The FilterInput must carry Partial=true so the judge can
// lean toward post for coherent partials, plus the tool names so the
// judge weights "thin text + tool work" as complete.
func TestDeliberate_ErrorPath_RoutesPartialThroughFilter(t *testing.T) {
	prov := &partialThenErrorProvider{}
	rf := &recordingFilter{}

	f, err := New(Config{
		AspectID: "test",
		Harness:  bridle.NewHarness(prov),
		Provider: "partial-then-error",
		Model:    "test-model",
		Runner:   noopRunner{},
		Filter:   rf,
		Return:   &recordingReturnHandler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Receive(bridle.InboxItem{MsgID: 7, From: "operator", Content: "diagnose"})

	if _, err := f.Deliberate(context.Background(), ""); err == nil {
		t.Fatal("expected error from Deliberate")
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()
	if len(rf.inputs) != 1 {
		t.Fatalf("filter calls = %d; want 1 (error-path partial routed through filter)", len(rf.inputs))
	}
	in := rf.inputs[0]
	if !in.Partial {
		t.Error("FilterInput.Partial = false; want true on error path")
	}
	if len(in.ToolNames) != 1 || in.ToolNames[0] != "noop" {
		t.Errorf("FilterInput.ToolNames = %v; want [noop]", in.ToolNames)
	}
}

// TestDeliberate_SuccessPath_PassesToolNamesToFilter verifies the
// success path also populates FilterInput.ToolNames so the judge can
// distinguish "thin text + real work via tools" from scratch. Uses
// scriptedProvider's two-result chain: first invocation emits tool
// calls (harness runs them via noopRunner), second emits the final
// text — bridle's harness accumulates both into result.ToolCalls and
// result.FinalText.
func TestDeliberate_SuccessPath_PassesToolNamesToFilter(t *testing.T) {
	prov := &scriptedProvider{
		results: []bridle.ProviderResult{
			{ToolCalls: []bridle.ToolInvocation{
				{ID: "a", Name: "Bash", Args: json.RawMessage(`{}`)},
				{ID: "b", Name: "Write", Args: json.RawMessage(`{}`)},
			}},
			{FinalText: "done"},
		},
	}
	rf := &recordingFilter{}
	f, err := New(Config{
		AspectID: "test",
		Harness:  bridle.NewHarness(prov),
		Provider: "scripted",
		Model:    "m",
		Runner:   noopRunner{},
		Filter:   rf,
		MaxStepsPerTurn: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Receive(bridle.InboxItem{MsgID: 1, From: "operator", Content: "go"})

	if _, err := f.Deliberate(context.Background(), ""); err != nil {
		t.Fatalf("Deliberate: %v", err)
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()
	if len(rf.inputs) != 1 {
		t.Fatalf("filter calls = %d; want 1", len(rf.inputs))
	}
	in := rf.inputs[0]
	if in.Partial {
		t.Error("FilterInput.Partial = true on success path; want false")
	}
	if len(in.ToolNames) != 2 || in.ToolNames[0] != "Bash" || in.ToolNames[1] != "Write" {
		t.Errorf("FilterInput.ToolNames = %v; want [Bash Write]", in.ToolNames)
	}
}

// multiBlockProvider is a subprocess-stream stub that replays a scripted
// sequence of bridle events in a single RunTurn call. Used to test that
// the funnel's streaming chat sink posts each text block and suppresses
// tool events.
type multiBlockProvider struct {
	events []bridle.Event
}

func (p *multiBlockProvider) Name() bridle.ProviderID { return "multi-block" }
func (p *multiBlockProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategorySubprocessStream}
}
func (p *multiBlockProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	var finalText string
	for _, ev := range p.events {
		sink.Emit(ev)
		if chunk, ok := ev.(bridle.ModelChunk); ok {
			finalText = chunk.Text
		}
	}
	return bridle.ProviderResult{
		FinalText:  finalText,
		StopReason: bridle.StopReasonModelDone,
	}, nil
}

func TestStreamTextToChat_PerBlockEmit(t *testing.T) {
	chat := &recordingChatGateway{}
	prov := &multiBlockProvider{
		events: []bridle.Event{
			bridle.ModelChunk{Text: "First text block"},
			bridle.ToolCallStart{ID: "tc1", Name: "grep"},
			bridle.ToolCallResult{ID: "tc1"},
			bridle.ModelChunk{Text: "Second text block"},
			bridle.ToolCallStart{ID: "tc2", Name: "write"},
			bridle.ToolCallResult{ID: "tc2"},
			bridle.ModelChunk{Text: "Third text block"},
		},
	}

	f, err := New(Config{
		AspectID:         "test-aspect",
		SystemPrompt:     "test",
		Harness:          bridle.NewHarness(prov),
		Provider:         "multi-block",
		Model:            "test-model",
		Runner:           NullRunner{},
		ChatGateway:      chat,
		StreamTextToChat: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.ReceiveWithMsgID(bridle.InboxItem{
		From: "operator", Content: "hello", ThreadRoot: 42,
	}, 100)

	_, err = f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	// 3 text blocks → 3 chat posts, 0 tool messages.
	if len(chat.sends) != 3 {
		t.Fatalf("expected 3 chat sends (one per text block), got %d: %+v", len(chat.sends), chat.sends)
	}

	// Verify no tool content leaked into chat.
	for i, s := range chat.sends {
		if strings.Contains(s.Text, "tool") {
			t.Errorf("send[%d]: tool content leaked to chat: %q", i, s.Text)
		}
	}

	// First block replies to trigger (100); subsequent chain to prior block.
	if chat.sends[0].ReplyTo != 100 {
		t.Errorf("first block reply_to=%d, want 100", chat.sends[0].ReplyTo)
	}
	if chat.sends[1].ReplyTo != 1 {
		t.Errorf("second block reply_to=%d, want 1 (prior block msg_id)", chat.sends[1].ReplyTo)
	}
	if chat.sends[2].ReplyTo != 2 {
		t.Errorf("third block reply_to=%d, want 2 (prior block msg_id)", chat.sends[2].ReplyTo)
	}

	// Auto-post must be suppressed when streaming — 3 sends total,
	// not 4 (3 streamed + 1 auto-post).
	// (verified implicitly: len(chat.sends)==3 means no 4th auto-post)
}

func TestStreamTextToChat_SingleBlockNoRegression(t *testing.T) {
	chat := &recordingChatGateway{}
	prov := &multiBlockProvider{
		events: []bridle.Event{
			bridle.ModelChunk{Text: "Single reply"},
		},
	}

	f, err := New(Config{
		AspectID:         "test-aspect",
		SystemPrompt:     "test",
		Harness:          bridle.NewHarness(prov),
		Provider:         "multi-block",
		Model:            "test-model",
		Runner:           NullRunner{},
		ChatGateway:      chat,
		StreamTextToChat: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.ReceiveWithMsgID(bridle.InboxItem{
		From: "operator", Content: "hello", ThreadRoot: 42,
	}, 100)

	_, err = f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if len(chat.sends) != 1 {
		t.Fatalf("expected 1 chat send, got %d", len(chat.sends))
	}
	if chat.sends[0].Text != "Single reply" {
		t.Errorf("text=%q want 'Single reply'", chat.sends[0].Text)
	}
	if chat.sends[0].ReplyTo != 100 {
		t.Errorf("reply_to=%d want 100", chat.sends[0].ReplyTo)
	}
}

func TestStreamTextToChat_EmptyBlocksSkipped(t *testing.T) {
	chat := &recordingChatGateway{}
	prov := &multiBlockProvider{
		events: []bridle.Event{
			bridle.ModelChunk{Text: "  \t\n  "}, // whitespace-only → skipped
			bridle.ModelChunk{Text: "Real content"},
		},
	}

	f, err := New(Config{
		AspectID:         "test-aspect",
		SystemPrompt:     "test",
		Harness:          bridle.NewHarness(prov),
		Provider:         "multi-block",
		Model:            "test-model",
		Runner:           NullRunner{},
		ChatGateway:      chat,
		StreamTextToChat: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.ReceiveWithMsgID(bridle.InboxItem{
		From: "operator", Content: "hello", ThreadRoot: 42,
	}, 100)

	_, err = f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if len(chat.sends) != 1 {
		t.Fatalf("expected 1 chat send (whitespace-only block skipped), got %d", len(chat.sends))
	}
	if chat.sends[0].Text != "Real content" {
		t.Errorf("text=%q want 'Real content'", chat.sends[0].Text)
	}
}

func TestStreamTextToChat_DisabledPreservesAutoPost(t *testing.T) {
	chat := &recordingChatGateway{}
	prov := &multiBlockProvider{
		events: []bridle.Event{
			bridle.ModelChunk{Text: "Buffered text"},
		},
	}

	f, err := New(Config{
		AspectID:     "test-aspect",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(prov),
		Provider:     "multi-block",
		Model:        "test-model",
		Runner:       NullRunner{},
		ChatGateway:  chat,
		// StreamTextToChat false → buffered auto-post path
	})
	if err != nil {
		t.Fatal(err)
	}

	f.ReceiveWithMsgID(bridle.InboxItem{
		From: "operator", Content: "hello", ThreadRoot: 42,
	}, 100)

	_, err = f.Deliberate(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	// Without streaming, the auto-post fires via NexusChatReturnHandler.
	if len(chat.sends) != 1 {
		t.Fatalf("expected 1 auto-post, got %d", len(chat.sends))
	}
	if chat.sends[0].Text != "Buffered text" {
		t.Errorf("text=%q want 'Buffered text'", chat.sends[0].Text)
	}
}

// NEX-300 main-turn slice: Config.MainTurnSampling fields flow
// through to the bridle.TurnRequest the funnel builds. Asserts via
// the scripted-provider's captured-request inspection. Pins both
// directions: explicit values land, zero/nil values stay unset on
// the request so providers fall through to their own defaults.
func TestDeliberate_NEX300_MainTurnSamplingThreadsToRequest(t *testing.T) {
	temp := 0.7
	topP := 0.95
	seed := 1234
	prov := &scriptedProvider{results: []bridle.ProviderResult{{
		FinalText: "ok",
		Usage:     bridle.Usage{InputTokens: 5, OutputTokens: 1},
	}}}
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(prov),
		Provider:     "scripted",
		Model:        "test-model",
		Runner:       noopRunner{},
		MainTurnSampling: MainTurnSampling{
			Temperature:     &temp,
			TopP:            &topP,
			Seed:            &seed,
			MaxOutputTokens: 4096,
			StopSequences:   []string{"</done>"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	if prov.last.Temperature == nil || *prov.last.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", prov.last.Temperature)
	}
	if prov.last.TopP == nil || *prov.last.TopP != 0.95 {
		t.Errorf("TopP = %v, want 0.95", prov.last.TopP)
	}
	if prov.last.Seed == nil || *prov.last.Seed != 1234 {
		t.Errorf("Seed = %v, want 1234", prov.last.Seed)
	}
	if prov.last.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens = %d, want 4096", prov.last.MaxOutputTokens)
	}
	if len(prov.last.StopSequences) != 1 || prov.last.StopSequences[0] != "</done>" {
		t.Errorf("StopSequences = %v, want [</done>]", prov.last.StopSequences)
	}
	// TopK left unset by operator — should stay nil on the wire.
	if prov.last.TopK != nil {
		t.Errorf("TopK should be nil when unset; got %v", *prov.last.TopK)
	}
}

// NEX-300 main-turn slice back-compat: a Config with zero
// MainTurnSampling leaves every sampling field unset on the bridle
// request — provider defaults preserved, no behaviour change for
// callers that don't opt in.
func TestDeliberate_NEX300_ZeroMainTurnSamplingPreservesDefaults(t *testing.T) {
	f, prov := newTestFunnel(t,
		bridle.ProviderResult{FinalText: "k", Usage: bridle.Usage{InputTokens: 1, OutputTokens: 1}},
	)
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	if prov.last.Temperature != nil {
		t.Errorf("Temperature should be nil when not configured; got %v", *prov.last.Temperature)
	}
	if prov.last.TopP != nil {
		t.Errorf("TopP should be nil; got %v", *prov.last.TopP)
	}
	if prov.last.Seed != nil {
		t.Errorf("Seed should be nil; got %v", *prov.last.Seed)
	}
	if prov.last.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens should be 0; got %d", prov.last.MaxOutputTokens)
	}
	if len(prov.last.StopSequences) != 0 {
		t.Errorf("StopSequences should be empty; got %v", prov.last.StopSequences)
	}
}

// NEX-335 BindingFn pattern: per-turn resolver supersedes static
// Harness/Provider/Model. Validates the dynamic-config rails the
// NEX-332 arc builds on.

func TestBindingFn_OverridesStaticHarness(t *testing.T) {
	// Static provider should NOT receive any call once BindingFn is set.
	staticProv := &scriptedProvider{}
	// Dynamic provider that BindingFn routes to.
	dynProv := &scriptedProvider{}

	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(staticProv),
		Provider:     "static",
		Model:        "static-model",
		Runner:       noopRunner{},
		BindingFn: func() Binding {
			return Binding{
				Provider: "dynamic",
				Model:    "dynamic-model",
				Harness:  bridle.NewHarness(dynProv),
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Deliberate(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}

	if got := dynProv.calls.Load(); got != 1 {
		t.Errorf("dynamic provider calls = %d, want 1", got)
	}
	if got := staticProv.calls.Load(); got != 0 {
		t.Errorf("static provider should not have been called; got %d", got)
	}
}

// TestBindingFn_SwapBetweenTurns is the resolver-pattern smoke test:
// the funnel picks up a swapped binding on the NEXT turn without
// reconstruction. This is the property NEX-332 phase 5 (config.refresh
// push) will exploit — the broker push handler just calls
// cache.Store(newBinding) and the next turn auto-routes.
func TestBindingFn_SwapBetweenTurns(t *testing.T) {
	provA := &scriptedProvider{}
	provB := &scriptedProvider{}
	bindingA := Binding{Provider: "A", Model: "A-model", Harness: bridle.NewHarness(provA)}
	bindingB := Binding{Provider: "B", Model: "B-model", Harness: bridle.NewHarness(provB)}

	var current atomic.Pointer[Binding]
	current.Store(&bindingA)

	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(&scriptedProvider{}), // unused; satisfies validation
		Provider:     "static",
		Model:        "static",
		Runner:       noopRunner{},
		BindingFn:    func() Binding { return *current.Load() },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1 → bindingA
	if _, err := f.Deliberate(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if provA.calls.Load() != 1 || provB.calls.Load() != 0 {
		t.Fatalf("after turn 1: A=%d B=%d, want A=1 B=0",
			provA.calls.Load(), provB.calls.Load())
	}

	// Operator-flip-mid-conversation: swap the cached binding.
	current.Store(&bindingB)

	// Turn 2 → bindingB
	if _, err := f.Deliberate(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if provA.calls.Load() != 1 || provB.calls.Load() != 1 {
		t.Fatalf("after turn 2: A=%d B=%d, want A=1 B=1",
			provA.calls.Load(), provB.calls.Load())
	}
}

// TestBindingFn_TurnRequestCarriesBindingProviderAndModel verifies the
// resolved binding's Provider+Model land on the TurnRequest sent to
// bridle — not the static cfg.Provider/Model fields. This is what
// downstream usage attribution + observability log against.
func TestBindingFn_TurnRequestCarriesBindingProviderAndModel(t *testing.T) {
	prov := &scriptedProvider{}
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(&scriptedProvider{}),
		Provider:     "static-id",
		Model:        "static-model",
		Runner:       noopRunner{},
		BindingFn: func() Binding {
			return Binding{
				Provider: "dynamic-id",
				Model:    "dynamic-model",
				Harness:  bridle.NewHarness(prov),
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	if string(prov.last.Session.ID) == "" {
		// Just verifying the request actually reached the provider.
	}
	// scriptedProvider stashes the ProviderRequest in .last; check
	// that req.Provider/req.Model came from the binding, not from cfg.
	// ProviderRequest doesn't carry the bridle.ProviderID directly
	// (it's a ProviderRequest, not TurnRequest), so we check via the
	// captured last.Model + observation that prov was called at all.
	if prov.last.Model != "dynamic-model" {
		t.Errorf("ProviderRequest.Model = %q, want dynamic-model (from binding)",
			prov.last.Model)
	}
}
