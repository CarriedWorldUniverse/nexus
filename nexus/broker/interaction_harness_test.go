package broker

// L4 multi-aspect interaction harness (NEX-366).
//
// Composes the real in-process broker (newTestServer) with full
// funnel-driven aspects connected over the real wsasp / wsclient WS path,
// each backed by a fake (scripted) provider. Lets interaction tests drive a
// scripted conversation aspect -> broker -> aspect and assert on delivery +
// per-aspect turn counts WITHOUT a live network, a real LLM, or secrets.
//
// This is the layer that was missing: funnel tests call Deliberate() in
// process (no broker) and broker tests use raw WS frames (no funnel); this
// harness is where the two meet, so emergent multi-aspect behaviour (e.g.
// the shadow<->plumb echo loop) becomes a deterministic, CI-able regression.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// newInteractionBroker stands up a chat-capable in-process broker for the
// interaction harness: real ChatStore + KnowledgeStore over an in-memory
// SQLite, a per-aspect token store, and a non-nil RecipientPolicy so
// @mention routing delivers chat to connected aspects. Mirrors
// newOperatorTestServerFull but trimmed to what the harness needs.
func newInteractionBroker(t *testing.T) (*httptest.Server, *Broker) {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	b := New(Config{
		Tokens:             NewTokenStore(),
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          chat.NewSQLStore(db),
		KnowledgeStore:     knowledge.New(db, nil),
		RecipientPolicy:    &RecipientPolicy{}, // mention routing needs only a non-nil policy
	}, roster.New())

	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	return srv, b
}

// replyProvider is a bridle.Provider that emits a fixed reply text on every
// turn (up to maxCalls, after which it falls silent so an ungated echo
// storm cannot run truly unbounded inside a test). calls counts turns.
type replyProvider struct {
	reply    string
	maxCalls int32
	calls    atomic.Int32
}

func (p *replyProvider) Name() bridle.ProviderID { return "scripted" }

func (p *replyProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{Category: bridle.CategoryDirectAPI}
}

func (p *replyProvider) RunTurn(_ context.Context, _ bridle.ProviderRequest, _ bridle.EventSink) (bridle.ProviderResult, error) {
	n := p.calls.Add(1)
	if p.maxCalls > 0 && n > p.maxCalls {
		// Bound the storm: stop posting once the cap is hit.
		return bridle.ProviderResult{StopReason: bridle.StopReasonModelDone}, nil
	}
	return bridle.ProviderResult{FinalText: p.reply, StopReason: bridle.StopReasonModelDone}, nil
}

// noopToolRunner satisfies bridle.ToolRunner; never invoked because
// replyProvider emits no tool calls.
type noopToolRunner struct{}

func (noopToolRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// harnessAspect is a full funnel-driven aspect wired to the test broker.
type harnessAspect struct {
	name       string
	prov       *replyProvider
	f          *funnel.Funnel
	ws         *wsasp.Client
	deliveries atomic.Int32
}

// turns reports how many deliberation turns this aspect's provider ran.
func (a *harnessAspect) turns() int { return int(a.prov.calls.Load()) }

// delivered reports how many chat.deliver frames this aspect received.
func (a *harnessAspect) delivered() int { return int(a.deliveries.Load()) }

// newHarnessAspect mints a per-aspect token, assembles funnel + gateway +
// bridge + wsClient in the production order (cycle broken by a nil-guarded
// OnDeliver closure, mirroring runtime/cmd/agentfunnel/main.go), and starts
// the WS client plus a deliberation loop. reply is the fixed text the aspect
// posts on every turn; use "@other ..." to route replies and form a
// cross-aspect conversation. maxCalls bounds runaway loops (0 = unbounded).
//
// Filter is left nil -> funnel defaults to AlwaysPostFilter, i.e. every
// non-empty reply is posted with no judge. That is the exact condition the
// echo loop occurs under, which is what the harness needs to reproduce it.
func newHarnessAspect(t *testing.T, ctx context.Context, b *Broker, wsURL, name, reply string, maxCalls int32) *harnessAspect {
	t.Helper()
	token := name + "-tok"
	b.cfg.Tokens.SetTokenForTest(name, token, false)

	a := &harnessAspect{name: name, prov: &replyProvider{reply: reply, maxCalls: maxCalls}}

	var bridge *wsasp.Bridge // assigned after the funnel exists; see below
	wsCfg := wsasp.Config{
		URL:        wsURL,
		AuthToken:  token,
		AspectName: name,
		OnDeliver: func(msg wsasp.DeliveredMessage) {
			a.deliveries.Add(1)
			if bridge != nil {
				bridge.OnDeliver(msg)
			}
		},
		Register: schemas.RegisterRequest{
			Name:           name,
			SessionID:      name + "-session", // validateRegister requires non-empty
			ContextMode:    schemas.ContextGlobal,
			Provider:       "scripted",
			Model:          "test-model",
			PrimarySurface: schemas.SurfaceFunnel,
			StartedAt:      time.Now().UTC(),
		},
	}
	ws, err := wsasp.NewClient(wsCfg)
	if err != nil {
		t.Fatalf("%s: wsasp.NewClient: %v", name, err)
	}
	a.ws = ws

	gateway := wsasp.NewGateway(ws)
	f, err := funnel.New(funnel.Config{
		AspectID:     name,
		SystemPrompt: "harness system prompt",
		Harness:      bridle.NewHarness(a.prov),
		Provider:     "scripted",
		Model:        "test-model",
		Runner:       noopToolRunner{},
		ChatGateway:  gateway,
	})
	if err != nil {
		t.Fatalf("%s: funnel.New: %v", name, err)
	}
	a.f = f
	bridge = wsasp.NewBridge(f)

	go func() { _ = ws.Run(ctx) }()
	go a.deliberateLoop(ctx)
	return a
}

// deliberateLoop mirrors agentfunnel's deliberation loop (faster tick here
// for test responsiveness): each tick drains the inbox by running turns
// until Deliberate reports the inbox is empty.
func (a *harnessAspect) deliberateLoop(ctx context.Context) {
	tk := time.NewTicker(15 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			for i := 0; i < 16; i++ {
				if _, err := a.f.Deliberate(ctx, ""); err != nil {
					break // empty inbox (or transient) -> wait for next tick
				}
			}
		}
	}
}

// waitConnected blocks until each named aspect has a live broker connection
// (so seeded messages can be delivered), failing the test on timeout.
func waitConnected(t *testing.T, b *Broker, names ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for _, name := range names {
		for b.dispatcher.connFor(name) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("aspect %q never connected to broker", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// seed injects an operator-origin chat message into the broker (recipients
// computed from @mentions / threading in content), returning its msg id.
func seed(t *testing.T, b *Broker, content string) int64 {
	t.Helper()
	id, err := b.HandleChatSend(context.Background(), "operator", content, 0, "")
	if err != nil {
		t.Fatalf("seed HandleChatSend(%q): %v", content, err)
	}
	return id
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
