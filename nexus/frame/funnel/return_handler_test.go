package funnel

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// recordingReturnHandler captures every OnTurnStart and Handle call
// so tests can assert the engine wires through to the interface at
// the right points with the right trigger context.
type recordingReturnHandler struct {
	mu      sync.Mutex
	starts  []TurnTrigger
	handles []handleCall
}

type handleCall struct {
	Result  DeliberateResult
	Trigger TurnTrigger
}

func (r *recordingReturnHandler) OnTurnStart(_ context.Context, t TurnTrigger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, t)
	return nil
}

func (r *recordingReturnHandler) Handle(_ context.Context, result DeliberateResult, t TurnTrigger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles = append(r.handles, handleCall{Result: result, Trigger: t})
	return nil
}

// TestTriggerFromInboxItem pins the trigger-construction helper. The
// engine builds the typed TurnTrigger from the popped bridle.InboxItem
// at the top of Deliberate; this test guards against drift in the
// field mapping (e.g. an InboxItem schema addition that the engine
// should also surface through the interface).
func TestTriggerFromInboxItem(t *testing.T) {
	item := bridle.InboxItem{
		MsgID:      4242,
		From:       "operator",
		Content:    "@keel quick question",
		ThreadRoot: 4100,
	}
	got := triggerFromInboxItem(item)
	if got.MsgID != 4242 {
		t.Errorf("MsgID: got %d want 4242", got.MsgID)
	}
	if got.From != "operator" {
		t.Errorf("From: got %q want operator", got.From)
	}
	if got.Content != "@keel quick question" {
		t.Errorf("Content: got %q", got.Content)
	}
	if got.ThreadRoot != 4100 {
		t.Errorf("ThreadRoot: got %d want 4100", got.ThreadRoot)
	}
	if got.Source != "chat" {
		t.Errorf("Source: got %q want chat (legacy default)", got.Source)
	}
}

// TestNoopReturnHandler is a sanity-check on the noop — it should
// always succeed and never touch anything.
func TestNoopReturnHandler(t *testing.T) {
	var h ReturnHandler = NoopReturnHandler{}
	if err := h.OnTurnStart(context.Background(), TurnTrigger{MsgID: 1}); err != nil {
		t.Errorf("OnTurnStart: unexpected err %v", err)
	}
	if err := h.Handle(context.Background(), DeliberateResult{}, TurnTrigger{}); err != nil {
		t.Errorf("Handle: unexpected err %v", err)
	}
}

// TestNexusChatReturnHandler_NilGateway pins the safe-degrade: a nil
// gateway turns the handler into a noop without panicking.
func TestNexusChatReturnHandler_NilGateway(t *testing.T) {
	h := &NexusChatReturnHandler{Gateway: nil}
	if err := h.OnTurnStart(context.Background(), TurnTrigger{MsgID: 1}); err != nil {
		t.Errorf("nil-gateway OnTurnStart should noop, got err %v", err)
	}
	if err := h.Handle(context.Background(), DeliberateResult{Filter: FilterDecision{ShouldPost: true}}, TurnTrigger{MsgID: 1}); err != nil {
		t.Errorf("nil-gateway Handle should noop, got err %v", err)
	}
}

// TestNexusChatReturnHandler_NoTrigger pins: when the trigger MsgID
// is 0 (operator REST, eval), OnTurnStart skips the reaction (nothing
// to react to). Handle still runs — it can still auto-post a
// top-level message.
func TestNexusChatReturnHandler_NoTrigger(t *testing.T) {
	g := &fakeGateway{}
	h := &NexusChatReturnHandler{Gateway: g}
	_ = h.OnTurnStart(context.Background(), TurnTrigger{MsgID: 0})
	if len(g.reactions) != 0 {
		t.Errorf("OnTurnStart with zero-MsgID should not call ReactTo, got %d calls", len(g.reactions))
	}
}

// TestNexusChatReturnHandler_OnTurnStart_Eyes pins the start-pulse
// reaction emoji. 👀 = "I saw it, I'm working on it."
func TestNexusChatReturnHandler_OnTurnStart_Eyes(t *testing.T) {
	g := &fakeGateway{}
	h := &NexusChatReturnHandler{Gateway: g}
	if err := h.OnTurnStart(context.Background(), TurnTrigger{MsgID: 4242}); err != nil {
		t.Fatalf("OnTurnStart: %v", err)
	}
	if len(g.reactions) != 1 {
		t.Fatalf("expected 1 reaction, got %d", len(g.reactions))
	}
	if g.reactions[0].Emoji != "👀" {
		t.Errorf("emoji: got %q want 👀", g.reactions[0].Emoji)
	}
	if g.reactions[0].MsgID != 4242 {
		t.Errorf("msgID: got %d want 4242", g.reactions[0].MsgID)
	}
}

// TestNexusChatReturnHandler_ShouldPostAutoPosts pins the autopost
// path: filter approves AND text non-empty → SendChat called with
// reply_to set to the trigger MsgID, and resolve-emoji is 👀 (toggle-
// off).
func TestNexusChatReturnHandler_ShouldPostAutoPosts(t *testing.T) {
	g := &fakeGateway{}
	h := &NexusChatReturnHandler{Gateway: g}
	trigger := TurnTrigger{MsgID: 4242, From: "operator"}
	result := DeliberateResult{
		TurnResult: bridle.TurnResult{FinalText: "Hello back."},
		Filter:     FilterDecision{ShouldPost: true},
	}
	if err := h.Handle(context.Background(), result, trigger); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(g.sentMessages) != 1 {
		t.Fatalf("expected 1 SendChat, got %d", len(g.sentMessages))
	}
	if g.sentMessages[0].Content != "Hello back." {
		t.Errorf("posted text: %q", g.sentMessages[0].Content)
	}
	if g.sentMessages[0].ReplyTo != 4242 {
		t.Errorf("reply_to: got %d want 4242", g.sentMessages[0].ReplyTo)
	}
	if len(g.reactions) != 1 || g.reactions[0].Emoji != "👀" {
		t.Errorf("expected one 👀 reaction (toggle-off), got %+v", g.reactions)
	}
}

// TestNexusChatReturnHandler_FilterSuppressedSubstantive pins the 🙊
// path: filter says don't post, but the model had substantive text →
// react with 🙊 so operator can audit the suppression.
func TestNexusChatReturnHandler_FilterSuppressedSubstantive(t *testing.T) {
	g := &fakeGateway{}
	h := &NexusChatReturnHandler{Gateway: g}
	trigger := TurnTrigger{MsgID: 4242}
	result := DeliberateResult{
		TurnResult: bridle.TurnResult{FinalText: "Lots of thinking here."},
		Filter:     FilterDecision{ShouldPost: false},
	}
	_ = h.Handle(context.Background(), result, trigger)
	if len(g.sentMessages) != 0 {
		t.Errorf("expected no SendChat when filter suppresses, got %d", len(g.sentMessages))
	}
	if len(g.reactions) != 1 || g.reactions[0].Emoji != "🙊" {
		t.Errorf("expected one 🙊 reaction, got %+v", g.reactions)
	}
}

// TestNexusChatReturnHandler_NothingToAdd pins the 👍 path: filter
// says don't post AND model genuinely had nothing → react with 👍.
func TestNexusChatReturnHandler_NothingToAdd(t *testing.T) {
	g := &fakeGateway{}
	h := &NexusChatReturnHandler{Gateway: g}
	trigger := TurnTrigger{MsgID: 4242}
	result := DeliberateResult{
		TurnResult: bridle.TurnResult{FinalText: ""},
		Filter:     FilterDecision{ShouldPost: false},
	}
	_ = h.Handle(context.Background(), result, trigger)
	if len(g.reactions) != 1 || g.reactions[0].Emoji != "👍" {
		t.Errorf("expected one 👍 reaction, got %+v", g.reactions)
	}
}

// TestFunnelNew_DefaultsToChatReturnHandler pins the new-funnel
// backward-compat path: a Config with ChatGateway set and Return
// nil yields a NexusChatReturnHandler (the pre-NEX-82 behavior).
func TestFunnelNew_DefaultsToChatReturnHandler(t *testing.T) {
	g := &fakeGateway{}
	f, err := New(Config{
		AspectID:    "test",
		Harness:     bridle.NewHarness(nil),
		Provider:    "claude-code",
		Model:       "claude-haiku-4-5-20251001",
		Runner:      nopRunner{},
		ChatGateway: g,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := f.cfg.Return.(*NexusChatReturnHandler); !ok {
		t.Errorf("Return: got %T want *NexusChatReturnHandler", f.cfg.Return)
	}
}

// TestFunnelNew_NoGatewayDefaultsToNoop pins: when no ChatGateway is
// configured and no explicit Return, the default is the noop handler.
// Headless paths (operator REST eval) land here.
func TestFunnelNew_NoGatewayDefaultsToNoop(t *testing.T) {
	f, err := New(Config{
		AspectID: "test",
		Harness:  bridle.NewHarness(nil),
		Provider: "claude-code",
		Model:    "claude-haiku-4-5-20251001",
		Runner:   nopRunner{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := f.cfg.Return.(NoopReturnHandler); !ok {
		t.Errorf("Return: got %T want NoopReturnHandler", f.cfg.Return)
	}
}

// TestFunnelNew_ExplicitReturnPreserved pins that an explicit Return
// overrides both defaults — agora-side handlers land here when
// they're wired.
func TestFunnelNew_ExplicitReturnPreserved(t *testing.T) {
	r := &recordingReturnHandler{}
	g := &fakeGateway{} // also set ChatGateway to confirm explicit wins
	f, err := New(Config{
		AspectID:    "test",
		Harness:     bridle.NewHarness(nil),
		Provider:    "claude-code",
		Model:       "claude-haiku-4-5-20251001",
		Runner:      nopRunner{},
		ChatGateway: g,
		Return:      r,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if f.cfg.Return != r {
		t.Errorf("Return: explicit handler not preserved (got %v want %v)", f.cfg.Return, r)
	}
}

// nopRunner is a no-op bridle.ToolRunner for funnel.New configs in
// tests that don't actually run turns. The funnel constructor
// requires a non-nil Runner; any value satisfies that here.
type nopRunner struct{}

func (nopRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return nil, nil
}
