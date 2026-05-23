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
	"strings"
	"time"
)

// Message is the canonical wire/storage shape for a chat row. Mirrors
// the chat_messages table; CreatedAt is RFC 3339 UTC per Lock 6 of
// the architecture.
type Message struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	Content   string    `json:"content"`
	ReplyTo   int64     `json:"reply_to,omitempty"` // 0 = no parent (caller-provided hint)
	Topic     string    `json:"topic,omitempty"`    // empty = default topic
	Kind      string    `json:"kind"`               // chat | hand | system
	CreatedAt time.Time `json:"created_at"`         // server-stamped at INSERT

	// ParentMsgID is the resolved linked-list parent (task #226). For
	// a top-level message this is 0 (NULL in storage). For replies it
	// is "latest msg in the thread at INSERT time" — which may differ
	// from ReplyTo when concurrent replies-to-the-same-target race; the
	// second one in serialized order chains under the first.
	ParentMsgID int64 `json:"parent_msg_id,omitempty"`

	// ThreadRootMsgID is the canonical thread identity. For a top-level
	// message this equals ID; for replies it equals the ThreadRootMsgID
	// of the row pointed to by ReplyTo. Aspects derive per-thread
	// session ids from this (deterministic uuid_v5 keyed on
	// aspect_name + ":" + ThreadRootMsgID).
	ThreadRootMsgID int64 `json:"thread_root_msg_id,omitempty"`

	// ReplyCount is the number of descendants in the subtree rooted at
	// this message (recursive — depth ≥ 1). Populated only by ListPage
	// today; other reads leave it at zero. Powers the dashboard's
	// "N replies" badge. Per-message CTE is fine at page sizes ≤ 500;
	// if this becomes a hot path, see task #121 for the O(N) join.
	ReplyCount int `json:"reply_count,omitempty"`
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

	// GetByID fetches a single message by its id. Returns the zero
	// Message and a sql.ErrNoRows-shaped error when not found. Used
	// by the recipient policy's ParentLookup to find the author of
	// a parent message when computing reply-recipient.
	GetByID(ctx context.Context, id int64) (Message, error)

	// ThreadParticipants returns every distinct from_agent that has
	// posted into the thread containing msgID, in stable order.
	// Drives Slack/Teams-style routing: replies in a thread reach
	// all participants without the sender having to @-tag each one.
	//
	// The walk is by thread_root_msg_id (column added by #226, indexed
	// by idx_chat_thread_root_msg_id). When msgID is itself a reply,
	// its row carries the same thread_root_msg_id as the root, so the
	// lookup works regardless of where in the thread the operator
	// replies.
	//
	// Returns an empty slice (not an error) when msgID is unknown or
	// its thread has no recorded participants — callers (the recipient
	// policy) treat empty as "fall back to parent-author rule," which
	// preserves pre-thread-participants behaviour for transient lookup
	// misses.
	ThreadParticipants(ctx context.Context, msgID int64) ([]string, error)

	// ListShared returns shared_files rows, newest first, capped to
	// `limit`. Used by the list_shared comms tool — gives an aspect
	// the recently-shared file roster.
	ListShared(ctx context.Context, limit int) ([]SharedFile, error)

	// GetShared fetches a single shared_files row by id. Used by the
	// get_shared comms tool. Returns sql.ErrNoRows when absent.
	GetShared(ctx context.Context, id int64) (SharedFile, error)

	// ListReplies returns direct replies (one level deep) to
	// parentID, oldest first. Powers chat.replies.
	ListReplies(ctx context.Context, parentID int64) ([]Message, error)

	// ListPage returns chat messages for the operator dashboard's
	// main feed. id-based pagination: afterID returns id > afterID
	// in ASC order; beforeID returns id < beforeID in DESC order
	// then reversed to ASC; both zero = newest page (DESC LIMIT
	// then reversed). Returns hasMore=true when more rows existed
	// past the requested limit.
	ListPage(ctx context.Context, beforeID, afterID int64, limit int) (msgs []Message, hasMore bool, err error)

	// GetReactions returns the reactions for a batch of msg_ids,
	// keyed by msg_id. Missing keys mean no reactions found.
	GetReactions(ctx context.Context, msgIDs []int64) (map[int64][]Reaction, error)
}

// Reaction is one (aspect, emoji) row from chat_reactions, exposed
// for GetReactions. Returned by value; mirrors the schema.
type Reaction struct {
	MsgID  int64
	Aspect string
	Emoji  string
}

// SharedFile is the gateway-level representation of a shared_files
// row. RecipientsJSON stays as opaque JSON — the model can
// json-decode if it needs the recipient list, otherwise the
// announce/share split is signalled by AnnounceMsgID being non-zero.
type SharedFile struct {
	ID             int64  `json:"id"`
	Path           string `json:"path"`
	Description    string `json:"description,omitempty"`
	SharedBy       string `json:"shared_by"`
	AnnounceMsgID  int64  `json:"announce_msg_id,omitempty"`
	RecipientsJSON string `json:"recipients_json,omitempty"`
	CreatedAt      string `json:"created_at"`
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

// Insert writes a row and returns the persisted Message. Resolves the
// linked-list thread columns (parent_msg_id, thread_root_msg_id; task
// #226) inside a BEGIN IMMEDIATE transaction so concurrent replies
// chain via serialized INSERT order rather than branching the tree:
//
//   - Top-level (replyTo == 0): parent_msg_id = NULL, then
//     thread_root_msg_id = self id (UPDATE after the INSERT mints the
//     id; one-shot UPDATE keeps the row's identity self-rooted).
//   - Reply (replyTo > 0): thread_root_msg_id inherits from the
//     replyTo row's own thread_root (or the replyTo id itself, if the
//     target is a top-level). parent_msg_id = "latest id whose
//     thread_root_msg_id matches this thread root" — usually the
//     replyTo target, but when two senders race a reply to the same
//     parent the second sees the first as its parent, producing a
//     linked list rather than a DAG.
//
// BEGIN IMMEDIATE (configured via _txlock=immediate in the DSN, see
// storage.Open) is the concurrency primitive — SQLite acquires the
// RESERVED lock at BeginTx, serializing concurrent writers at BEGIN
// so the SELECT-MAX and INSERT pair is atomic against them.
func (s *SQLStore) Insert(ctx context.Context, from, content string, replyTo int64, topic string) (Message, error) {
	if from == "" {
		return Message{}, ErrEmptyFrom
	}
	if content == "" {
		return Message{}, ErrEmptyContent
	}

	// reply_to stored as NULL when zero so the foreign key applies
	// cleanly. thread_id is the schema's FK to threads(id), separate
	// concept from the linked-list thread_root_msg_id; left NULL for
	// v1 chat traffic. Topic remains reserved at the API boundary
	// pending #226-followup work that may surface named subjects.
	_ = topic
	var replyToArg any = nil
	if replyTo != 0 {
		replyToArg = replyTo
	}

	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: begin: %w", err)
	}
	// BEGIN IMMEDIATE happens at BeginTx (DSN sets _txlock=immediate),
	// so the RESERVED lock is held for the SELECT-MAX → INSERT pair
	// below and concurrent writers serialize at BEGIN. Defer rollback
	// so any early-return cleans up.
	defer func() { _ = tx.Rollback() }()

	var threadRootArg any = nil
	var parentArg any = nil
	if replyTo != 0 {
		// Inherit the thread root from the replyTo target. Coalesce
		// covers legacy rows where backfill hasn't populated the
		// column (theoretically impossible after Part 1's backfill,
		// but defending in depth keeps the FK consistent).
		var threadRoot sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(thread_root_msg_id, id) FROM chat_messages WHERE id = ?
		`, replyTo).Scan(&threadRoot)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// replyTo points at a non-existent row. FK is set-null so
			// the INSERT will still succeed; treat the message as a
			// new top-level for thread-root purposes (caller's
			// reply_to hint stays as a breadcrumb).
		case err != nil:
			return Message{}, fmt.Errorf("chat.Insert: resolve thread_root: %w", err)
		default:
			threadRootArg = threadRoot.Int64
			// Parent = latest msg already in this thread at INSERT
			// time. Under serialized writes this is deterministic;
			// concurrent replies-to-the-same-target chain rather than
			// fork.
			var latest sql.NullInt64
			err := tx.QueryRowContext(ctx, `
				SELECT MAX(id) FROM chat_messages WHERE thread_root_msg_id = ?
			`, threadRoot.Int64).Scan(&latest)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return Message{}, fmt.Errorf("chat.Insert: resolve parent: %w", err)
			}
			if latest.Valid {
				parentArg = latest.Int64
			} else {
				// Thread root exists but has no rows tagged to it
				// yet (legacy gap or single-row root). Use the
				// thread root itself as the parent.
				parentArg = threadRoot.Int64
			}
		}
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO chat_messages (thread_id, from_agent, content, reply_to, parent_msg_id, thread_root_msg_id, kind)
		VALUES (NULL, ?, ?, ?, ?, ?, 'chat')
	`, from, content, replyToArg, parentArg, threadRootArg)
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: exec: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: last id: %w", err)
	}

	// Top-level: thread_root = own id. Done as a follow-up UPDATE
	// inside the same transaction because the id is only known after
	// the INSERT.
	if threadRootArg == nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE chat_messages SET thread_root_msg_id = ? WHERE id = ?
		`, id, id); err != nil {
			return Message{}, fmt.Errorf("chat.Insert: self-root: %w", err)
		}
	}

	// Read back inside the same transaction so the caller sees the
	// server-stamped created_at plus the resolved thread columns.
	var msg Message
	var replyToCol, parentCol, threadRootCol sql.NullInt64
	var topicCol sql.NullString
	var createdAtRaw string
	err = tx.QueryRowContext(ctx, `
		SELECT id, from_agent, content, reply_to, thread_id, kind, created_at, parent_msg_id, thread_root_msg_id
		FROM chat_messages WHERE id = ?
	`, id).Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw, &parentCol, &threadRootCol)
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: read-back: %w", err)
	}
	if replyToCol.Valid {
		msg.ReplyTo = replyToCol.Int64
	}
	if topicCol.Valid {
		msg.Topic = topicCol.String
	}
	if parentCol.Valid {
		msg.ParentMsgID = parentCol.Int64
	}
	if threadRootCol.Valid {
		msg.ThreadRootMsgID = threadRootCol.Int64
	}
	msg.CreatedAt, err = parseSQLiteTime(createdAtRaw)
	if err != nil {
		return Message{}, fmt.Errorf("chat.Insert: parse created_at %q: %w", createdAtRaw, err)
	}

	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("chat.Insert: commit: %w", err)
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
		SELECT cm.id, cm.from_agent, cm.content, cm.reply_to, cm.thread_id, cm.kind, cm.created_at, cm.parent_msg_id, cm.thread_root_msg_id
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
		var replyToCol, parentCol, threadRootCol sql.NullInt64
		var topicCol sql.NullString
		var createdAtRaw string
		if err := rows.Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw, &parentCol, &threadRootCol); err != nil {
			return nil, fmt.Errorf("chat.ListThread: scan: %w", err)
		}
		if replyToCol.Valid {
			msg.ReplyTo = replyToCol.Int64
		}
		if topicCol.Valid {
			msg.Topic = topicCol.String
		}
		if parentCol.Valid {
			msg.ParentMsgID = parentCol.Int64
		}
		if threadRootCol.Valid {
			msg.ThreadRootMsgID = threadRootCol.Int64
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

// subtreeCounts computes the number of descendants (depth ≥ 1) for
// each id in `ids`, in a single recursive CTE. Returns a map keyed by
// ancestor id; ids with no descendants are present in the map with
// value zero so callers don't have to second-guess "missing == zero".
//
// One roundtrip regardless of |ids|. The CTE seeds with rows whose
// reply_to is in `ids`, walks down via UNION ALL, and groups by the
// originating ancestor (tracked through the recursion). Cost is
// proportional to the total reachable subtree, which at v1 chat
// volumes is small. For the operator dashboard's main feed the page
// is bounded at 500 and most threads are shallow — this is fine.
//
// If a future workload pushes per-thread sizes past ~1k, see task #121
// for the O(N) join replacement using a denormalized reply_count
// column on chat_messages.
func (s *SQLStore) subtreeCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		out[id] = 0 // ensure caller can read every id
	}
	if len(ids) == 0 {
		return out, nil
	}

	// Build the IN clause and arg list. SQLite's placeholder is `?`;
	// no driver-side $N variant. Repeat the args twice — once for the
	// initial seed (reply_to IN ids), once for the ancestor anchor.
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma

	q := `
		WITH RECURSIVE descend(ancestor, id) AS (
			-- Seed: direct children of any page-resident id, tagged
			-- with which ancestor each came from.
			SELECT cm.reply_to AS ancestor, cm.id AS id
			FROM chat_messages cm
			WHERE cm.reply_to IN (` + placeholders + `)

			UNION ALL

			-- Recurse: descendants of descendants, preserving the
			-- original ancestor tag so the final GROUP BY rolls back
			-- to the right page row.
			SELECT d.ancestor, cm.id
			FROM chat_messages cm
			JOIN descend d ON cm.reply_to = d.id
		)
		SELECT ancestor, COUNT(*) AS n
		FROM descend
		GROUP BY ancestor
	`

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("subtreeCounts: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ancestor int64
		var n int
		if err := rows.Scan(&ancestor, &n); err != nil {
			return nil, fmt.Errorf("subtreeCounts: scan: %w", err)
		}
		out[ancestor] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subtreeCounts: rows: %w", err)
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

// ToggleReaction enforces single-emoji-per-reactor semantics
// (decision 2026-05-12): a given (msg_id, reactor) pair holds at most
// ONE row at any time. The legacy multi-emoji-stack behavior is gone.
//
// Three observable outcomes for ReactTo(msg, reactor, emoji):
//
//  1. No existing row for (msg, reactor): INSERT new emoji. reacted=true.
//  2. Existing row with same emoji: DELETE it (pure toggle-off).
//     reacted=false.
//  3. Existing row with DIFFERENT emoji: DELETE old, INSERT new
//     (replace). reacted=true.
//
// The schema's old UNIQUE(msg_id, reactor, emoji) constraint stays in
// place — it's still valid under the new semantics (we just never
// insert a second-emoji row for the same reactor). Legacy rows from
// before this change (one reactor with multiple emojis on one msg)
// are tolerated: the first re-react from that reactor on that msg
// deletes ALL their existing rows in the transaction below, then
// inserts a single new one. Migration-free collapse.
//
// Atomicity: DELETE-then-INSERT runs inside a transaction so a
// concurrent reader never observes "no reaction at all" mid-toggle
// and can't catch us between the two statements.
func (s *SQLStore) ToggleReaction(ctx context.Context, msgID int64, reactor, emoji string) (bool, error) {
	if msgID == 0 || reactor == "" || emoji == "" {
		return false, fmt.Errorf("chat.ToggleReaction: msgID, reactor, emoji all required")
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("chat.ToggleReaction: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// Read what (if anything) the reactor already has on this msg.
	// At most one row in the steady state; tolerate >1 for legacy
	// rows by deleting them all.
	rows, err := tx.QueryContext(ctx, `
		SELECT emoji FROM chat_reactions
		WHERE msg_id = ? AND reactor = ?
	`, msgID, reactor)
	if err != nil {
		return false, fmt.Errorf("chat.ToggleReaction: select: %w", err)
	}
	var existing []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			rows.Close()
			return false, fmt.Errorf("chat.ToggleReaction: scan: %w", err)
		}
		existing = append(existing, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("chat.ToggleReaction: rows: %w", err)
	}

	// Determine the outcome BEFORE we mutate, so the return value is
	// derivable from intent rather than side-effects.
	sameOnly := len(existing) == 1 && existing[0] == emoji

	// Always delete every existing row for (msg, reactor) — collapses
	// legacy stacks and clears the slot for the new emoji (or for the
	// pure toggle-off case).
	if len(existing) > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM chat_reactions
			WHERE msg_id = ? AND reactor = ?
		`, msgID, reactor); err != nil {
			return false, fmt.Errorf("chat.ToggleReaction: delete: %w", err)
		}
	}

	if !sameOnly {
		// Insert the new emoji (cases 1 + 3 in the docstring).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_reactions (msg_id, reactor, emoji)
			VALUES (?, ?, ?)
		`, msgID, reactor, emoji); err != nil {
			return false, fmt.Errorf("chat.ToggleReaction: insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("chat.ToggleReaction: commit: %w", err)
	}
	// `reacted` matches "is there now a row for (msg, reactor, emoji)?"
	// — false only for the pure toggle-off case.
	return !sameOnly, nil
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
		SELECT id, from_agent, content, reply_to, thread_id, kind, created_at, parent_msg_id, thread_root_msg_id
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
		var replyToCol, parentCol, threadRootCol sql.NullInt64
		var topicCol sql.NullString
		var createdAtRaw string
		if err := rows.Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw, &parentCol, &threadRootCol); err != nil {
			return nil, fmt.Errorf("chat.ListSince: scan: %w", err)
		}
		if replyToCol.Valid {
			msg.ReplyTo = replyToCol.Int64
		}
		if topicCol.Valid {
			msg.Topic = topicCol.String
		}
		if parentCol.Valid {
			msg.ParentMsgID = parentCol.Int64
		}
		if threadRootCol.Valid {
			msg.ThreadRootMsgID = threadRootCol.Int64
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

// GetByID fetches a single chat message by id. Returns sql.ErrNoRows
// when not found so callers can distinguish "missing parent" from
// "DB error" — RecipientPolicy treats no-rows as "no parent author"
// rather than aborting recipient computation.
func (s *SQLStore) GetByID(ctx context.Context, id int64) (Message, error) {
	var msg Message
	var replyToCol, parentCol, threadRootCol sql.NullInt64
	var topicCol sql.NullString
	var createdAtRaw string
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, from_agent, content, reply_to, thread_id, kind, created_at, parent_msg_id, thread_root_msg_id
		FROM chat_messages WHERE id = ?
	`, id).Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw, &parentCol, &threadRootCol)
	if err != nil {
		return Message{}, err
	}
	if replyToCol.Valid {
		msg.ReplyTo = replyToCol.Int64
	}
	if topicCol.Valid {
		msg.Topic = topicCol.String
	}
	if parentCol.Valid {
		msg.ParentMsgID = parentCol.Int64
	}
	if threadRootCol.Valid {
		msg.ThreadRootMsgID = threadRootCol.Int64
	}
	msg.CreatedAt, err = parseSQLiteTime(createdAtRaw)
	if err != nil {
		return Message{}, fmt.Errorf("chat.GetByID: parse created_at %q: %w", createdAtRaw, err)
	}
	return msg, nil
}

// ThreadParticipants returns every distinct from_agent in the thread
// containing msgID, in alphabetical order. Walks chat_messages by
// thread_root_msg_id: we look up the given msg's thread_root in a
// subquery, then collect the distinct senders sharing that root.
//
// Returns an empty slice with no error when msgID is unknown, when
// the thread has only one participant (the sender), or when the
// thread_root_msg_id column isn't populated for the row (legacy rows
// before the #226 migration may have NULL until the backfill catches
// them — backfillChatThreadRoots in storage/schema.go handles this
// idempotently on every boot, so the gap closes by the next start).
func (s *SQLStore) ThreadParticipants(ctx context.Context, msgID int64) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT from_agent
		FROM chat_messages
		WHERE thread_root_msg_id = (
			SELECT thread_root_msg_id FROM chat_messages WHERE id = ?
		)
		AND thread_root_msg_id IS NOT NULL
		ORDER BY from_agent
	`, msgID)
	if err != nil {
		return nil, fmt.Errorf("chat.ThreadParticipants: query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("chat.ThreadParticipants: scan: %w", err)
		}
		if name != "" {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat.ThreadParticipants: rows: %w", err)
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

// ListShared implements Store. Returns shared_files rows newest-first
// up to `limit`. Operator-callers pass a sane cap (50-100); the model
// shouldn't see thousands of rows in one tool result.
func (s *SQLStore) ListShared(ctx context.Context, limit int) ([]SharedFile, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, path, description, shared_by, announce_msg_id, recipients_json, created_at
		FROM shared_files
		ORDER BY id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("chat.ListShared: %w", err)
	}
	defer rows.Close()
	var out []SharedFile
	for rows.Next() {
		f, err := scanSharedFile(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("chat.ListShared scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetShared implements Store. Returns sql.ErrNoRows when the id
// doesn't exist — callers wrap into tool-result-shaped JSON.
func (s *SQLStore) GetShared(ctx context.Context, id int64) (SharedFile, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, path, description, shared_by, announce_msg_id, recipients_json, created_at
		FROM shared_files WHERE id = ?
	`, id)
	f, err := scanSharedFile(row.Scan)
	if err != nil {
		return SharedFile{}, err
	}
	return f, nil
}

// scanSharedFile populates a SharedFile from sql Scan-shape.
func scanSharedFile(scan func(...any) error) (SharedFile, error) {
	var f SharedFile
	var desc, recips sql.NullString
	var announce sql.NullInt64
	if err := scan(&f.ID, &f.Path, &desc, &f.SharedBy, &announce, &recips, &f.CreatedAt); err != nil {
		return SharedFile{}, err
	}
	if desc.Valid {
		f.Description = desc.String
	}
	if recips.Valid {
		f.RecipientsJSON = recips.String
	}
	if announce.Valid {
		f.AnnounceMsgID = announce.Int64
	}
	return f, nil
}

// ListReplies returns direct replies to parentID, oldest first.
// "Direct" means one level: messages whose reply_to == parentID.
// Powers the dashboard chat.replies frame; the SPA recurses if it
// needs the full subtree.
func (s *SQLStore) ListReplies(ctx context.Context, parentID int64) ([]Message, error) {
	if parentID <= 0 {
		return nil, fmt.Errorf("chat.ListReplies: parent_id must be positive")
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, from_agent, content, reply_to, thread_id, kind, created_at, parent_msg_id, thread_root_msg_id
		FROM chat_messages
		WHERE reply_to = ?
		ORDER BY id ASC
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("chat.ListReplies: query: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var msg Message
		var replyToCol, parentCol, threadRootCol sql.NullInt64
		var topicCol sql.NullString
		var createdAtRaw string
		if err := rows.Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw, &parentCol, &threadRootCol); err != nil {
			return nil, fmt.Errorf("chat.ListReplies: scan: %w", err)
		}
		if replyToCol.Valid {
			msg.ReplyTo = replyToCol.Int64
		}
		if topicCol.Valid {
			msg.Topic = topicCol.String
		}
		if parentCol.Valid {
			msg.ParentMsgID = parentCol.Int64
		}
		if threadRootCol.Valid {
			msg.ThreadRootMsgID = threadRootCol.Int64
		}
		t, err := parseSQLiteTime(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("chat.ListReplies: time parse: %w", err)
		}
		msg.CreatedAt = t
		out = append(out, msg)
	}
	return out, rows.Err()
}

// ListPage powers the dashboard's main chat feed with id-based
// pagination. Three modes:
//
//   - afterID > 0, beforeID == 0: rows with id > afterID, ASC,
//     limited. Used for "load newer than what I have."
//   - beforeID > 0, afterID == 0: rows with id < beforeID, DESC
//     limit then reversed to ASC. Used for "load older than what
//     I have."
//   - both zero: newest page — same as beforeID = MaxInt64.
//
// Both modes set together is rejected (caller bug). limit defaults
// to 100 when zero, capped at 500.
func (s *SQLStore) ListPage(ctx context.Context, beforeID, afterID int64, limit int) ([]Message, bool, error) {
	if beforeID > 0 && afterID > 0 {
		return nil, false, fmt.Errorf("chat.ListPage: pass one of before_id or after_id, not both")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	// Probe with limit+1 so we can report has_more without a second
	// COUNT query.
	probe := limit + 1

	var (
		q    string
		args []any
	)
	switch {
	case afterID > 0:
		q = `SELECT id, from_agent, content, reply_to, thread_id, kind, created_at, parent_msg_id, thread_root_msg_id
		     FROM chat_messages WHERE id > ? ORDER BY id ASC LIMIT ?`
		args = []any{afterID, probe}
	default:
		// before_id > 0 OR both zero (newest page).
		anchor := beforeID
		if anchor <= 0 {
			anchor = 1<<63 - 1 // MaxInt64
		}
		q = `SELECT id, from_agent, content, reply_to, thread_id, kind, created_at, parent_msg_id, thread_root_msg_id
		     FROM chat_messages WHERE id < ? ORDER BY id DESC LIMIT ?`
		args = []any{anchor, probe}
	}

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("chat.ListPage: query: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var msg Message
		var replyToCol, parentCol, threadRootCol sql.NullInt64
		var topicCol sql.NullString
		var createdAtRaw string
		if err := rows.Scan(&msg.ID, &msg.From, &msg.Content, &replyToCol, &topicCol, &msg.Kind, &createdAtRaw, &parentCol, &threadRootCol); err != nil {
			return nil, false, fmt.Errorf("chat.ListPage: scan: %w", err)
		}
		if replyToCol.Valid {
			msg.ReplyTo = replyToCol.Int64
		}
		if topicCol.Valid {
			msg.Topic = topicCol.String
		}
		if parentCol.Valid {
			msg.ParentMsgID = parentCol.Int64
		}
		if threadRootCol.Valid {
			msg.ThreadRootMsgID = threadRootCol.Int64
		}
		t, err := parseSQLiteTime(createdAtRaw)
		if err != nil {
			return nil, false, fmt.Errorf("chat.ListPage: time parse: %w", err)
		}
		msg.CreatedAt = t
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := false
	if len(msgs) > limit {
		msgs = msgs[:limit]
		hasMore = true
	}

	// Per-message subtree counts. One recursive CTE that walks the
	// whole reply graph starting from every page-resident id at once,
	// excludes the seeds, and groups by ancestor. Single roundtrip,
	// no per-message N+1. Cost is bounded by total descendants under
	// the page — at v1 thread depths (handfuls of replies per root)
	// this is fine; if a single thread ever grows to thousands, see
	// task #121 for the O(N) join replacement.
	if len(msgs) > 0 {
		ids := make([]int64, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		counts, err := s.subtreeCounts(ctx, ids)
		if err != nil {
			// Counts are a UX nicety; don't fail the page on a count
			// query error. Surface in logs at the caller's tier.
			return nil, false, fmt.Errorf("chat.ListPage: subtree counts: %w", err)
		}
		for i := range msgs {
			msgs[i].ReplyCount = counts[msgs[i].ID]
		}
	}

	// before/newest paths queried DESC; reverse to oldest-first.
	if afterID == 0 {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	return msgs, hasMore, nil
}

// GetReactions returns reactions for a batch of msg_ids, keyed by
// msg_id. Empty input → empty map. Missing msg_ids in the result
// mean the message had no reactions (callers don't need to
// distinguish "no reactions" from "msg doesn't exist" — both render
// as "no reactions" in the dashboard UI).
//
// The IN-list expansion is unbounded by design: callers pass the
// page they've already paged from chat_messages, so the size is
// already capped at the page limit (≤500). A 500-element IN list
// is tolerable on SQLite.
func (s *SQLStore) GetReactions(ctx context.Context, msgIDs []int64) (map[int64][]Reaction, error) {
	if len(msgIDs) == 0 {
		return map[int64][]Reaction{}, nil
	}
	// Build "?, ?, ?, ..." placeholder string + arg slice.
	placeholders := strings.Repeat("?,", len(msgIDs))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
	args := make([]any, len(msgIDs))
	for i, id := range msgIDs {
		args[i] = id
	}

	q := `SELECT msg_id, reactor, emoji
	      FROM chat_reactions
	      WHERE msg_id IN (` + placeholders + `)
	      ORDER BY msg_id ASC, id ASC`

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("chat.GetReactions: query: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Reaction)
	for rows.Next() {
		var r Reaction
		if err := rows.Scan(&r.MsgID, &r.Aspect, &r.Emoji); err != nil {
			return nil, fmt.Errorf("chat.GetReactions: scan: %w", err)
		}
		out[r.MsgID] = append(out[r.MsgID], r)
	}
	return out, rows.Err()
}
