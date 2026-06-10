package broker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// retainingChatStore is a fakeChatStore that actually retains inserted
// messages and serves ListSince — so the Replayer (and thus the
// wake-delivery replay path) has real history to walk. fakeChatStore's
// ListSince returns nil, which is fine for scale-only wake tests but
// cannot exercise message delivery.
type retainingChatStore struct {
	mu   sync.Mutex
	msgs []chat.Message
}

func (s *retainingChatStore) Insert(_ context.Context, from, content string, replyTo int64, topic string) (chat.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := chat.Message{
		ID:        int64(len(s.msgs) + 1),
		From:      from,
		Content:   content,
		ReplyTo:   replyTo,
		Topic:     topic,
		CreatedAt: time.Now(),
	}
	m.ThreadRootMsgID = m.ID
	s.msgs = append(s.msgs, m)
	return m, nil
}

func (s *retainingChatStore) ListSince(_ context.Context, sinceID int64, limit int) ([]chat.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]chat.Message, 0, len(s.msgs))
	for _, m := range s.msgs {
		if m.ID > sinceID {
			out = append(out, m)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// Remaining chat.Store surface — unused by the wake-delivery path.
func (s *retainingChatStore) ListThread(context.Context, int64, int64, int) ([]chat.Message, error) {
	return nil, nil
}
func (s *retainingChatStore) ToggleReaction(context.Context, int64, string, string) (bool, error) {
	return false, nil
}
func (s *retainingChatStore) AnnounceSharedFile(context.Context, string, string, string) (int64, int64, error) {
	return 0, 0, nil
}
func (s *retainingChatStore) ShareFile(context.Context, string, string, []string) (int64, error) {
	return 0, nil
}
func (s *retainingChatStore) GetByID(context.Context, int64) (chat.Message, error) {
	return chat.Message{}, nil
}
func (s *retainingChatStore) ThreadParticipants(context.Context, int64) ([]string, error) {
	return nil, nil
}
func (s *retainingChatStore) ListShared(context.Context, int) ([]chat.SharedFile, error) {
	return nil, nil
}
func (s *retainingChatStore) GetShared(context.Context, int64) (chat.SharedFile, error) {
	return chat.SharedFile{}, nil
}
func (s *retainingChatStore) ListReplies(context.Context, int64) ([]chat.Message, error) {
	return nil, nil
}
func (s *retainingChatStore) ListPage(context.Context, int64, int64, int) ([]chat.Message, bool, error) {
	return nil, false, nil
}
func (s *retainingChatStore) GetReactions(context.Context, []int64) (map[int64][]chat.Reaction, error) {
	return nil, nil
}

// newWakeDeliveryBroker builds a chat-capable broker with a retaining
// store, a Replayer over that store, and a fake-scaler wake controller —
// everything the wake → register → replay path needs end to end.
func newWakeDeliveryBroker(t *testing.T, scaler *fakeScaler, policies map[string]string) (*Broker, *retainingChatStore) {
	t.Helper()
	store := &retainingChatStore{}
	policy := &RecipientPolicy{}
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          store,
		RecipientPolicy:    policy,
		Replayer:           NewReplayer(store, *policy),
		AspectWakePolicy:   policies,
	}, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	b.wake = newWakeController(scaler, policies, nil, b.log)
	return b, store
}

// TestWakeRecordsPendingWatermark: a napping (no-conn) wake-on-mention
// aspect that is mentioned both scales AND records the triggering msg id
// as its pending-wake watermark (watermark = msgID-1 so AddressedSince,
// which is strictly-greater-than, includes the triggering message).
func TestWakeRecordsPendingWatermark(t *testing.T) {
	scaler := newFakeScaler()
	b, _ := newWakeDeliveryBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})

	msgID, err := b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	waitScale(t, scaler)

	b.wake.mu.Lock()
	since, ok := b.wake.pendingWake["plumb"]
	b.wake.mu.Unlock()
	if !ok {
		t.Fatal("pendingWake not recorded for woken aspect")
	}
	if since != msgID-1 {
		t.Fatalf("pendingWake watermark = %d, want msgID-1 = %d", since, msgID-1)
	}
}

// TestWakeDeliversTriggeringMessageOnRegister: the full gap-closing
// path. plumb is napping (no conn) → mentioned → wakes. plumb then cold-
// starts and registers with RequestReplay=false / SinceMsgID=0 (the
// real wsasp sendRegister shape). It must still receive a chat.deliver
// for the triggering message, and the watermark must be cleared.
func TestWakeDeliversTriggeringMessageOnRegister(t *testing.T) {
	scaler := newFakeScaler()
	b, _ := newWakeDeliveryBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})
	srv := newWSServer(t, b)

	msgID, err := b.HandleChatSend(context.Background(), "shadow", "wake up @plumb", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	waitScale(t, scaler)

	// plumb cold-starts and registers (no replay opt-in).
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	env := expectKindWithin(t, c, frames.KindChatDeliver, brokerAsyncWait)
	var p frames.ChatDeliverPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("decode deliver: %v", err)
	}
	if int64(p.ID) != msgID {
		t.Fatalf("delivered msg id = %d, want triggering id %d", p.ID, msgID)
	}
	if p.Content != "wake up @plumb" {
		t.Fatalf("delivered content = %q, want the triggering message", p.Content)
	}
	if !p.Replay {
		t.Fatalf("wake delivery should be a replay frame")
	}

	// Watermark cleared after the register consumes it.
	waitPendingCleared(t, b.wake, "plumb")
}

// TestNormalRegisterNoReplayWithoutOptIn: a non-woken aspect that
// registers with RequestReplay=false gets NO replay — the NEX-131 opt-in
// default must not regress just because the wake path also replays.
func TestNormalRegisterNoReplayWithoutOptIn(t *testing.T) {
	scaler := newFakeScaler()
	// No wake policy for plumb → no pending watermark is ever recorded.
	b, _ := newWakeDeliveryBroker(t, scaler, map[string]string{})
	srv := newWSServer(t, b)

	// A message addressed to plumb exists in history.
	if _, err := b.HandleChatSend(context.Background(), "shadow", "earlier @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	// plumb registers normally, no replay opt-in.
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	// No chat.deliver should arrive (opt-in default preserved).
	expectNoFrame(t, c, 300*time.Millisecond)
}

// TestWakeDeliversAllMessagesSinceNap: multiple messages addressed to a
// napping aspect during the nap→wake gap → all of them are replayed on
// register, from the oldest (watermark stays at the first message).
func TestWakeDeliversAllMessagesSinceNap(t *testing.T) {
	scaler := newFakeScaler()
	b, _ := newWakeDeliveryBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})
	srv := newWSServer(t, b)

	ctx := context.Background()
	// First mention wakes + records watermark. The two follow-ons land
	// while the pod is still booting (debounced, no extra scale) but must
	// not be skipped.
	id1, err := b.HandleChatSend(ctx, "shadow", "one @plumb", 0, "")
	if err != nil {
		t.Fatalf("send 1: %v", err)
	}
	waitScale(t, scaler)
	id2, err := b.HandleChatSend(ctx, "shadow", "two @plumb", 0, "")
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	id3, err := b.HandleChatSend(ctx, "shadow", "three @plumb", 0, "")
	if err != nil {
		t.Fatalf("send 3: %v", err)
	}

	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	got := map[int64]string{}
	for i := 0; i < 3; i++ {
		env := expectKindWithin(t, c, frames.KindChatDeliver, brokerAsyncWait)
		var p frames.ChatDeliverPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("decode deliver %d: %v", i, err)
		}
		got[int64(p.ID)] = p.Content
	}
	for _, id := range []int64{id1, id2, id3} {
		if _, ok := got[id]; !ok {
			t.Fatalf("message id %d not delivered; got %v", id, got)
		}
	}
}

// waitPendingCleared polls (bounded) until the pending-wake watermark for
// aspect is gone — the register path clears it synchronously, but the
// replay goroutine it spawns runs after, so poll defensively.
func waitPendingCleared(t *testing.T, w *wakeController, aspect string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		_, pending := w.pendingWake[aspect]
		w.mu.Unlock()
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pendingWake for %s never cleared after register", aspect)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
