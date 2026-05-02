package chat

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nexus-cw/nexus/nexus/storage"
)

// openTestStore boots a fresh SQLite under t.TempDir + storage
// schema, returning a SQLStore ready for use. Mirrors the
// storage_test pattern.
func openTestStore(t *testing.T) *SQLStore {
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
	return NewSQLStore(db)
}

func TestSQLStore_InsertAndRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	msg, err := s.Insert(ctx, "operator", "hello world", 0, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if msg.ID == 0 {
		t.Error("inserted message should have non-zero id")
	}
	if msg.From != "operator" {
		t.Errorf("from: got %q, want operator", msg.From)
	}
	if msg.Content != "hello world" {
		t.Errorf("content: got %q", msg.Content)
	}
	if msg.Kind != "chat" {
		t.Errorf("kind: got %q, want chat", msg.Kind)
	}
	if msg.CreatedAt.IsZero() {
		t.Error("created_at should be server-stamped")
	}
	if msg.ReplyTo != 0 {
		t.Errorf("reply_to should be 0 for top-level: got %d", msg.ReplyTo)
	}
	if msg.Topic != "" {
		t.Errorf("topic should be empty for default: got %q", msg.Topic)
	}
}

func TestSQLStore_InsertRejectsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.Insert(ctx, "", "content", 0, "")
	if !errors.Is(err, ErrEmptyFrom) {
		t.Errorf("expected ErrEmptyFrom: got %v", err)
	}
	_, err = s.Insert(ctx, "operator", "", 0, "")
	if !errors.Is(err, ErrEmptyContent) {
		t.Errorf("expected ErrEmptyContent: got %v", err)
	}
}

func TestSQLStore_InsertWithReplyTo(t *testing.T) {
	// Topic is currently a no-op pass-through (see Insert comment);
	// once topics get a backing table or column this test will assert
	// round-trip. For now: validate reply_to wires correctly.
	s := openTestStore(t)
	ctx := context.Background()

	parent, err := s.Insert(ctx, "operator", "first", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	reply, err := s.Insert(ctx, "anvil", "second", parent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if reply.ReplyTo != parent.ID {
		t.Errorf("reply_to: got %d, want %d", reply.ReplyTo, parent.ID)
	}
	if reply.ID <= parent.ID {
		t.Errorf("ids should be monotonic: parent=%d reply=%d", parent.ID, reply.ID)
	}
}

func TestSQLStore_FormatRFC3339(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	msg, err := s.Insert(ctx, "operator", "ping", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	formatted := msg.FormatRFC3339()
	// SQLite second-precision: 2026-05-02T05:30:00Z (no fractional)
	if len(formatted) < 20 || formatted[len(formatted)-1] != 'Z' {
		t.Errorf("expected RFC 3339 UTC with trailing Z: got %q", formatted)
	}
	if formatted[10] != 'T' {
		t.Errorf("expected RFC 3339 'T' separator at index 10: got %q", formatted)
	}
}

func TestSQLStore_ListThreadReturnsRoot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	root, err := s.Insert(ctx, "operator", "thread root", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListThread(ctx, root.ID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (the root), got %d", len(msgs))
	}
	if msgs[0].ID != root.ID {
		t.Errorf("root id mismatch: got %d, want %d", msgs[0].ID, root.ID)
	}
}

func TestSQLStore_ListThreadIncludesReplies(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	root, _ := s.Insert(ctx, "operator", "start", 0, "")
	r1, _ := s.Insert(ctx, "anvil", "reply 1", root.ID, "")
	r2, _ := s.Insert(ctx, "wren", "reply 2", root.ID, "")
	rNested, _ := s.Insert(ctx, "harrow", "deep reply", r1.ID, "")
	// Unrelated message in different thread; must NOT appear
	other, _ := s.Insert(ctx, "operator", "other thread", 0, "")
	_ = other

	msgs, err := s.ListThread(ctx, root.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (root + 3 descendants), got %d", len(msgs))
	}
	wantIDs := map[int64]bool{root.ID: true, r1.ID: true, r2.ID: true, rNested.ID: true}
	for _, m := range msgs {
		if !wantIDs[m.ID] {
			t.Errorf("unexpected msg in thread: id=%d from=%s", m.ID, m.From)
		}
	}
	// Should be ordered oldest-first
	for i := 1; i < len(msgs); i++ {
		if msgs[i].ID < msgs[i-1].ID {
			t.Errorf("thread should be id-ascending: idx %d=%d < idx %d=%d", i, msgs[i].ID, i-1, msgs[i-1].ID)
		}
	}
}

func TestSQLStore_ListThreadSinceID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	root, _ := s.Insert(ctx, "operator", "root", 0, "")
	r1, _ := s.Insert(ctx, "anvil", "r1", root.ID, "")
	r2, _ := s.Insert(ctx, "wren", "r2", root.ID, "")
	_ = r1

	msgs, err := s.ListThread(ctx, root.ID, r1.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 (only r2 > r1), got %d", len(msgs))
	}
	if msgs[0].ID != r2.ID {
		t.Errorf("got id=%d, want r2 id=%d", msgs[0].ID, r2.ID)
	}
}

func TestSQLStore_ListThreadLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	root, _ := s.Insert(ctx, "operator", "root", 0, "")
	for i := 0; i < 10; i++ {
		s.Insert(ctx, "anvil", "reply", root.ID, "")
	}

	msgs, err := s.ListThread(ctx, root.ID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Errorf("limit=5 should cap at 5: got %d", len(msgs))
	}
}

func TestSQLStore_ListThreadRejectsZeroID(t *testing.T) {
	s := openTestStore(t)
	_, err := s.ListThread(context.Background(), 0, 0, 0)
	if err == nil {
		t.Error("zero thread_id should error")
	}
}

func TestSQLStore_ListThreadEmptyResult(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Non-existent thread id
	msgs, err := s.ListThread(ctx, 99999, 0, 0)
	if err != nil {
		t.Fatalf("non-existent thread should not error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected empty result, got %d", len(msgs))
	}
}
