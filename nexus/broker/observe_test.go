package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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
		t.Fatalf("embedded frame decode: %v", err)
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
	deadline := time.Now().Add(2 * time.Second)
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
	env := expectKindWithin(t, c, frames.KindObserveFrame, 2*time.Second)
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
	deadline := time.Now().Add(2 * time.Second)
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
	expectKindWithin(t, c, frames.KindObserveFrame, 2*time.Second)

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
	deadline := time.Now().Add(2 * time.Second)
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
