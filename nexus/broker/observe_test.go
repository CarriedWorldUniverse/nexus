package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

// installObservabilityRecipientPolicy attaches a RecipientPolicy that
// fans out to @-mentioned aspects. The observability inbound tests
// need recipients computed so the Hub sees the inbound side; without
// a policy, HandleChatSend only emits the outbound ChatFrame for the
// sender. Frame name is left empty — the tests don't exercise
// default-to-Frame behavior.
func installObservabilityRecipientPolicy(b *Broker) {
	policy := RecipientPolicy{
		// Aspects returns the empty set so @all expansion is a no-op;
		// the tests use explicit @<name> mentions which don't need
		// roster lookup.
		Aspects: func() []string { return nil },
	}
	b.cfg.RecipientPolicy = &policy
}

// decodeObserveFrame extracts the ObserveFramePayload and decodes
// its embedded observability.Frame for an observe.frame envelope.
func decodeObserveFrame(t *testing.T, env frames.Envelope) (frames.ObserveFramePayload, observability.Frame) {
	t.Helper()
	var op frames.ObserveFramePayload
	if err := json.Unmarshal(env.Payload, &op); err != nil {
		t.Fatalf("observe.frame payload decode: %v", err)
	}
	var f observability.Frame
	if err := json.Unmarshal(op.Frame, &f); err != nil {
		t.Fatalf("observe frame decode: %v", err)
	}
	return op, f
}

// decodeChatFrame unmarshals the ChatFrame payload from an
// observability.Frame whose Kind is FrameChat.
func decodeChatFrame(t *testing.T, f observability.Frame) observability.ChatFrame {
	t.Helper()
	if f.Kind != observability.FrameChat {
		t.Fatalf("expected FrameChat, got %s", f.Kind)
	}
	var cf observability.ChatFrame
	if err := json.Unmarshal(f.Payload, &cf); err != nil {
		t.Fatalf("chat frame decode: %v", err)
	}
	return cf
}

// subscribeObserve performs subscribe.observe + waits for the
// subscribe.ack, returning all observe.frame envelopes that arrived
// before the ack (buffered tail replay).
func subscribeObserve(t *testing.T, c *websocket.Conn, aspect string, sinceSeq int64) []frames.Envelope {
	t.Helper()
	req, err := frames.NewRequest(frames.KindSubscribeObserve, frames.SubscribeObservePayload{
		Aspect:   aspect,
		SinceSeq: sinceSeq,
	})
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	sendFrame(t, c, req)

	var replay []frames.Envelope
	deadline := time.Now().Add(brokerAsyncWait)
	for time.Now().Before(deadline) {
		env, ok := recvFrameWithTimeout(t, c, time.Until(deadline))
		if !ok {
			t.Fatal("subscribe.observe: ack never arrived")
		}
		if env.Kind == frames.KindSubscribeAck && env.InReplyTo == req.ID {
			return replay
		}
		if env.Kind == frames.KindObserveFrame {
			replay = append(replay, env)
			continue
		}
		// Drop unrelated frames.
	}
	t.Fatal("subscribe.observe: timed out")
	return nil
}

func TestObserve_BufferedTailReplay(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)

	// Pre-seed the Hub's buffer before the operator subscribes.
	// N=10 historical frames exercises the drain-then-flag ordering
	// in handleSubscribeObserve: the operator must receive frames
	// 1..N in sequence order without any later-arriving live frame
	// jumping the queue. (A concurrency probe — fire chat.send while
	// the subscribe is in flight — is hard to schedule deterministically;
	// the structural fix is what we lean on, this asserts the
	// invariant that fix preserves.)
	const N = 10
	g := b.observability.GrouperFor("plumb")
	for i := int64(1); i <= N; i++ {
		g.OnChat(chat.Message{
			ID:        i,
			From:      "plumb",
			Content:   "seeded",
			CreatedAt: time.Now().UTC(),
		}, observability.DirectionOutbound)
	}

	c := dialWS(t, srv, tok)
	replay := subscribeObserve(t, c, "plumb", 0)
	if len(replay) != N {
		t.Fatalf("replay length: got %d want %d", len(replay), N)
	}
	for i, env := range replay {
		op, f := decodeObserveFrame(t, env)
		if op.Aspect != "plumb" {
			t.Errorf("replay[%d] aspect: got %q want plumb", i, op.Aspect)
		}
		if f.Sequence != int64(i+1) {
			t.Errorf("replay[%d] sequence: got %d want %d", i, f.Sequence, i+1)
		}
	}
}

func TestObserve_LiveOutboundFanOut(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)
	installObservabilityRecipientPolicy(b)

	c := dialWS(t, srv, tok)
	subscribeObserve(t, c, "plumb", 0)

	if _, err := b.HandleChatSend(context.Background(), "plumb", "@operator hello", 0, ""); err != nil {
		t.Fatal(err)
	}
	env := expectKindWithin(t, c, frames.KindObserveFrame, brokerAsyncWait)
	op, f := decodeObserveFrame(t, env)
	if op.Aspect != "plumb" {
		t.Errorf("aspect: got %q want plumb", op.Aspect)
	}
	cf := decodeChatFrame(t, f)
	if cf.From != "plumb" || cf.Direction != observability.DirectionOutbound {
		t.Errorf("chat frame: %+v", cf)
	}
	if !strings.Contains(cf.Content, "hello") {
		t.Errorf("content: %q", cf.Content)
	}
}

func TestObserve_LiveInboundFanOut(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)
	installObservabilityRecipientPolicy(b)

	c := dialWS(t, srv, tok)
	subscribeObserve(t, c, "plumb", 0)

	// "operator" sends @plumb — RecipientPolicy resolves @plumb as a
	// mention so the recipients slice contains "plumb"; the Hub then
	// emits an inbound ChatFrame on plumb's stream.
	if _, err := b.HandleChatSend(context.Background(), "operator", "@plumb hello", 0, ""); err != nil {
		t.Fatal(err)
	}

	// Two frames will fire: outbound on operator's stream (we're not
	// subscribed), inbound on plumb's stream (we ARE subscribed). The
	// only one we receive should be the inbound on plumb.
	deadline := time.Now().Add(brokerAsyncWait)
	var got observability.ChatFrame
	for time.Now().Before(deadline) {
		env, ok := recvFrameWithTimeout(t, c, time.Until(deadline))
		if !ok {
			t.Fatal("no observe.frame received within timeout")
		}
		if env.Kind != frames.KindObserveFrame {
			continue
		}
		_, f := decodeObserveFrame(t, env)
		got = decodeChatFrame(t, f)
		break
	}
	if got.From != "operator" || got.Direction != observability.DirectionInbound {
		t.Errorf("expected inbound from operator, got: %+v", got)
	}
}

func TestObserve_PerAspectIsolation(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)
	installObservabilityRecipientPolicy(b)

	c := dialWS(t, srv, tok)
	subscribeObserve(t, c, "plumb", 0)

	// Send a message that touches "keel" only (sender keel, no
	// mentions). The operator is subscribed to "plumb" and must NOT
	// see any observe.frame for keel's traffic.
	if _, err := b.HandleChatSend(context.Background(), "keel", "internal note", 0, ""); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, c, 200*time.Millisecond)
}

func TestObserve_UnsubscribeStopsDelivery(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)
	installObservabilityRecipientPolicy(b)

	c := dialWS(t, srv, tok)
	subscribeObserve(t, c, "plumb", 0)

	if _, err := b.HandleChatSend(context.Background(), "plumb", "first", 0, ""); err != nil {
		t.Fatal(err)
	}
	expectKindWithin(t, c, frames.KindObserveFrame, brokerAsyncWait)

	mustResponse(t, c, frames.KindUnsubscribeObserve, frames.UnsubscribeObservePayload{Aspect: "plumb"})

	if _, err := b.HandleChatSend(context.Background(), "plumb", "second", 0, ""); err != nil {
		t.Fatal(err)
	}
	expectNoFrame(t, c, 200*time.Millisecond)
}

func TestObserve_MultiAspectSubscription(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)
	installObservabilityRecipientPolicy(b)

	c := dialWS(t, srv, tok)
	subscribeObserve(t, c, "plumb", 0)
	subscribeObserve(t, c, "keel", 0)

	if _, err := b.HandleChatSend(context.Background(), "plumb", "from plumb", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.HandleChatSend(context.Background(), "keel", "from keel", 0, ""); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	deadline := time.Now().Add(brokerAsyncWait)
	for len(seen) < 2 && time.Now().Before(deadline) {
		env, ok := recvFrameWithTimeout(t, c, time.Until(deadline))
		if !ok {
			break
		}
		if env.Kind != frames.KindObserveFrame {
			continue
		}
		op, _ := decodeObserveFrame(t, env)
		seen[op.Aspect] = true
	}
	if !seen["plumb"] || !seen["keel"] {
		t.Errorf("expected both aspects in seen set, got %+v", seen)
	}
}

// TestObserve_TurnLifecycleEndToEnd is the Phase G smoke test for the
// turn-side observability pipeline. It exercises everything between the
// funnel's ObservabilityHook contract and the operator's observe.frame
// stream:
//
//   - BeginTurn opens an in_flight TurnFrame
//   - OnBridleEvent folds ModelChunk + ToolCallStart + ToolCallResult
//     into the events list and pre-parses an Edit artifact
//   - EndTurn flips status to complete and emits the terminal snapshot
//
// The operator's WS connection should receive multiple observe.frame
// envelopes (one per Grouper emission); the smoke test verifies the
// final frame carries the expected status, label, event ordering, and
// artifact shape. This is the integration-side cousin to the Grouper
// unit tests in nexus/observability/.
func TestObserve_TurnLifecycleEndToEnd(t *testing.T) {
	srv, b, _, _, tok := newOperatorTestServerFull(t)
	installObservabilityRecipientPolicy(b)

	c := dialWS(t, srv, tok)
	subscribeObserve(t, c, "plumb", 0)

	g := b.observability.GrouperFor("plumb")
	g.BeginTurn("turn-smoke-1", "main", "claude-opus-4-7", "claudecode", 42)
	g.OnBridleEvent(bridle.ModelChunk{Text: "let me check that file"})
	g.OnBridleEvent(bridle.ToolCallStart{
		ID:   "edit-1",
		Name: "Edit",
		Args: json.RawMessage(`{"file_path":"main.go","old_string":"old","new_string":"new"}`),
	})
	g.OnBridleEvent(bridle.ToolCallResult{
		ID:     "edit-1",
		Result: json.RawMessage(`"ok"`),
	})
	g.EndTurn()

	// Drain frames until we see a terminal "complete" TurnFrame. The
	// Grouper emits one snapshot per call (BeginTurn + each event +
	// EndTurn = 5 emissions), so several arrive on the wire — the
	// renderer cares about the final one.
	deadline := time.Now().Add(brokerAsyncWait)
	var final observability.TurnFrame
	gotFinal := false
	for time.Now().Before(deadline) && !gotFinal {
		env, ok := recvFrameWithTimeout(t, c, time.Until(deadline))
		if !ok {
			break
		}
		if env.Kind != frames.KindObserveFrame {
			continue
		}
		_, f := decodeObserveFrame(t, env)
		if f.Kind != observability.FrameTurn {
			continue
		}
		var tf observability.TurnFrame
		if err := json.Unmarshal(f.Payload, &tf); err != nil {
			t.Fatalf("decode TurnFrame: %v", err)
		}
		if tf.Status == observability.TurnComplete {
			final = tf
			gotFinal = true
		}
	}
	if !gotFinal {
		t.Fatal("never received a complete TurnFrame")
	}

	if final.TurnID != "turn-smoke-1" {
		t.Errorf("TurnID=%q want turn-smoke-1", final.TurnID)
	}
	if final.Label != "main" {
		t.Errorf("Label=%q want main", final.Label)
	}
	if final.TriggerMsg != 42 {
		t.Errorf("TriggerMsg=%d want 42", final.TriggerMsg)
	}

	// Expected events: one text run + one tool call (with result attached).
	if len(final.Events) < 2 {
		t.Fatalf("events=%+v want >=2", final.Events)
	}
	if final.Events[0].Kind != observability.TurnEventText ||
		!strings.Contains(final.Events[0].Text, "let me check") {
		t.Errorf("events[0]=%+v want text with 'let me check'", final.Events[0])
	}
	tc := final.Events[1]
	if tc.Kind != observability.TurnEventToolCall || tc.Tool == nil ||
		tc.Tool.Name != "Edit" {
		t.Errorf("events[1]=%+v want tool_call Edit", tc)
	}
	if tc.Tool.Result == nil || tc.Tool.Result.IsError {
		t.Errorf("tool result missing or errored: %+v", tc.Tool.Result)
	}
	if tc.Tool.Artifact == nil || tc.Tool.Artifact.FilePath != "main.go" {
		t.Errorf("artifact missing or wrong path: %+v", tc.Tool.Artifact)
	}
	if tc.Tool.Artifact.OldText != "old" || tc.Tool.Artifact.NewText != "new" {
		t.Errorf("artifact diff wrong: %+v", tc.Tool.Artifact)
	}
}
