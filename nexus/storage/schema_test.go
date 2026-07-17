package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestResolveOpenConfigDefaultsToSQLiteFile(t *testing.T) {
	t.Setenv(EnvDBDSN, "")
	t.Setenv("NEXUS_DATA_DIR", "")

	dir := t.TempDir()
	cfg := ResolveOpenConfig(dir)

	if cfg.DriverName != "sqlite3" {
		t.Fatalf("DriverName = %q, want sqlite3", cfg.DriverName)
	}
	if !cfg.UsesLocalFile {
		t.Fatalf("UsesLocalFile = false, want true")
	}
	if cfg.Path != filepath.Join(dir, DBFileName) {
		t.Fatalf("Path = %q, want %q", cfg.Path, filepath.Join(dir, DBFileName))
	}
	if !strings.HasPrefix(cfg.DSN, "file:"+filepath.ToSlash(cfg.Path)+"?") {
		t.Fatalf("DSN = %q, want file URI for %q", cfg.DSN, cfg.Path)
	}
	if !strings.Contains(cfg.DSN, "_pragma=journal_mode(WAL)") {
		t.Fatalf("DSN = %q, want WAL pragma on SQLite-file path", cfg.DSN)
	}
}

func TestResolveOpenConfigUsesLibSQLDSNFromEnv(t *testing.T) {
	const dsn = "http://sqld.cwb.svc.cluster.local:8080"
	t.Setenv(EnvDBDSN, dsn)

	cfg := ResolveOpenConfig(t.TempDir())

	if cfg.DriverName != "libsql" {
		t.Fatalf("DriverName = %q, want libsql", cfg.DriverName)
	}
	if cfg.UsesLocalFile {
		t.Fatalf("UsesLocalFile = true, want false")
	}
	if cfg.DSN != dsn {
		t.Fatalf("DSN = %q, want %q", cfg.DSN, dsn)
	}
	if cfg.Path != "" {
		t.Fatalf("Path = %q, want empty for remote libSQL", cfg.Path)
	}
	if !slices.Contains(sql.Drivers(), "libsql") {
		t.Fatalf("database/sql driver libsql is not registered; drivers: %v", sql.Drivers())
	}
}

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
		"mcp_profiles",
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

// TestMCPProfilesTable verifies NEX-168's mcp_profiles table: one row per
// aspect, JSON profile blob, FK to aspects(name) with ON DELETE CASCADE so
// removing an aspect drops its profile cleanly. The table must come up
// fresh on Bootstrap and survive a re-open (idempotent).
func TestMCPProfilesTable(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Verify the column shape via PRAGMA.
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(mcp_profiles)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	gotCols := map[string]string{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotCols[name] = ctype
	}
	rows.Close()
	wantCols := map[string]string{
		"aspect_name": "TEXT",
		"profile":     "TEXT",
		"updated_at":  "TEXT",
	}
	for col, ctype := range wantCols {
		got, ok := gotCols[col]
		if !ok {
			t.Errorf("mcp_profiles missing column %q", col)
			continue
		}
		if got != ctype {
			t.Errorf("mcp_profiles.%s type = %q, want %q", col, got, ctype)
		}
	}

	// Seed an aspect row (mcp_profiles has FK on aspects.name).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO aspects (name, aspect_pubkey, provider, model)
		VALUES ('forge', X'00', 'anthropic', 'claude-opus-4-7')
	`); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}

	// Insert a profile and read it back.
	const profileJSON = `{"mcpServers":{"github":{"command":"node","env":{"TOKEN":"x"}}}}`
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mcp_profiles (aspect_name, profile) VALUES (?, ?)
	`, "forge", profileJSON); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT profile FROM mcp_profiles WHERE aspect_name = 'forge'`).Scan(&got); err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if got != profileJSON {
		t.Errorf("profile round-trip: got %q want %q", got, profileJSON)
	}

	// FK CASCADE: deleting the aspect drops the profile.
	if _, err := db.ExecContext(ctx, `DELETE FROM aspects WHERE name = 'forge'`); err != nil {
		t.Fatalf("delete aspect: %v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_profiles WHERE aspect_name = 'forge'`).Scan(&count); err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if count != 0 {
		t.Errorf("FK CASCADE failed: %d profile rows survived aspect delete (want 0)", count)
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

// TestLibSQLPoolIdleCap_Structure verifies the configured idle cap is
// bounded to stay below sqld's default hrana stream TTL (10s). This is
// the structural invariant that keeps the fix from re-introducing the
// expired-stream log flood if anyone later raises the cap.
func TestLibSQLPoolIdleCap_Structure(t *testing.T) {
	const sqldDefaultStreamTTL = 10 * time.Second
	if sqldHranaStreamIdleCap <= 0 {
		t.Fatalf("sqldHranaStreamIdleCap = %v, want > 0", sqldHranaStreamIdleCap)
	}
	if sqldHranaStreamIdleCap >= sqldDefaultStreamTTL {
		t.Fatalf("sqldHranaStreamIdleCap = %v, want < sqld default stream TTL (%v)",
			sqldHranaStreamIdleCap, sqldDefaultStreamTTL)
	}
}

// TestOpen_LibSQLDriverAppliesPoolIdleCap verifies that Open applies the
// sqldHranaStreamIdleCap to the database/sql pool when the driver is
// "libsql". We exercise the production code path with libsql-client-go's
// local file mode (no live sqld required) and observe the cap's effect:
// an idle connection released past the 5s cap is closed by the pool,
// which database/sql surfaces via Stats.MaxIdleTimeClosed.
//
// If SetConnMaxIdleTime were not called (e.g. the libsql branch were
// skipped), the released connection would remain in the pool past 6s
// and MaxIdleTimeClosed would stay 0 — this test would fail.
func TestOpen_LibSQLDriverAppliesPoolIdleCap(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, DBFileName)
	// libsql-client-go requires a URL scheme; file:// opens a local
	// SQLite file the same way http:// opens a remote sqld.
	dsn := "file://" + dbPath

	t.Setenv(EnvDBDSN, dsn)

	ctx := context.Background()
	db, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Bound the idle pool to one slot so we have a single connection
	// to watch — the cap only closes the oldest idle connection when
	// the pool is at its idle limit.
	db.SetMaxIdleConns(1)

	// Acquire a connection from the pool, then release it. After this
	// the pool holds exactly one idle connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("release conn: %v", err)
	}

	// Wait past the idle cap (5s) plus a second of margin so the
	// pool's idle-connection sweep has a chance to close it.
	time.Sleep(sqldHranaStreamIdleCap + time.Second)

	stats := db.Stats()
	if stats.MaxIdleTimeClosed == 0 {
		t.Errorf("MaxIdleTimeClosed = 0, want >= 1 — the idle cap (%v) was not applied to the pool",
			sqldHranaStreamIdleCap)
	}
}
