package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func openTestStore(t *testing.T) (*SQLStore, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), filepath.Join(dir), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := storage.Bootstrap(context.Background(), db); err != nil {
		db.Close()
		t.Fatalf("storage.Bootstrap: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSQLStore(db), db
}

// insertChatMsg helper — usage rows FK to chat_messages, so tests
// that exercise the join need a real chat row to point at.
func insertChatMsg(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO chat_messages (from_agent, content, kind) VALUES ('operator', 'test', 'chat')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestSQLStore_RecordRoundTrip(t *testing.T) {
	s, db := openTestStore(t)
	msgID := insertChatMsg(t, db)
	r, err := s.Record(context.Background(), Record{
		MsgID: msgID, TurnID: "t-1", AspectID: "frame", Model: "claude-opus-4",
		InputTokens: 1000, OutputTokens: 200,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if r.ID == 0 {
		t.Error("expected non-zero id")
	}
	if r.MsgID != msgID || r.InputTokens != 1000 || r.OutputTokens != 200 {
		t.Errorf("round-trip drift: %+v", r)
	}
	if r.RecordedAt.IsZero() {
		t.Error("recorded_at should be server-stamped")
	}
}

func TestSQLStore_RecordValidation(t *testing.T) {
	s, db := openTestStore(t); _ = db
	ctx := context.Background()
	cases := []struct {
		name string
		r    Record
	}{
		{"missing TurnID", Record{AspectID: "f", Model: "m"}},
		{"missing AspectID", Record{TurnID: "t", Model: "m"}},
		{"missing Model", Record{TurnID: "t", AspectID: "f"}},
		{"negative input", Record{TurnID: "t", AspectID: "f", Model: "m", InputTokens: -1}},
		{"negative output", Record{TurnID: "t", AspectID: "f", Model: "m", OutputTokens: -5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Record(ctx, c.r)
			if err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestSQLStore_RecordWithoutMsgID(t *testing.T) {
	// Internal turns (compaction summarize) record with MsgID=0 —
	// stored as NULL, retrievable via ListByAspect.
	s, db := openTestStore(t); _ = db
	r, err := s.Record(context.Background(), Record{
		TurnID: "compact-1", AspectID: "frame", Model: "claude-haiku",
		InputTokens: 100, OutputTokens: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.MsgID != 0 {
		t.Errorf("MsgID should be 0 for non-comms turn: got %d", r.MsgID)
	}
}

func TestSQLStore_ListByMsg(t *testing.T) {
	s, db := openTestStore(t)
	ctx := context.Background()
	m1 := insertChatMsg(t, db)
	m2 := insertChatMsg(t, db)
	s.Record(ctx, Record{MsgID: m1, TurnID: "t-1", AspectID: "frame", Model: "m", InputTokens: 100, OutputTokens: 20})
	s.Record(ctx, Record{MsgID: m1, TurnID: "t-2", AspectID: "frame", Model: "m", InputTokens: 200, OutputTokens: 30}) // multi-turn deliberation
	s.Record(ctx, Record{MsgID: m2, TurnID: "t-3", AspectID: "frame", Model: "m", InputTokens: 50, OutputTokens: 10})

	got, err := s.ListByMsg(ctx, m1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 rows for msg %d, got %d", m1, len(got))
	}
}

func TestSQLStore_SumByAspect(t *testing.T) {
	s, db := openTestStore(t); _ = db
	ctx := context.Background()
	s.Record(ctx, Record{TurnID: "t-1", AspectID: "frame", Model: "m", InputTokens: 100, OutputTokens: 20})
	s.Record(ctx, Record{TurnID: "t-2", AspectID: "frame", Model: "m", InputTokens: 200, OutputTokens: 50})
	s.Record(ctx, Record{TurnID: "t-3", AspectID: "anvil", Model: "m", InputTokens: 999, OutputTokens: 999})

	in, out, err := s.SumByAspect(ctx, "frame", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if in != 300 || out != 70 {
		t.Errorf("frame totals: got input=%d output=%d, want 300/70", in, out)
	}
}

func TestSQLStore_SumByAspectEmpty(t *testing.T) {
	s, db := openTestStore(t); _ = db
	in, out, err := s.SumByAspect(context.Background(), "noone", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if in != 0 || out != 0 {
		t.Errorf("nonexistent aspect should sum to 0/0: got %d/%d", in, out)
	}
}
