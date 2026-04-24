// Package sessions owns Nexus's read-only mirror of aspect session
// trees. Aspects forward each local append as a session.entry.appended
// frame; the broker hands it to WriteEntry, which persists to the
// session_projection table.
//
// The projection is strictly observability. Aspects replay from their
// local JSONL for provider context — Nexus never claims to own the
// session data (per transport spec §8). Dropped frames degrade the
// dashboard view but do not affect aspect correctness.
package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Projection is the handle for writing session entries to the
// Nexus-side mirror.
type Projection struct {
	db *sql.DB
}

// New wraps an already-open *sql.DB (the broker's shared Nexus DB).
func New(db *sql.DB) *Projection {
	return &Projection{db: db}
}

// Entry is the persisted shape. Mirrors frames.SessionEntryAppendedPayload
// to keep packages loosely coupled.
type Entry struct {
	Aspect    string
	SessionID string
	EntryID   string
	ParentID  string
	EntryKind string
	EntryTS   string         // ISO-8601 or whatever the aspect sent; stored verbatim
	Payload   map[string]any // encoded as JSON in the row
}

// WriteEntry inserts an entry into the projection table. Duplicate
// (aspect, session_id, entry_id) tuples are treated as idempotent
// (INSERT OR IGNORE) — aspects that retry a failed forward or
// project entries across reconnects won't produce duplicates.
func (p *Projection) WriteEntry(ctx context.Context, e Entry) error {
	if p == nil || p.db == nil {
		return errors.New("sessions: nil projection")
	}
	if e.Aspect == "" || e.EntryID == "" || e.EntryKind == "" {
		return errors.New("sessions.WriteEntry: aspect, entry_id, entry_kind required")
	}

	var payloadJSON sql.NullString
	if len(e.Payload) > 0 {
		raw, err := json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("sessions.WriteEntry: marshal payload: %w", err)
		}
		payloadJSON = sql.NullString{String: string(raw), Valid: true}
	}

	var parentID sql.NullString
	if e.ParentID != "" {
		parentID = sql.NullString{String: e.ParentID, Valid: true}
	}

	const query = `
	INSERT OR IGNORE INTO session_projection
	  (aspect, session_id, entry_id, parent_id, entry_kind, entry_ts, payload)
	VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := p.db.ExecContext(ctx, query,
		e.Aspect, e.SessionID, e.EntryID, parentID, e.EntryKind, e.EntryTS, payloadJSON,
	)
	if err != nil {
		return fmt.Errorf("sessions.WriteEntry: %w", err)
	}
	return nil
}

// Count returns the number of projected entries for an aspect/session.
// Handy for tests.
func (p *Projection) Count(ctx context.Context, aspect, sessionID string) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_projection WHERE aspect = ? AND session_id = ?`,
		aspect, sessionID,
	).Scan(&n)
	return n, err
}
