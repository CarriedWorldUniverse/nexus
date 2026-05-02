// Package usage records per-turn token attribution for forensics.
// Per Lock 4 + operator #9254/#9258: NOT chat-visible. Pure
// dashboard-query path — "we burned through tokens this week,
// where did it go?" answered by clicking through chat history.
//
// Each row joins back to chat_messages via msg_id (when the turn
// was triggered by a chat) so the dashboard can render
// "this chat message cost X input / Y output tokens" inline on the
// chat scroll, but ONLY when the operator opens the cost view —
// the row itself is never surfaced as a chat post.

package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Record is one row of chat_usage. AspectID identifies who ran the
// turn; MsgID is the triggering chat message id, or 0 for turns
// that ran without a chat trigger (e.g. compaction summarize calls,
// internal operator-driven deliberation).
type Record struct {
	ID           int64     `json:"id"`
	MsgID        int64     `json:"msg_id,omitempty"`
	TurnID       string    `json:"turn_id"`
	AspectID     string    `json:"aspect_id"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	RecordedAt   time.Time `json:"recorded_at"`
}

// Store is the persistence seam for usage attribution. Implementations
// MUST be safe for concurrent use; the funnel writes from the
// deliberation goroutine, dashboard queries from HTTP handlers.
type Store interface {
	// Record persists one usage row. Returns the assigned id and
	// the server-stamped RecordedAt. Negative token counts are
	// rejected — providers occasionally report nonsense and we
	// don't want to poison the analytics surface.
	Record(ctx context.Context, r Record) (Record, error)

	// ListByMsg returns all usage rows attributed to a specific
	// chat msg_id, oldest first. Used by the dashboard's
	// "cost-of-this-message" view.
	ListByMsg(ctx context.Context, msgID int64) ([]Record, error)

	// ListByAspect returns usage rows for an aspect within an
	// optional time range. Used by the dashboard's "where did
	// today's tokens go?" view. Limit caps result count.
	ListByAspect(ctx context.Context, aspect string, since, until time.Time, limit int) ([]Record, error)

	// SumByAspect returns aggregated input/output tokens for an
	// aspect within an optional time range. Used by the
	// dashboard's high-level cost view. Zero values for since/until
	// mean unbounded.
	SumByAspect(ctx context.Context, aspect string, since, until time.Time) (input, output int, err error)
}

// SQLStore is the sqlite-backed Store. Holds *sql.DB; concurrent-safe
// since *sql.DB is.
type SQLStore struct {
	DB *sql.DB
}

// NewSQLStore constructs a SQLStore against an open DB with schema
// applied (storage.Bootstrap).
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

// Record inserts one usage row. Validates non-negative counts,
// non-empty turn_id and aspect, non-empty model. Server-stamps the
// timestamp via the schema default; reads back to surface the
// stored value.
func (s *SQLStore) Record(ctx context.Context, r Record) (Record, error) {
	if r.TurnID == "" {
		return Record{}, fmt.Errorf("usage.Record: TurnID required")
	}
	if r.AspectID == "" {
		return Record{}, fmt.Errorf("usage.Record: AspectID required")
	}
	if r.Model == "" {
		return Record{}, fmt.Errorf("usage.Record: Model required")
	}
	if r.InputTokens < 0 || r.OutputTokens < 0 {
		return Record{}, fmt.Errorf("usage.Record: token counts must be non-negative (got input=%d output=%d)",
			r.InputTokens, r.OutputTokens)
	}

	var msgArg any
	if r.MsgID > 0 {
		msgArg = r.MsgID
	}

	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO chat_usage (msg_id, turn_id, aspect, model, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?)
	`, msgArg, r.TurnID, r.AspectID, r.Model, r.InputTokens, r.OutputTokens)
	if err != nil {
		return Record{}, fmt.Errorf("usage.Record: exec: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Record{}, fmt.Errorf("usage.Record: id: %w", err)
	}

	var stored Record
	var msgCol sql.NullInt64
	var ts string
	err = s.DB.QueryRowContext(ctx, `
		SELECT id, msg_id, turn_id, aspect, model, input_tokens, output_tokens, recorded_at
		FROM chat_usage WHERE id = ?
	`, id).Scan(&stored.ID, &msgCol, &stored.TurnID, &stored.AspectID, &stored.Model,
		&stored.InputTokens, &stored.OutputTokens, &ts)
	if err != nil {
		return Record{}, fmt.Errorf("usage.Record: read-back: %w", err)
	}
	if msgCol.Valid {
		stored.MsgID = msgCol.Int64
	}
	stored.RecordedAt, err = parseSQLiteTime(ts)
	if err != nil {
		return Record{}, fmt.Errorf("usage.Record: parse recorded_at %q: %w", ts, err)
	}
	return stored, nil
}

// ListByMsg returns rows attributed to a chat msg, oldest first.
func (s *SQLStore) ListByMsg(ctx context.Context, msgID int64) ([]Record, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, msg_id, turn_id, aspect, model, input_tokens, output_tokens, recorded_at
		FROM chat_usage WHERE msg_id = ? ORDER BY id ASC
	`, msgID)
	if err != nil {
		return nil, fmt.Errorf("usage.ListByMsg: query: %w", err)
	}
	return scanRecords(rows)
}

// ListByAspect returns rows for an aspect with optional time range.
// Zero since/until = unbounded on that side. Limit caps result.
func (s *SQLStore) ListByAspect(ctx context.Context, aspect string, since, until time.Time, limit int) ([]Record, error) {
	q := `SELECT id, msg_id, turn_id, aspect, model, input_tokens, output_tokens, recorded_at
		FROM chat_usage WHERE aspect = ?`
	args := []any{aspect}
	if !since.IsZero() {
		q += ` AND recorded_at >= ?`
		args = append(args, since.UTC().Format("2006-01-02 15:04:05"))
	}
	if !until.IsZero() {
		q += ` AND recorded_at < ?`
		args = append(args, until.UTC().Format("2006-01-02 15:04:05"))
	}
	q += ` ORDER BY id ASC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage.ListByAspect: query: %w", err)
	}
	return scanRecords(rows)
}

// SumByAspect aggregates token counts for an aspect within an
// optional range.
func (s *SQLStore) SumByAspect(ctx context.Context, aspect string, since, until time.Time) (int, int, error) {
	q := `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM chat_usage WHERE aspect = ?`
	args := []any{aspect}
	if !since.IsZero() {
		q += ` AND recorded_at >= ?`
		args = append(args, since.UTC().Format("2006-01-02 15:04:05"))
	}
	if !until.IsZero() {
		q += ` AND recorded_at < ?`
		args = append(args, until.UTC().Format("2006-01-02 15:04:05"))
	}
	var input, output int
	err := s.DB.QueryRowContext(ctx, q, args...).Scan(&input, &output)
	if err != nil {
		return 0, 0, fmt.Errorf("usage.SumByAspect: query: %w", err)
	}
	return input, output, nil
}

// scanRecords iterates rows and decodes them. Common for both
// ListByMsg and ListByAspect.
func scanRecords(rows *sql.Rows) ([]Record, error) {
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var msgCol sql.NullInt64
		var ts string
		if err := rows.Scan(&r.ID, &msgCol, &r.TurnID, &r.AspectID, &r.Model,
			&r.InputTokens, &r.OutputTokens, &ts); err != nil {
			return nil, fmt.Errorf("usage scan: %w", err)
		}
		if msgCol.Valid {
			r.MsgID = msgCol.Int64
		}
		var err error
		r.RecordedAt, err = parseSQLiteTime(ts)
		if err != nil {
			return nil, fmt.Errorf("usage parse recorded_at %q: %w", ts, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage rows: %w", err)
	}
	return out, nil
}

// parseSQLiteTime mirrors chat.parseSQLiteTime — keeping the parser
// local to this package avoids a chat→usage import dependency.
func parseSQLiteTime(raw string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04:05.000"} {
		t, err := time.ParseInLocation(layout, raw, time.UTC)
		if err == nil {
			return t.UTC(), nil
		}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
