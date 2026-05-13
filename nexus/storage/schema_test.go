package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesAndBootstraps(t *testing.T) {
	dir := t.TempDir()

	ctx := context.Background()
	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	expectedTables := []string{
		"knowledge", "knowledge_fts", "threads", "chat_messages",
		"chat_reactions", "shared_files", "chat_usage",
		"tickets", "activity", "schema_meta",
	}
	for _, tbl := range expectedTables {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE (type='table' OR type='virtual') AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after first open: %v", tbl, err)
		}
	}

	var version string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM schema_meta WHERE key='version'`,
	).Scan(&version); err != nil {
		t.Fatalf("schema_meta lookup: %v", err)
	}
	if version != "1" {
		t.Errorf("schema version = %q, want 1", version)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO knowledge(from_agent, topic, content) VALUES (?, ?, ?)`,
		"keel", "test-entry", "broker restart sequence: stop aspects first, broker last",
	); err != nil {
		t.Fatalf("insert knowledge: %v", err)
	}

	var hit string
	err = db.QueryRowContext(ctx,
		`SELECT content FROM knowledge_fts WHERE knowledge_fts MATCH 'restart'`,
	).Scan(&hit)
	if err != nil {
		t.Errorf("fts match 'restart' failed: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	dbPath := filepath.Join(dir, DBFileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("nexus.db missing after close: %v", err)
	}

	db2, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM knowledge WHERE from_agent='keel' AND topic='test-entry'`,
	).Scan(&count); err != nil {
		t.Fatalf("re-read count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count after reopen = %d, want 1 (idempotent bootstrap must preserve data)", count)
	}
}

func TestDefaultDataDirFromEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEXUS_DATA_DIR", dir)

	ctx := context.Background()
	db, err := Open(ctx, "", nil)
	if err != nil {
		t.Fatalf("open with env: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(filepath.Join(dir, DBFileName)); err != nil {
		t.Errorf("expected %s in env-specified dir: %v", DBFileName, err)
	}
}

func TestFTSTriggersOnUpdateAndDelete(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO knowledge(from_agent, topic, content) VALUES ('keel', 'trigger-test', 'original content about ships')`,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE knowledge SET content='revised content about boats' WHERE topic='trigger-test'`,
	); err != nil {
		t.Fatalf("update: %v", err)
	}

	var origHits int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM knowledge_fts WHERE knowledge_fts MATCH 'ships'`,
	).Scan(&origHits)
	if origHits != 0 {
		t.Errorf("FTS still matches old content 'ships' after update: %d hits", origHits)
	}

	var newHits int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM knowledge_fts WHERE knowledge_fts MATCH 'boats'`,
	).Scan(&newHits)
	if newHits != 1 {
		t.Errorf("FTS did not pick up new content 'boats' after update: %d hits", newHits)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM knowledge WHERE topic='trigger-test'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var afterDelete int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM knowledge_fts WHERE knowledge_fts MATCH 'boats'`,
	).Scan(&afterDelete)
	if afterDelete != 0 {
		t.Errorf("FTS still has row after DELETE: %d hits", afterDelete)
	}
}

// TestBackfillChatThreadRoots verifies #226.1's linked-list backfill:
// top-level msgs get thread_root = own id (parent stays NULL); replies
// walk the reply_to chain to find their root and set parent_msg_id to
// their immediate reply_to ancestor.
func TestBackfillChatThreadRoots(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Seed: 5 messages forming two threads.
	//   #1 (root A) ← #2 ← #3
	//   #4 (root B) ← #5
	// Insert directly without thread_root_msg_id to simulate pre-#226
	// data. Then run the backfill (Open already calls it once on
	// bootstrap, but the rows were inserted after — manually re-run).
	type seed struct {
		id      int64
		replyTo int64 // 0 = top-level
	}
	seeds := []seed{
		{1, 0},
		{2, 1},
		{3, 2},
		{4, 0},
		{5, 4},
	}
	for _, s := range seeds {
		var replyTo any
		if s.replyTo == 0 {
			replyTo = nil
		} else {
			replyTo = s.replyTo
		}
		_, err := db.ExecContext(ctx, `
			INSERT INTO chat_messages (id, from_agent, content, reply_to)
			VALUES (?, 'test', 'msg', ?)
		`, s.id, replyTo)
		if err != nil {
			t.Fatalf("seed msg %d: %v", s.id, err)
		}
	}

	// Clear the columns to simulate pre-existing rows that pre-date the
	// migration (Open's bootstrap may have already filled them on
	// insert via column defaults, depending on driver behavior).
	if _, err := db.ExecContext(ctx, `UPDATE chat_messages SET thread_root_msg_id = NULL, parent_msg_id = NULL`); err != nil {
		t.Fatalf("clear thread cols: %v", err)
	}

	// Run backfill.
	if err := backfillChatThreadRoots(ctx, db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Verify each row.
	type want struct {
		id         int64
		threadRoot int64
		parent     int64 // 0 = NULL
	}
	wants := []want{
		{1, 1, 0}, // root A
		{2, 1, 1}, // child of A
		{3, 1, 2}, // grandchild of A
		{4, 4, 0}, // root B
		{5, 4, 4}, // child of B
	}
	for _, w := range wants {
		var threadRoot, parent any
		err := db.QueryRowContext(ctx,
			`SELECT thread_root_msg_id, parent_msg_id FROM chat_messages WHERE id = ?`,
			w.id,
		).Scan(&threadRoot, &parent)
		if err != nil {
			t.Errorf("msg %d: %v", w.id, err)
			continue
		}
		gotRoot, _ := threadRoot.(int64)
		if gotRoot != w.threadRoot {
			t.Errorf("msg %d thread_root: got %d, want %d", w.id, gotRoot, w.threadRoot)
		}
		var gotParent int64
		if parent != nil {
			gotParent, _ = parent.(int64)
		}
		if gotParent != w.parent {
			t.Errorf("msg %d parent: got %d, want %d", w.id, gotParent, w.parent)
		}
	}
}

// TestBackfillChatThreadRoots_Idempotent verifies the backfill is safe
// to run repeatedly. Each subsequent run should be a no-op (zero rows
// updated, fast convergence).
func TestBackfillChatThreadRoots_Idempotent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, `
		INSERT INTO chat_messages (id, from_agent, content, reply_to)
		VALUES (1, 'test', 'root', NULL), (2, 'test', 'reply', 1)
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Three passes — all should succeed.
	for i := 0; i < 3; i++ {
		if err := backfillChatThreadRoots(ctx, db); err != nil {
			t.Fatalf("backfill iter %d: %v", i, err)
		}
	}
}
