package broker

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	"github.com/coder/websocket"
)

// spawnTestSigningSecret signs aspect session JWTs in the spawn tests,
// matching the broker's SessionSigningSecret so tryVerifyAspectJWT
// accepts them on the WS upgrade.
var spawnTestSigningSecret = []byte("test-secret-32-bytes-padding-spwn")

// signSpawnAspectJWT mints an aspect session JWT for name, signed with
// spawnTestSigningSecret — the credential shape a real aspect presents
// on /connect after keyfile validate.
func signSpawnAspectJWT(t *testing.T, name string) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.Sign(spawnTestSigningSecret, jwt.Claims{
		Sub: name,
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign aspect jwt: %v", err)
	}
	return tok
}

// fakeSpawnRunner satisfies dispatch.Submitter AND broker.SpawnSubmitter,
// recording the SubmitSpawn call the handler routes to it.
type fakeSpawnRunner struct {
	mu      sync.Mutex
	parent  string
	brief   string
	count   int
	thread  string
	calls   int
	handles []dispatch.SpawnHandle
	err     error
}

func (f *fakeSpawnRunner) Submit(context.Context, dispatch.Brief) (string, error) { return "", nil }

func (f *fakeSpawnRunner) SubmitSpawn(_ context.Context, parent, brief string, count int, thread string) ([]dispatch.SpawnHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.parent, f.brief, f.count, f.thread = parent, brief, count, thread
	return f.handles, f.err
}

// plainSubmitter satisfies only dispatch.Submitter — a broker wired
// with it has no spawn capability.
type plainSubmitter struct{}

func (plainSubmitter) Submit(context.Context, dispatch.Brief) (string, error) { return "", nil }

func newSpawnTestServer(t *testing.T, runner dispatch.Submitter) *httptest.Server {
	t.Helper()
	return newSpawnTestServerCfg(t, runner, nil)
}

// newSpawnTestServerCfg builds the spawn test broker, letting a test
// tweak the Config (auth bypass, etc.) before boot.
func newSpawnTestServerCfg(t *testing.T, runner dispatch.Submitter, mutate func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Config{
		AuthToken:            "testtoken",
		AllowLegacyMaster:    true,
		SessionSigningSecret: spawnTestSigningSecret,
		HeartbeatIntervalS:   15,
		StaleAfter:           30 * time.Second,
		Runner:               runner,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b := New(cfg, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	return srv
}

func sendSpawn(t *testing.T, c *websocket.Conn, p frames.SpawnRequestPayload) frames.Envelope {
	t.Helper()
	env, err := frames.NewRequest(frames.KindSpawnRequest, p)
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	resp := recvFrame(t, c)
	if resp.InReplyTo != env.ID {
		t.Fatalf("response InReplyTo = %q, want %q", resp.InReplyTo, env.ID)
	}
	return resp
}

func spawnErrorOf(t *testing.T, resp frames.Envelope) string {
	t.Helper()
	if resp.Kind != frames.Kind("spawn.request.error") {
		t.Fatalf("kind = %q, want spawn.request.error", resp.Kind)
	}
	var body map[string]string
	if err := frames.PayloadAs(resp, &body); err != nil {
		t.Fatal(err)
	}
	return body["error"]
}

func TestSpawnRequestReturnsHandles(t *testing.T) {
	fr := &fakeSpawnRunner{handles: []dispatch.SpawnHandle{
		{RunID: "run-1", Name: "plumb.sub-1"},
		{Name: "plumb.sub-2"},                                     // queued: accepted, no RunID yet
		{RunID: "run-3", Name: "plumb.sub-3", Error: "mint boom"}, // launch failed
	}}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X", Count: 3, Thread: "NEX-571"})
	if resp.Kind != frames.KindSpawnResult {
		t.Fatalf("kind = %q, want %q", resp.Kind, frames.KindSpawnResult)
	}
	var sp frames.SpawnResultPayload
	if err := frames.PayloadAs(resp, &sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.Hands) != 3 || sp.Hands[0].RunID != "run-1" || sp.Hands[1].Name != "plumb.sub-2" {
		t.Fatalf("hands = %+v", sp.Hands)
	}
	if sp.Hands[2].Error != "mint boom" {
		t.Fatalf("failed hand's Error must relay to the requester, hands = %+v", sp.Hands)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 1 || fr.parent != "plumb" || fr.brief != "do X" || fr.count != 3 || fr.thread != "NEX-571" {
		t.Fatalf("SubmitSpawn got parent=%q brief=%q count=%d thread=%q calls=%d",
			fr.parent, fr.brief, fr.count, fr.thread, fr.calls)
	}
}

func TestSpawnRequestDefaultsCountToOne(t *testing.T) {
	fr := &fakeSpawnRunner{handles: []dispatch.SpawnHandle{{RunID: "run-1", Name: "plumb.sub-1"}}}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	if resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"}); resp.Kind != frames.KindSpawnResult {
		t.Fatalf("kind = %q", resp.Kind)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.count != 1 {
		t.Fatalf("count = %d, want default 1", fr.count)
	}
}

func TestSpawnRequestRejectsEmptyBrief(t *testing.T) {
	fr := &fakeSpawnRunner{}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "   ", Count: 1})
	if msg := spawnErrorOf(t, resp); msg == "" {
		t.Fatal("expected error message")
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 0 {
		t.Fatal("SubmitSpawn must not be called for an empty brief")
	}
}

func TestSpawnRequestRejectsCountOverMax(t *testing.T) {
	fr := &fakeSpawnRunner{}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X", Count: 5})
	spawnErrorOf(t, resp) // default SpawnMaxPerRequest is 4
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 0 {
		t.Fatal("SubmitSpawn must not be called when count exceeds the cap")
	}
}

func TestSpawnRequestRequiresRegisteredAspect(t *testing.T) {
	fr := &fakeSpawnRunner{}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	// No register frame: connection has no aspect identity (the legacy
	// master token resolves to AgentID "operator", which never spawns).
	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	spawnErrorOf(t, resp)
}

// NEX-609: an authenticated-but-UNREGISTERED aspect connection spawns
// as its JWT-verified identity. This is the comms-sidecar shape: inside
// a pod aspect, agentfunnel owns the one-session-per-name registration
// slot, so nexus-comms-mcp connects with the aspect's session JWT and
// -register=false — the JWT sub is the same credential registration
// would have bound.
func TestSpawnRequestAllowsUnregisteredAuthenticatedAspect(t *testing.T) {
	fr := &fakeSpawnRunner{handles: []dispatch.SpawnHandle{{RunID: "run-1", Name: "harrow.tine"}}}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, signSpawnAspectJWT(t, "harrow"))
	// No register frame — the JWT identity alone vouches for the parent.
	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	if resp.Kind != frames.KindSpawnResult {
		t.Fatalf("kind = %q, want %q", resp.Kind, frames.KindSpawnResult)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 1 || fr.parent != "harrow" {
		t.Fatalf("SubmitSpawn calls=%d parent=%q, want 1/harrow", fr.calls, fr.parent)
	}
}

// The unregistered fallback must not open spawn to hands: a connection
// authenticated with a DERIVED identity's JWT (a hand's comms-mcp) is
// still rejected — no sub-of-sub.
func TestSpawnRequestUnregisteredDerivedJWTStillRejected(t *testing.T) {
	fr := &fakeSpawnRunner{}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, signSpawnAspectJWT(t, "harrow.tine"))
	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	if msg := spawnErrorOf(t, resp); !strings.Contains(msg, "sub-of-sub") {
		t.Fatalf("error = %q, want a no-sub-of-sub rejection", msg)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 0 {
		t.Fatal("a hand's unregistered connection must not spawn")
	}
}

func TestSpawnRequestRejectsDerivedParent(t *testing.T) {
	fr := &fakeSpawnRunner{}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb.sub-1")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	spawnErrorOf(t, resp)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 0 {
		t.Fatal("a hand must not be able to spawn hands (no sub-of-sub)")
	}
}

// S1: the spawn parent is bound to the CONNECTION'S authenticated
// identity, not just the registered name. A connection that
// authenticated as plumb (aspect session JWT, sub=plumb) but registered
// as anvil must not spawn hands of anvil.
func TestSpawnRequestRejectsAuthIdentityMismatch(t *testing.T) {
	fr := &fakeSpawnRunner{}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, signSpawnAspectJWT(t, "plumb"))
	registerAspect(t, c, "anvil")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	if msg := spawnErrorOf(t, resp); !strings.Contains(msg, "spawn identity mismatch") {
		t.Fatalf("error = %q, want a spawn identity mismatch rejection", msg)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 0 {
		t.Fatal("SubmitSpawn must not be called on an auth/registration identity mismatch")
	}
}

// The matching case: JWT sub == registered name spawns normally.
func TestSpawnRequestAllowsMatchingAuthIdentity(t *testing.T) {
	fr := &fakeSpawnRunner{handles: []dispatch.SpawnHandle{{RunID: "run-1", Name: "plumb.sub-1"}}}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, signSpawnAspectJWT(t, "plumb"))
	registerAspect(t, c, "plumb")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	if resp.Kind != frames.KindSpawnResult {
		t.Fatalf("kind = %q, want %q", resp.Kind, frames.KindSpawnResult)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 1 || fr.parent != "plumb" {
		t.Fatalf("SubmitSpawn calls=%d parent=%q, want 1/plumb", fr.calls, fr.parent)
	}
}

// Auth-bypass mode (no verifiable token) keeps working: the bypass
// identity is admin, and admin connections — like the legacy master
// used by the other tests in this file — are exempt from the identity
// bind, per the deregister/dispatch handler convention.
func TestSpawnRequestBypassModeStillSpawns(t *testing.T) {
	fr := &fakeSpawnRunner{handles: []dispatch.SpawnHandle{{RunID: "run-1", Name: "plumb.sub-1"}}}
	srv := newSpawnTestServerCfg(t, fr, func(cfg *Config) {
		cfg.OperatorAuthBypass = true
	})
	// Token resolves nowhere → bypass accepts the connection.
	c := dialWS(t, srv, "not-a-real-token")
	registerAspect(t, c, "plumb")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	if resp.Kind != frames.KindSpawnResult {
		t.Fatalf("kind = %q, want %q (bypass mode must keep spawning)", resp.Kind, frames.KindSpawnResult)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 1 || fr.parent != "plumb" {
		t.Fatalf("SubmitSpawn calls=%d parent=%q, want 1/plumb", fr.calls, fr.parent)
	}
}

func TestSpawnRequestWithoutSpawnCapableRunner(t *testing.T) {
	srv := newSpawnTestServer(t, plainSubmitter{})
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")
	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	spawnErrorOf(t, resp)
}
