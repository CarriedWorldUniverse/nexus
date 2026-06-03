package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/runtime/heraldkeyfile"
	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// mockProvider is a minimal Provider for agent turn tests.
type mockProvider struct {
	reply       string
	invokeCalls atomic.Int32
}

func (m *mockProvider) Invoke(_ context.Context, _ providers.InvokeRequest) (providers.InvokeResult, error) {
	m.invokeCalls.Add(1)
	return providers.InvokeResult{
		Output:     m.reply,
		StopReason: providers.StopEndTurn,
		Tokens:     providers.TokenCounts{Input: 10, Output: 20, Total: 30},
	}, nil
}
func (m *mockProvider) Stream(context.Context, providers.InvokeRequest) (providers.StreamIterator, error) {
	return nil, providers.ErrUnsupported
}
func (m *mockProvider) TokenCount(context.Context, string, string) (int, error) { return 0, nil }
func (m *mockProvider) Compact(context.Context, []providers.Entry, string) (providers.CompactionResult, error) {
	return providers.CompactionResult{}, providers.ErrUnsupported
}
func (m *mockProvider) Embed(context.Context, providers.EmbedRequest) (providers.EmbedResult, error) {
	return providers.EmbedResult{}, providers.ErrUnsupported
}
func (m *mockProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Chat: true, MaxContextTokens: 200_000}
}
func (m *mockProvider) Models(context.Context) ([]providers.Model, error) { return nil, nil }
func (m *mockProvider) TriageModel() string                               { return "mock-triage" }

// fakeNexus spins up an httptest WS server that accepts /connect,
// handles register/deregister frames, and routes all other inbound
// frames (turn.result, etc.) to a shared channel that tests can
// consume. Having a single reader goroutine per connection avoids
// concurrent Read calls on the same *websocket.Conn, which
// coder/websocket does not permit.
type fakeNexus struct {
	srv   *httptest.Server
	token string

	mu          sync.Mutex
	conns       []*websocket.Conn
	inboundCh   chan frames.Envelope // non-register/deregister frames land here
	registers   atomic.Int32
	deregisters atomic.Int32

	lastAssertion atomic.Value // string
}

func newFakeNexus(t *testing.T, token string) *fakeNexus {
	t.Helper()
	f := &fakeNexus{
		token:     token,
		inboundCh: make(chan frames.Envelope, 32),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/herald/.well-known/openid-configuration" {
			base := "http://" + r.Host
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token_endpoint":"` + base + `/herald/token","jwks_uri":"` + base + `/jwks"}`))
			return
		}
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
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.conns {
			_ = c.Close(websocket.StatusNormalClosure, "test done")
		}
	})
	return f
}

func (f *fakeNexus) URL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/connect" }

func (f *fakeNexus) BaseWS() string   { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }
func (f *fakeNexus) HTTPBase() string { return f.srv.URL }

// serveLoop is the SOLE reader on its connection. Handles
// register/deregister inline; everything else is forwarded to
// inboundCh for test assertions.
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
		case frames.KindRegister:
			f.registers.Add(1)
			var rp frames.RegisterPayload
			_ = frames.PayloadAs(env, &rp)
			f.lastAssertion.Store(rp.Assertion)
			ack, _ := frames.NewResponse(frames.KindRegisterAck, env.ID, frames.RegisterAckPayload{
				HeartbeatIntervalS: 15,
				StaleAfterS:        30,
			})
			raw, _ := frames.Encode(ack)
			_ = wsc.Write(ctx, websocket.MessageText, raw)
		case frames.KindDeregister:
			f.deregisters.Add(1)
			ack, _ := frames.NewResponse(frames.KindDeregister, env.ID, nil)
			raw, _ := frames.Encode(ack)
			_ = wsc.Write(ctx, websocket.MessageText, raw)
		default:
			// Non-blocking fan-out — a full channel means the test
			// isn't consuming fast enough; drop and log for debug.
			select {
			case f.inboundCh <- env:
			default:
			}
		}
	}
}

// PushTurn sends a turn frame to the first connected client and
// waits (up to 5s) for a correlated turn.result via inboundCh.
// Writes happen from the test goroutine — safe because
// coder/websocket permits concurrent Write calls but not concurrent
// Read calls. The read is already owned by serveLoop.
func (f *fakeNexus) PushTurn(t *testing.T, prompt string) (string, frames.Envelope) {
	t.Helper()
	f.mu.Lock()
	var wsc *websocket.Conn
	if len(f.conns) > 0 {
		wsc = f.conns[0]
	}
	f.mu.Unlock()
	if wsc == nil {
		t.Fatal("PushTurn: no connected clients")
	}

	req, _ := frames.NewRequest(frames.KindTurn, frames.TurnPayload{Prompt: prompt})
	raw, _ := frames.Encode(req)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsc.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("PushTurn write: %v", err)
	}

	// Wait for the correlated turn.result to arrive via inboundCh.
	// Drain any unrelated frames that might be ahead of it.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-f.inboundCh:
			if env.InReplyTo == req.ID {
				return req.ID, env
			}
			// Unrelated frame; keep draining.
		case <-deadline:
			t.Fatalf("PushTurn: timed out waiting for turn.result with in_reply_to=%s", req.ID)
		}
	}
}

func newAgent(t *testing.T, nexusURL, token string, provider providers.Provider) *Agent {
	t.Helper()
	a, err := New(Config{
		Home: t.TempDir(),
		Aspect: schemas.AspectConfig{
			Name:        "testaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
			Port:        0,
		},
		Provider:    provider,
		UpstreamURL: nexusURL,
		AuthToken:   token,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func newAgentWithHerald(t *testing.T, nexusURL, token string, provider providers.Provider, kf *heraldkeyfile.Keyfile) *Agent {
	t.Helper()
	a, err := New(Config{
		Home:          t.TempDir(),
		Aspect:        schemas.AspectConfig{Name: "testaspect", ContextMode: schemas.ContextGlobal, Provider: "claude-api", Port: 0},
		Provider:      provider,
		UpstreamURL:   nexusURL,
		AuthToken:     token,
		HeraldKeyfile: kf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestSendRegisterAttachesAssertion(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	priv, pub, _ := casket.DeriveAgentKey([]byte("0123456789abcdef0123456789abcdef"), "plumb")
	kf := &heraldkeyfile.Keyfile{
		Key:         base64.StdEncoding.EncodeToString(priv),
		KeyID:       "agent-uuid-9",
		URL:         nx.BaseWS(),
		Slug:        "plumb",
		Fingerprint: identity.Fingerprint(pub),
	}
	a := newAgentWithHerald(t, nx.URL(), "tok", &mockProvider{reply: "ok"}, kf)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.registers.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.registers.Load() == 0 {
		t.Fatal("agent never registered")
	}
	got, _ := nx.lastAssertion.Load().(string)
	if got == "" {
		t.Fatal("no assertion in register frame")
	}
	claims, err := identity.DecodeAccessClaims(got)
	if err != nil {
		t.Fatalf("decode assertion: %v", err)
	}
	if claims["sub"] != "agent-uuid-9" {
		t.Errorf("sub = %v, want agent-uuid-9", claims["sub"])
	}
	if aud, _ := claims["aud"].(string); aud != nx.HTTPBase()+"/herald/token" {
		t.Errorf("aud = %v, want %s/herald/token", claims["aud"], nx.HTTPBase())
	}
}

func TestNewRequiresFields(t *testing.T) {
	cases := map[string]Config{
		"empty home":     {Aspect: schemas.AspectConfig{Name: "x"}, Provider: &mockProvider{}, UpstreamURL: "ws://x"},
		"empty name":     {Home: "/tmp", Provider: &mockProvider{}, UpstreamURL: "ws://x"},
		"nil provider":   {Home: "/tmp", Aspect: schemas.AspectConfig{Name: "x"}, UpstreamURL: "ws://x"},
		"empty upstream": {Home: "/tmp", Aspect: schemas.AspectConfig{Name: "x"}, Provider: &mockProvider{}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(cfg)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestStartRegistersAndDeregisters(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	a := newAgent(t, nx.URL(), "tok", &mockProvider{reply: "ok"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()

	// Wait for register.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.registers.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.registers.Load() == 0 {
		t.Fatal("agent never registered")
	}

	cancel()
	<-done

	// Deregister should have fired during graceful shutdown.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && nx.deregisters.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.deregisters.Load() != 1 {
		t.Errorf("deregisters = %d, want 1", nx.deregisters.Load())
	}
}

func TestTurnDispatchViaWS(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	mp := &mockProvider{reply: "hello from model"}
	a := newAgent(t, nx.URL(), "tok", mp)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	// Wait for register + our serveLoop to enter read mode.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.registers.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.registers.Load() == 0 {
		t.Fatal("agent never registered")
	}

	// Drive a turn from the Nexus side.
	_, resp := nx.PushTurn(t, "ping?")
	if resp.Kind != frames.KindTurnResult {
		t.Errorf("resp kind = %q, want turn.result", resp.Kind)
	}

	var result frames.TurnResultPayload
	if err := frames.PayloadAs(resp, &result); err != nil {
		t.Fatal(err)
	}
	if result.Output != "hello from model" {
		t.Errorf("output = %q", result.Output)
	}
	if len(result.EntryIDs) != 2 {
		t.Errorf("entry_ids len = %d, want 2", len(result.EntryIDs))
	}
	if mp.invokeCalls.Load() != 1 {
		t.Errorf("invokeCalls = %d, want 1", mp.invokeCalls.Load())
	}

	// Session tree should now have 2 entries (user + assistant).
	branch, err := a.Tree().Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(branch) != 2 {
		t.Errorf("branch len = %d, want 2", len(branch))
	}
}

func TestFailLoudOnExplicitOutpostUnreachable(t *testing.T) {
	// An aspect with NEXUS_OUTPOST set should refuse to start if the
	// outpost is unreachable at initial connect (transport spec §3.5).
	a, err := New(Config{
		Home: t.TempDir(),
		Aspect: schemas.AspectConfig{
			Name:        "testaspect",
			ContextMode: schemas.ContextGlobal,
			Provider:    "claude-api",
		},
		Provider:                  &mockProvider{},
		UpstreamURL:               "ws://127.0.0.1:1/connect", // nothing listens here
		UpstreamIsExplicitOutpost: true,
		AuthToken:                 "tok",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = a.Start(ctx)
	if err == nil {
		t.Fatal("expected error when explicit outpost unreachable")
	}
	if !strings.Contains(err.Error(), "initial connect failed") && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want initial-connect failure", err)
	}
}

func TestHTTPEdge(t *testing.T) {
	cases := map[string]string{
		"ws://host:8080":            "http://host:8080",
		"wss://host":                "https://host",
		"http://host":               "http://host",
		"https://host:9":            "https://host:9",
		"WSS://Host":                "https://Host",
		"ws://host:8080/connect":    "http://host:8080",
		"https://host/herald/x?y=1": "https://host",
	}
	for in, want := range cases {
		if got := httpEdge(in); got != want {
			t.Errorf("httpEdge(%q) = %q, want %q", in, got, want)
		}
	}
}
