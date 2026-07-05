package workerstatus

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func newTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpsertThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	hb := time.UnixMilli(10_000)
	started := time.UnixMilli(1_000)
	expires := time.UnixMilli(20_000)

	err := s.Upsert(ctx, Status{
		Agent: "anvil", Role: "builder", Personality: "plumb", WorkItemID: "wi-1",
		State: "running", AuthOk: true, TokenExpiresAt: expires,
		Provider: "claude-code", Model: "claude-opus-4-7",
		CLIVersion: "2.1.0", ImageTag: "runner:cli-2.1.0",
		LastHeartbeat: hb, StartedAt: started, Turns: 3, TokensUsed: 4200,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "anvil")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "anvil" || got.Role != "builder" || got.WorkItemID != "wi-1" ||
		got.State != "running" || !got.AuthOk || got.Provider != "claude-code" ||
		got.Turns != 3 || got.TokensUsed != 4200 {
		t.Fatalf("after upsert: %+v", got)
	}
	if got.LastHeartbeat.UnixMilli() != 10_000 {
		t.Fatalf("last_heartbeat = %v", got.LastHeartbeat)
	}
	if got.StartedAt.UnixMilli() != 1_000 {
		t.Fatalf("started_at = %v", got.StartedAt)
	}
	if got.TokenExpiresAt.UnixMilli() != 20_000 {
		t.Fatalf("token_expires_at = %v", got.TokenExpiresAt)
	}
}

// TestUpsertUpdatesInPlaceAndKeepsStartedAt covers the heartbeat
// re-emission path: a worker upserts its status repeatedly across its
// lifetime (boot, each turn boundary, each ~60s tick). started_at must
// stay pinned to the FIRST report even though later heartbeats don't
// resend it (funnel emits StartedAt once at boot and zero thereafter is
// not the real intent — but the store defends the invariant either way:
// a zero incoming started_at never clobbers a previously recorded one).
func TestUpsertUpdatesInPlaceAndKeepsStartedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	started := time.UnixMilli(1_000)

	if err := s.Upsert(ctx, Status{
		Agent: "anvil", State: "spawning", StartedAt: started,
		LastHeartbeat: time.UnixMilli(1_000), Turns: 0,
	}); err != nil {
		t.Fatal(err)
	}
	// Later heartbeat: state advances, turns increments, StartedAt
	// omitted (zero value) — must not stomp the recorded started_at.
	if err := s.Upsert(ctx, Status{
		Agent: "anvil", State: "running",
		LastHeartbeat: time.UnixMilli(60_000), Turns: 2, TokensUsed: 900,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "anvil")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "running" || got.Turns != 2 || got.TokensUsed != 900 {
		t.Fatalf("after second upsert: %+v", got)
	}
	if got.StartedAt.UnixMilli() != 1_000 {
		t.Fatalf("started_at clobbered: got %v, want 1000ms", got.StartedAt)
	}
	if got.LastHeartbeat.UnixMilli() != 60_000 {
		t.Fatalf("last_heartbeat = %v", got.LastHeartbeat)
	}

	rows, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("List rows = %d, want 1 (upsert must replace, not duplicate)", len(rows))
	}
}

func TestListOrdersMostRecentHeartbeatFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Upsert(ctx, Status{Agent: "old", State: "running", LastHeartbeat: time.UnixMilli(1_000)})
	_ = s.Upsert(ctx, Status{Agent: "new", State: "running", LastHeartbeat: time.UnixMilli(3_000)})
	_ = s.Upsert(ctx, Status{Agent: "mid", State: "running", LastHeartbeat: time.UnixMilli(2_000)})

	got, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Agent != "new" || got[1].Agent != "mid" || got[2].Agent != "old" {
		t.Fatalf("List order = %+v, want new, mid, old", got)
	}
}

func TestDeleteRemovesRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Upsert(ctx, Status{Agent: "anvil", State: "done", LastHeartbeat: time.UnixMilli(1)})
	if err := s.Delete(ctx, "anvil"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after delete = %+v, want empty", rows)
	}
}

func TestStaleDetection(t *testing.T) {
	now := time.UnixMilli(100_000)
	fresh := Status{LastHeartbeat: time.UnixMilli(90_000)} // 10s old
	stale := Status{LastHeartbeat: time.UnixMilli(1_000)}  // ~99s old
	never := Status{}

	if fresh.Stale(now, 60*time.Second) {
		t.Error("fresh status reported stale")
	}
	if !stale.Stale(now, 60*time.Second) {
		t.Error("stale status reported fresh")
	}
	if !never.Stale(now, 60*time.Second) {
		t.Error("zero LastHeartbeat must always be stale")
	}
}
