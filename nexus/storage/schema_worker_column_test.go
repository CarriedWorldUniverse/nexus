package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// NEX-827 adds nexus_settings.nexus_md_worker. Production runs an
// EXISTING database, so the case that matters is not the fresh-schema
// path (schema.sql) but the migration path (columnsToAdd) — and the
// requirement is stronger than "the column appears": the row's existing
// content must survive, because that row is the prompt every identity in
// the network boots with.
func TestNexusSettingsWorkerColumn_MigratesExistingDB(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Build a PRE-NEX-827 database by hand: the old table shape, with
	// content in it, exactly as a deployed broker would have.
	// Same driver + DSN the real Open uses, so this exercises the actual
	// migration path rather than a differently-configured lookalike.
	path := filepath.Join(dir, "nexus.db")
	old, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := old.ExecContext(ctx, `
		CREATE TABLE nexus_settings (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			nexus_md   TEXT NOT NULL DEFAULT '',
			version    INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT INTO nexus_settings (id, nexus_md, version) VALUES (1, '## live central v4', 4);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("open + bootstrap over existing db: %v", err)
	}
	defer db.Close()

	var content string
	var version int64
	var worker string
	if err := db.QueryRowContext(ctx,
		`SELECT nexus_md, nexus_md_worker, version FROM nexus_settings WHERE id = 1`,
	).Scan(&content, &worker, &version); err != nil {
		t.Fatalf("select after migration (column missing?): %v", err)
	}

	if content != "## live central v4" {
		t.Errorf("migration altered existing central content: %q", content)
	}
	if version != 4 {
		t.Errorf("migration altered the version counter: %d, want 4", version)
	}
	if worker != "" {
		t.Errorf("new column should default empty (so behaviour is unchanged), got %q", worker)
	}
}

// Bootstrap runs on every start, so the addition must be idempotent —
// a second Open must not error on "duplicate column".
func TestNexusSettingsWorkerColumn_Idempotent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO nexus_settings (id, nexus_md, nexus_md_worker, version)
		 VALUES (1, '## interactive', '## headless', 7)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("second open (migration not idempotent): %v", err)
	}
	defer db2.Close()

	var worker string
	if err := db2.QueryRowContext(ctx,
		`SELECT nexus_md_worker FROM nexus_settings WHERE id = 1`).Scan(&worker); err != nil {
		t.Fatalf("select after reopen: %v", err)
	}
	if worker != "## headless" {
		t.Errorf("reopen clobbered the worker variant: %q", worker)
	}
}
