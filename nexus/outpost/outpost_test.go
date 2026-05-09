package outpost

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/internal/testcerts"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// fakeNexus is a tiny WS server that records incoming frames and
// acks outpost.register. Used in place of a real Nexus for tests.
type fakeNexus struct {
	srv               *httptest.Server
	token             string
	mu                sync.Mutex
	conns             []*websocket.Conn
	outpostRegistered atomic.Int32
	aspectRegistered  atomic.Int32
	ch                chan frames.Envelope
}

func newFakeNexus(t *testing.T, token string) *fakeNexus {
	t.Helper()
	f := &fakeNexus{
		token: token,
		ch:    make(chan frames.Envelope, 32),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connect" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		wsc.SetReadLimit(1 << 20)
		f.mu.Lock()
		f.conns = append(f.conns, wsc)
		f.mu.Unlock()
		f.serveLoop(wsc)
	}))
	t.Cleanup(func() {
		f.srv.Close()
	})
	return f
}

func (f *fakeNexus) URL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/connect" }

func (f *fakeNexus) serveLoop(wsc *websocket.Conn) {
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
		switch env.Kind {
		case frames.KindOutpostRegister:
			f.outpostRegistered.Add(1)
			ack, _ := frames.NewResponse(frames.KindOutpostRegisterAck, env.ID, frames.OutpostRegisterAckPayload{
				HeartbeatIntervalS: 15,
			})
			raw, _ := frames.Encode(ack)
			_ = wsc.Write(ctx, websocket.MessageText, raw)
		case frames.KindRegister:
			f.aspectRegistered.Add(1)
			ack, _ := frames.NewResponse(frames.KindRegisterAck, env.ID, frames.RegisterAckPayload{
				HeartbeatIntervalS: 15,
				StaleAfterS:        30,
			})
			raw, _ := frames.Encode(ack)
			_ = wsc.Write(ctx, websocket.MessageText, raw)
			select {
			case f.ch <- env:
			default:
			}
		default:
			select {
			case f.ch <- env:
			default:
			}
		}
	}
}

// freePort picks an available TCP port for the Outpost listener.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestOutpostRegistersUpstream(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	o, err := New(Config{
		ListenAddr:  freePort(t),
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.outpostRegistered.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.outpostRegistered.Load() == 0 {
		t.Fatal("outpost never registered upstream")
	}

	cancel()
	<-done
}

func TestAspectConnectIsForwardedWithViaStamp(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	listenAddr := freePort(t)
	o, err := New(Config{
		ListenAddr:  listenAddr,
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost-42",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// Wait for the upstream register.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.outpostRegistered.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.outpostRegistered.Load() == 0 {
		t.Fatal("outpost never registered upstream; abort")
	}

	// Connect as an aspect to the Outpost. Outpost serves TLS now, so
	// dial with wss:// + a TLS-skip-verify HTTPClient since the test
	// cert is self-signed and not in any trust store.
	if !strings.Contains(listenAddr, ":") {
		t.Fatalf("unexpected listen addr format: %q", listenAddr)
	}
	outpostURL := "wss://" + listenAddr + "/connect"

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	insecureClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	c, _, err := websocket.Dial(dialCtx, outpostURL, &websocket.DialOptions{
		HTTPClient: insecureClient,
		HTTPHeader: http.Header{"Authorization": {"Bearer tok"}},
	})
	if err != nil {
		t.Fatalf("aspect dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")

	// Send a register frame.
	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "viaaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			SessionID:   "sess-x",
			Home:        "/tmp/viaaspect",
			StartedAt:   time.Now().UTC(),
		},
	})
	raw, _ := frames.Encode(regEnv)
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer writeCancel()
	if err := c.Write(writeCtx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}

	// Wait for the fakeNexus to see it, with via_outpost stamped.
	select {
	case env := <-nx.ch:
		if env.Kind != frames.KindRegister {
			t.Fatalf("fakeNexus got kind %q, want register", env.Kind)
		}
		var forwarded frames.ForwardedRegisterPayload
		if err := frames.PayloadAs(env, &forwarded); err != nil {
			t.Fatal(err)
		}
		if forwarded.Name != "viaaspect" {
			t.Errorf("forwarded name = %q", forwarded.Name)
		}
		if forwarded.ViaOutpost != "test-outpost-42" {
			t.Errorf("via_outpost = %q, want test-outpost-42", forwarded.ViaOutpost)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fakeNexus never received the forwarded register")
	}
}

// Regression for issue #20: when an aspect registers via outpost,
// the broker's correlated register.ack must route back to the
// originating aspect's connection. Pre-fix the ack fell into
// handleDownstreamFrame's default "kind not handled" branch and the
// aspect's Request timed out.
func TestRegisterAckRoutesBackToAspect(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	listenAddr := freePort(t)
	o, err := New(Config{
		ListenAddr:  listenAddr,
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost-ack",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	defer func() { cancel(); <-done }()

	for d := time.Now().Add(3 * time.Second); time.Now().Before(d) && nx.outpostRegistered.Load() == 0; {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.outpostRegistered.Load() == 0 {
		t.Fatal("outpost never registered upstream")
	}

	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	c, _, err := websocket.Dial(dialCtx, "wss://"+listenAddr+"/connect", &websocket.DialOptions{
		HTTPClient: insecure,
		HTTPHeader: http.Header{"Authorization": {"Bearer tok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name: "ackaspect", ContextMode: schemas.ContextGlobal,
			Provider: "claude-api", SessionID: "sess-1",
			Home: "/tmp/x", StartedAt: time.Now().UTC(),
		},
	})
	raw, _ := frames.Encode(regEnv)
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := c.Write(wctx, websocket.MessageText, raw); err != nil {
		wcancel()
		t.Fatal(err)
	}
	wcancel()

	rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer rcancel()
	_, data, err := c.Read(rctx)
	if err != nil {
		t.Fatalf("aspect never received register.ack: %v", err)
	}
	ack, err := frames.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Kind != frames.KindRegisterAck {
		t.Errorf("kind = %q, want register.ack", ack.Kind)
	}
	if ack.InReplyTo != regEnv.ID {
		t.Errorf("InReplyTo = %q, want %q", ack.InReplyTo, regEnv.ID)
	}
}

// Regression for issue #33: with per-aspect tokens configured, the
// outpost rejects register frames whose payload.Name doesn't match
// the inbound connection's resolved identity.
func TestOutpostRejectsRegisterWithMismatchedIdentity(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	listenAddr := freePort(t)
	o, err := New(Config{
		ListenAddr:  listenAddr,
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost-id",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
		AspectTokens: map[string]string{
			"wren": "wren-token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- o.Run(ctx) }()
	defer func() { cancel(); <-doneCh }()

	for d := time.Now().Add(3 * time.Second); time.Now().Before(d) && nx.outpostRegistered.Load() == 0; {
		time.Sleep(10 * time.Millisecond)
	}

	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	// Authenticate as wren but try to register as harrow — pre-fix
	// would have forwarded; post-fix outpost closes the connection.
	c, _, err := websocket.Dial(dialCtx, "wss://"+listenAddr+"/connect", &websocket.DialOptions{
		HTTPClient: insecure,
		HTTPHeader: http.Header{"Authorization": {"Bearer wren-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusInternalError, "test cleanup")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name: "harrow", ContextMode: schemas.ContextGlobal,
			Provider: "claude-api", SessionID: "evil",
			Home: "/tmp/evil", StartedAt: time.Now().UTC(),
		},
	})
	raw, _ := frames.Encode(regEnv)
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.Write(wctx, websocket.MessageText, raw)
	wcancel()

	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	_, _, err = c.Read(rctx)
	if err == nil {
		t.Fatal("expected outpost to close connection on identity mismatch")
	}
	closeStatus := websocket.CloseStatus(err)
	if closeStatus != websocket.StatusPolicyViolation {
		t.Errorf("close status = %d, want StatusPolicyViolation (%d)",
			closeStatus, websocket.StatusPolicyViolation)
	}
}

// Regression for #20 routing: an unsolicited downstream turn frame
// stamped with TargetAspect must be delivered to the matching local
// aspect connection. This is the broker→outpost→aspect path for
// turn dispatch (the path TODO'd as "not yet implemented" pre-fix).
func TestUnsolicitedTurnRoutesToTargetAspect(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	certPath, keyPath := testcerts.Mint(t)

	listenAddr := freePort(t)
	o, err := New(Config{
		ListenAddr:  listenAddr,
		UpstreamURL: nx.URL(),
		AuthToken:   "tok",
		OutpostID:   "test-outpost-turn",
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() { doneCh <- o.Run(ctx) }()
	defer func() { cancel(); <-doneCh }()

	for d := time.Now().Add(3 * time.Second); time.Now().Before(d) && nx.outpostRegistered.Load() == 0; {
		time.Sleep(10 * time.Millisecond)
	}

	// Connect aspect + register so o.aspects["wren"] is populated.
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	c, _, err := websocket.Dial(dialCtx, "wss://"+listenAddr+"/connect", &websocket.DialOptions{
		HTTPClient: insecure,
		HTTPHeader: http.Header{"Authorization": {"Bearer tok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name: "wren", ContextMode: schemas.ContextGlobal,
			Provider: "claude-api", SessionID: "sess",
			Home: "/tmp/wren", StartedAt: time.Now().UTC(),
		},
	})
	raw, _ := frames.Encode(regEnv)
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.Write(wctx, websocket.MessageText, raw)
	wcancel()

	// Drain register.ack.
	rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
	if _, _, err := c.Read(rctx); err != nil {
		rcancel()
		t.Fatalf("aspect never received register.ack: %v", err)
	}
	rcancel()

	// fakeNexus pushes an unsolicited turn frame stamped with
	// TargetAspect=wren down to the outpost. Outpost should deliver
	// to the wren conn.
	turnEnv, _ := frames.NewRequest(frames.KindTurn, frames.TurnPayload{Prompt: "hi"})
	turnEnv.TargetAspect = "wren"
	turnRaw, _ := frames.Encode(turnEnv)
	nx.mu.Lock()
	upstreamConn := nx.conns[0]
	nx.mu.Unlock()
	if err := upstreamConn.Write(context.Background(), websocket.MessageText, turnRaw); err != nil {
		t.Fatalf("fakeNexus write turn: %v", err)
	}

	// The aspect should receive the turn frame.
	rctx, rcancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer rcancel()
	_, data, err := c.Read(rctx)
	if err != nil {
		t.Fatalf("aspect never received unsolicited turn: %v", err)
	}
	got, err := frames.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != frames.KindTurn {
		t.Errorf("kind = %q, want turn", got.Kind)
	}
	if got.TargetAspect != "wren" {
		t.Errorf("TargetAspect = %q, want wren", got.TargetAspect)
	}
}

// Silence unused imports if slim build.
var _ = json.Marshal
