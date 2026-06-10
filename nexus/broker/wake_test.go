package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// fakeScaler records ScaleDeployment calls so wake tests assert on the
// scale traffic without a real k8s client. Each call signals `called`
// (when set) — wake scales in a goroutine, so tests synchronize on the
// channel instead of asserting immediately.
type fakeScaler struct {
	mu     sync.Mutex
	calls  []scaleCall
	errFn  func(name string, replicas int32) error
	called chan struct{}
}

// newFakeScaler builds a fakeScaler with the call-signal channel armed,
// for tests that need to wait on async scale calls.
func newFakeScaler() *fakeScaler {
	return &fakeScaler{called: make(chan struct{}, 16)}
}

type scaleCall struct {
	name     string
	replicas int32
}

func (f *fakeScaler) ScaleDeployment(_ context.Context, name string, replicas int32) error {
	f.mu.Lock()
	f.calls = append(f.calls, scaleCall{name: name, replicas: replicas})
	errFn := f.errFn
	f.mu.Unlock()
	select {
	case f.called <- struct{}{}: // nil channel: never ready, default fires
	default:
	}
	if errFn != nil {
		return errFn(name, replicas)
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

// waitScale blocks (bounded) until the fake scaler reports one more
// ScaleDeployment call.
func waitScale(t *testing.T, f *fakeScaler) {
	t.Helper()
	select {
	case <-f.called:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for ScaleDeployment; calls so far: %v", f.scaleCalls())
	}
}

// assertNoScale asserts no ScaleDeployment call lands within the grace
// window — the bounded-absence counterpart of waitScale.
func assertNoScale(t *testing.T, f *fakeScaler) {
	t.Helper()
	select {
	case <-f.called:
		t.Fatalf("unexpected ScaleDeployment call: %v", f.scaleCalls())
	case <-time.After(150 * time.Millisecond):
	}
}

// waitDisarmed polls (bounded) until the controller's debounce stamp
// for aspect is gone — the failure-path disarm runs inside the scale
// goroutine after ScaleDeployment returns, so it trails the call signal.
func waitDisarmed(t *testing.T, w *wakeController, aspect string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		_, armed := w.lastWake[aspect]
		w.mu.Unlock()
		if !armed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("debounce for %s never disarmed after failed wake", aspect)
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	scaler := newFakeScaler()
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})

	if _, err := b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	waitScale(t, scaler)
	calls := scaler.scaleCalls()
	if len(calls) != 1 {
		t.Fatalf("scale calls = %v, want exactly one", calls)
	}
	if calls[0] != (scaleCall{name: "plumb", replicas: 1}) {
		t.Fatalf("scale call = %+v, want plumb→1", calls[0])
	}
}

func TestWakeSkipsLiveRecipient(t *testing.T) {
	scaler := newFakeScaler()
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})
	srv := newWSServer(t, b)

	// plumb holds a live WS registration — no wake needed.
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	if _, err := b.HandleChatSend(context.Background(), "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	assertNoScale(t, scaler)
}

func TestWakeDebouncesInFlightWake(t *testing.T) {
	scaler := newFakeScaler()
	b := newWakeBroker(t, scaler, map[string]string{"plumb": WakePolicyWakeOnMention})

	now := time.Now()
	b.wake.now = func() time.Time { return now }

	ctx := context.Background()
	if _, err := b.HandleChatSend(ctx, "shadow", "ping @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend 1: %v", err)
	}
	waitScale(t, scaler)

	// Second mention 10s later — well inside the 60s debounce while the
	// pod is still scheduling. Must not double-wake.
	now = now.Add(10 * time.Second)
	if _, err := b.HandleChatSend(ctx, "shadow", "still there @plumb?", 0, ""); err != nil {
		t.Fatalf("HandleChatSend 2: %v", err)
	}
	assertNoScale(t, scaler)

	// Past the debounce window a fresh mention may wake again (the
	// register path normally clears the need, but a failed boot must
	// not wedge wake forever).
	now = now.Add(2 * time.Minute)
	if _, err := b.HandleChatSend(ctx, "shadow", "hello again @plumb", 0, ""); err != nil {
		t.Fatalf("HandleChatSend 3: %v", err)
	}
	waitScale(t, scaler)
	if calls := scaler.scaleCalls(); len(calls) != 2 {
		t.Fatalf("scale calls = %v, want two after debounce expiry", calls)
	}
}

func TestWakeRespectsPolicyGating(t *testing.T) {
	scaler := newFakeScaler()
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
	assertNoScale(t, scaler)
}

func TestWakeFailureDoesNotFailSend(t *testing.T) {
	scaler := newFakeScaler()
	scaler.errFn = func(string, int32) error { return errors.New("k8s says no") }
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
	waitScale(t, scaler)
	waitDisarmed(t, b.wake, "plumb")
	if _, err := b.HandleChatSend(context.Background(), "shadow", "@plumb retry", 0, ""); err != nil {
		t.Fatalf("HandleChatSend retry: %v", err)
	}
	waitScale(t, scaler)
	if calls := scaler.scaleCalls(); len(calls) != 2 {
		t.Fatalf("scale calls = %v, want retry after failure", calls)
	}
}

// gateScaler blocks its first ScaleDeployment call until release is
// closed, then fails it; subsequent calls succeed immediately. Drives
// the stamp-CAS interleaving test.
type gateScaler struct {
	mu      sync.Mutex
	n       int
	entered chan struct{} // closed when the first call enters
	release chan struct{} // first call blocks until closed
}

func (s *gateScaler) ScaleDeployment(context.Context, string, int32) error {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	if n == 1 {
		close(s.entered)
		<-s.release
		return errors.New("late failure")
	}
	return nil
}

// signalWriter signals ch on every log write — the final log line is
// the only completion hook the scale goroutine exposes.
type signalWriter struct{ ch chan struct{} }

func (w *signalWriter) Write(p []byte) (int, error) {
	select {
	case w.ch <- struct{}{}:
	default:
	}
	return len(p), nil
}

func waitLog(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scale goroutine to log")
	}
}

// A failed wake's disarm must not clobber a newer successful wake's
// debounce stamp: A stamps and its scale call hangs; B restamps and
// succeeds; A's late failure compare-and-deletes only its own stamp,
// so B's survives.
func TestWakeFailedScaleKeepsNewerStamp(t *testing.T) {
	scaler := &gateScaler{entered: make(chan struct{}), release: make(chan struct{})}
	logCh := make(chan struct{}, 8)
	log := slog.New(slog.NewTextHandler(&signalWriter{ch: logCh}, nil))
	w := newWakeController(scaler, map[string]string{"plumb": WakePolicyWakeOnMention}, nil, log)

	t0 := time.Now()
	now := t0
	w.now = func() time.Time { return now }

	ctx := context.Background()
	w.MaybeWake(ctx, "plumb", 1) // A stamps t0; its scale call blocks in the scaler
	select {
	case <-scaler.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first scale call never entered the scaler")
	}

	now = t0.Add(2 * time.Minute) // past the debounce window
	t1 := now
	w.MaybeWake(ctx, "plumb", 2) // B restamps t1 and scales successfully
	waitLog(t, logCh)            // B's success log — B's goroutine is done

	close(scaler.release) // A's scale call now returns its failure
	waitLog(t, logCh)     // A's failure log — A's disarm path is done

	w.mu.Lock()
	stamp, ok := w.lastWake["plumb"]
	w.mu.Unlock()
	if !ok || !stamp.Equal(t1) {
		t.Fatalf("lastWake = %v (present=%v), want B's stamp %v to survive A's failure", stamp, ok, t1)
	}
}

// MaybeWake on an unconfigured (nil) controller is a safe no-op — the
// chat hook calls it unconditionally.
func TestWakeNilControllerIsNoOp(t *testing.T) {
	var w *wakeController
	w.MaybeWake(context.Background(), "plumb", 1) // must not panic
}

// newWSServer serves the given broker's /connect for tests that need a
// live aspect WS (mirrors newTestServer but with a caller-built broker).
func newWSServer(t *testing.T, b *Broker) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	return srv
}
