package broker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

// memWorkerStatus is an in-memory workerstatus.Store fake, mirroring
// memRuns in runs_adapter_test.go.
type memWorkerStatus struct {
	rows map[string]workerstatus.Status
}

func (m *memWorkerStatus) Migrate(context.Context) error { return nil }

func (m *memWorkerStatus) Upsert(_ context.Context, s workerstatus.Status) error {
	if m.rows == nil {
		m.rows = map[string]workerstatus.Status{}
	}
	m.rows[s.Agent] = s
	return nil
}

func (m *memWorkerStatus) Get(_ context.Context, agent string) (workerstatus.Status, error) {
	return m.rows[agent], nil
}

func (m *memWorkerStatus) List(context.Context) ([]workerstatus.Status, error) {
	var out []workerstatus.Status
	for _, s := range m.rows {
		out = append(out, s)
	}
	return out, nil
}

func (m *memWorkerStatus) Delete(_ context.Context, agent string) error {
	delete(m.rows, agent)
	return nil
}

func TestWorkerStatusFrameUpsertsRow(t *testing.T) {
	store := &memWorkerStatus{}
	b := New(Config{WorkerStatusStore: store}, roster.New())
	c := &wsConn{broker: b, registeredAs: "anvil", log: slog.Default()}

	env, err := frames.New(frames.KindWorkerStatus, frames.WorkerStatusPayload{
		Agent: "anvil", Role: "builder", WorkItemID: "wi-1", State: "running",
		AuthOk: true, Provider: "claude-code", Model: "claude-opus-4-7",
		LastHeartbeat: time.UnixMilli(5000), Turns: 2, TokensUsed: 800,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.handleWorkerStatusFrame(env)

	got := store.rows["anvil"]
	if got.Agent != "anvil" || got.Role != "builder" || got.WorkItemID != "wi-1" ||
		got.State != "running" || !got.AuthOk || got.Turns != 2 || got.TokensUsed != 800 {
		t.Fatalf("row after worker.status frame = %+v", got)
	}
	if got.LastHeartbeat.UnixMilli() != 5000 {
		t.Fatalf("last_heartbeat = %v", got.LastHeartbeat)
	}
}

// TestWorkerStatusFrameAttributesToConnectionIdentity confirms a worker
// cannot forge another worker's status row via payload.Agent — the
// connection's authenticated registeredAs always wins (same posture as
// observe.* frames, per keel-cli's caveat #236).
func TestWorkerStatusFrameAttributesToConnectionIdentity(t *testing.T) {
	store := &memWorkerStatus{}
	b := New(Config{WorkerStatusStore: store}, roster.New())
	c := &wsConn{broker: b, registeredAs: "anvil", log: slog.Default()}

	env, err := frames.New(frames.KindWorkerStatus, frames.WorkerStatusPayload{
		Agent: "someone-else", State: "running", LastHeartbeat: time.UnixMilli(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.handleWorkerStatusFrame(env)

	if _, ok := store.rows["someone-else"]; ok {
		t.Fatal("payload.Agent spoofed a row under a different identity")
	}
	if got := store.rows["anvil"]; got.Agent != "anvil" {
		t.Fatalf("row not attributed to registeredAs: %+v", got)
	}
}

func TestWorkerStatusFrameDroppedWhenUnregistered(t *testing.T) {
	store := &memWorkerStatus{}
	b := New(Config{WorkerStatusStore: store}, roster.New())
	c := &wsConn{broker: b, registeredAs: "", log: slog.Default()}

	env, err := frames.New(frames.KindWorkerStatus, frames.WorkerStatusPayload{
		Agent: "anvil", State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.handleWorkerStatusFrame(env)

	if len(store.rows) != 0 {
		t.Fatalf("expected no row from an unregistered connection, got %+v", store.rows)
	}
}

func TestWorkerStatusFrameNoStoreConfiguredIsNoop(t *testing.T) {
	b := New(Config{}, roster.New())
	c := &wsConn{broker: b, registeredAs: "anvil", log: slog.Default()}

	env, err := frames.New(frames.KindWorkerStatus, frames.WorkerStatusPayload{
		Agent: "anvil", State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Must not panic when WorkerStatusStore is nil.
	c.handleWorkerStatusFrame(env)
}

func TestWorkerStatusFrameMalformedPayloadDropped(t *testing.T) {
	store := &memWorkerStatus{}
	b := New(Config{WorkerStatusStore: store}, roster.New())
	c := &wsConn{broker: b, registeredAs: "anvil", log: slog.Default()}

	env := frames.Envelope{Kind: frames.KindWorkerStatus} // no payload at all
	c.handleWorkerStatusFrame(env)

	if len(store.rows) != 0 {
		t.Fatalf("expected no row from malformed frame, got %+v", store.rows)
	}
}
