package runs

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Status string

const (
	StatusSubmitted Status = "submitted"
	StatusAccepted  Status = "accepted"
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
	Reason        string    `json:"reason,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
	DurationSecs  int       `json:"duration_secs,omitempty"`
	Logs          string    `json:"-"`
}

// Store is the runs read-model. The dispatch runner is the only writer.
type Store interface {
	Migrate(ctx context.Context) error
	Insert(ctx context.Context, r Run) error
	MarkAccepted(ctx context.Context, runID string, acceptedAt time.Time) error
	MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int, reason string) error
	RecordLogs(ctx context.Context, runID, logs string) error
	GetLogs(ctx context.Context, runID string) (string, error)
	ListRunning(ctx context.Context) ([]Run, error)
	List(ctx context.Context, limit int) ([]Run, error)
	ListCompleted(ctx context.Context, limit int) ([]Run, error)
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
			reason          TEXT NOT NULL DEFAULT '',
			started_at      INTEGER NOT NULL,
			completed_at    INTEGER NOT NULL DEFAULT 0,
			pr_url          TEXT NOT NULL DEFAULT '',
			duration_secs   INTEGER NOT NULL DEFAULT 0,
			logs            TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(started_at DESC);`)
	if err != nil {
		return fmt.Errorf("runs.Migrate: %w", err)
	}
	if err := s.ensureLogsColumn(ctx); err != nil {
		return err
	}
	if err := s.ensureReasonColumn(ctx); err != nil {
		return err
	}
	return nil
}

func (s *SQLStore) ensureLogsColumn(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `PRAGMA table_info(runs)`)
	if err != nil {
		return fmt.Errorf("runs.Migrate columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("runs.Migrate scan columns: %w", err)
		}
		if name == "logs" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("runs.Migrate columns: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN logs TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("runs.Migrate add logs: %w", err)
	}
	return nil
}

func (s *SQLStore) ensureReasonColumn(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `PRAGMA table_info(runs)`)
	if err != nil {
		return fmt.Errorf("runs.Migrate columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("runs.Migrate scan columns: %w", err)
		}
		if name == "reason" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("runs.Migrate columns: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN reason TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("runs.Migrate add reason: %w", err)
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

func (s *SQLStore) MarkAccepted(ctx context.Context, runID string, acceptedAt time.Time) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE runs SET status = ?, started_at = ?
		WHERE run_id = ? AND status IN (?, ?, ?)`,
		string(StatusAccepted), acceptedAt.UnixMilli(), runID,
		string(StatusSubmitted), string(StatusQueued), string(StatusRunning))
	if err != nil {
		return fmt.Errorf("runs.MarkAccepted: %w", err)
	}
	return nil
}

func (s *SQLStore) MarkDone(ctx context.Context, runID string, status Status, completedAt time.Time, prURL string, durationSecs int, reason string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE runs SET status = ?, completed_at = ?, pr_url = ?, duration_secs = ?, reason = ?
		WHERE run_id = ? AND status IN (?, ?, ?, ?)`,
		string(status), completedAt.UnixMilli(), prURL, durationSecs, reason, runID,
		string(StatusSubmitted), string(StatusQueued), string(StatusRunning), string(StatusAccepted))
	if err != nil {
		return fmt.Errorf("runs.MarkDone: %w", err)
	}
	return nil
}

func (s *SQLStore) RecordLogs(ctx context.Context, runID, logs string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE runs SET logs = ? WHERE run_id = ?`, logs, runID)
	if err != nil {
		return fmt.Errorf("runs.RecordLogs: %w", err)
	}
	return nil
}

func (s *SQLStore) GetLogs(ctx context.Context, runID string) (string, error) {
	var logs string
	err := s.DB.QueryRowContext(ctx, `SELECT logs FROM runs WHERE run_id = ?`, runID).Scan(&logs)
	if err != nil {
		return "", fmt.Errorf("runs.GetLogs: %w", err)
	}
	return logs, nil
}

func (s *SQLStore) ListRunning(ctx context.Context) ([]Run, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, reason, started_at, completed_at, pr_url, duration_secs
		FROM runs WHERE status IN (?, ?, ?, ?) ORDER BY started_at DESC`,
		string(StatusSubmitted), string(StatusQueued), string(StatusRunning), string(StatusAccepted))
	if err != nil {
		return nil, fmt.Errorf("runs.ListRunning: %w", err)
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

func (s *SQLStore) List(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, reason, started_at, completed_at, pr_url, duration_secs
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

// ListCompleted returns the most-recently-completed runs (status IN
// complete/failed/cancelled), most recent first by completed_at, capped
// at limit (default/clamp mirrors List). Used by the console fleet
// pane's "Recently completed" section, which reads run history the
// live worker_status table can no longer show once a run's row is
// reaped at JobDone.
func (s *SQLStore) ListCompleted(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT run_id, ticket, agent, thread, dispatch_msg_id, parent_run_id,
		       command, repo, status, reason, started_at, completed_at, pr_url, duration_secs
		FROM runs WHERE status IN (?, ?, ?) ORDER BY completed_at DESC LIMIT ?`,
		string(StatusComplete), string(StatusFailed), string(StatusCancelled), limit)
	if err != nil {
		return nil, fmt.Errorf("runs.ListCompleted: %w", err)
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
		       command, repo, status, reason, started_at, completed_at, pr_url, duration_secs
		FROM runs WHERE run_id = ?`, runID)
	return scanRun(row)
}

type scanner interface{ Scan(...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var status string
	var startedMs, completedMs int64
	if err := sc.Scan(&r.RunID, &r.Ticket, &r.Agent, &r.Thread, &r.DispatchMsgID,
		&r.ParentRunID, &r.Command, &r.Repo, &status, &r.Reason, &startedMs, &completedMs,
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
