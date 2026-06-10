package broker

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	"github.com/coder/websocket"
)

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
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		Runner:             runner,
	}, roster.New())
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
		{Name: "plumb.sub-2"}, // queued: accepted, no RunID yet
	}}
	srv := newSpawnTestServer(t, fr)
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")

	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X", Count: 2, Thread: "NEX-571"})
	if resp.Kind != frames.KindSpawnResult {
		t.Fatalf("kind = %q, want %q", resp.Kind, frames.KindSpawnResult)
	}
	var sp frames.SpawnResultPayload
	if err := frames.PayloadAs(resp, &sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.Hands) != 2 || sp.Hands[0].RunID != "run-1" || sp.Hands[1].Name != "plumb.sub-2" {
		t.Fatalf("hands = %+v", sp.Hands)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.calls != 1 || fr.parent != "plumb" || fr.brief != "do X" || fr.count != 2 || fr.thread != "NEX-571" {
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
	// No register frame: connection has no aspect identity.
	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	spawnErrorOf(t, resp)
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

func TestSpawnRequestWithoutSpawnCapableRunner(t *testing.T) {
	srv := newSpawnTestServer(t, plainSubmitter{})
	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb")
	resp := sendSpawn(t, c, frames.SpawnRequestPayload{Brief: "do X"})
	spawnErrorOf(t, resp)
}
