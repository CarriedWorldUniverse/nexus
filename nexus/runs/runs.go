package runs

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusComplete  Status = "complete"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Run is the persisted dispatch-run read-model.
type Run struct {
	RunID         string    `json:"run_id"`
	Ticket        string    `json:"ticket"`
	Agent         string    `json:"agent"`
	Thread        string    `json:"thread"`
	DispatchMsgID int64     `json:"dispatch_msg_id"`
	ParentRunID   string    `json:"parent_run_id,omitempty"`
	Command       string    `json:"command"`
	Repo          string    `json:"repo,omitempty"`
	Status        Status    `json:"status"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
	DurationSecs  int       `json:"duration_secs,omitempty"`
}

// Store is the runs read-model. The dispatch runner is the only writer.
type Store interface {
	Migrate(ctx context.Context) error
	Insert(ctx context.Context, r Run) error
	MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int) error
	List(ctx context.Context, limit int) ([]Run, error)
	Get(ctx context.Context, runID string) (Run, error)
}

type SQLStore struct{ DB *sql.DB }

func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{DB: db} }

func (s *SQLStore) Migrate(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS runs (
			run_id          TEXT PRIMARY KEY,
			ticket          TEXT NOT NULL,
			agent           TEXT NOT NULL,
			thread          TEXT NOT NULL,
			dispatch_msg_id INTEGER NOT NULL DEFAULT 0,
			parent_run_id   TEXT NOT NULL DEFAULT '',
			command         TEXT NOT NULL DEFAULT '',
			repo            TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL,
			started_at      INTEGER NOT NULL,
			completed_at    INTEGER NOT NULL DEFAULT 0,
			pr_url          TEXT NOT NULL DEFAULT '',
			duration_secs   INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);`)
	if err != nil {
		return fmt.Errorf("runs.Migrate: %w", err)
	}
	return nil
}

func (s *SQLStore) Insert(ctx context.Context, r Run) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO runs (run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		                  command, repo, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		r.RunID, r.Ticket, r.Agent, r.Thread, r.DispatchMsgID, r.ParentRunID,
		r.Command, r.Repo, string(r.Status), r.StartedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("runs.Insert: %w", err)
	}
	return nil
}

func (s *SQLStore) MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE runs SET status = ?, completed_at = ?, pr_url = ?, duration_secs = ?
		WHERE run_id = ? AND status = ?`,
		string(status), completedAt.UnixMilli(), prURL, durationSecs, runID, string(StatusRunning))
	if err != nil {
		return fmt.Errorf("runs.MarkDone: %w", err)
	}
	return nil
}

func (s *SQLStore) List(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, started_at, completed_at, pr_url, duration_secs
		FROM runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("runs.List: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLStore) Get(ctx context.Context, runID string) (Run, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, started_at, completed_at, pr_url, duration_secs
		FROM runs WHERE run_id = ?`, runID)
	return scanRun(row)
}

type scanner interface{ Scan(...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var status string
	var startedMs, completedMs int64
	if err := sc.Scan(&r.RunID, &r.Ticket, &r.Agent, &r.Thread, &r.DispatchMsgID,
		&r.ParentRunID, &r.Command, &r.Repo, &status, &startedMs, &completedMs,
		&r.PRURL, &r.DurationSecs); err != nil {
		return Run{}, err
	}
	r.Status = Status(status)
	r.StartedAt = time.UnixMilli(startedMs)
	if completedMs > 0 {
		r.CompletedAt = time.UnixMilli(completedMs)
	}
	return r, nil
}
