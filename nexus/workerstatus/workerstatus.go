// Package workerstatus persists the M1 Unit 5 worker-status heartbeat
// (PHASE2-DESIGN §5): every worker publishes one machine-readable status
// shape on a heartbeat over the existing dispatch.status frame path, and
// this package's Store upserts it into a `worker_status` table keyed on
// agent name. GET /api/admin/workers reads the consolidated fleet
// straight from List — one query, no scraped prose.
//
// Mirrors nexus/runs' SQLStore idiom: a small hand-rolled migration,
// upsert-by-primary-key writes, plain SELECT reads.
package workerstatus

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Status is one worker's most-recently-reported state. Field shape
// mirrors frames.WorkerStatusPayload (nexus/frames/payloads.go) —
// kept as a separate type so this package doesn't import nexus/frames
// (the broker's frame handler is the only translator between the two).
type Status struct {
	Agent          string    `json:"agent"`
	Role           string    `json:"role,omitempty"`
	Personality    string    `json:"personality,omitempty"`
	WorkItemID     string    `json:"work_item_id,omitempty"`
	State          string    `json:"state"`
	AuthOk         bool      `json:"auth_ok"`
	TokenExpiresAt time.Time `json:"token_expires_at,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Model          string    `json:"model,omitempty"`
	CLIVersion     string    `json:"cli_version,omitempty"`
	ImageTag       string    `json:"image_tag,omitempty"`
	LastHeartbeat  time.Time `json:"last_heartbeat"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	Turns          int       `json:"turns,omitempty"`
	TokensUsed     int       `json:"tokens_used,omitempty"`
}

// Stale reports whether this worker's last heartbeat is older than
// maxAge relative to now. Used by unit 6's orchestrator auto-reap
// (PHASE2-DESIGN §2.1 "stale heartbeat > N min") — see README.md.
// A zero LastHeartbeat (never reported) is always stale.
func (s Status) Stale(now time.Time, maxAge time.Duration) bool {
	if s.LastHeartbeat.IsZero() {
		return true
	}
	return now.Sub(s.LastHeartbeat) > maxAge
}

// Store is the worker_status read-model. The broker's worker.status
// frame handler is the only writer; GET /api/admin/workers and unit 6's
// orchestrator auto-reap are readers.
type Store interface {
	Migrate(ctx context.Context) error
	// Upsert writes or replaces the row for s.Agent. Callers set
	// LastHeartbeat before calling — Upsert does not stamp it.
	Upsert(ctx context.Context, s Status) error
	Get(ctx context.Context, agent string) (Status, error)
	// List returns every known worker row, most-recently-heartbeated
	// first. This is the entire query behind GET /api/admin/workers.
	List(ctx context.Context) ([]Status, error)
	// Delete removes a worker's row (e.g. after a confirmed reap).
	Delete(ctx context.Context, agent string) error
}

type SQLStore struct{ DB *sql.DB }

func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{DB: db} }

func (s *SQLStore) Migrate(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS worker_status (
			agent            TEXT PRIMARY KEY,
			role             TEXT NOT NULL DEFAULT '',
			personality      TEXT NOT NULL DEFAULT '',
			work_item_id     TEXT NOT NULL DEFAULT '',
			state            TEXT NOT NULL DEFAULT '',
			auth_ok          INTEGER NOT NULL DEFAULT 0,
			token_expires_at INTEGER NOT NULL DEFAULT 0,
			provider         TEXT NOT NULL DEFAULT '',
			model            TEXT NOT NULL DEFAULT '',
			cli_version      TEXT NOT NULL DEFAULT '',
			image_tag        TEXT NOT NULL DEFAULT '',
			last_heartbeat   INTEGER NOT NULL DEFAULT 0,
			started_at       INTEGER NOT NULL DEFAULT 0,
			turns            INTEGER NOT NULL DEFAULT 0,
			tokens_used      INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_worker_status_heartbeat ON worker_status(last_heartbeat DESC);`)
	if err != nil {
		return fmt.Errorf("workerstatus.Migrate: %w", err)
	}
	return nil
}

func (s *SQLStore) Upsert(ctx context.Context, st Status) error {
	if st.Agent == "" {
		return fmt.Errorf("workerstatus.Upsert: agent required")
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO worker_status (agent, role, personality, work_item_id, state,
			auth_ok, token_expires_at, provider, model, cli_version, image_tag,
			last_heartbeat, started_at, turns, tokens_used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent) DO UPDATE SET
			role = excluded.role,
			personality = excluded.personality,
			work_item_id = excluded.work_item_id,
			state = excluded.state,
			auth_ok = excluded.auth_ok,
			token_expires_at = excluded.token_expires_at,
			provider = excluded.provider,
			model = excluded.model,
			cli_version = excluded.cli_version,
			image_tag = excluded.image_tag,
			last_heartbeat = excluded.last_heartbeat,
			started_at = CASE WHEN excluded.started_at != 0 THEN excluded.started_at ELSE worker_status.started_at END,
			turns = excluded.turns,
			tokens_used = excluded.tokens_used`,
		st.Agent, st.Role, st.Personality, st.WorkItemID, st.State,
		boolToInt(st.AuthOk), timeToMillis(st.TokenExpiresAt), st.Provider, st.Model,
		st.CLIVersion, st.ImageTag, timeToMillis(st.LastHeartbeat), timeToMillis(st.StartedAt),
		st.Turns, st.TokensUsed)
	if err != nil {
		return fmt.Errorf("workerstatus.Upsert: %w", err)
	}
	return nil
}

func (s *SQLStore) Get(ctx context.Context, agent string) (Status, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT agent, role, personality, work_item_id, state, auth_ok, token_expires_at,
		       provider, model, cli_version, image_tag, last_heartbeat, started_at, turns, tokens_used
		FROM worker_status WHERE agent = ?`, agent)
	return scanStatus(row)
}

func (s *SQLStore) List(ctx context.Context) ([]Status, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT agent, role, personality, work_item_id, state, auth_ok, token_expires_at,
		       provider, model, cli_version, image_tag, last_heartbeat, started_at, turns, tokens_used
		FROM worker_status ORDER BY last_heartbeat DESC`)
	if err != nil {
		return nil, fmt.Errorf("workerstatus.List: %w", err)
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		st, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *SQLStore) Delete(ctx context.Context, agent string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM worker_status WHERE agent = ?`, agent)
	if err != nil {
		return fmt.Errorf("workerstatus.Delete: %w", err)
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanStatus(sc scanner) (Status, error) {
	var st Status
	var authOk int
	var tokenExpiresMs, lastHeartbeatMs, startedMs int64
	if err := sc.Scan(&st.Agent, &st.Role, &st.Personality, &st.WorkItemID, &st.State,
		&authOk, &tokenExpiresMs, &st.Provider, &st.Model, &st.CLIVersion, &st.ImageTag,
		&lastHeartbeatMs, &startedMs, &st.Turns, &st.TokensUsed); err != nil {
		return Status{}, fmt.Errorf("workerstatus: scan: %w", err)
	}
	st.AuthOk = authOk != 0
	if tokenExpiresMs > 0 {
		st.TokenExpiresAt = time.UnixMilli(tokenExpiresMs)
	}
	if lastHeartbeatMs > 0 {
		st.LastHeartbeat = time.UnixMilli(lastHeartbeatMs)
	}
	if startedMs > 0 {
		st.StartedAt = time.UnixMilli(startedMs)
	}
	return st, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func timeToMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
