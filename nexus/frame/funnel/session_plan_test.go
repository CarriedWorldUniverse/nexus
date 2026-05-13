// Tests covering scenarios from agent-network/docs/
// 2026-05-13-session-thread-disconnect-test-plan.md §5.1.
//
// These pin the funnel-side invariants we made testable today via
// #211 (EventFilterJudged + ErrorClass), #219 (StopReasonProcessExit
// preserves partial content), and #196 (bare judge wiring).
//
// Scenarios covered here are the ones exercisable against a scripted
// bridle.Provider — S-1 (partial-content recovery), S-2 (clean error
// path), S-8 (filter suppression visibility). S-3/S-4/S-5/S-6/S-7
// need an integration harness (live WS, real claudecode argv, etc.)
// and land in a separate package.

package funnel

import (
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// s2ErroringProvider returns an error from RunTurn without producing any
// content. Models S-2: subprocess died before assistant text.
type s2ErroringProvider struct {
	err error
}

func (p *s2ErroringProvider) Name() bridle.ProviderID { return "erroring" }

func (p *s2ErroringProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{
		Category:               bridle.CategoryDirectAPI,
		SupportsCustomTools:    true,
		SupportsBeforeToolCall: true,
		SupportsAfterToolCall:  true,
	}
}

func (p *s2ErroringProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	return bridle.ProviderResult{}, p.err
}

// S-2: provider error before producing content must surface as a
// Deliberate error AND must NOT post anything to chat. The funnel's
// auto-post branch should be unreachable on the error path. This is
// the inverse of the S-1 (partial-content) case — without an explicit
// test, regressions where Deliberate swallows errors silently could
// land unnoticed.
func TestPlan_S2_NoContentNoPost(t *testing.T) {
	sink := &recordingSink{}
	prov := &s2ErroringProvider{err: errors.New("subprocess died: exit status 1")}

	chat := &recordingChatGateway{}
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness:      bridle.NewHarness(prov),
		Provider:     "erroring",
		Model:        "m",
		Runner:       noopRunner{},
		Events:       sink,
		ChatGateway:  chat,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = f.Deliberate(context.Background(), "ping")
	if err == nil {
		t.Fatal("Deliberate must return an error when provider errors with no content")
	}

	// No chat send: auto-post is gated on result.FinalText being
	// non-empty, and zero-value ProviderResult has empty FinalText.
	if len(chat.sends) != 0 {
		t.Errorf("chat sends on error path: got %d, want 0; sends=%v", len(chat.sends), chat.sends)
	}

	// turn.end must still fire (Lock 5 invariant — every turn.start has
	// a paired turn.end). But ErrorClass on the payload remains empty
	// because we didn't take the partial-content branch — that branch
	// is keyed off StopReasonProcessExit, and zero-value results have
	// empty StopReason. ErrorClass is only set when there's content
	// to preserve.
	var sawEnd bool
	for _, e := range sink.snapshot() {
		if e.Type != EventTurnEnd {
			continue
		}
		sawEnd = true
		payload := e.Payload.(TurnEndPayload)
		if payload.ErrorClass != "" {
			t.Errorf("ErrorClass on no-content error: got %q, want empty (the partial-content branch should not fire when nothing was produced)", payload.ErrorClass)
		}
	}
	if !sawEnd {
		t.Error("turn.end did not fire on provider error — Lock 5 invariant broken")
	}
}

// recordingChatGateway captures send/react calls for assertion.
// Minimal — only fields the auto-post and react paths touch.
type recordingChatGateway struct {
	sends []recordedSend
	reacts []recordedReact
}

type recordedSend struct {
	Text    string
	ReplyTo int64
	Topic   string
}

type recordedReact struct {
	MsgID int64
	Emoji string
}

func (g *recordingChatGateway) SendChat(_ context.Context, text string, replyTo int64, topic string) (int64, error) {
	g.sends = append(g.sends, recordedSend{Text: text, ReplyTo: replyTo, Topic: topic})
	return int64(len(g.sends)), nil
}

func (g *recordingChatGateway) ReactTo(_ context.Context, msgID int64, emoji string) error {
	g.reacts = append(g.reacts, recordedReact{MsgID: msgID, Emoji: emoji})
	return nil
}

// The rest of the ChatGateway methods are unused by these tests but
// must be present to satisfy the interface. Return harmless defaults.
func (g *recordingChatGateway) ReadThread(_ context.Context, _ int64, _ int64) ([]ChatMessage, error) {
	return nil, nil
}
func (g *recordingChatGateway) AnnounceFile(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}
func (g *recordingChatGateway) ShareFile(_ context.Context, _ string, _ []string) (int64, error) {
	return 0, nil
}
func (g *recordingChatGateway) ReadMessage(_ context.Context, _ int64) (ChatMessage, error) {
	return ChatMessage{}, nil
}
func (g *recordingChatGateway) ListShared(_ context.Context, _ int) ([]SharedFileRef, error) {
	return nil, nil
}
func (g *recordingChatGateway) GetShared(_ context.Context, _ int64) (SharedFileRef, error) {
	return SharedFileRef{}, nil
}
func (g *recordingChatGateway) StoreKnowledge(_ context.Context, _, _, _ string, _ bool) (int64, error) {
	return 0, nil
}
func (g *recordingChatGateway) SearchKnowledge(_ context.Context, _ KnowledgeQuery) ([]KnowledgeHit, error) {
	return nil, nil
}
func (g *recordingChatGateway) GetKnowledgeShared(_ context.Context, _, _ string) (bool, bool, error) {
	return false, false, nil
}

// S-1 supplement: partial-content turn auto-posts. TestEmit_TurnEndErrorClass
// already covers the event payload; this verifies the chat-side effect.
// Without this, a regression where ErrorClass is set correctly but the
// auto-post is somehow gated off StopReason would go unnoticed.
func TestPlan_S1_PartialContentAutoPosts(t *testing.T) {
	sink := &recordingSink{}
	chat := &recordingChatGateway{}
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness: bridle.NewHarness(&scriptedProvider{
			results: []bridle.ProviderResult{
				{
					FinalText:  "partial extract that survived the cap",
					Usage:      bridle.Usage{InputTokens: 100, OutputTokens: 7000},
					StopReason: bridle.StopReasonProcessExit,
				},
			},
		}),
		Provider:    "scripted",
		Model:       "m",
		Runner:      noopRunner{},
		Events:      sink,
		ChatGateway: chat,
		Filter:      AlwaysPostFilter{}, // ensure filter doesn't gate this test
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := f.Deliberate(context.Background(), "long prompt"); err != nil {
		t.Fatalf("Deliberate: %v", err)
	}

	if len(chat.sends) != 1 {
		t.Fatalf("chat sends: got %d, want 1 (partial content must auto-post)", len(chat.sends))
	}
	if chat.sends[0].Text != "partial extract that survived the cap" {
		t.Errorf("posted text mismatch: got %q", chat.sends[0].Text)
	}
}

// S-8: filter suppression posts nothing and emits filter.judged with
// ShouldPost=false. Pins the visibility invariant the dashboard relies
// on — pre-#211 a no-post was indistinguishable from a no-content turn.
func TestPlan_S8_SuppressedTurnEmitsJudgedFalse(t *testing.T) {
	sink := &recordingSink{}
	chat := &recordingChatGateway{}

	// neverPostFilter always suppresses; mirrors a worst-case judge ruling.
	f, err := New(Config{
		AspectID:     "frame",
		SystemPrompt: "test",
		Harness: bridle.NewHarness(&scriptedProvider{
			results: []bridle.ProviderResult{
				{
					FinalText:  "scratch-mode self talk",
					Usage:      bridle.Usage{InputTokens: 10, OutputTokens: 5},
					StopReason: bridle.StopReasonModelDone,
				},
			},
		}),
		Provider:    "scripted",
		Model:       "m",
		Runner:      noopRunner{},
		Events:      sink,
		ChatGateway: chat,
		Filter:      neverPostFilter{reason: "scratch"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := f.Deliberate(context.Background(), "ping"); err != nil {
		t.Fatalf("Deliberate: %v", err)
	}

	// No post.
	if len(chat.sends) != 0 {
		t.Errorf("chat sends on suppressed turn: got %d, want 0", len(chat.sends))
	}

	// filter.judged with ShouldPost=false MUST fire.
	var judged *FilterJudgedPayload
	for _, e := range sink.snapshot() {
		if e.Type == EventFilterJudged {
			p := e.Payload.(FilterJudgedPayload)
			judged = &p
			break
		}
	}
	if judged == nil {
		t.Fatal("filter.judged did not fire on suppressed turn — pre-#211 silent-suppress regression")
	}
	if judged.ShouldPost {
		t.Errorf("ShouldPost: got true, want false on suppressed turn")
	}
	if judged.Reason != "scratch" {
		t.Errorf("Reason: got %q, want %q", judged.Reason, "scratch")
	}
	if judged.FinalTextLen != len("scratch-mode self talk") {
		t.Errorf("FinalTextLen: got %d, want %d", judged.FinalTextLen, len("scratch-mode self talk"))
	}
}

// neverPostFilter is an OutputFilter that always returns ShouldPost=false.
type neverPostFilter struct{ reason string }

func (f neverPostFilter) Judge(_ context.Context, _ FilterInput) FilterDecision {
	return FilterDecision{ShouldPost: false, Reason: f.reason}
}
