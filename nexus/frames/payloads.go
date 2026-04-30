package frames

import (
	"time"

	"github.com/nexus-cw/nexus/shared/schemas"
)

// -------------------------------------------------------------------
// Registration
// -------------------------------------------------------------------

// RegisterPayload is the aspect-register frame body. Mirrors the
// existing RegisterRequest from shared/schemas so migration from the
// HTTP-register path is a shape-preserving move.
type RegisterPayload struct {
	schemas.RegisterRequest
}

// RegisterAckPayload tells the client what cadence to heartbeat at
// (for app-level heartbeats if/when we add them; v1 relies on WS
// ping/pong) and when the server will consider them stale.
type RegisterAckPayload struct {
	HeartbeatIntervalS int `json:"heartbeat_interval_s"`
	StaleAfterS        int `json:"stale_after_s"`
}

// DeregisterPayload is sent on graceful shutdown.
type DeregisterPayload struct {
	schemas.DeregisterRequest
}

// OutpostRegisterPayload carries what the Nexus needs to know about
// a newly-connected Outpost.
type OutpostRegisterPayload struct {
	OutpostID    string            `json:"outpost_id"`
	Host         string            `json:"host"`
	Version      string            `json:"version"`
	Capabilities []string          `json:"capabilities"`
	StartedAt    time.Time         `json:"started_at"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// OutpostRegisterAckPayload is the upstream acknowledgement.
type OutpostRegisterAckPayload struct {
	HeartbeatIntervalS int `json:"heartbeat_interval_s"`
}

// OutpostDeregisterPayload — graceful Outpost shutdown.
type OutpostDeregisterPayload struct {
	OutpostID string `json:"outpost_id"`
	Reason    string `json:"reason,omitempty"`
}

// ViaOutpostStamp is attached to aspect registration frames that are
// forwarded upward by an Outpost. Nexus uses it to record the route.
// Serialised as a sibling field on the forwarded register payload.
type ViaOutpostStamp struct {
	ViaOutpost string `json:"via_outpost,omitempty"`
}

// ForwardedRegisterPayload is what an Outpost sends up after an
// aspect registers locally.
type ForwardedRegisterPayload struct {
	schemas.RegisterRequest
	ViaOutpostStamp
}

// -------------------------------------------------------------------
// Turn dispatch
// -------------------------------------------------------------------

// TurnPayload is sent upstream → aspect to trigger a single turn.
type TurnPayload struct {
	Prompt        string `json:"prompt"`
	SystemPrompt  string `json:"system_prompt,omitempty"`
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinking_level,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
}

// TurnResultPayload is the aspect's reply.
type TurnResultPayload struct {
	Output     string     `json:"output"`
	StopReason string     `json:"stop_reason"`
	Tokens     TokenUsage `json:"tokens"`
	EntryIDs   []string   `json:"entry_ids"`
}

// TokenUsage mirrors provider token accounting without pulling the
// providers package into every frame handler.
type TokenUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Total  int `json:"total"`
}

// -------------------------------------------------------------------
// Dispatch
// -------------------------------------------------------------------
//
// Per hand-dispatch v0.1 §5.1: protocol uses generic vocabulary.
// `dispatch` is a unit of work submitted by an aspect to the
// dispatcher; the dispatcher boots an interchangeable worker slot
// loaded with the dispatching aspect's identity framing. There is no
// "target aspect" (the worker is the dispatching aspect on a fresh
// turn) and no "hand name" (slots are anonymous; persona is inherited
// from the dispatcher per-dispatch).

// DispatchPayload is sent by an aspect to the dispatcher to enqueue a
// unit of work. The dispatcher fairness-schedules and spawns a worker
// loaded with the dispatching aspect's home (NEXUS.md / SOUL.md /
// PRIMER). Per spec §2.2 queue items carry: aspect, thread, payload,
// submitted_at, dispatch_id. submitted_at lives on the envelope
// timestamp; the rest are body fields here.
type DispatchPayload struct {
	Aspect     string         `json:"aspect"`
	Thread     string         `json:"thread,omitempty"`
	DispatchID string         `json:"dispatch_id,omitempty"`
	Payload    map[string]any `json:"payload"`
}

// DispatchResultPayload comes back once a worker has completed its
// turn. Identity flows: the worker booted as the dispatching aspect,
// so the result is attributed to that aspect (§2.1 result attribution).
type DispatchResultPayload struct {
	Aspect     string         `json:"aspect"`
	Thread     string         `json:"thread,omitempty"`
	DispatchID string         `json:"dispatch_id,omitempty"`
	Output     map[string]any `json:"output"`
	Tokens     TokenUsage     `json:"tokens"`
	Model      string         `json:"model,omitempty"`
	Error      string         `json:"error,omitempty"` // non-empty if the worker ran but failed
}

// DispatchErrorPayload signals that dispatch couldn't happen at all —
// queue saturated, hard-ceiling reached, identity mismatch, etc.
// Distinct from DispatchResult with an error field (which means the
// worker DID run and failed during execution).
type DispatchErrorPayload struct {
	Aspect     string `json:"aspect,omitempty"`
	DispatchID string `json:"dispatch_id,omitempty"`
	Reason     string `json:"reason"`
	Code       string `json:"code"` // "queue_full" | "hard_ceiling" | "identity_mismatch" | ...
}

// -------------------------------------------------------------------
// Chat / comms
// -------------------------------------------------------------------

// ChatSendPayload is an aspect posting to the shared chat bus.
type ChatSendPayload struct {
	From     string   `json:"from"`
	Content  string   `json:"content"`
	ReplyTo  int      `json:"reply_to,omitempty"`
	Thread   string   `json:"thread,omitempty"`
	Mentions []string `json:"mentions,omitempty"`
	Topic    string   `json:"topic,omitempty"`
}

// ChatDeliverPayload is a message being delivered to an aspect that
// should see it (mentioned, reply, thread participant, etc.).
type ChatDeliverPayload struct {
	ID      int      `json:"id"`
	From    string   `json:"from"`
	Content string   `json:"content"`
	ReplyTo int      `json:"reply_to,omitempty"`
	Thread  string   `json:"thread,omitempty"`
	At      string   `json:"at"`
	Reason  string   `json:"reason"` // why this aspect is being notified: "mention", "reply", "thread", "all"
}

// ChatReactionPayload toggles an emoji reaction.
type ChatReactionPayload struct {
	From  string `json:"from"`
	MsgID int    `json:"msg_id"`
	Emoji string `json:"emoji"`
}

// ChatReadPayload is a request for a specific message or thread.
// Response comes back as a ChatDeliverPayload (for a single message)
// or a ChatReadResultPayload (for a thread).
type ChatReadPayload struct {
	MsgID    int    `json:"msg_id,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

// -------------------------------------------------------------------
// Knowledge
// -------------------------------------------------------------------

// KnowledgeStorePayload is an aspect writing a knowledge entry.
type KnowledgeStorePayload struct {
	Topic   string `json:"topic"`
	Content string `json:"content"`
	Shared  bool   `json:"shared,omitempty"`
}

// KnowledgeSearchPayload is an aspect querying the knowledge store.
type KnowledgeSearchPayload struct {
	Text         string   `json:"text"`
	OwnAgent     bool     `json:"own_agent,omitempty"`
	Shared       bool     `json:"shared,omitempty"`
	Peers        []string `json:"peers,omitempty"`
	TopK         int      `json:"top_k,omitempty"`
	MaxRank      float64  `json:"max_rank,omitempty"`
}

// KnowledgeSearchResultPayload is the response.
type KnowledgeSearchResultPayload struct {
	Hits []KnowledgeHit `json:"hits"`
}

// KnowledgeHit mirrors the knowledge store Hit shape without importing
// the knowledge package into frames (keeps the dependency graph flat).
type KnowledgeHit struct {
	ID        int64   `json:"id"`
	FromAgent string  `json:"from_agent"`
	Topic     string  `json:"topic"`
	Content   string  `json:"content"`
	Shared    bool    `json:"shared"`
	UpdatedAt string  `json:"updated_at"`
	Score     float64 `json:"score"`
	Matched   string  `json:"matched"`
}

// -------------------------------------------------------------------
// Session projection
// -------------------------------------------------------------------

// SessionEntryAppendedPayload is emitted by an aspect every time it
// appends to its local session JSONL. Nexus stores this in a read-
// only projection table for dashboard rendering. NOT a source of
// truth — the local JSONL owns the data.
type SessionEntryAppendedPayload struct {
	Aspect    string         `json:"aspect"`
	SessionID string         `json:"session_id"`
	EntryID   string         `json:"entry_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	EntryKind string         `json:"entry_kind"`
	TS        time.Time      `json:"ts"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// SessionRewindPayload signals that the aspect moved its active head
// to an earlier entry.
type SessionRewindPayload struct {
	Aspect      string `json:"aspect"`
	SessionID   string `json:"session_id"`
	NewHeadID   string `json:"new_head_id"`
	PreviousID  string `json:"previous_id"`
}

// SessionForkPayload signals that the aspect forked to a new branch.
type SessionForkPayload struct {
	Aspect     string `json:"aspect"`
	SessionID  string `json:"session_id"`
	ForkPoint  string `json:"fork_point"`
	NewHeadID  string `json:"new_head_id"`
}

// -------------------------------------------------------------------
// Lifecycle
// -------------------------------------------------------------------

// ShutdownPayload is sent upstream → aspect (or Outpost → aspects, or
// Nexus → Outposts) to request a graceful wind-down.
type ShutdownPayload struct {
	Reason        string `json:"reason"`
	GracePeriodS  int    `json:"grace_period_s,omitempty"`
}
