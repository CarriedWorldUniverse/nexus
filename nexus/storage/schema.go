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
	_ "embed"
	"database/sql"
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
	dsn := "file:" + uriPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"

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
// on an already-bootstrapped one.
func Bootstrap(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("storage: nil db")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("exec schema: %w", err)
	}
	return nil
}

