package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TriageDecision captures the outcome the funnel records for every
// inbox msg_id the aspect processes during a turn. Per the
// 2026-05-10-funnel-triage-contract spec, every inbox item must land
// in this table — silent drops are the bug we're closing.
type TriageDecision struct {
	ID         int64
	AspectName string
	MsgID      int64
	TurnID     string
	Decision   string // "reply" or "skip"
	Reason     string
	ReplyMsgID int64 // 0 when no reply
	CreatedAt  time.Time
}

// TriageStore is the persistence seam for inbox triage. SQLTriageStore
// is the production impl; tests can substitute an in-memory mock to
// keep funnel-level coverage free of sqlite setup.
type TriageStore interface {
	// Record writes a single triage decision. Caller supplies all
	// fields except ID and CreatedAt — the store stamps both. ReplyMsgID
	// zero means "no reply produced" (typical for skip decisions but
	// also valid for reply decisions where the model used send_chat
	// without specifying reply_to, so the linkage is implicit).
	//
	// Duplicate (aspect_name, msg_id, turn_id) is NOT prevented by
	// the schema — the model could legitimately call triage() twice
	// for the same msg if it changes its mind. The enforcer at
	// turn-end reads the latest decision per msg_id to determine
	// whether synthetic skips are needed.
	Record(ctx context.Context, dec TriageDecision) (int64, error)

	// ListByTurn returns every triage decision the funnel persisted
	// for the given turn_id. Used by the enforcer to identify which
	// inbox msg_ids the model failed to triage.
	ListByTurn(ctx context.Context, turnID string) ([]TriageDecision, error)

	// ListByAspect returns recent decisions for an aspect, newest
	// first. Used by the dashboard's 1:1 view; lands here so the
	// store has a single read API rather than scattering queries
	// across consumers.
	ListByAspect(ctx context.Context, aspectName string, limit int) ([]TriageDecision, error)
}

// SQLTriageStore is the sqlite-backed TriageStore. Safe for
// concurrent use (sql.DB is).
type SQLTriageStore struct {
	DB *sql.DB
}

// NewSQLTriageStore constructs a SQLTriageStore. DB must already have
// the schema applied via storage.Bootstrap (inbox_triage table at
// schema.sql §inbox_triage).
func NewSQLTriageStore(db *sql.DB) *SQLTriageStore {
	return &SQLTriageStore{DB: db}
}

// Record inserts a triage row. Returns the assigned id.
func (s *SQLTriageStore) Record(ctx context.Context, dec TriageDecision) (int64, error) {
	if dec.AspectName == "" {
		return 0, errors.New("chat.TriageStore.Record: aspect_name required")
	}
	if dec.MsgID == 0 {
		return 0, errors.New("chat.TriageStore.Record: msg_id required")
	}
	if dec.TurnID == "" {
		return 0, errors.New("chat.TriageStore.Record: turn_id required")
	}
	if dec.Decision != "reply" && dec.Decision != "skip" {
		return 0, fmt.Errorf("chat.TriageStore.Record: decision must be 'reply' or 'skip', got %q", dec.Decision)
	}

	const query = `
		INSERT INTO inbox_triage (aspect_name, msg_id, turn_id, decision, reason, reply_msg_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	var replyMsgID sql.NullInt64
	if dec.ReplyMsgID > 0 {
		replyMsgID = sql.NullInt64{Int64: dec.ReplyMsgID, Valid: true}
	}
	res, err := s.DB.ExecContext(ctx, query,
		dec.AspectName, dec.MsgID, dec.TurnID, dec.Decision, dec.Reason, replyMsgID)
	if err != nil {
		return 0, fmt.Errorf("chat.TriageStore.Record: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("chat.TriageStore.Record: last insert id: %w", err)
	}
	return id, nil
}

// ListByTurn returns triage rows for a single turn, oldest first.
// The funnel's enforcer uses this to compute which inbox msg_ids
// were left untriaged.
func (s *SQLTriageStore) ListByTurn(ctx context.Context, turnID string) ([]TriageDecision, error) {
	if turnID == "" {
		return nil, errors.New("chat.TriageStore.ListByTurn: turn_id required")
	}
	const query = `
		SELECT id, aspect_name, msg_id, turn_id, decision, reason, reply_msg_id, created_at
		FROM inbox_triage
		WHERE turn_id = ?
		ORDER BY id ASC
	`
	return s.scan(ctx, query, turnID)
}

// ListByAspect returns recent decisions for an aspect, newest first.
// limit <= 0 falls back to 100; callers should pass a sane cap.
func (s *SQLTriageStore) ListByAspect(ctx context.Context, aspectName string, limit int) ([]TriageDecision, error) {
	if aspectName == "" {
		return nil, errors.New("chat.TriageStore.ListByAspect: aspect_name required")
	}
	if limit <= 0 {
		limit = 100
	}
	const query = `
		SELECT id, aspect_name, msg_id, turn_id, decision, reason, reply_msg_id, created_at
		FROM inbox_triage
		WHERE aspect_name = ?
		ORDER BY id DESC
		LIMIT ?
	`
	return s.scan(ctx, query, aspectName, limit)
}

func (s *SQLTriageStore) scan(ctx context.Context, query string, args ...any) ([]TriageDecision, error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chat.TriageStore: query: %w", err)
	}
	defer rows.Close()

	var out []TriageDecision
	for rows.Next() {
		var dec TriageDecision
		var replyMsgID sql.NullInt64
		var createdAtRaw string
		if err := rows.Scan(&dec.ID, &dec.AspectName, &dec.MsgID, &dec.TurnID,
			&dec.Decision, &dec.Reason, &replyMsgID, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("chat.TriageStore: scan: %w", err)
		}
		if replyMsgID.Valid {
			dec.ReplyMsgID = replyMsgID.Int64
		}
		dec.CreatedAt, err = parseSQLiteTime(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("chat.TriageStore: parse created_at %q: %w", createdAtRaw, err)
		}
		out = append(out, dec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat.TriageStore: rows: %w", err)
	}
	return out, nil
}
