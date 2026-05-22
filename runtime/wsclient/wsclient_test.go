package wsclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// fakeServer spins up an httptest WS server with a simple echo-reply
// handler that echoes back the request kind + sets InReplyTo.
func fakeServer(t *testing.T, token string, onFrame func(*websocket.Conn, frames.Envelope)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Logf("accept: %v", err)
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		wsc.SetReadLimit(1 << 20)
		ctx := context.Background()
		for {
			_, data, err := wsc.Read(ctx)
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil {
				continue
			}
			onFrame(wsc, env)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestRunAndSend(t *testing.T) {
	var received atomic.Int32
	srv := fakeServer(t, "tok", func(_ *websocket.Conn, _ frames.Envelope) {
		received.Add(1)
	})

	c, err := New(Config{
		URL:       wsURL(srv),
		AuthToken: "tok",
		Handler:   HandlerFunc(func(frames.Envelope) {}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	waitUntilConnected(t, c)

	env, _ := frames.New(frames.KindRegister, nil)
	if err := c.Send(ctx, env); err != nil {
		t.Fatal(err)
	}

	// Give the server's goroutine a moment.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if received.Load() != 1 {
		t.Errorf("server received = %d, want 1", received.Load())
	}

	cancel()
	<-done
}

func TestRequestResponse(t *testing.T) {
	srv := fakeServer(t, "tok", func(wsc *websocket.Conn, env frames.Envelope) {
		// Echo back a register.ack with in_reply_to = env.ID.
		resp, _ := frames.NewResponse(frames.KindRegisterAck, env.ID, frames.RegisterAckPayload{
			HeartbeatIntervalS: 42,
		})
		raw, _ := frames.Encode(resp)
		_ = wsc.Write(context.Background(), websocket.MessageText, raw)
	})

	c, _ := New(Config{
		URL:       wsURL(srv),
		AuthToken: "tok",
		Handler:   HandlerFunc(func(frames.Envelope) {}),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitUntilConnected(t, c)

	reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
	defer reqCancel()
	req, _ := frames.NewRequest(frames.KindRegister, nil)
	resp, err := c.Request(reqCtx, req)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Kind != frames.KindRegisterAck {
		t.Errorf("resp kind = %q", resp.Kind)
	}
	if resp.InReplyTo != req.ID {
		t.Errorf("InReplyTo = %q, want %q", resp.InReplyTo, req.ID)
	}
	var ack frames.RegisterAckPayload
	if err := frames.PayloadAs(resp, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.HeartbeatIntervalS != 42 {
		t.Errorf("ack.HeartbeatIntervalS = %d", ack.HeartbeatIntervalS)
	}
}

func TestRequestTimesOut(t *testing.T) {
	// Server that never replies.
	srv := fakeServer(t, "tok", func(_ *websocket.Conn, _ frames.Envelope) {})

	c, _ := New(Config{
		URL:       wsURL(srv),
		AuthToken: "tok",
		Handler:   HandlerFunc(func(frames.Envelope) {}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitUntilConnected(t, c)

	reqCtx, reqCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer reqCancel()
	req, _ := frames.NewRequest(frames.KindRegister, nil)
	_, err := c.Request(reqCtx, req)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestRequestRejectsMissingID(t *testing.T) {
	c, _ := New(Config{
		URL:       "ws://unused",
		AuthToken: "tok",
		Handler:   HandlerFunc(func(frames.Envelope) {}),
	})
	env, _ := frames.New(frames.KindRegister, nil) // no ID
	_, err := c.Request(context.Background(), env)
	if err == nil {
		t.Error("Request should reject envelope without ID")
	}
}

func TestUncorrelatedFramesGoToHandler(t *testing.T) {
	var gotCh = make(chan frames.Envelope, 1)
	srv := fakeServer(t, "tok", func(wsc *websocket.Conn, _ frames.Envelope) {
		// Push an unsolicited frame (e.g. turn request from upstream).
		push, _ := frames.NewRequest(frames.KindTurn, frames.TurnPayload{Prompt: "ping"})
		raw, _ := frames.Encode(push)
		_ = wsc.Write(context.Background(), websocket.MessageText, raw)
	})

	c, _ := New(Config{
		URL:       wsURL(srv),
		AuthToken: "tok",
		Handler: HandlerFunc(func(env frames.Envelope) {
			select {
			case gotCh <- env:
			default:
			}
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitUntilConnected(t, c)

	// Kick the server with any frame; it'll push the turn back.
	kickoff, _ := frames.New(frames.KindRegister, nil)
	if err := c.Send(ctx, kickoff); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-gotCh:
		if got.Kind != frames.KindTurn {
			t.Errorf("handler received %q, want turn", got.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never called with unsolicited turn frame")
	}
}

func TestFailFirstConnect(t *testing.T) {
	// No server at all. With FailFirstConnect=true, Run returns an
	// error rather than retrying forever.
	c, _ := New(Config{
		URL:              "ws://127.0.0.1:1", // nothing listens on port 1
		AuthToken:        "tok",
		Handler:          HandlerFunc(func(frames.Envelope) {}),
		FailFirstConnect: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if err == nil {
		t.Error("expected error on first-connect failure with FailFirstConnect=true")
	}
	if !strings.Contains(err.Error(), "initial connect failed") {
		t.Errorf("err = %v, want initial-connect failure", err)
	}
}

func TestReconnectsAfterDrop(t *testing.T) {
	var connectCount atomic.Int32

	// Server that closes each new connection immediately so we can
	// count connect attempts.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectCount.Add(1)
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = wsc.Close(websocket.StatusNormalClosure, "immediate drop")
	}))
	defer srv.Close()

	c, _ := New(Config{
		URL:       wsURL(srv),
		AuthToken: "tok",
		Handler:   HandlerFunc(func(frames.Envelope) {}),
		// Tight backoffs so the test doesn't spin for minutes.
		MinReconnectDelay: 10 * time.Millisecond,
		MaxReconnectDelay: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)

	// With tight retries we should see multiple connect attempts.
	if connectCount.Load() < 2 {
		t.Errorf("connect count = %d, want at least 2 (proves reconnect loop)", connectCount.Load())
	}
}

// waitUntilConnected spin-waits up to 2s for the client to report
// Connected() == true.
func waitUntilConnected(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Connected() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client never connected within 2s")
}

// Compile-time assertion that HandlerFunc satisfies Handler.
var _ Handler = HandlerFunc(nil)

// silence unused imports in some skinny builds
var _ sync.Mutex

// silentServer accepts the WS upgrade then stops responding — no
// pings, no close. Simulates broker death with half-open socket.
func silentServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		// Hold the connection open, never read, never write.
		<-r.Context().Done()
		_ = wsc.Close(websocket.StatusNormalClosure, "shutdown")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReadIdleTimeoutReconnects(t *testing.T) {
	srv := silentServer(t, "tok")

	var reconnects atomic.Int32
	c, err := New(Config{
		URL:               wsURL(srv),
		AuthToken:         "tok",
		Handler:           HandlerFunc(func(frames.Envelope) {}),
		ReadIdleTimeout:   200 * time.Millisecond,
		MinReconnectDelay: 10 * time.Millisecond,
		MaxReconnectDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-c.Events():
				if ev.Connected {
					reconnects.Add(1)
				}
			}
		}
	}()

	_ = c.Run(ctx)
	<-done

	// Within 2s with a 200ms idle timeout, we should have multiple
	// connect cycles. >=2 means at least one reconnect happened.
	if got := reconnects.Load(); got < 2 {
		t.Fatalf("expected >=2 connect events from idle-timeout reconnects, got %d", got)
	}
}

// pingingServer accepts a WS and sends a text frame every interval.
// Simulates the broker's keepalive: while frames flow, the client
// must not reconnect.
func pingingServer(t *testing.T, token string, interval time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tk.C:
				env, _ := frames.New(frames.KindRegister, nil)
				raw, _ := frames.Encode(env)
				if err := wsc.Write(r.Context(), websocket.MessageText, raw); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReadIdleTimeoutDoesNotReconnectWhenHealthy(t *testing.T) {
	srv := pingingServer(t, "tok", 50*time.Millisecond)

	var connects atomic.Int32
	c, err := New(Config{
		URL:             wsURL(srv),
		AuthToken:       "tok",
		Handler:         HandlerFunc(func(frames.Envelope) {}),
		ReadIdleTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-c.Events():
				if ev.Connected {
					connects.Add(1)
				}
			}
		}
	}()

	_ = c.Run(ctx)
	<-done

	if got := connects.Load(); got != 1 {
		t.Fatalf("expected exactly 1 connect event (no reconnects on healthy line), got %d", got)
	}
}
