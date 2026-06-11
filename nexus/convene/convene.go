// Package convene is the persisted read-model for !convene roundtables
// (roundtable spec component 3). A convene pulls several named aspects
// into one thread to argue a problem to consensus; a facilitator judges
// convergence and posts the summary.
//
// The broker is the only writer. The schema mirrors the runs table
// (nexus/runs) deliberately: a small, flat read-model keyed by an id,
// with a status lifecycle and create/close timestamps. The convene's
// participants and facilitator are stored so an operator watch surface
// (convenes.list) and the close-authz path can read them back without
// re-parsing the original command.
package convene

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Status is the convene lifecycle. A convene opens when the command is
// parsed and the briefs post; the facilitator closes it converged when
// consensus lands, or abandoned when the deliberation is stuck (or a
// round cap trips).
type Status string

const (
	StatusOpen      Status = "open"
	StatusConverged Status = "converged"
	StatusAbandoned Status = "abandoned"
)

// Convene is the persisted convene read-model.
type Convene struct {
	ConveneID    string    `json:"convene_id"`
	RootMsgID    int64     `json:"root_msg_id"`
	Facilitator  string    `json:"facilitator"`
	Participants []string  `json:"participants"`
	Problem      string    `json:"problem"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	ClosedAt     time.Time `json:"closed_at,omitempty"`
	SummaryMsgID int64     `json:"summary_msg_id,omitempty"`
}

// Store is the convene read-model. The broker is the only writer.
type Store interface {
	Migrate(ctx context.Context) error
	Insert(ctx context.Context, c Convene) error
	Close(ctx context.Context, conveneID string, status Status, closedAt time.Time, summaryMsgID int64) error
	Get(ctx context.Context, conveneID string) (Convene, error)
	List(ctx context.Context, limit int) ([]Convene, error)
}

type SQLStore struct{ DB *sql.DB }

func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{DB: db} }

func (s *SQLStore) Migrate(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS convenes (
			convene_id      TEXT PRIMARY KEY,
			root_msg_id     INTEGER NOT NULL DEFAULT 0,
			facilitator     TEXT NOT NULL,
			participants    TEXT NOT NULL DEFAULT '',
			problem         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL,
			created_at      INTEGER NOT NULL,
			closed_at       INTEGER NOT NULL DEFAULT 0,
			summary_msg_id  INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_convenes_created ON convenes(created_at DESC);`)
	if err != nil {
		return fmt.Errorf("convene.Migrate: %w", err)
	}
	return nil
}

func (s *SQLStore) Insert(ctx context.Context, c Convene) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO convenes (convene_id, root_msg_id, facilitator, participants,
		                      problem, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(convene_id) DO NOTHING`,
		c.ConveneID, c.RootMsgID, c.Facilitator, strings.Join(c.Participants, ","),
		c.Problem, string(statusOrOpen(c.Status)), c.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("convene.Insert: %w", err)
	}
	return nil
}

// Close transitions an open convene to a terminal status. Idempotent and
// guarded: the WHERE status='open' clause makes a second close (or a close
// of an already-terminal convene) a no-op, so a re-fired close frame can't
// flip converged→abandoned or overwrite the summary.
func (s *SQLStore) Close(ctx context.Context, conveneID string, status Status, closedAt time.Time, summaryMsgID int64) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE convenes SET status = ?, closed_at = ?, summary_msg_id = ?
		WHERE convene_id = ? AND status = ?`,
		string(status), closedAt.UnixMilli(), summaryMsgID, conveneID, string(StatusOpen))
	if err != nil {
		return fmt.Errorf("convene.Close: %w", err)
	}
	return nil
}

func (s *SQLStore) Get(ctx context.Context, conveneID string) (Convene, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT convene_id, root_msg_id, facilitator, participants, problem,
		       status, created_at, closed_at, summary_msg_id
		FROM convenes WHERE convene_id = ?`, conveneID)
	return scanConvene(row)
}

func (s *SQLStore) List(ctx context.Context, limit int) ([]Convene, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT convene_id, root_msg_id, facilitator, participants, problem,
		       status, created_at, closed_at, summary_msg_id
		FROM convenes ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("convene.List: %w", err)
	}
	defer rows.Close()
	var out []Convene
	for rows.Next() {
		c, err := scanConvene(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanConvene(sc scanner) (Convene, error) {
	var c Convene
	var status, participants string
	var createdMs, closedMs int64
	if err := sc.Scan(&c.ConveneID, &c.RootMsgID, &c.Facilitator, &participants,
		&c.Problem, &status, &createdMs, &closedMs, &c.SummaryMsgID); err != nil {
		return Convene{}, err
	}
	c.Status = Status(status)
	c.Participants = splitParticipants(participants)
	c.CreatedAt = time.UnixMilli(createdMs)
	if closedMs > 0 {
		c.ClosedAt = time.UnixMilli(closedMs)
	}
	return c, nil
}

func splitParticipants(csv string) []string {
	if csv == "" {
		return nil
	}
	return strings.Split(csv, ",")
}

func statusOrOpen(s Status) Status {
	if s == "" {
		return StatusOpen
	}
	return s
}
