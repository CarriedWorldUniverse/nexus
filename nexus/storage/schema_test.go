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
		"chat_reactions", "shared_files",
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
