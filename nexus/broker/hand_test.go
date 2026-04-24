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
func newBrokerWithQueue(t *testing.T, fn func(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error)) (*testHandler, *roster.Roster, *Broker) {
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
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		HandQueue:          q,
	}, r)
	return &testHandler{b: b}, r, b
}

func TestHandDispatchEndToEnd(t *testing.T) {
	handler, _, _ := newBrokerWithQueue(t, func(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error) {
		return frames.HandResultPayload{
			TargetAspect: req.TargetAspect,
			HandName:     req.HandName,
			ThreadID:     req.ThreadID,
			Output:       map[string]any{"echoed": req.Input["text"]},
			Tokens:       frames.TokenUsage{Input: 10, Output: 20, Total: 30},
		}, nil
	})
	srv := httptestNewServer(t, handler)

	c := dialWSURL(t, srv, "testtoken")

	req, _ := frames.NewRequest(frames.KindHandDispatch, frames.HandDispatchPayload{
		TargetAspect: "wren",
		HandName:     "verify-canon",
		ThreadID:     "t-1",
		Invoker:      "keel",
		Input:        map[string]any{"text": "sample"},
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}

	// Read the hand.result.
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, err := frames.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind != frames.KindHandResult {
		t.Fatalf("kind = %q, want hand.result", env.Kind)
	}
	if env.InReplyTo != req.ID {
		t.Errorf("InReplyTo = %q, want %q", env.InReplyTo, req.ID)
	}
	var result frames.HandResultPayload
	if err := frames.PayloadAs(env, &result); err != nil {
		t.Fatal(err)
	}
	if result.TargetAspect != "wren" {
		t.Errorf("TargetAspect = %q", result.TargetAspect)
	}
	if result.Output["echoed"] != "sample" {
		t.Errorf("Output.echoed = %v", result.Output["echoed"])
	}
}

func TestHandDispatchWithNoQueueReturnsError(t *testing.T) {
	// Broker without HandQueue.
	handler, _, _ := newTestServerNoQueue(t)
	srv := httptestNewServer(t, handler)
	c := dialWSURL(t, srv, "testtoken")

	req, _ := frames.NewRequest(frames.KindHandDispatch, frames.HandDispatchPayload{
		TargetAspect: "wren", HandName: "verify-canon",
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, raw)

	_, data, _ := c.Read(ctx)
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindHandError {
		t.Errorf("kind = %q, want hand.error", env.Kind)
	}
	var errPayload frames.HandErrorPayload
	_ = frames.PayloadAs(env, &errPayload)
	if errPayload.Code != "no_dispatcher" {
		t.Errorf("code = %q, want no_dispatcher", errPayload.Code)
	}
}

func TestHandDispatchBadPayload(t *testing.T) {
	handler, _, _ := newBrokerWithQueue(t, func(context.Context, frames.HandDispatchPayload) (frames.HandResultPayload, error) {
		t.Fatal("executor should not be called for invalid payload")
		return frames.HandResultPayload{}, nil
	})
	srv := httptestNewServer(t, handler)
	c := dialWSURL(t, srv, "testtoken")

	req, _ := frames.NewRequest(frames.KindHandDispatch, frames.HandDispatchPayload{
		// Missing TargetAspect and HandName
	})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, raw)

	_, data, _ := c.Read(ctx)
	env, _ := frames.Decode(data)
	if env.Kind != frames.KindHandError {
		t.Errorf("kind = %q, want hand.error", env.Kind)
	}
	var errPayload frames.HandErrorPayload
	_ = frames.PayloadAs(env, &errPayload)
	if errPayload.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", errPayload.Code)
	}
}

// Support helpers — duplicate of ws_test.go but targeting a
// pre-built handler.
func newTestServerNoQueue(t *testing.T) (*testHandler, *roster.Roster, *Broker) {
	t.Helper()
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
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
