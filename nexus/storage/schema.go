// Package storage opens the Nexus SQLite database and runs the
// idempotent schema DDL. Per registration spec v0.5 §2.8 and §10: the
// schema lives in source (schema.sql, embedded below); the database
// itself is runtime-created under NEXUS_DATA_DIR and never committed.
//
// SQLite driver: ncruces/go-sqlite3 — pure-Go WASM, cross-platform with
// a single binary, no CGO toolchain required.
//
// sqlite-vec extension: DEFERRED. The `embedding` / `embed_model` /
// `embed_dim` columns in knowledge are reserved day-one but no vector
// extension is loaded. Activation path when we flip to vector
// retrieval: pair a compatible sqlite-vec-go-bindings + go-sqlite3
// version (currently out of sync upstream, see #7695), load the
// extension here, backfill embeddings. No schema migration needed.
package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

//go:embed schema.sql
var schemaSQL string

// DefaultDataDir is used when NEXUS_DATA_DIR is unset.
const DefaultDataDir = "./data"

// DBFileName is the fixed filename inside the data dir.
const DBFileName = "nexus.db"

// Open resolves the data directory (NEXUS_DATA_DIR env or dir arg, falling
// back to DefaultDataDir), creates it if missing, opens nexus.db inside
// it (SQLite creates the file if absent), and runs Bootstrap.
//
// The returned *sql.DB has WAL mode, foreign keys on, and the Nexus
// schema in place. Safe to call on every startup.
func Open(ctx context.Context, dir string, log *slog.Logger) (*sql.DB, error) {
	if dir == "" {
		dir = os.Getenv("NEXUS_DATA_DIR")
	}
	if dir == "" {
		dir = DefaultDataDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir %q: %w", dir, err)
	}

	path := filepath.Join(dir, DBFileName)
	// Windows paths need forward slashes and URI-style prefix so the
	// driver doesn't parse backslashes as escape characters.
	uriPath := filepath.ToSlash(path)
	// busy_timeout MUST be the first pragma. The driver applies _pragma
	// directives in order on each NEW connection; journal_mode(WAL) can hit
	// a transient lock when connections open concurrently. With busy_timeout
	// already in effect, that switch waits (up to 5s) instead of failing
	// immediately with "database is locked" — the cross-platform fix for the
	// concurrent-first-connection pragma race (which Windows' stricter file
	// locking surfaces readily).
	// _txlock=immediate makes BeginTx issue BEGIN IMMEDIATE instead of
	// BEGIN (DEFERRED). Required by chat.Insert's linked-list threading:
	// the SELECT-MAX(id)-then-INSERT pair must be atomic against other
	// writers, otherwise two concurrent replies to the same parent both
	// read the same MAX and fork the thread (#226 invariant violation).
	// Under WAL, writers serialize at BEGIN; reads stay non-blocking.
	dsn := "file:" + uriPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_txlock=immediate"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: sql.Open %q: %w", path, err)
	}

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		closeOnError(db, log, "ping failure")
		return nil, fmt.Errorf("storage: ping %q: %w", path, err)
	}

	if err := Bootstrap(ctx, db); err != nil {
		closeOnError(db, log, "bootstrap failure")
		return nil, fmt.Errorf("storage: bootstrap: %w", err)
	}

	if log != nil {
		log.Info("storage opened", "path", path)
	}
	return db, nil
}

// closeOnError closes the DB during an Open error path and logs any close
// error (don't swallow it silently — a close failure on a WASM SQLite
// driver can indicate unflushed WAL which risks data loss).
func closeOnError(db *sql.DB, log *slog.Logger, context string) {
	if err := db.Close(); err != nil && log != nil {
		log.Warn("storage: db.Close on error path", "context", context, "err", err)
	}
}

// Bootstrap runs the embedded schema.sql against db. The DDL is
// idempotent (CREATE TABLE IF NOT EXISTS, CREATE INDEX IF NOT EXISTS,
// CREATE TRIGGER IF NOT EXISTS) so it is safe on an empty database or
// on an already-bootstrapped one. Conditional column additions live
// in addMissingColumns below — ALTER TABLE isn't naturally idempotent
// so we check PRAGMA table_info first.
func Bootstrap(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("storage: nil db")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("exec schema: %w", err)
	}
	if err := addMissingColumns(ctx, db); err != nil {
		return fmt.Errorf("add missing columns: %w", err)
	}
	if err := createPostMigrationIndexes(ctx, db); err != nil {
		return fmt.Errorf("create post-migration indexes: %w", err)
	}
	if err := backfillChatThreadRoots(ctx, db); err != nil {
		return fmt.Errorf("backfill chat thread roots: %w", err)
	}
	// Note: provider_credentials → credentials data migration (#NEX-75)
	// lives in the credentials package (it needs the data-encryption key
	// derived from session_signing_secret to re-encrypt rows into the
	// new opaque-bundle shape). Caller invokes credentials.MigrateLegacyTable
	// after storage.Bootstrap and credentials.NewStore.
	return nil
}

// createPostMigrationIndexes creates indexes that depend on columns
// added by addMissingColumns. Cannot live in schema.sql because that
// runs BEFORE the column migrations — on a pre-#226 database, the
// schema.sql CREATE INDEX would fail because the columns don't exist
// yet. By running after addMissingColumns, we guarantee the referenced
// columns are present. All statements use IF NOT EXISTS so this is
// safe to call on every boot.
func createPostMigrationIndexes(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_chat_parent_msg_id      ON chat_messages(parent_msg_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_thread_root_msg_id ON chat_messages(thread_root_msg_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_topic              ON chat_messages(topic)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("exec %q: %w", s, err)
		}
	}
	return nil
}

// backfillChatThreadRoots populates the parent_msg_id + thread_root_msg_id
// columns added by #226 for pre-existing chat_messages rows. Idempotent —
// skips rows that already have thread_root_msg_id set. Best-effort on
// parent: legacy rows use reply_to as parent (we can't reconstruct the
// "latest msg in thread at insert time" for historical data; reply_to is
// the closest authoritative parent we have).
//
// Thread root is walked via the reply_to chain: top-level msgs (reply_to
// IS NULL) get thread_root = id. Replies recursively walk reply_to until
// reaching a top-level; their thread_root becomes that ancestor's id.
//
// Cycle safety: SQLite's CTE has implicit recursion depth limits; the
// walk caps at 1000 iterations to defend against pathological cyclic
// data (shouldn't exist via the FK, but doesn't hurt).
func backfillChatThreadRoots(ctx context.Context, db *sql.DB) error {
	// Skip entirely if the columns don't exist (early-boot path where
	// addMissingColumns ran but on a fresh DB with no chat_messages
	// table yet — schema.sql creates the table with the columns, so
	// the check is for safety not correctness).
	exists, err := columnExists(ctx, db, "chat_messages", "thread_root_msg_id")
	if err != nil || !exists {
		return err
	}

	// Step 1: top-level msgs (reply_to IS NULL) get thread_root = id.
	// parent_msg_id stays NULL — they're roots.
	if _, err := db.ExecContext(ctx, `
		UPDATE chat_messages
		   SET thread_root_msg_id = id
		 WHERE thread_root_msg_id IS NULL
		   AND reply_to IS NULL
	`); err != nil {
		return fmt.Errorf("backfill top-level roots: %w", err)
	}

	// Step 2: replies walk the reply_to chain up to find their root.
	// Recursive CTE: start at unresolved rows, climb reply_to until we
	// hit a row with thread_root_msg_id set. Update each unresolved row
	// with the root's id and (as best-effort) reply_to as parent.
	//
	// Run iteratively: each pass resolves one level. Repeat until no
	// rows change or we hit the safety cap.
	const maxIters = 1000
	for i := 0; i < maxIters; i++ {
		res, err := db.ExecContext(ctx, `
			UPDATE chat_messages
			   SET thread_root_msg_id = (
				   SELECT parent.thread_root_msg_id
				     FROM chat_messages parent
				    WHERE parent.id = chat_messages.reply_to
				      AND parent.thread_root_msg_id IS NOT NULL
			   ),
				   parent_msg_id = reply_to
			 WHERE thread_root_msg_id IS NULL
			   AND reply_to IS NOT NULL
			   AND EXISTS (
				   SELECT 1 FROM chat_messages parent
				    WHERE parent.id = chat_messages.reply_to
				      AND parent.thread_root_msg_id IS NOT NULL
			   )
		`)
		if err != nil {
			return fmt.Errorf("backfill replies iter %d: %w", i, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil // converged
		}
	}
	return fmt.Errorf("backfill chat thread roots: did not converge after %d iterations (possible cycle in reply_to chain)", maxIters)
}

// columnAddition declares an ALTER TABLE ADD COLUMN we want to run
// once and only if the column doesn't already exist. SQLite's
// `ALTER TABLE ADD COLUMN IF NOT EXISTS` doesn't exist; this is the
// equivalent done in Go.
type columnAddition struct {
	table  string
	column string
	ddl    string // full ALTER TABLE statement
}

// columnsToAdd lists every conditional column the running codebase
// expects. Each entry is checked against PRAGMA table_info on Bootstrap;
// missing ones are added. Append to this slice rather than editing
// schema.sql when introducing a new column on an existing table.
var columnsToAdd = []columnAddition{
	// Task #218 — per-aspect default credentials so aspects calling
	// claude.completion() without an explicit credential= arg can be
	// routed to the right key without per-call configuration.
	{
		table:  "aspects",
		column: "default_anthropic_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN default_anthropic_credential TEXT",
	},
	{
		table:  "aspects",
		column: "default_openai_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN default_openai_credential TEXT",
	},
	// Task #NEX-74/75 — per-aspect default credentials for non-provider
	// kinds (jira, imap). Parallel shape to the provider defaults above:
	// aspects calling broker.credential.fetch with no explicit name get
	// routed to these. Operator wires via `nexus credential aspect-default`
	// or admin REST (#NEX-76).
	{
		table:  "aspects",
		column: "default_jira_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN default_jira_credential TEXT",
	},
	{
		table:  "aspects",
		column: "default_imap_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN default_imap_credential TEXT",
	},
	// NEX-263 — per-aspect model override columns. Null = inherit keyfile
	// value (ProviderConfig.model / FilterProviderConfig.model /
	// DistillerModel respectively). Operator sets these via the dashboard
	// Settings → Aspects page (NEX-265) so model selection no longer
	// requires editing keyfile JSON + restart. The matching *_credential
	// columns name a broker credential to use for that kind; null falls
	// back to the relevant default_<kind>_credential / keyfile config.
	{
		table:  "aspects",
		column: "primary_model",
		ddl:    "ALTER TABLE aspects ADD COLUMN primary_model TEXT",
	},
	{
		table:  "aspects",
		column: "primary_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN primary_credential TEXT",
	},
	{
		table:  "aspects",
		column: "judge_model",
		ddl:    "ALTER TABLE aspects ADD COLUMN judge_model TEXT",
	},
	{
		table:  "aspects",
		column: "judge_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN judge_credential TEXT",
	},
	// NEX-365 #3 — per-aspect judge PROVIDER override (claude-api /
	// claude-code). Lets a Claude-primary aspect run its cheap-judge on a
	// different provider family (e.g. a DeepSeek Anthropic-shape endpoint)
	// without changing its main turn. Null = inherit network default >
	// keyfile filter_provider. Network-wide equivalent lives on
	// network_defaults.judge_provider (added below).
	{
		table:  "aspects",
		column: "judge_provider",
		ddl:    "ALTER TABLE aspects ADD COLUMN judge_provider TEXT",
	},
	{
		table:  "aspects",
		column: "compact_model",
		ddl:    "ALTER TABLE aspects ADD COLUMN compact_model TEXT",
	},
	{
		table:  "aspects",
		column: "compact_credential",
		ddl:    "ALTER TABLE aspects ADD COLUMN compact_credential TEXT",
	},
	// Task #226 — linked-list thread model. parent_msg_id is the chain
	// parent (NULL for thread roots). thread_root_msg_id is the
	// canonical thread identity used by aspects for per-thread session
	// IDs. Both backfilled by backfillChatThreadRoots for existing rows.
	// NEX-365 #3 — network-wide judge PROVIDER default. Mirrors the
	// per-aspect aspects.judge_provider column above for existing DBs
	// whose network_defaults table predates this column (fresh DBs get it
	// from schema.sql's CREATE TABLE).
	{
		table:  "network_defaults",
		column: "judge_provider",
		ddl:    "ALTER TABLE network_defaults ADD COLUMN judge_provider TEXT",
	},
	{
		table:  "chat_messages",
		column: "parent_msg_id",
		ddl:    "ALTER TABLE chat_messages ADD COLUMN parent_msg_id INTEGER REFERENCES chat_messages(id) ON DELETE SET NULL",
	},
	{
		table:  "chat_messages",
		column: "thread_root_msg_id",
		ddl:    "ALTER TABLE chat_messages ADD COLUMN thread_root_msg_id INTEGER REFERENCES chat_messages(id) ON DELETE SET NULL",
	},
	{
		table:  "chat_messages",
		column: "topic",
		ddl:    "ALTER TABLE chat_messages ADD COLUMN topic TEXT",
	},
}

func addMissingColumns(ctx context.Context, db *sql.DB) error {
	for _, c := range columnsToAdd {
		exists, err := columnExists(ctx, db, c.table, c.column)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", c.table, c.column, err)
		}
		if exists {
			continue
		}
		if _, err := db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	// PRAGMA table_info returns one row per column. Can't parameterize
	// the table name in a PRAGMA, but the values come from our own
	// columnsToAdd slice (compile-time constants), not user input.
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
