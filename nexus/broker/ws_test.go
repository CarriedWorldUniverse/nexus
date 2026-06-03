package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func newTestServer(t *testing.T) (*httptest.Server, *roster.Roster, *Broker) {
	t.Helper()
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true, // tests use the legacy fallback path
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)
	// Real startup sets b.ctx in ListenAndServe; the test harness bypasses it,
	// so init it here — handlers (e.g. the register herald-auth redeem) use it.
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
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

// Regression for issue #32: a non-admin caller cannot deregister
// another aspect. Pre-fix, handleDeregisterFrame trusted payload.Name
// without checking caller identity — any authenticated peer could DoS
// any other aspect by guessing/observing its session_id.
// Regression for issue #22: when an Origin header is presented (browser
// caller), the broker must reject upgrades whose Origin isn't in the
// configured allowlist. Non-browser aspects (no Origin) still connect
// freely; that's covered by every other test in this file. We exercise
// both rejection and acceptance to distinguish "allowlist enforced"
// from "library rejects everything."
func TestOriginAllowlistRejectsUnlistedBrowserOrigin(t *testing.T) {
	r := roster.New()
	// httptest binds a random ephemeral port on 127.0.0.1, so allowlist
	// the actual server origin once we know it.
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	allowed := "http://" + strings.TrimPrefix(srv.URL, "http://")
	b.cfg.OriginPatterns = []string{allowed}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"

	// Negative case: unlisted origin must be rejected.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	_, _, err := websocket.Dial(ctx1, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer testtoken"},
			"Origin":        {"http://evil.example.com"},
		},
	})
	if err == nil {
		t.Fatal("dial with unlisted origin should fail, got nil err")
	}

	// Positive case: an allowlisted origin connects. This pins that the
	// allowlist is doing actual matching, not blanket-rejecting all
	// Origin-bearing requests.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	c, _, err := websocket.Dial(ctx2, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer testtoken"},
			"Origin":        {allowed},
		},
	})
	if err != nil {
		t.Fatalf("dial with allowlisted origin should succeed, got: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
}

// Regression for issue #25: handleConnect rejects with 503 once
// MaxConnections is reached. Pre-fix the broker accepted unbounded
// connections, exhausting fds/goroutines under flood.
func TestConnectionCapRejectsAfterLimit(t *testing.T) {
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		MaxConnections:     2,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)

	// Open 2 connections (at the cap).
	c1 := dialWS(t, srv, "testtoken")
	c2 := dialWS(t, srv, "testtoken")
	_ = c2

	// Third connect must fail with 503.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer testtoken"}},
	})
	if err == nil {
		t.Fatal("third dial should fail at connection cap")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		gotStatus := -1
		if resp != nil {
			gotStatus = resp.StatusCode
		}
		t.Errorf("status = %d, want 503", gotStatus)
	}

	// Close one held connection and assert a slot opens up. This pins
	// releaseConn's symmetry: a bug that decremented connTotal but
	// not connPerIP (or vice versa) would leave the cap full and
	// fail this retry.
	_ = c1.Close(websocket.StatusNormalClosure, "freeing slot")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		retryCtx, retryCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		c3, _, dialErr := websocket.Dial(retryCtx, wsURL, &websocket.DialOptions{
			HTTPHeader: map[string][]string{"Authorization": {"Bearer testtoken"}},
		})
		retryCancel()
		if dialErr == nil {
			_ = c3.Close(websocket.StatusNormalClosure, "done")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("after closing one connection, retry never succeeded — releaseConn accounting bug")
}

// Per-IP cap exercised independently: with global cap generous and
// per-IP cap tight, three dials from the same source-IP fail at the
// per-IP limit even though global has headroom.
func TestPerIPConnectionCapRejectsAfterLimit(t *testing.T) {
	r := roster.New()
	b := New(Config{
		AuthToken:           "testtoken",
		AllowLegacyMaster:   true,
		MaxConnections:      32,
		MaxConnectionsPerIP: 2,
		HeartbeatIntervalS:  15,
		StaleAfter:          30 * time.Second,
	}, r)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)

	_ = dialWS(t, srv, "testtoken")
	_ = dialWS(t, srv, "testtoken")

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer testtoken"}},
	})
	if err == nil {
		t.Fatal("third dial should fail at per-IP cap")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		gotStatus := -1
		if resp != nil {
			gotStatus = resp.StatusCode
		}
		t.Errorf("status = %d, want 503", gotStatus)
	}
}

// Regression for issue #34: a connection streaming malformed frames
// gets cut off after MaxConsecutiveBadFrames.
func TestBadFrameCapClosesConnection(t *testing.T) {
	r := roster.New()
	b := New(Config{
		AuthToken:               "testtoken",
		AllowLegacyMaster:       true,
		MaxConsecutiveBadFrames: 3, // small for fast test
		HeartbeatIntervalS:      15,
		StaleAfter:              30 * time.Second,
	}, r)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)

	c := dialWS(t, srv, "testtoken")

	// Send 4 garbage frames. Per-frame writes until the server
	// closes; the read after close returns an error.
	for i := 0; i < 4; i++ {
		writeCtx, writeCancel := context.WithTimeout(context.Background(), 1*time.Second)
		err := c.Write(writeCtx, websocket.MessageText, []byte("not-json-at-all"))
		writeCancel()
		if err != nil {
			break // server already closed
		}
	}
	// Read should observe the close. Allow up to 2s.
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	_, _, err := c.Read(readCtx)
	if err == nil {
		t.Fatal("expected connection close after bad-frame cap")
	}
	closeStatus := websocket.CloseStatus(err)
	if closeStatus != websocket.StatusPolicyViolation {
		t.Errorf("close status = %d, want StatusPolicyViolation (%d)",
			closeStatus, websocket.StatusPolicyViolation)
	}
}

// Regression for issue #21: when AspectHomes is configured, the
// broker derives the aspect's home from its own discovery scan and
// overrides whatever payload.Home the aspect sends. Closes the
// cmd.Dir control vector — a stolen aspect token can't repoint the
// worker's working directory by register payload.
func TestRegisterDerivesHomeFromBrokerDiscovery(t *testing.T) {
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		AspectHomes: map[string]string{
			"smoketest": "/canonical/discovered/path",
		},
	}, r)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)

	c := dialWS(t, srv, "testtoken")
	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoketest",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-1",
			Home:        "/attacker/controlled/path",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, c, regEnv)
	ack := recvFrame(t, c)
	if ack.Kind == frames.Kind("register.error") {
		t.Fatalf("register failed: %v", ack)
	}

	entries := r.List()
	if len(entries) != 1 {
		t.Fatalf("expected 1 roster entry, got %d", len(entries))
	}
	if entries[0].Home != "/canonical/discovered/path" {
		t.Errorf("Home = %q, want broker-discovered path; payload was overridden",
			entries[0].Home)
	}
}

func TestDeregisterRejectsCrossAspectIdentity(t *testing.T) {
	srv, r, b := newTestServer(t)

	// Wire per-aspect tokens so we connect as a non-admin aspect.
	b.cfg.Tokens.SetTokenForTest("wren", "wrentok", false)
	b.cfg.Tokens.SetTokenForTest("anvil", "anviltok", false)

	// Register anvil first via its own token so there's something to
	// try to deregister.
	canvil := dialWS(t, srv, "anviltok")
	regAnvil, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "anvil",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "anvil-sess-1",
			Home:        "/tmp/anvil",
			StartedAt:   time.Now().UTC(),
		},
	})
	sendFrame(t, canvil, regAnvil)
	_ = recvFrame(t, canvil)

	if len(r.List()) != 1 {
		t.Fatalf("expected anvil registered, got %d", len(r.List()))
	}

	// Connect as wren and try to deregister anvil.
	cwren := dialWS(t, srv, "wrentok")
	deregEnv, _ := frames.NewRequest(frames.KindDeregister, frames.DeregisterPayload{
		DeregisterRequest: schemas.DeregisterRequest{
			Name:      "anvil",
			SessionID: "anvil-sess-1",
			Reason:    "DoS attempt",
		},
	})
	sendFrame(t, cwren, deregEnv)
	resp := recvFrame(t, cwren)

	if resp.Kind != frames.Kind("deregister.error") {
		t.Errorf("kind = %q, want deregister.error", resp.Kind)
	}

	// Anvil must still be registered.
	if len(r.List()) != 1 {
		t.Errorf("anvil was wrongly deregistered by wren: roster = %d", len(r.List()))
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

// -------------------------------------------------------------------
// Herald-auth on register (bootstrap step 3a)
// -------------------------------------------------------------------

// fakeCustodian injects a redeem/forget seam so the register handshake
// can be unit-tested without a live herald. forgot is mutex-guarded
// because Forget fires on the serve goroutine during cleanup while the
// test reads it.
type fakeCustodian struct {
	redeem func(ctx context.Context, assertion string) (string, error)

	mu     sync.Mutex
	forgot []string
}

func (f *fakeCustodian) Redeem(ctx context.Context, a string) (string, error) {
	return f.redeem(ctx, a)
}

func (f *fakeCustodian) Client(subject string) (*client.Client, error) {
	return client.WithStaticToken("http://x", "tok"), nil
}

func (f *fakeCustodian) Forget(subject string) {
	f.mu.Lock()
	f.forgot = append(f.forgot, subject)
	f.mu.Unlock()
}

func (f *fakeCustodian) forgotten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.forgot))
	copy(out, f.forgot)
	return out
}

// registerWith sends a register frame carrying the given assertion and
// returns the broker's reply (ack or error). Matches the required
// RegisterRequest fields validateRegister enforces (see
// TestRegisterFrameAddsRoster).
func registerWith(t *testing.T, c *websocket.Conn, name, assertion string) frames.Envelope {
	t.Helper()
	env, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:         name,
			ContextMode:  schemas.ContextGlobal,
			Provider:     "claude-api",
			Port:         0,
			Capabilities: []string{"smoke"},
			SessionID:    "sess-1",
			Home:         "/tmp/x",
			StartedAt:    time.Now().UTC(),
		},
		Assertion: assertion,
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	return recvFrame(t, c)
}

func ackSubject(t *testing.T, env frames.Envelope) string {
	t.Helper()
	var p frames.RegisterAckPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		t.Fatalf("ack payload: %v", err)
	}
	return p.HeraldSubject
}

func TestRegisterHeraldAssertionBinds(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = &fakeCustodian{redeem: func(_ context.Context, a string) (string, error) {
		if a == "" {
			return "", fmt.Errorf("empty")
		}
		return "agent-1", nil
	}}
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "ha", "a-real-looking-assertion")
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("kind=%s", ack.Kind)
	}
	if got := ackSubject(t, ack); got != "agent-1" {
		t.Fatalf("herald_subject=%q want agent-1", got)
	}
}

func TestRegisterNoAssertion(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = &fakeCustodian{redeem: func(context.Context, string) (string, error) { return "x", nil }}
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "na", "")
	if ack.Kind != frames.KindRegisterAck || ackSubject(t, ack) != "" {
		t.Fatalf("no-assertion register should not bind; ack=%s subj=%q", ack.Kind, ackSubject(t, ack))
	}
}

func TestRegisterBadAssertion(t *testing.T) {
	srv, r, b := newTestServer(t)
	b.custodian = &fakeCustodian{redeem: func(context.Context, string) (string, error) {
		return "", fmt.Errorf("herald rejected")
	}}
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "bad", "nope")
	// respondError emits <kind>.error — for a register frame that is
	// "register.error" (there is no generic frames.KindError).
	if want := frames.Kind("register.error"); ack.Kind != want {
		t.Fatalf("bad assertion should error with %s, got %s", want, ack.Kind)
	}
	// A failed assertion must leave NO roster state — the redeem runs before
	// roster.Register, so no phantom "live" aspect can be created.
	if _, ok := r.Get("bad"); ok {
		t.Fatal("failed-assertion register must not leave the aspect in the roster")
	}
}

func TestRegisterCustodianNil(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = nil // herald-auth disabled
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "off", "ignored-assertion")
	if ack.Kind != frames.KindRegisterAck || ackSubject(t, ack) != "" {
		t.Fatalf("custodian-nil should ignore the assertion; ack=%s subj=%q", ack.Kind, ackSubject(t, ack))
	}
}

func TestRegisterForgetOnClose(t *testing.T) {
	srv, _, b := newTestServer(t)
	fc := &fakeCustodian{redeem: func(context.Context, string) (string, error) { return "agent-1", nil }}
	b.custodian = fc
	c := dialWS(t, srv, "testtoken")
	registerWith(t, c, "fc", "sig")
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	// cleanup runs async on the serve goroutine; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fc.forgotten()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := fc.forgotten()
	if len(got) == 0 || got[0] != "agent-1" {
		t.Fatalf("Forget not called on close: %v", got)
	}
}
