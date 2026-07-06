package broker

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// TestWorkerStatusE2E_SendBeforeRunNeverArrives reproduces the M1 Unit 5
// gap end-to-end (real wsasp.Client + real wsclient dial + real broker
// WS handler), mirroring agentfunnel main.go's actual call order: the
// boot heartbeat is Emit()'d — which calls wsasp.Client.SendWorkerStatus,
// which uses SendBestEffort — BEFORE Client.Run(ctx) has ever been
// invoked. Since Run() is what starts the dial loop, the underlying
// wsclient.Client is guaranteed Connected()==false at that call, so
// SendBestEffort's "if !Connected() return errNotConnected" guard drops
// the frame before it ever reaches the wire. This test asserts that
// exact drop: calling SendWorkerStatus pre-Run never lands a row, even
// after waiting past the point a subsequent successful register would
// complete.
func TestWorkerStatusE2E_SendBeforeRunNeverArrives(t *testing.T) {
	r := roster.New()
	store := &memWorkerStatus{}
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		WorkerStatusStore:  store,
	}, r)
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"

	c, err := wsasp.NewClient(wsasp.Config{
		URL:        wsURL,
		AuthToken:  "testtoken",
		AspectName: "e2e-agent",
		OnDeliver:  func(wsasp.DeliveredMessage) {},
		Register: schemas.RegisterRequest{
			Name:        "e2e-agent",
			SessionID:   "sess-1",
			ContextMode: "thread",
			Provider:    "claude-code",
			StartedAt:   time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("wsasp.NewClient: %v", err)
	}

	// Mirrors main.go: SendWorkerStatus called BEFORE Run(ctx) starts the
	// dial loop. Connected() is guaranteed false here.
	if err := c.SendWorkerStatus(context.Background(), frames.WorkerStatusPayload{
		Agent: "e2e-agent",
		State: "spawning",
	}); err == nil {
		t.Fatal("expected SendWorkerStatus to fail pre-Run (not connected), got nil error")
	}

	ctx, cancel := context.WithTimeout(context.Background(), brokerAsyncWait)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// Give the client every opportunity to dial + register successfully
	// (mirrors what production logs show DOES happen) before checking the
	// store — the pre-Run send above should still have produced nothing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Ready() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !c.Ready() {
		t.Fatal("client never became Ready (connected + registered) — can't distinguish the two failure modes")
	}

	rows, lerr := store.List(context.Background())
	if lerr != nil {
		t.Fatalf("store.List: %v", lerr)
	}
	if len(rows) != 0 {
		t.Fatalf("expected the pre-Run send to have been dropped (0 rows), got %d: %+v", len(rows), rows)
	}
}

// TestWorkerStatusE2E_SendAfterReadyArrives is the control case: once the
// wsasp.Client reports Ready() (connected AND registered), a
// SendWorkerStatus call must land a row. This isolates whether the gap
// is purely the pre-Run/pre-register ordering bug above, or something
// deeper in the post-registration wire path.
func TestWorkerStatusE2E_SendAfterReadyArrives(t *testing.T) {
	r := roster.New()
	store := &memWorkerStatus{}
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		WorkerStatusStore:  store,
	}, r)
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"

	c, err := wsasp.NewClient(wsasp.Config{
		URL:        wsURL,
		AuthToken:  "testtoken",
		AspectName: "e2e-agent-2",
		OnDeliver:  func(wsasp.DeliveredMessage) {},
		Register: schemas.RegisterRequest{
			Name:        "e2e-agent-2",
			SessionID:   "sess-2",
			ContextMode: "thread",
			Provider:    "claude-code",
			StartedAt:   time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("wsasp.NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), brokerAsyncWait)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Ready() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !c.Ready() {
		t.Fatal("client never became Ready (connected + registered)")
	}

	if err := c.SendWorkerStatus(context.Background(), frames.WorkerStatusPayload{
		Agent: "e2e-agent-2",
		State: "running",
	}); err != nil {
		t.Fatalf("SendWorkerStatus after Ready: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, lerr := store.List(context.Background())
		if lerr != nil {
			t.Fatalf("store.List: %v", lerr)
		}
		if len(rows) == 1 {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected exactly 1 row after Ready-gated SendWorkerStatus; never arrived")
}
