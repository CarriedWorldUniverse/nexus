package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/nexus/handqueue"
	"github.com/nexus-cw/nexus/nexus/roster"
)

// newBrokerWithQueue returns a test Broker whose HandQueue runs the
// given ExecutorFunc. Uses a mock executor so tests avoid spawning
// real subprocesses.
func newBrokerWithQueue(t *testing.T, fn func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error)) (*testHandler, *roster.Roster, *Broker) {
	t.Helper()
	r := roster.New()
	q, err := handqueue.New(handqueue.Config{
		MaxConcurrent: 2,
		Executor:      handqueue.ExecutorFunc(fn),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		HandQueue:          q,
	}, r)
	return &testHandler{b: b}, r, b
}

func TestDispatchEndToEnd(t *testing.T) {
	handler, _, _ := newBrokerWithQueue(t, func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
		return frames.DispatchResultPayload{
			Aspect:     req.Aspect,
			Thread:     req.Thread,
			DispatchID: req.DispatchID,
			Output:     map[string]any{"echoed": req.Payload["text"]},
			Tokens:     frames.TokenUsage{Input: 10, Output: 20, Total: 30},
		}, nil
	})
	srv := httptestNewServer(t, handler)

	c := dialWSURL(t, srv, "testtoken")

	req, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		Aspect:     "wren",
		Thread:     "t-1",
		DispatchID: "d-1",
		Payload:    map[string]any{"text": "sample"},
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}

	// Read the dispatch.result.
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, err := frames.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind != frames.KindDispatchResult {
		t.Fatalf("kind = %q, want dispatch.result", env.Kind)
	}
	if env.InReplyTo != req.ID {
		t.Errorf("InReplyTo = %q, want %q", env.InReplyTo, req.ID)
	}
	var result frames.DispatchResultPayload
	if err := frames.PayloadAs(env, &result); err != nil {
		t.Fatal(err)
	}
	if result.Aspect != "wren" {
		t.Errorf("Aspect = %q", result.Aspect)
	}
	if result.Output["echoed"] != "sample" {
		t.Errorf("Output.echoed = %v", result.Output["echoed"])
	}
}

func TestDispatchWithNoQueueReturnsError(t *testing.T) {
	// Broker without HandQueue.
	handler, _, _ := newTestServerNoQueue(t)
	srv := httptestNewServer(t, handler)
	c := dialWSURL(t, srv, "testtoken")

	req, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		Aspect: "wren",
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, raw)

	_, data, _ := c.Read(ctx)
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindDispatchError {
		t.Errorf("kind = %q, want dispatch.error", env.Kind)
	}
	var errPayload frames.DispatchErrorPayload
	_ = frames.PayloadAs(env, &errPayload)
	if errPayload.Code != "no_dispatcher" {
		t.Errorf("code = %q, want no_dispatcher", errPayload.Code)
	}
}

func TestDispatchBadPayload(t *testing.T) {
	handler, _, _ := newBrokerWithQueue(t, func(context.Context, frames.DispatchPayload) (frames.DispatchResultPayload, error) {
		t.Fatal("executor should not be called for invalid payload")
		return frames.DispatchResultPayload{}, nil
	})
	srv := httptestNewServer(t, handler)
	c := dialWSURL(t, srv, "testtoken")

	req, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		// Missing Aspect
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, raw)

	_, data, _ := c.Read(ctx)
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindDispatchError {
		t.Errorf("kind = %q, want dispatch.error", env.Kind)
	}
	var errPayload frames.DispatchErrorPayload
	_ = frames.PayloadAs(env, &errPayload)
	if errPayload.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", errPayload.Code)
	}
}

// TestDispatchIdentityMismatch — caller's resolved identity must
// match the dispatch's aspect field per hand-dispatch v0.1 §5.4.
// Token-store wires "wren" with a per-aspect token; the WS dials in
// as wren but submits a dispatch claiming aspect="anvil". Broker MUST
// reject with code "identity_mismatch", NOT execute the dispatch.
func TestDispatchIdentityMismatch(t *testing.T) {
	r := roster.New()
	executorCalled := false
	q, err := handqueue.New(handqueue.Config{
		MaxConcurrent: 2,
		Executor: handqueue.ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			executorCalled = true
			return frames.DispatchResultPayload{Aspect: req.Aspect}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	store := NewTokenStore()
	store.SetTokenForTest("wren", "wren-tok", false)
	store.SetTokenForTest("anvil", "anvil-tok", false)

	b := New(Config{
		Tokens:             store,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		HandQueue:          q,
	}, r)
	srv := httptestNewServer(t, &testHandler{b: b})
	c := dialWSURL(t, srv, "wren-tok")

	req, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		Aspect:     "anvil", // <-- wren tries to dispatch as anvil
		Thread:     "t-1",
		DispatchID: "d-1",
		Payload:    map[string]any{"text": "spoofed"},
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindDispatchError {
		t.Fatalf("kind = %q, want dispatch.error", env.Kind)
	}
	var errPayload frames.DispatchErrorPayload
	_ = frames.PayloadAs(env, &errPayload)
	if errPayload.Code != "identity_mismatch" {
		t.Errorf("code = %q, want identity_mismatch", errPayload.Code)
	}
	if executorCalled {
		t.Error("executor ran for spoofed dispatch — identity check did not block")
	}
}

// TestDispatchIdentityMatch — same setup but caller's identity matches
// the dispatch's aspect field. Should succeed.
func TestDispatchIdentityMatch(t *testing.T) {
	r := roster.New()
	q, err := handqueue.New(handqueue.Config{
		MaxConcurrent: 2,
		Executor: handqueue.ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			return frames.DispatchResultPayload{Aspect: req.Aspect}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	store := NewTokenStore()
	store.SetTokenForTest("wren", "wren-tok", false)

	b := New(Config{
		Tokens:             store,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		HandQueue:          q,
	}, r)
	srv := httptestNewServer(t, &testHandler{b: b})
	c := dialWSURL(t, srv, "wren-tok")

	req, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		Aspect: "wren", DispatchID: "d-1", Payload: map[string]any{"text": "ok"},
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, raw)
	_, data, _ := c.Read(ctx)
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindDispatchResult {
		t.Errorf("kind = %q, want dispatch.result (matching identity should succeed)", env.Kind)
	}
}

// TestDispatchAdminCanActAsAny — Frame's admin token can dispatch on
// behalf of any aspect (coordination role). Aspect tokens cannot.
func TestDispatchAdminCanActAsAny(t *testing.T) {
	r := roster.New()
	q, err := handqueue.New(handqueue.Config{
		MaxConcurrent: 2,
		Executor: handqueue.ExecutorFunc(func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
			return frames.DispatchResultPayload{Aspect: req.Aspect}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })

	store := NewTokenStore()
	store.SetTokenForTest(FrameAgentID, "frame-tok", true)

	b := New(Config{
		Tokens:             store,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		HandQueue:          q,
	}, r)
	srv := httptestNewServer(t, &testHandler{b: b})
	c := dialWSURL(t, srv, "frame-tok")

	// Frame dispatches a job claiming aspect=wren — should succeed
	// because Frame's admin flag is true.
	req, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		Aspect: "wren", DispatchID: "d-2", Payload: map[string]any{"text": "ok"},
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, raw)
	_, data, _ := c.Read(ctx)
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindDispatchResult {
		t.Errorf("kind = %q, want dispatch.result (admin should succeed)", env.Kind)
	}
}

// TestConnectRejectsUnknownPerAspectToken — when the broker has a
// TokenStore but no legacy master, an unknown bearer is rejected at
// upgrade time.
func TestConnectRejectsUnknownPerAspectToken(t *testing.T) {
	r := roster.New()
	store := NewTokenStore()
	store.SetTokenForTest("wren", "wren-tok", false)

	b := New(Config{
		Tokens:             store,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)
	srv := httptestNewServer(t, &testHandler{b: b})

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer not-a-real-token"}},
	})
	if err == nil {
		t.Fatal("dial should have failed with unknown token")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Support helpers — duplicate of ws_test.go but targeting a
// pre-built handler.
func newTestServerNoQueue(t *testing.T) (*testHandler, *roster.Roster, *Broker) {
	t.Helper()
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)
	return &testHandler{b: b}, r, b
}

func httptestNewServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func dialWSURL(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "done") })
	return c
}
