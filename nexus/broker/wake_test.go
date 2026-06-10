package broker

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// fakeScaler records ScaleDeployment calls so wake tests assert on the
// scale traffic without a real k8s client.
type fakeScaler struct {
	mu    sync.Mutex
	calls []scaleCall
	errFn func(name string, replicas int32) error
}

type scaleCall struct {
	name     string
	replicas int32
}

func (f *fakeScaler) ScaleDeployment(_ context.Context, name string, replicas int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, scaleCall{name: name, replicas: replicas})
	if f.errFn != nil {
		return f.errFn(name, replicas)
	}
	return nil
}

func (f *fakeScaler) scaleCalls() []scaleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]scaleCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newWakeBroker builds a chat-capable broker with a fake-scaler wake
// controller installed: ChatStore persists, RecipientPolicy routes
// @mentions, no aspect holds a live conn unless the test dials one.
func newWakeBroker(t *testing.T, scaler *fakeScaler, policies map[string]string) *Broker {
	t.Helper()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          &fakeChatStore{},
		RecipientPolicy:    &RecipientPolicy{},
		AspectWakePolicy:   policies,
	}, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	b.wake = newWakeController(scaler, policies, nil, b.log)
	return b
}

func TestWakeOnMentionScalesNappingRecipient(t *testing.T) {
	scaler := &fakeScaler{}
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})

	if _, err := b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	calls := scaler.scaleCalls()
	if len(calls) != 1 {
		t.Fatalf("scale calls = %v, want exactly one", calls)
	}
	if calls[0] != (scaleCall{name: "plumb", replicas: 1}) {
		t.Fatalf("scale call = %+v, want plumb→1", calls[0])
	}
}

func TestWakeSkipsLiveRecipient(t *testing.T) {
	scaler := &fakeScaler{}
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})
	srv := newWSServer(t, b)

	// plumb holds a live WS registration — no wake needed.
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	if _, err := b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	if calls := scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none for a live recipient", calls)
	}
}

func TestWakeDebouncesInFlightWake(t *testing.T) {
	scaler := &fakeScaler{}
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})

	now := time.Now()
	b.wake.now = func() time.Time { return now }

	ctx := context.Background()
	if _, err := b.HandleChatSend(ctx, "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend 1: %v", err)
	}
	// Second mention 10s later — well inside the 60s debounce while the
	// pod is still scheduling. Must not double-wake.
	now = now.Add(10 * time.Second)
	if _, err := b.HandleChatSend(ctx, "shadow", "still there @plumb?", 0, ""); err != nil {
		t.Fatalf("HandleChatSend 2: %v", err)
	}
	if calls := scaler.scaleCalls(); len(calls) != 1 {
		t.Fatalf("scale calls = %v, want one (debounced)", calls)
	}

	// Past the debounce window a fresh mention may wake again (the
	// register path normally clears the need, but a failed boot must
	// not wedge wake forever).
	now = now.Add(2 * time.Minute)
	if _, err := b.HandleChatSend(ctx, "shadow", "hello again @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend 3: %v", err)
	}
	if calls := scaler.scaleCalls(); len(calls) != 2 {
		t.Fatalf("scale calls = %v, want two after debounce expiry", calls)
	}
}

func TestWakeRespectsPolicyGating(t *testing.T) {
	scaler := &fakeScaler{}
	b := newWakeBroker(t, scaler, map[string]string{
		"anvil": WakePolicyDispatchOnly,
		"keel":  WakePolicyAlwaysOn,
		// harrow: absent — today's semantics, no wake behavior.
	})

	ctx := context.Background()
	for _, msg := range []string{"@anvil ping", "@keel ping", "@harrow ping"} {
		if _, err := b.HandleChatSend(ctx, "shadow", msg, 0, ""); err != nil {
			t.Fatalf("HandleChatSend %q: %v", msg, err)
		}
	}
	if calls := scaler.scaleCalls(); len(calls) != 0 {
		t.Fatalf("scale calls = %v, want none (dispatch-only / always-on / unconfigured)", calls)
	}
}

func TestWakeFailureDoesNotFailSend(t *testing.T) {
	scaler := &fakeScaler{errFn: func(string, int32) error { return errors.New("k8s says no") }}
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})

	msgID, err := b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: wake failure must not fail the send, got %v", err)
	}
	if msgID == 0 {
		t.Fatal("message not persisted on wake failure")
	}
	if got := b.cfg.ChatStore.(*fakeChatStore).insertCount(); got != 1 {
		t.Fatalf("ChatStore inserts = %d, want 1 (replay delivers on register)", got)
	}

	// A failed wake must not arm the debounce: the next mention retries.
	if _, err := b.HandleChatSend(context.Background(), "shadow", "@plumb retry", 0, ""); err != nil {
		t.Fatalf("HandleChatSend retry: %v", err)
	}
	if calls := scaler.scaleCalls(); len(calls) != 2 {
		t.Fatalf("scale calls = %v, want retry after failure", calls)
	}
}

// MaybeWake on an unconfigured (nil) controller is a safe no-op — the
// chat hook calls it unconditionally.
func TestWakeNilControllerIsNoOp(t *testing.T) {
	var w *wakeController
	w.MaybeWake(context.Background(), "plumb") // must not panic
}

// newWSServer serves the given broker's /connect for tests that need a
// live aspect WS (mirrors newTestServer but with a caller-built broker).
func newWSServer(t *testing.T, b *Broker) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	return srv
}
