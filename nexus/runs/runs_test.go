package runs

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

func TestInsertThenMarkDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	start := time.UnixMilli(1_000)

	err := s.Insert(ctx, Run{
		RunID: "run-abc", Ticket: "NEX-1", Agent: "anvil", Thread: "NEX-1",
		DispatchMsgID: 42, Command: "do the thing", Repo: "org/repo",
		Status: StatusRunning, StartedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || got.Agent != "anvil" || got.DispatchMsgID != 42 {
		t.Fatalf("after insert: %+v", got)
	}

	done := time.UnixMilli(5_000)
	if err := s.MarkDone(ctx, "run-abc", StatusComplete, done, "https://pr/1", 4); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "run-abc")
	if got.Status != StatusComplete || got.PRURL != "https://pr/1" || got.DurationSecs != 4 {
		t.Fatalf("after done: %+v", got)
	}
	if got.CompletedAt.UnixMilli() != 5_000 {
		t.Fatalf("completed_at = %v", got.CompletedAt)
	}
}

func TestMarkDoneDoesNotOverwriteTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Insert(ctx, Run{RunID: "run-c", Ticket: "NEX-1", Agent: "anvil", Status: StatusRunning, StartedAt: time.UnixMilli(1)})
	if err := s.MarkDone(ctx, "run-c", StatusCancelled, time.UnixMilli(2), "", 0); err != nil {
		t.Fatal(err)
	}
	// a later failed-mark (from emitJobDeleted) must NOT overwrite cancelled
	if err := s.MarkDone(ctx, "run-c", StatusFailed, time.UnixMilli(3), "", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "run-c")
	if got.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled (terminal not overwritten)", got.Status)
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i, id := range []string{"run-1", "run-2", "run-3"} {
		_ = s.Insert(ctx, Run{RunID: id, Ticket: id, Agent: "anvil",
			Status: StatusRunning, StartedAt: time.UnixMilli(int64(i + 1))})
	}
	got, err := s.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].RunID != "run-3" {
		t.Fatalf("list newest-first: %+v", got)
	}
}
