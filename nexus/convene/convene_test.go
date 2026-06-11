package convene

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
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewSQLStore(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestInsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	c := Convene{
		ConveneID:    "cv-1",
		RootMsgID:    42,
		Facilitator:  "shadow",
		Participants: []string{"plumb", "anvil"},
		Problem:      "should bridle adopt a registry?",
		Status:       StatusOpen,
		CreatedAt:    now,
	}
	if err := s.Insert(ctx, c); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Get(ctx, "cv-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Facilitator != "shadow" || got.RootMsgID != 42 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.Participants) != 2 || got.Participants[0] != "plumb" || got.Participants[1] != "anvil" {
		t.Errorf("participants = %v, want [plumb anvil]", got.Participants)
	}
	if got.Status != StatusOpen {
		t.Errorf("status = %q, want open", got.Status)
	}
	if got.Problem != c.Problem {
		t.Errorf("problem = %q", got.Problem)
	}
}

func TestInsertDefaultsStatusOpen(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Insert(ctx, Convene{ConveneID: "cv-2", Facilitator: "shadow", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, _ := s.Get(ctx, "cv-2")
	if got.Status != StatusOpen {
		t.Errorf("status = %q, want open (defaulted)", got.Status)
	}
}

func TestCloseConverged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Insert(ctx, Convene{ConveneID: "cv-3", Facilitator: "shadow", Status: StatusOpen, CreatedAt: time.Now()})
	closedAt := time.Now().Truncate(time.Millisecond)
	if err := s.Close(ctx, "cv-3", StatusConverged, closedAt, 99); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, _ := s.Get(ctx, "cv-3")
	if got.Status != StatusConverged {
		t.Errorf("status = %q, want converged", got.Status)
	}
	if got.SummaryMsgID != 99 {
		t.Errorf("summary_msg_id = %d, want 99", got.SummaryMsgID)
	}
	if got.ClosedAt.IsZero() {
		t.Error("closed_at not set")
	}
}

func TestCloseIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Insert(ctx, Convene{ConveneID: "cv-4", Facilitator: "shadow", Status: StatusOpen, CreatedAt: time.Now()})
	if err := s.Close(ctx, "cv-4", StatusConverged, time.Now(), 10); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second close with a different status must NOT flip the terminal state.
	if err := s.Close(ctx, "cv-4", StatusAbandoned, time.Now(), 20); err != nil {
		t.Fatalf("second close: %v", err)
	}
	got, _ := s.Get(ctx, "cv-4")
	if got.Status != StatusConverged {
		t.Errorf("status = %q, want converged (second close ignored)", got.Status)
	}
	if got.SummaryMsgID != 10 {
		t.Errorf("summary_msg_id = %d, want 10 (not overwritten)", got.SummaryMsgID)
	}
}

func TestListRecentFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now()
	_ = s.Insert(ctx, Convene{ConveneID: "old", Facilitator: "shadow", Status: StatusOpen, CreatedAt: base})
	_ = s.Insert(ctx, Convene{ConveneID: "new", Facilitator: "shadow", Status: StatusOpen, CreatedAt: base.Add(time.Minute)})
	list, err := s.List(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].ConveneID != "new" {
		t.Errorf("first = %q, want new (recent first)", list[0].ConveneID)
	}
}
