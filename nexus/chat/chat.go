// Package chat is the persistence and shared-API layer for the
// network's chat feed. It backs the broker's chat.send handling
// and exposes an in-process write path for the embedded Frame
// (F1.4b.1 of the aspect-funnel architecture).
//
// Two consumers today:
//
//   - The broker's WS handler (handleChatSendFrame) writes inbound
//     chat.send frames from out-of-process aspects via Insert.
//   - The in-process Frame's ChatGateway (F1.4b.2) writes via
//     Insert when the funnel's CommsRunner handles a send_chat tool
//     call from the model.
//
// Both paths land in the same chat_messages table, so Lock 6's
// replay-via-DB-query story works uniformly across in-process and
// out-of-process senders.
//
// Reads support Lock 2's chat.read tool: ListThread returns messages
// in a thread (or by since-id) so the model can pull context that
// wasn't pushed to it.

package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Message is the canonical wire/storage shape for a chat row. Mirrors
// the chat_messages table; CreatedAt is RFC 3339 UTC per Lock 6 of
// the architecture.
type Message struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	Content   string    `json:"content"`
	ReplyTo   int64     `json:"reply_to,omitempty"` // 0 = no parent
	Topic     string    `json:"topic,omitempty"`    // empty = default topic
	Kind      string    `json:"kind"`               // chat | hand | system
	CreatedAt time.Time `json:"created_at"`         // server-stamped at INSERT
}

// Store is the persistence seam for chat messages. Implementations
// must be safe for concurrent use; the broker writes from goroutines
// per WS connection and the in-process Frame writes from the
// deliberation goroutine.
type Store interface {
	// Insert persists a new message. Returns the assigned id and the
	// server-stamped CreatedAt. The store is the source of truth for
	// id and timestamp — callers MUST NOT pre-assign either.
	//
	// Empty `from` and empty `content` are rejected (validation lives
	// here so every caller gets the same guarantee). `replyTo`
	// referencing a non-existent message is allowed (FK is set-null
	// on delete) — the store does not enforce graph consistency.
	Insert(ctx context.Context, from, content string, replyTo int64, topic string) (Message, error)

	// ListThread returns messages in a thread, oldest first. If
	// `sinceID` is non-zero, only messages with id > sinceID are
	// returned. `limit` caps the result count (0 = unlimited but
	// callers should pass a sensible cap — large threads exist).
	//
	// `threadID` is the root message id of the thread (the message
	// with no reply_to that started it). The query walks the
	// reply_to chain via a recursive CTE — slow on huge trees, fine
	// for v1's traffic. F1.4c+ may move this to a denormalized
	// thread_id column if needed.
	ListThread(ctx context.Context, threadID, sinceID int64, limit int) ([]Message, error)
}

// SQLStore is the sqlite-backed Store. Holds a *sql.DB; safe for
// concurrent use because *sql.DB itself is.
type SQLStore struct {
	DB *sql.DB
}

// NewSQLStore constructs a SQLStore against the given DB. The DB
// must already have the schema applied (storage.Bootstrap).
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

// ErrEmptyFrom is returned by Insert when the sender is empty.
var ErrEmptyFrom = errors.New("chat.Insert: from is required")

// ErrEmptyContent is returned by Insert when the content is empty.
var ErrEmptyContent = errors.New("chat.Insert: content is required")

// Insert writes a row and returns the persisted Message. We use a
// single Exec with the default created_at to let SQLite stamp the
// time, then read it back with a SELECT — round-tripping ensures
// callers always see the exact value the database stored, including
// the milliseconds the schema's `datetime('now')` produces.
func (s *SQLStore) Insert(ctx context.Context, from, content string, replyTo int64, topic string) (Message, error) {
	if from == "" {
		return Message{}, ErrEmptyFrom
	}
	if content == "" {
		return Message{}, ErrEmptyContent
	}

	// reply_to gets stored as NULL if zero so the foreign key
	// constraint applies cleanly. thread_id is the schema's FK to
	// threads(id) and is NOT a free-form topic string — it's left
	// NULL for v1 chat traffic. Topic is currently dropped on the
	// floor; F1.4c will add a real topic column or table once topics
	// are wired into routing. For now the topic argument is reserved
	// at the API boundary so callers can already pass it through.
	_ = topic
	var replyToArg any = nil
	if replyTo != 0 {
		replyToArg = replyTo
	}

	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO chat_messages (thread_id, from_agent, content, reply_to, kind)
		VALUES (NULL, ?, ?, ?, 'chat')
	`, from, content, replyToArg)
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: exec: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: last id: %w", err)
	}

	// Read back the row so the caller sees the server-stamped
	// created_at. Done in the same transaction-less context — the
	// id we just minted is unique, so a separate SELECT is correct.
	var msg Message
	var replyToCol sql.NullInt64
	var topicCol sql.NullString
	var createdAtRaw string
	err = s.DB.QueryRowContext(ctx, `
		SELECT id, from_agent, content, reply_to, thread_id, kind, created_at
		FROM chat_messages WHERE id = ?
	`, id).Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw)
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: read-back: %w", err)
	}
	if replyToCol.Valid {
		msg.ReplyTo = replyToCol.Int64
	}
	if topicCol.Valid {
		msg.Topic = topicCol.String
	}
	msg.CreatedAt, err = parseSQLiteTime(createdAtRaw)
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: parse created_at %q: %w", createdAtRaw, err)
	}
	return msg, nil
}

// ListThread returns messages in the thread rooted at threadID,
// optionally filtered to id > sinceID. Walks reply_to via recursive
// CTE; ordered ascending by id (== oldest first since id is
// monotonic). Limit 0 = no SQL limit; callers should pass a
// reasonable cap.
func (s *SQLStore) ListThread(ctx context.Context, threadID, sinceID int64, limit int) ([]Message, error) {
	if threadID <= 0 {
		return nil, fmt.Errorf("chat.ListThread: thread_id must be positive")
	}

	// Build the CTE: start at threadID, recurse through reply_to.
	// The CTE returns every message whose ancestry chain includes
	// threadID (i.e. threadID itself plus descendants). This walks
	// downward from a root — id ascending = chronological.
	const query = `
		WITH RECURSIVE thread(id) AS (
			SELECT id FROM chat_messages WHERE id = ?
			UNION ALL
			SELECT cm.id FROM chat_messages cm
			JOIN thread t ON cm.reply_to = t.id
		)
		SELECT cm.id, cm.from_agent, cm.content, cm.reply_to, cm.thread_id, cm.kind, cm.created_at
		FROM chat_messages cm
		JOIN thread t ON cm.id = t.id
		WHERE cm.id > ?
		ORDER BY cm.id ASC
	`
	args := []any{threadID, sinceID}
	q := query
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("chat.ListThread: query: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var msg Message
		var replyToCol sql.NullInt64
		var topicCol sql.NullString
		var createdAtRaw string
		if err := rows.Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("chat.ListThread: scan: %w", err)
		}
		if replyToCol.Valid {
			msg.ReplyTo = replyToCol.Int64
		}
		if topicCol.Valid {
			msg.Topic = topicCol.String
		}
		msg.CreatedAt, err = parseSQLiteTime(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("chat.ListThread: parse created_at %q: %w", createdAtRaw, err)
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat.ListThread: rows: %w", err)
	}
	return out, nil
}

// parseSQLiteTime parses SQLite's default `datetime('now')` output.
// SQLite returns "YYYY-MM-DD HH:MM:SS" in UTC by default. Per Lock 6
// the wire format is RFC 3339 UTC with a trailing Z; we add the Z
// when re-rendering (FormatRFC3339 below) but parse the SQLite shape
// here. Returning a UTC time.Time at this seam keeps caller code
// independent of the storage representation.
func parseSQLiteTime(raw string) (time.Time, error) {
	// Layouts to try in order of likelihood. SQLite default is the
	// first; the second covers explicit-fractional cases.
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04:05.000"} {
		t, err := time.ParseInLocation(layout, raw, time.UTC)
		if err == nil {
			return t.UTC(), nil
		}
	}
	// Fall back to RFC 3339 (covers any future schema migration that
	// stores already-formatted strings).
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// FormatRFC3339 renders a Message's CreatedAt in the canonical wire
// format (RFC 3339 UTC, e.g. 2026-05-02T05:30:00Z). Per Lock 6
// of the architecture: the wire and storage are UTC; clients render
// in their local TZ at presentation.
func (m Message) FormatRFC3339() string {
	return m.CreatedAt.UTC().Format(time.RFC3339)
}
