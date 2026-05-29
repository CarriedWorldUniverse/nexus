package wsasp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/coder/websocket"
)

// TestRestoreUnsentPending verifies the slice-restore that fixes the
// drainPendingLoop data-loss bug. Before this helper landed, the
// drain loop re-prepended only the failing item — pending[i+1:] was
// silently dropped on transient send failures mid-burst (chat.send /
// react_to frames at the tail of the buffer would vanish).
func TestRestoreUnsentPending(t *testing.T) {
	mk := func(id string) frames.Envelope { return frames.Envelope{ID: id} }
	ids := func(envs []frames.Envelope) []string {
		out := make([]string, len(envs))
		for i, e := range envs {
			out[i] = e.ID
		}
		return out
	}

	cases := []struct {
		name       string
		drained    []frames.Envelope
		failedAt   int
		concurrent []frames.Envelope
		want       []string
	}{
		{
			name:     "fail on second of four — unsent SUFFIX preserved",
			drained:  []frames.Envelope{mk("A"), mk("B"), mk("C"), mk("D")},
			failedAt: 1,
			want:     []string{"B", "C", "D"}, // bug: previously only "B"
		},
		{
			name:     "fail on first — entire batch preserved",
			drained:  []frames.Envelope{mk("A"), mk("B"), mk("C")},
			failedAt: 0,
			want:     []string{"A", "B", "C"},
		},
		{
			name:       "concurrent items appended after unsent suffix",
			drained:    []frames.Envelope{mk("A"), mk("B"), mk("C")},
			failedAt:   1,
			concurrent: []frames.Envelope{mk("X"), mk("Y")},
			want:       []string{"B", "C", "X", "Y"},
		},
		{
			name:     "fail on last — only last preserved",
			drained:  []frames.Envelope{mk("A"), mk("B"), mk("C")},
			failedAt: 2,
			want:     []string{"C"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreUnsentPending(tc.drained, tc.failedAt, tc.concurrent)
			if !reflect.DeepEqual(ids(got), tc.want) {
				t.Errorf("got %v; want %v", ids(got), tc.want)
			}
		})
	}
}

// Cursor persistence is the load-bearing Lock 6 invariant on the
// aspect side. These tests exercise it without standing up a real
// WS connection — the wsclient itself has its own coverage.

func TestNewClient_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no URL", Config{AspectName: "anvil", OnDeliver: noopDeliver}},
		{"no AspectName", Config{URL: "wss://x", OnDeliver: noopDeliver}},
		{"no OnDeliver", Config{URL: "wss://x", AspectName: "anvil"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestClient_AdvanceCursorPersists(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")

	c, err := NewClient(Config{
		URL:        "wss://example/connect",
		AspectName: "anvil",
		CursorFile: cursorFile,
		OnDeliver:  noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.advanceCursor(42)

	data, err := os.ReadFile(cursorFile)
	if err != nil {
		t.Fatalf("cursor file not written: %v", err)
	}
	if got, _ := strconv.ParseInt(string(data), 10, 64); got != 42 {
		t.Errorf("persisted cursor = %s, want 42", string(data))
	}
}

func TestClient_AdvanceCursorMonotonic(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", CursorFile: cursorFile, OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.advanceCursor(50)
	c.advanceCursor(20) // older — must not regress
	c.advanceCursor(60)

	data, _ := os.ReadFile(cursorFile)
	if got, _ := strconv.ParseInt(string(data), 10, 64); got != 60 {
		t.Errorf("cursor regressed: got %s, want 60", string(data))
	}
}

func TestClient_LoadCursorReadsPersistedValue(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")
	if err := os.WriteFile(cursorFile, []byte("99999"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", CursorFile: cursorFile, OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cursor != 99999 {
		t.Errorf("cursor = %d, want 99999", c.cursor)
	}
}

func TestClient_LoadCursorMissingFileColdStarts(t *testing.T) {
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a",
		CursorFile: "/nonexistent/dir/cursor",
		OnDeliver:  noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cursor != 0 {
		t.Errorf("missing cursor file should cold-start at 0: got %d", c.cursor)
	}
}

func TestClient_LoadCursorBadValueColdStarts(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")
	os.WriteFile(cursorFile, []byte("not a number"), 0o600)

	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", CursorFile: cursorFile, OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cursor != 0 {
		t.Errorf("garbled cursor file should cold-start at 0: got %d", c.cursor)
	}
}

func TestClient_NoCursorFileNoPersist(t *testing.T) {
	// Empty CursorFile means no persistence — advanceCursor still
	// updates in-memory but doesn't write anywhere.
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.advanceCursor(100)
	if c.cursor != 100 {
		t.Errorf("in-memory cursor not updated: got %d", c.cursor)
	}
}

func TestCursorFileForAspect(t *testing.T) {
	got := CursorFileForAspect("/aspects/anvil")
	want := filepath.Join("/aspects/anvil", "cursor")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func noopDeliver(_ DeliveredMessage) {}

// TestQueueOrSend_CapsPendingAndDropsOldest pins the unbounded-buffer
// fix: a long disconnect that accumulates more than maxPendingFrames
// outbound frames drops the oldest in-memory rather than letting the
// slice grow without bound (OOM risk on funnel-heavy aspects). New
// frames are kept; the dropped oldest frame's kind/id surface in a
// WARN log for operator visibility.
func TestQueueOrSend_CapsPendingAndDropsOldest(t *testing.T) {
	c, err := NewClient(Config{
		URL:        "wss://example/connect",
		AspectName: "anvil",
		OnDeliver:  noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pre-seed the pending buffer past the cap. We bypass queueOrSend's
	// "try send first" branch by appending directly under the lock,
	// then call queueOrSend once to exercise the drop path.
	c.mu.Lock()
	for i := 0; i < maxPendingFrames; i++ {
		c.pending = append(c.pending, frames.Envelope{ID: fmt.Sprintf("old-%d", i)})
	}
	c.mu.Unlock()

	// At-cap. queueOrSend should drop the oldest ("old-0") and append
	// the new one. ws.Connected() returns false (no Run), so the
	// branch falls through directly to the buffer-append path.
	c.queueOrSend(context.Background(), frames.Envelope{ID: "new-frame"})

	c.mu.Lock()
	defer c.mu.Unlock()
	if got := len(c.pending); got != maxPendingFrames {
		t.Fatalf("buffer length = %d, want %d (cap held)", got, maxPendingFrames)
	}
	if c.pending[0].ID != "old-1" {
		t.Errorf("after drop, head ID = %q, want %q (old-0 should be gone)", c.pending[0].ID, "old-1")
	}
	if c.pending[maxPendingFrames-1].ID != "new-frame" {
		t.Errorf("after append, tail ID = %q, want %q", c.pending[maxPendingFrames-1].ID, "new-frame")
	}
}

func TestRegisterPrecedesDrainedSendsAfterReconnect(t *testing.T) {
	// Recorder for the order of inbound frames on the server side.
	var (
		mu    sync.Mutex
		order []frames.Kind
	)
	record := func(env frames.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, env.Kind)
	}

	// Server that records every frame it receives and stays up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		for {
			_, data, err := wsc.Read(r.Context())
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil {
				continue
			}
			record(env)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(Config{
		URL:        "ws" + strings.TrimPrefix(srv.URL, "http"),
		AspectName: "test",
		OnDeliver:  func(DeliveredMessage) {},
		Register:   schemas.RegisterRequest{Name: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Queue three chat sends BEFORE Run starts → they go into
	// pending immediately (ws not connected yet).
	bg := context.Background()
	_, _ = c.SendChat(bg, "first", 0, "")
	_, _ = c.SendChat(bg, "second", 0, "")
	_, _ = c.SendChat(bg, "third", 0, "")

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	// Wait for the server to see 4 frames (register + 3 chats).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 4 {
		t.Fatalf("server only received %d frames: %v", len(order), order)
	}
	if order[0] != frames.KindRegister {
		t.Fatalf("first frame should be register, got %v; full order: %v", order[0], order)
	}
	for i := 1; i < 4; i++ {
		if order[i] != frames.KindChatSend {
			t.Fatalf("frame %d should be chat.send, got %v", i, order[i])
		}
	}
}

// Regression for NEX-237: NewClient must forward Config.TokenProvider into
// the underlying wsclient.Config. Prior to the fix the field was silently
// dropped, which meant aspects wired with a JWT-refresh closure would dial
// with the stale initial token forever and never recover from token expiry.
func TestNewClient_ForwardsTokenProviderToWSClient(t *testing.T) {
	called := false
	provider := func(_ context.Context) (string, error) {
		called = true
		return "fresh-token", nil
	}

	c, err := NewClient(Config{
		URL:           "wss://example/connect",
		AspectName:    "anvil",
		OnDeliver:     noopDeliver,
		TokenProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}

	// wsclient.Client.cfg is unexported across packages; reach in via
	// reflection + unsafe so we can both check it's set and invoke it.
	// The test is a regression guard, not a public-API user.
	wsCfg := reflect.ValueOf(c.ws).Elem().FieldByName("cfg")
	tpField := wsCfg.FieldByName("TokenProvider")
	if !tpField.IsValid() {
		t.Fatal("wsclient.Config has no TokenProvider field — schema changed")
	}
	if tpField.IsNil() {
		t.Fatal("TokenProvider not forwarded to wsclient.Config")
	}

	// Re-form the unexported field value as a callable one via unsafe.
	callable := reflect.NewAt(tpField.Type(), unsafe.Pointer(tpField.UnsafeAddr())).Elem()
	results := callable.Call([]reflect.Value{reflect.ValueOf(context.Background())})
	if !called {
		t.Fatal("forwarded TokenProvider did not invoke the supplied closure")
	}
	if got := results[0].String(); got != "fresh-token" {
		t.Errorf("TokenProvider returned %q, want %q", got, "fresh-token")
	}
}

// TestHandleFrame_RoutesEscalationRequest pins the broker-pushed
// escalation.request path: a request frame fed through the same
// inbound dispatch entry point (handleFrame) that routes chat.deliver
// must decode and surface to the OnEscalationRequest callback, with
// the request envelope's correlation id (env.ID, set by NewRequest)
// passed through so the operator's decision can be correlated back.
func TestHandleFrame_RoutesEscalationRequest(t *testing.T) {
	var (
		gotPayload   frames.EscalationRequestPayload
		gotRequestID string
		calls        int
	)
	c, err := NewClient(Config{
		URL:        "wss://example/connect",
		AspectName: "operator",
		OnDeliver:  noopDeliver,
		OnEscalationRequest: func(p frames.EscalationRequestPayload, requestID string) {
			calls++
			gotPayload = p
			gotRequestID = requestID
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: "anvil",
		Tool:   "bash",
		Reason: "destructive command",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.handleFrame(env)

	if calls != 1 {
		t.Fatalf("OnEscalationRequest called %d times, want 1", calls)
	}
	if gotPayload.Aspect != "anvil" {
		t.Errorf("payload.Aspect = %q, want %q", gotPayload.Aspect, "anvil")
	}
	if gotPayload.Tool != "bash" {
		t.Errorf("payload.Tool = %q, want %q", gotPayload.Tool, "bash")
	}
	if gotRequestID != env.ID {
		t.Errorf("requestID = %q, want env.ID %q", gotRequestID, env.ID)
	}
}

// TestHandleFrame_EscalationRequestNilCallbackIsNoop confirms the
// callback is OPTIONAL: an aspect that doesn't wire one (the default
// non-operator case) must not crash when the broker pushes an
// escalation.request.
func TestHandleFrame_EscalationRequestNilCallbackIsNoop(t *testing.T) {
	c, err := NewClient(Config{
		URL:        "wss://example/connect",
		AspectName: "anvil",
		OnDeliver:  noopDeliver,
		// OnEscalationRequest deliberately nil.
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: "anvil",
		Tool:   "bash",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Must not panic.
	c.handleFrame(env)
}
