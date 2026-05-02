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
	"encoding/json"
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

	// ToggleReaction adds the reaction if absent, removes it if
	// present. Returns the new state (true = now reacted, false =
	// now unreacted) so callers can render confirmation. Per Lock 3:
	// reactions are toggle-semantics — calling react_to(msg, "👍")
	// twice with the same reactor first reacts, then unreacts.
	ToggleReaction(ctx context.Context, msgID int64, reactor, emoji string) (reacted bool, err error)

	// AnnounceSharedFile records a file announcement: persists a
	// chat message with the description and links a shared_files row
	// to it. Returns both the announce message id (which is what the
	// model will see in chat) and the share id (so the model can
	// reference the file resource).
	AnnounceSharedFile(ctx context.Context, sharedBy, path, description string) (msgID, shareID int64, err error)

	// ShareFile records a direct share to recipients without posting
	// to chat. Recipients is a list of aspect ids (case-sensitive).
	// Returns the share id.
	ShareFile(ctx context.Context, sharedBy, path string, recipients []string) (shareID int64, err error)

	// ListSince returns messages with id > sinceID, oldest first,
	// across the whole chat history. Used for Lock 6 replay scans —
	// the broker filters by recipient via the RecipientPolicy
	// before deciding which to push back to a reconnecting aspect.
	//
	// Limit caps the result count; the broker should paginate by
	// passing a sensible cap (e.g. 500) and re-call with the new
	// since-cursor until the page is short of the limit.
	//
	// Doing the recipient filter at the broker layer (not here)
	// keeps this method simple and matches operator #9177's framing:
	// "give me all message for forge after id=N, deliver each as a
	// separate comm.send."
	ListSince(ctx context.Context, sinceID int64, limit int) ([]Message, error)
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

// ToggleReaction implements toggle-semantics for a (msg, reactor,
// emoji) triple. The unique constraint on (msg_id, reactor, emoji)
// is the source of truth: an INSERT that fails with constraint
// violation means the row exists, and we DELETE instead.
//
// Race window: two concurrent toggles from the same reactor on the
// same msg+emoji can interleave (insert→insert-fails-delete vs
// delete→insert), but the outcome is correct under any interleaving
// because each operation is atomic. The reported `reacted` may not
// match the caller's mental model under heavy concurrent toggling,
// but that's only a UI concern and doesn't break the table.
func (s *SQLStore) ToggleReaction(ctx context.Context, msgID int64, reactor, emoji string) (bool, error) {
	if msgID == 0 || reactor == "" || emoji == "" {
		return false, fmt.Errorf("chat.ToggleReaction: msgID, reactor, emoji all required")
	}
	// Try INSERT first; on UNIQUE conflict, DELETE.
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO chat_reactions (msg_id, reactor, emoji)
		VALUES (?, ?, ?)
	`, msgID, reactor, emoji)
	if err == nil {
		return true, nil
	}
	// Constraint violation = row exists, toggle off.
	res, derr := s.DB.ExecContext(ctx, `
		DELETE FROM chat_reactions
		WHERE msg_id = ? AND reactor = ? AND emoji = ?
	`, msgID, reactor, emoji)
	if derr != nil {
		return false, fmt.Errorf("chat.ToggleReaction: delete after insert conflict: %w (insert err: %v)", derr, err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Insert failed for a non-conflict reason (FK violation,
		// disk full, etc.) and the delete found nothing to remove.
		// Surface the original insert error.
		return false, fmt.Errorf("chat.ToggleReaction: %w", err)
	}
	return false, nil
}

// AnnounceSharedFile inserts a chat message announcing the file and
// a shared_files row linking back to it. Done in a transaction so
// the chat post and the file record commit together — partial state
// (an announcement message with no shared_files row, or vice versa)
// would confuse downstream consumers.
func (s *SQLStore) AnnounceSharedFile(ctx context.Context, sharedBy, path, description string) (int64, int64, error) {
	if sharedBy == "" || path == "" || description == "" {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: sharedBy, path, description all required")
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after commit; safe to defer unconditionally

	msgRes, err := tx.ExecContext(ctx, `
		INSERT INTO chat_messages (thread_id, from_agent, content, reply_to, kind)
		VALUES (NULL, ?, ?, NULL, 'chat')
	`, sharedBy, description)
	if err != nil {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: insert message: %w", err)
	}
	msgID, err := msgRes.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: msg id: %w", err)
	}

	shareRes, err := tx.ExecContext(ctx, `
		INSERT INTO shared_files (path, description, shared_by, announce_msg_id, recipients_json)
		VALUES (?, ?, ?, ?, NULL)
	`, path, description, sharedBy, msgID)
	if err != nil {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: insert share: %w", err)
	}
	shareID, err := shareRes.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: share id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("chat.AnnounceSharedFile: commit: %w", err)
	}
	return msgID, shareID, nil
}

// ShareFile inserts a shared_files row with no chat message — the
// share is private to the named recipients. recipients_json stores
// the recipient list as a JSON array; aspects looking up shares
// addressed to them filter on this column.
func (s *SQLStore) ShareFile(ctx context.Context, sharedBy, path string, recipients []string) (int64, error) {
	if sharedBy == "" || path == "" {
		return 0, fmt.Errorf("chat.ShareFile: sharedBy and path required")
	}
	if len(recipients) == 0 {
		return 0, fmt.Errorf("chat.ShareFile: at least one recipient required")
	}

	recipientsJSON, err := json.Marshal(recipients)
	if err != nil {
		return 0, fmt.Errorf("chat.ShareFile: marshal recipients: %w", err)
	}

	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO shared_files (path, description, shared_by, announce_msg_id, recipients_json)
		VALUES (?, NULL, ?, NULL, ?)
	`, path, sharedBy, string(recipientsJSON))
	if err != nil {
		return 0, fmt.Errorf("chat.ShareFile: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("chat.ShareFile: id: %w", err)
	}
	return id, nil
}

// ListSince returns messages with id > sinceID across the whole
// chat history, oldest first, capped at limit. Used for Lock 6
// replay scans — broker filters by recipient before delivering.
//
// Limit 0 means "no SQL limit"; callers should pass a sensible cap
// (e.g. 500). For huge offline windows the broker paginates by
// re-calling with the new since-cursor (the last id seen) until a
// page comes back short of the limit.
func (s *SQLStore) ListSince(ctx context.Context, sinceID int64, limit int) ([]Message, error) {
	q := `
		SELECT id, from_agent, content, reply_to, thread_id, kind, created_at
		FROM chat_messages
		WHERE id > ?
		ORDER BY id ASC
	`
	args := []any{sinceID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("chat.ListSince: query: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var msg Message
		var replyToCol sql.NullInt64
		var topicCol sql.NullString
		var createdAtRaw string
		if err := rows.Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("chat.ListSince: scan: %w", err)
		}
		if replyToCol.Valid {
			msg.ReplyTo = replyToCol.Int64
		}
		if topicCol.Valid {
			msg.Topic = topicCol.String
		}
		msg.CreatedAt, err = parseSQLiteTime(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("chat.ListSince: parse created_at %q: %w", createdAtRaw, err)
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat.ListSince: rows: %w", err)
	}
	return out, nil
}

// FormatRFC3339 renders a Message's CreatedAt in the canonical wire
// format (RFC 3339 UTC, e.g. 2026-05-02T05:30:00Z). Per Lock 6
// of the architecture: the wire and storage are UTC; clients render
// in their local TZ at presentation.
func (m Message) FormatRFC3339() string {
	return m.CreatedAt.UTC().Format(time.RFC3339)
}
