package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/shared/schemas"
)

func newTestServer(t *testing.T) (*httptest.Server, *roster.Roster, *Broker) {
	t.Helper()
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	return srv, r, b
}

// newMux mirrors the mux construction in ListenAndServe so tests can
// drive the handlers without spinning up the real ListenAndServe
// goroutine (which blocks and manages its own lifecycle).
func newMux(b *Broker) *testHandler {
	// Simpler than constructing a net/http.ServeMux since we only
	// need the /connect path for these tests. /api/aspects and
	// /health aren't exercised here.
	return &testHandler{b: b}
}

type testHandler struct{ b *Broker }

func (t *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/connect":
		t.b.handleConnect(w, r)
	case "/api/aspects":
		t.b.auth(http.HandlerFunc(t.b.handleList)).ServeHTTP(w, r)
	case "/health":
		t.b.handleHealth(w, r)
	default:
		http.NotFound(w, r)
	}
}

// dialWS connects a client WS to the test server's /connect endpoint
// with the given token. Returns the connection + cleanup.
func dialWS(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "done") })
	return c
}

// sendFrame is a tiny helper to write a frame onto a WS.
func sendFrame(t *testing.T, c *websocket.Conn, env frames.Envelope) {
	t.Helper()
	raw, err := frames.Encode(env)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// recvFrame reads one frame from the WS.
func recvFrame(t *testing.T, c *websocket.Conn) frames.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	env, err := frames.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

// -------------------------------------------------------------------
// Tests
// -------------------------------------------------------------------

func TestConnectRejectsBadToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer wrong"}},
	})
	if err == nil {
		t.Fatal("expected dial error for bad token")
	}
	if resp == nil || resp.StatusCode != 401 {
		if resp != nil {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	}
}

func TestConnectAcceptsValidToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := dialWS(t, srv, "testtoken")
	_ = c // connection is alive if dialWS returned
}

func TestRegisterFrameAddsRoster(t *testing.T) {
	srv, r, _ := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	env, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:         "smoketest",
			ContextMode:  schemas.ContextGlobal,
			Provider:     "claude-api",
			Port:         0,
			Capabilities: []string{"smoke"},
			SessionID:    "sess-1",
			Home:         "/tmp/smoke",
			StartedAt:    time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)

	ack := recvFrame(t, c)
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("ack kind = %q, want %q", ack.Kind, frames.KindRegisterAck)
	}
	if ack.InReplyTo != env.ID {
		t.Errorf("ack InReplyTo = %q, want %q", ack.InReplyTo, env.ID)
	}
	var ackPayload frames.RegisterAckPayload
	if err := frames.PayloadAs(ack, &ackPayload); err != nil {
		t.Fatal(err)
	}
	if ackPayload.HeartbeatIntervalS != 15 {
		t.Errorf("heartbeat_interval_s = %d", ackPayload.HeartbeatIntervalS)
	}

	// Roster should now have the entry.
	found := false
	for _, a := range r.List() {
		if a.Name == "smoketest" {
			found = true
		}
	}
	if !found {
		t.Error("aspect not in roster after register")
	}
}

func TestRegisterInvalidPayload(t *testing.T) {
	srv, r, _ := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	// Missing name — validator should reject.
	env, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
		},
	})
	sendFrame(t, c, env)

	resp := recvFrame(t, c)
	if resp.Kind != frames.Kind("register.error") {
		t.Errorf("kind = %q, want register.error", resp.Kind)
	}

	// Roster must NOT have any entry.
	if len(r.List()) != 0 {
		t.Errorf("roster should be empty after failed register, got %d", len(r.List()))
	}
}

func TestDeregisterRemovesRoster(t *testing.T) {
	srv, r, _ := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	_ = recvFrame(t, c) // ack

	deregEnv, _ := frames.NewRequest(frames.KindDeregister, frames.DeregisterPayload{
		DeregisterRequest: schemas.DeregisterRequest{
			Name:      "smoketest",
			SessionID: "sess-1",
			Reason:    "test done",
		},
	})
	sendFrame(t, c, deregEnv)
	ack := recvFrame(t, c)
	if ack.InReplyTo != deregEnv.ID {
		t.Errorf("deregister ack InReplyTo = %q", ack.InReplyTo)
	}

	if len(r.List()) != 0 {
		t.Errorf("roster should be empty after deregister, got %d", len(r.List()))
	}
}

func TestDisconnectAutoDeregisters(t *testing.T) {
	srv, r, _ := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	_ = recvFrame(t, c)

	if len(r.List()) != 1 {
		t.Fatalf("roster before close = %d", len(r.List()))
	}

	// Abrupt close should trigger cleanup → deregister.
	_ = c.Close(websocket.StatusGoingAway, "test abrupt close")

	// Give the server goroutine a moment to process the close.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.List()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("roster still has %d entries after disconnect; autoderegister failed", len(r.List()))
}

func TestConcurrentRegistrationRejectedWhenLive(t *testing.T) {
	// The roster deliberately protects live sessions from being
	// displaced by a stale duplicate claiming the same name with a
	// different session id. Only after the live connection drops
	// (and the reaper marks the entry stale) can a new session take
	// over — which is what happens naturally on reconnect via the
	// auto-deregister-on-disconnect path (see TestDisconnectAuto).
	// This test pins that guarantee.
	srv, r, _ := newTestServer(t)

	c1 := dialWS(t, srv, "testtoken")
	reg1, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c1, reg1)
	_ = recvFrame(t, c1)

	// Second connection tries to register with a new session id while
	// sess-1 is still live. Roster rejects: preventing a stale dupe
	// from evicting a live session.
	c2 := dialWS(t, srv, "testtoken")
	reg2, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-2",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c2, reg2)
	resp := recvFrame(t, c2)
	if resp.Kind != frames.Kind("register.error") {
		t.Errorf("second register should be rejected while sess-1 is live, got %q", resp.Kind)
	}

	entries := r.List()
	if len(entries) != 1 {
		t.Errorf("roster should have 1 entry, got %d", len(entries))
	}
	if entries[0].SessionID != "sess-1" {
		t.Errorf("winning session = %q, want sess-1 (rejecting second registration protects the live one)", entries[0].SessionID)
	}
}

func TestTwoPortZeroAspectsCoexist(t *testing.T) {
	// WS-era aspects all register with port=0 (no inbound listener).
	// The roster must NOT treat port 0 as a collision key — that
	// would reject every aspect after the first.
	srv, r, _ := newTestServer(t)

	register := func(name, session string) frames.Envelope {
		c := dialWS(t, srv, "testtoken")
		env, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
			RegisterRequest: schemas.RegisterRequest{
				Name:        name,
				ContextMode: schemas.ContextGlobal,
				Provider:    "claude-api",
				SessionID:   session,
				Home:        "/tmp/" + name,
				StartedAt:   time.Now().UTC(),
				// Port deliberately unset (zero).
			},
		})
		sendFrame(t, c, env)
		return recvFrame(t, c)
	}

	if ack := register("alpha", "sess-a"); ack.Kind != frames.KindRegisterAck {
		t.Fatalf("alpha register: kind=%q, want register.ack", ack.Kind)
	}
	if ack := register("beta", "sess-b"); ack.Kind != frames.KindRegisterAck {
		t.Fatalf("beta register (second port-0 aspect): kind=%q, want register.ack — port 0 collision bug?", ack.Kind)
	}

	if len(r.List()) != 2 {
		t.Errorf("roster size = %d, want 2 (both port-0 aspects should coexist)", len(r.List()))
	}
}

func TestReconnectAfterDropReplacesSession(t *testing.T) {
	// After the live connection drops, the aspect is auto-deregistered
	// (see TestDisconnectAutoDeregisters). A fresh connection with a
	// new session id then registers cleanly — the legitimate reconnect
	// path.
	srv, r, _ := newTestServer(t)

	c1 := dialWS(t, srv, "testtoken")
	reg1, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c1, reg1)
	_ = recvFrame(t, c1)

	_ = c1.Close(websocket.StatusGoingAway, "drop")

	// Wait for the auto-deregister on disconnect.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(r.List()) > 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Now sess-2 can register.
	c2 := dialWS(t, srv, "testtoken")
	reg2, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-2",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c2, reg2)
	ack := recvFrame(t, c2)
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("reconnect with new session should succeed, got %q", ack.Kind)
	}
	entries := r.List()
	if len(entries) != 1 || entries[0].SessionID != "sess-2" {
		t.Errorf("expected sess-2 live, got %+v", entries)
	}
}

func TestUnknownKindDropped(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := dialWS(t, srv, "testtoken")

	// Fabricate a frame with an unknown kind. The server should log-
	// and-drop per forward-compat; the connection stays open.
	raw, _ := json.Marshal(map[string]any{
		"kind": "something.new",
		"ts":   time.Now().UTC(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}

	// Follow up with a valid register — should still succeed,
	// proving the connection wasn't killed by the unknown frame.
	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/tmp/smoke",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	ack := recvFrame(t, c)
	if ack.Kind != frames.KindRegisterAck {
		t.Errorf("register after unknown-kind drop failed; got %q", ack.Kind)
	}
}
