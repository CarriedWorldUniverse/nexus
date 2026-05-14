package frames

import (
	"encoding/json"
	"time"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// -------------------------------------------------------------------
// Registration
// -------------------------------------------------------------------

// RegisterPayload is the aspect-register frame body. Mirrors the
// existing RegisterRequest from shared/schemas so migration from the
// HTTP-register path is a shape-preserving move.
type RegisterPayload struct {
	schemas.RegisterRequest

	// SinceMsgID, when non-zero, requests Lock 6 replay: Nexus
	// queries the chat DB for messages addressed to this aspect
	// with msg_id > SinceMsgID and emits each as its own
	// ChatDeliverPayload (with Replay=true) before resuming live
	// delivery. Aspects with no persisted state file (cold start,
	// state file lost) leave this 0; they get only live frames
	// going forward — acceptable degradation per Lock 6.
	SinceMsgID int64 `json:"since_msg_id,omitempty"`
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

	// SinceMsgID mirrors RegisterPayload.SinceMsgID for forwarded
	// aspects: outposts MUST propagate the field if the downstream
	// aspect set it. Lock 6 replay applies regardless of whether the
	// connection is direct or routed via an Outpost.
	SinceMsgID int64 `json:"since_msg_id,omitempty"`
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
//
// For hard_ceiling rejections per spec §6.3, Active/SoftCap/Limit are
// populated so the caller can decide whether to retry, abort, or
// surface upward.
type DispatchErrorPayload struct {
	Aspect     string `json:"aspect,omitempty"`
	DispatchID string `json:"dispatch_id,omitempty"`
	Reason     string `json:"reason"`
	Code       string `json:"code"` // "queue_full" | "hard_ceiling" | "identity_mismatch" | ...
	Active     int    `json:"active,omitempty"`
	SoftCap    int    `json:"soft_cap,omitempty"`
	Limit      int    `json:"limit,omitempty"`
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
//
// Lock 6 (operator #9206/#9213/#9218): ReceivedAt is the message's
// server-stamped Nexus-side ingestion time, RFC 3339 UTC. Aspects
// surface this to the model on replay so deliberation can decide
// whether a stale request is still actionable. Same field for live
// frames (near-zero age) and replay frames (potentially hours old).
//
// The ID field is the chat msg_id, which doubles as the cursor for
// Lock 6's replay-via-DB-query path: aspects persist the highest ID
// they've processed and pass it as `since_msg_id` on register.
type ChatDeliverPayload struct {
	ID         int    `json:"id"`
	From       string `json:"from"`
	Content    string `json:"content"`
	ReplyTo    int    `json:"reply_to,omitempty"`
	Thread     string `json:"thread,omitempty"`
	ReceivedAt string `json:"received_at"`      // RFC 3339 UTC; server-stamped at Nexus DB insert
	Reason     string `json:"reason"`           // mention | reply | thread | all
	Replay     bool   `json:"replay,omitempty"` // true iff this frame was emitted as part of a since_msg_id replay

	// ReplyCount is the number of descendants in the subtree rooted
	// at this message — recursive, not just direct children. Set by
	// chat.list (operator dashboard's main feed) so the SPA can show
	// the "N replies" expander. Live chat.deliver frames leave it
	// zero — the SPA increments locally on each incoming reply.
	ReplyCount int `json:"reply_count,omitempty"`

	// ThreadRoot is the canonical thread identity (task #226 linked-
	// list thread model). Equals the row's own id for top-level
	// messages; equals the thread-root of the reply target for
	// replies. Aspects use it to key per-thread session ids
	// (deterministic uuid_v5 of aspect_name + ":" + ThreadRoot).
	// Zero for legacy rows pre-#226 migration.
	ThreadRoot int `json:"thread_root,omitempty"`
}

// ChatReactionPayload toggles an emoji reaction.
type ChatReactionPayload struct {
	From  string `json:"from"`
	MsgID int    `json:"msg_id"`
	Emoji string `json:"emoji"`
}

// ChatReadPayload is a request for a specific message or thread.
// Response comes back as a ChatReadResultPayload.
//
// Lock 2 pull path: aspects use this to read context they weren't
// pushed, without triggering a fresh deliberation cycle. SinceID
// caps how far back the response includes (e.g. for paginated
// re-reads of a long thread).
type ChatReadPayload struct {
	MsgID    int   `json:"msg_id,omitempty"`
	ThreadID int64 `json:"thread_id,omitempty"`
	SinceID  int64 `json:"since_id,omitempty"`
}

// ChatReadResultPayload is the response to a ChatRead request — the
// thread's messages oldest-first. Limit applied server-side to bound
// large threads; aspects can paginate via SinceID.
type ChatReadResultPayload struct {
	Messages []ChatDeliverPayload `json:"messages"`
}

// AnnounceFilePayload surfaces a file path to chat with a brief
// description. Server creates a chat_messages row + shared_files
// row linked to it; the response (an Ack-shaped frame) carries the
// new chat msg_id.
type AnnounceFilePayload struct {
	From        string `json:"from"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// ShareFilePayload records a direct share to recipients without a
// chat post. Server creates a shared_files row with recipients_json
// populated; response carries the share_id.
type ShareFilePayload struct {
	From       string   `json:"from"`
	Path       string   `json:"path"`
	Recipients []string `json:"recipients"`
}

// FileResultPayload is the ack for AnnounceFile or ShareFile. For
// announces, MsgID is the chat msg_id the model can reference; for
// shares, ShareID is the resource id. Exactly one is non-zero.
type FileResultPayload struct {
	MsgID   int64 `json:"msg_id,omitempty"`
	ShareID int64 `json:"share_id,omitempty"`
}

// AspectActivityPayload is Lock 5 telemetry over the wire — the
// out-of-process counterpart to the in-process funnel.EventSink.
// Aspects emit these; Nexus fans them out to dashboard activity
// surfaces (the activity strip, mobile "agent responding"
// indicator). Ephemeral — not stored, not chat posts.
//
// Type matches funnel.EventType strings ("turn.start", "turn.end",
// "compact.start", "compact.end", "filter.judging",
// "provider.retry"). Payload is opaque JSON the dashboard layer
// shapes per type — keeps the frame definition stable as new event
// types are added.
type AspectActivityPayload struct {
	Type      string          `json:"type"`
	AspectID  string          `json:"aspect_id"`
	EmittedAt string          `json:"emitted_at"` // RFC 3339 UTC
	Payload   json.RawMessage `json:"payload"`
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
	Text     string   `json:"text"`
	OwnAgent bool     `json:"own_agent,omitempty"`
	Shared   bool     `json:"shared,omitempty"`
	Peers    []string `json:"peers,omitempty"`
	TopK     int      `json:"top_k,omitempty"`
	MaxRank  float64  `json:"max_rank,omitempty"`
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
// Credentials (NEX-77) — aspect-to-Nexus credential fetch
// -------------------------------------------------------------------

// CredentialFetchPayload is an aspect requesting a kind-typed
// credential from the broker's credential store.
//
// Kind is required (e.g. "jira", "imap", "provider"). Name is optional:
//   - Name unset  → broker resolves via the aspect's default for that
//                   kind (aspects.default_<kind>_credential). Returns
//                   credentials.ErrNoDefault if no default configured.
//   - Name set    → broker fetches that named credential, checks the
//                   aspect is on its allowed_aspects list, audits.
//
// The fetched bundle's shape is kind-specific (see credentials package
// docs). Caller (the MCP) unmarshals based on Kind.
type CredentialFetchPayload struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// CredentialFetchResultPayload returns the decrypted bundle to the
// aspect. The bundle is JSON-encoded as a free-form object — callers
// know the shape from Kind. Never logged on the broker side.
//
// For kind='provider' the bundle is {api_shape, base_url, key,
// default_model?}. For kind='jira' it's {atlassian_email,
// atlassian_token, atlassian_subdomain}. For kind='imap' it's
// {host, port, user, password, ssl}.
type CredentialFetchResultPayload struct {
	Name   string                 `json:"name"`
	Kind   string                 `json:"kind"`
	Bundle map[string]any         `json:"bundle"`
	// ExpiresAt is reserved for future server-side TTL — v1 always
	// emits empty string (no TTL). MCPs should cache the bundle for
	// the duration of their process and re-fetch on restart.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// -------------------------------------------------------------------
// Operator dashboard request/response (dashboard-ws-port spec §3.2)
//
// All operator frames carry a correlation_id (Envelope.ID); the
// broker echoes it on the matching .result so the SPA can route
// responses back to pending Promises in js/comms.js.
// -------------------------------------------------------------------

// RosterListPayload is the (intentionally empty) request body.
// Operator's dashboard pulls the current aspect roster on view-load
// and on subscribe.roster reconnect; the request carries no scope —
// operator sees everything.
type RosterListPayload struct{}

// RosterAspect is one row in roster.list.result. Subset of the
// internal roster + extra metadata the dashboard's Status/Agents
// views render.
type RosterAspect struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	LastSeen     string   `json:"last_seen,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Model        string   `json:"model,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	ContextMode  string   `json:"context_mode,omitempty"`
	Role         string   `json:"role,omitempty"`
}

// RosterListResultPayload — newest first, all aspects, with status
// from the in-memory Roster.
type RosterListResultPayload struct {
	Aspects []RosterAspect `json:"aspects"`
}

// ChatListPayload is the operator-scoped chat feed read. id-based
// pagination: AfterID returns messages with id > AfterID; BeforeID
// returns id < BeforeID. Both zero = newest page (uses a default-
// limit's worth of newest rows).
//
// Distinct from ChatReadPayload, which is thread-scoped and
// available to aspects. Operator dashboard uses this for the main
// "all chat" feed; the topic-scoped variant is deferred (chat_messages
// has no persisted topic column today — schema migration required).
type ChatListPayload struct {
	AfterID  int64 `json:"after_id,omitempty"`
	BeforeID int64 `json:"before_id,omitempty"`
	Limit    int   `json:"limit,omitempty"`
}

// ChatListResultPayload — messages oldest-first, plus has_more for
// "load older" affordance at the page boundary.
type ChatListResultPayload struct {
	Messages []ChatDeliverPayload `json:"messages"`
	HasMore  bool                 `json:"has_more"`
}

// ChatRepliesPayload requests every message whose reply_to ==
// parent_id. Dashboard renders a thread view from one message.
type ChatRepliesPayload struct {
	ParentID int64 `json:"parent_id"`
}

// ChatRepliesResultPayload — direct replies only (one level deep);
// the dashboard recurses if needed.
type ChatRepliesResultPayload struct {
	ParentID int64                `json:"parent_id"`
	Messages []ChatDeliverPayload `json:"messages"`
}

// ReactionsFetchPayload requests reactions for a batch of msg_ids.
// Used by the chat view when rendering a page so it can show
// reaction counts inline.
type ReactionsFetchPayload struct {
	MsgIDs []int64 `json:"msg_ids"`
}

// ReactionRow is one (aspect, emoji) reaction on a message.
type ReactionRow struct {
	Aspect string `json:"aspect"`
	Emoji  string `json:"emoji"`
}

// ReactionsFetchResultPayload — keyed by msg_id (string in JSON
// because JSON object keys must be strings). Empty array when no
// reactions exist; missing key when msg_id wasn't in the input.
type ReactionsFetchResultPayload struct {
	Reactions map[string][]ReactionRow `json:"reactions"`
}

// ChatReactionUpdatePayload is the push frame broadcast when a chat
// reaction toggles. Carries the FULL current reactions list for the
// affected message (not a delta) so the SPA can replace in-place
// without merge logic. Reactor + emoji + op are included for clients
// that want to surface "X reacted with Y" UI; clients that just want
// the new counts can ignore them and consume Reactions directly.
//
// op: "added" when ToggleReaction inserted (no prior matching
// triple); "removed" when it deleted.
type ChatReactionUpdatePayload struct {
	MsgID     int           `json:"msg_id"`
	Reactor   string        `json:"reactor"`
	Emoji     string        `json:"emoji"`
	Op        string        `json:"op"` // "added" | "removed"
	Reactions []ReactionRow `json:"reactions"`
}

// KnowledgeListPayload mirrors the knowledge.Store.List shape:
// scope by from_agent (omit for the operator's own entries via the
// caller-identity convention; explicit name for peer reads).
type KnowledgeListPayload struct {
	Agent string `json:"agent,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// KnowledgeListResultPayload — entries newest-updated first.
type KnowledgeListResultPayload struct {
	Entries []KnowledgeHit `json:"entries"`
}

// KnowledgeStoreResultPayload echoes the row id back to the SPA.
type KnowledgeStoreResultPayload struct {
	ID int64 `json:"id"`
}

// AspectSayPayload posts a chat message addressed to the named
// aspect. Sugar over chat.send with auto-prepended "@<aspect>"; the
// SPA's Aspects view renders a "talk to" affordance that uses this.
type AspectSayPayload struct {
	Aspect  string `json:"aspect"`
	Content string `json:"content"`
}

// AspectSayResultPayload — the new chat msg_id, so the SPA can
// follow up on its own message in the chat stream.
type AspectSayResultPayload struct {
	MsgID int64 `json:"msg_id"`
}

// -------------------------------------------------------------------
// Subscription frames (5d)
// -------------------------------------------------------------------

// SubscribePayload is the body of subscribe.* frames. Currently no
// fields are used (subscribe.chat scoping by topics is deferred —
// chat_messages has no persisted topic column today). Reserved
// shape for forward-compat: when topic-scoping lands, add Topics
// here without changing the kind.
type SubscribePayload struct {
	// Topics is reserved for future topic-scoped chat subscription.
	// Empty means "all" (the only behavior in v1).
	Topics []string `json:"topics,omitempty"`
}

// SubscribeAckPayload echoes the subscription kind so the SPA can
// confirm which channel the ack relates to. Idempotent re-subscribes
// also produce an ack so the SPA's RPC layer can resolve the Promise.
type SubscribeAckPayload struct {
	Kind string `json:"kind"` // the subscribe kind that was acked
}

// RosterUpdatePayload is pushed when an aspect connects, disconnects,
// or status-changes. The dashboard's Status / Agents views replace
// the row with this delta. Status mirrors AspectState.Status —
// "live" | "stale" | "down" — and is the broker's authoritative
// roster state at fan-out time.
type RosterUpdatePayload struct {
	Aspect       string   `json:"aspect"`
	Status       string   `json:"status"`
	LastSeen     string   `json:"last_seen,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Model        string   `json:"model,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	ContextMode  string   `json:"context_mode,omitempty"`
	// Reason names the trigger ("connect" | "disconnect" |
	// "status_change") so the SPA can render a brief notification
	// without inferring from prior state.
	Reason string `json:"reason"`
}

// SubscribeObservePayload — operator subscribes to one aspect's
// observability stream. SinceSeq is optional: pass 0 (or omit) for the
// full retained tail; pass a known sequence to only get frames newer
// than it (useful on reconnect after a brief drop).
type SubscribeObservePayload struct {
	Aspect   string `json:"aspect"`
	SinceSeq int64  `json:"since_seq,omitempty"`
}

// UnsubscribeObservePayload — operator drops one aspect from its
// observability subscription set.
type UnsubscribeObservePayload struct {
	Aspect string `json:"aspect"`
}

// ObserveFramePayload — server push of one observability frame to a
// subscriber. Frame is the package-shaped value from
// nexus/observability.Frame, marshaled to JSON. Aspect is also
// surfaced at the envelope payload level so the client doesn't need
// to peek into Frame to route.
type ObserveFramePayload struct {
	Aspect string          `json:"aspect"`
	Frame  json.RawMessage `json:"frame"`
}

// ObserveBeginPayload — aspect/agentfunnel forwards a Grouper
// BeginTurn boundary upstream so the broker's Hub can open the same
// turn for this aspect on the broadcast side. Aspect is advisory; the
// broker authoritatively uses the wsConn's registered identity per
// keel-cli's attribution caveat (#236).
type ObserveBeginPayload struct {
	Aspect     string `json:"aspect,omitempty"`
	TurnID     string `json:"turn_id"`
	Label      string `json:"label"`
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	TriggerMsg int64  `json:"trigger_msg,omitempty"`
}

// ObserveEventPayload — one bridle.Event marshaled for upstream
// transport. EventKind discriminates which bridle event type is
// encoded in Event; the broker decodes by kind and forwards to the
// per-aspect Grouper's OnBridleEvent. JSON-encoding bridle events
// directly avoids a separate wire vocabulary at the cost of being
// coupled to bridle's field shapes (acceptable — bridle is pinned
// per nexus go.mod).
type ObserveEventPayload struct {
	Aspect    string          `json:"aspect,omitempty"`
	EventKind string          `json:"event_kind"`
	Event     json.RawMessage `json:"event"`
}

// ObserveEndPayload — closes the in-flight turn on the broker side.
// No body needed beyond aspect attribution (advisory).
type ObserveEndPayload struct {
	Aspect string `json:"aspect,omitempty"`
}

// AspectStatusPulsePayload is pushed when an aspect emits a
// mid-work status pulse (#118 — currently aspirational; the
// payload shape lands here so 5e can render UI for it once the
// pulse origin lights up).
type AspectStatusPulsePayload struct {
	Aspect string `json:"aspect"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
	TS     string `json:"ts"`
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
	Aspect     string `json:"aspect"`
	SessionID  string `json:"session_id"`
	NewHeadID  string `json:"new_head_id"`
	PreviousID string `json:"previous_id"`
}

// SessionForkPayload signals that the aspect forked to a new branch.
type SessionForkPayload struct {
	Aspect    string `json:"aspect"`
	SessionID string `json:"session_id"`
	ForkPoint string `json:"fork_point"`
	NewHeadID string `json:"new_head_id"`
}

// -------------------------------------------------------------------
// Lifecycle
// -------------------------------------------------------------------

// ShutdownPayload is sent upstream → aspect (or Outpost → aspects, or
// Nexus → Outposts) to request a graceful wind-down.
type ShutdownPayload struct {
	Reason       string `json:"reason"`
	GracePeriodS int    `json:"grace_period_s,omitempty"`
}

// -------------------------------------------------------------------
// Tickets (operator-aspect WS extension §4.1)
// -------------------------------------------------------------------

// TicketCreatePayload — aspect or operator creates a ticket.
type TicketCreatePayload struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Assignee    string `json:"assignee,omitempty"`
	Priority    string `json:"priority,omitempty"` // low | normal | high | urgent
	Domain      string `json:"domain,omitempty"`
	SourceMsgID int64  `json:"source_msg_id,omitempty"`
}

// TicketUpdatePayload — patch fields. Pointer fields distinguish
// "field omitted" (nil) from "field cleared to NULL" (empty string).
// Mirrors the broker's `!== undefined` semantics for the same case.
type TicketUpdatePayload struct {
	ID          int64   `json:"id"`
	Status      *string `json:"status,omitempty"`
	Assignee    *string `json:"assignee,omitempty"`
	Priority    *string `json:"priority,omitempty"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Domain      *string `json:"domain,omitempty"`
}

// TicketListPayload — combinable filters; Limit caps at 200, default 50.
type TicketListPayload struct {
	Assignee string `json:"assignee,omitempty"`
	Status   string `json:"status,omitempty"`
	Creator  string `json:"creator,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// TicketSummary is the per-row shape returned by list — projection
// drops description to avoid response overflow at scale.
type TicketSummary struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Priority  string `json:"priority"`
	Domain    string `json:"domain,omitempty"`
	Assignee  string `json:"assignee,omitempty"`
	Creator   string `json:"creator"`
	CreatedAt string `json:"created_at"` // RFC 3339 UTC
}

// TicketListResultPayload is the response to TicketListPayload.
type TicketListResultPayload struct {
	Tickets []TicketSummary `json:"tickets"`
}

// TicketGetPayload — fetch one ticket with description + notes.
type TicketGetPayload struct {
	ID int64 `json:"id"`
}

// TicketDetail extends TicketSummary with description + lifecycle
// timestamps. Returned by ticket.get; not used for list rows.
type TicketDetail struct {
	TicketSummary
	Description string `json:"description,omitempty"`
	SourceMsgID int64  `json:"source_msg_id,omitempty"`
	UpdatedAt   string `json:"updated_at"`
	ClosedAt    string `json:"closed_at,omitempty"`
}

// TicketNote is one entry in a ticket's chronological note thread.
type TicketNote struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// TicketGetResultPayload pairs a ticket with its notes.
type TicketGetResultPayload struct {
	Ticket TicketDetail `json:"ticket"`
	Notes  []TicketNote `json:"notes"`
}

// TicketNoteAddPayload — append a progress note. Author derives from
// the connection's identity, not from the payload (no spoofing).
type TicketNoteAddPayload struct {
	TicketID int64  `json:"ticket_id"`
	Content  string `json:"content"`
}

// -------------------------------------------------------------------
// Files (per 2026-05-04-files-subsystem-spec.md — Nexus is broker, not store)
// -------------------------------------------------------------------

// FileAnnouncePayload — aspect or operator publishes a file reference.
// The bytes stay on the announcing aspect's filesystem (ws:// URL) or a
// public URL (https://, gs://, s3://); Nexus stores only the reference.
type FileAnnouncePayload struct {
	Name        string `json:"name"`
	URL         string `json:"url"` // ws://<aspect>/file/<path> or public URL
	MimeType    string `json:"mime_type,omitempty"`
	Description string `json:"description,omitempty"`
}

// FileAnnounceResultPayload — ack with the new files-table id.
type FileAnnounceResultPayload struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"` // RFC 3339 UTC
}

// FileListPayload — list announced files.
type FileListPayload struct {
	Owner string `json:"owner,omitempty"` // filter by announcing aspect-id
	Limit int    `json:"limit,omitempty"` // default 50
}

// FileSummary is the metadata view returned in list. URL deliberately
// omitted — it's an internal routing detail, requesters always go
// through Nexus via file.get.
type FileSummary struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Owner       string `json:"owner"` // announcing aspect-id
	MimeType    string `json:"mime_type,omitempty"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// FileListResultPayload is the response.
type FileListResultPayload struct {
	Files []FileSummary `json:"files"`
}

// FileGetPayload — request a specific file. Nexus inspects the URL
// scheme and either returns the public URL directly (https://) or
// dispatches a file.fetch to the owning aspect's funnel (ws://) and
// forwards the file.deliver response to the requester.
type FileGetPayload struct {
	ID int64 `json:"id"`
}

// FileGetResultPayload — exactly one of {URL, Content} is non-empty.
// URL is set for public references; Content is set for ws:// references
// and carries the bytes inline (base64 in v0.1; binary WS frames are
// the post-cutover upgrade path for large assets).
type FileGetResultPayload struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type,omitempty"`
	URL      string `json:"url,omitempty"`      // public URL — requester fetches independently
	Content  string `json:"content,omitempty"`  // base64-encoded bytes — set for ws:// references
	Encoding string `json:"encoding,omitempty"` // "base64" when Content is set
}

// FileFetchPayload — Nexus → aspect funnel. Internal frame. The funnel
// handles this directly via its service-frame dispatch table without
// invoking the deliberation loop or model. Funnel resolves the path
// component against the aspect's local filesystem (with path traversal
// hardening — reject `..` segments, absolute paths, paths escaping the
// aspect's home).
type FileFetchPayload struct {
	RequestID string `json:"request_id"` // correlates with the originating file.get
	Path      string `json:"path"`       // <path> from ws://<aspect>/file/<path>
}

// FileDeliverPayload — aspect funnel → Nexus. Carries bytes (or an
// error if the file is unreadable / not found / outside the home dir).
type FileDeliverPayload struct {
	RequestID string `json:"request_id"`
	Content   string `json:"content,omitempty"`  // base64-encoded bytes
	Encoding  string `json:"encoding,omitempty"` // "base64"
	MimeType  string `json:"mime_type,omitempty"`
	Error     string `json:"error,omitempty"`
}

// -------------------------------------------------------------------
// Docs (operator-aspect WS extension §4.3)
// -------------------------------------------------------------------

// DocsListPayload — enumerate docs under the configured root. Path
// filter is an optional subdir relative to root; absolute paths and
// `..` segments rejected server-side.
type DocsListPayload struct {
	Path string `json:"path,omitempty"`
}

// DocEntry is a single doc file's metadata.
type DocEntry struct {
	Path     string `json:"path"` // relative to docs root
	Size     int64  `json:"size"`
	Modified string `json:"modified"` // RFC 3339 UTC
}

// DocsListResultPayload is the response.
type DocsListResultPayload struct {
	Docs []DocEntry `json:"docs"`
}

// DocsGetPayload — read a single doc. Server enforces: relative path,
// no `..` segments, must resolve inside the docs root, must be UTF-8
// text (binary docs rejected with status=400).
type DocsGetPayload struct {
	Path string `json:"path"`
}

// DocsGetResultPayload returns the file content as UTF-8 text.
type DocsGetResultPayload struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Modified string `json:"modified"`
}

// -------------------------------------------------------------------
// Usage (operator-aspect WS extension §4.4)
// -------------------------------------------------------------------

// UsageQueryPayload — period bucket + optional aspect filter + group_by
// dimension. Backed by the chat_usage table (F3.1).
type UsageQueryPayload struct {
	Period  string `json:"period,omitempty"`   // 1h | 24h | 7d | 30d (default 7d)
	Aspect  string `json:"aspect,omitempty"`   // filter to one aspect
	GroupBy string `json:"group_by,omitempty"` // aspect | msg_id | day (default aspect)
}

// UsageRow is one aggregated bucket. Key shape depends on GroupBy:
// aspect-id, msg-id (string-rendered int), or YYYY-MM-DD.
type UsageRow struct {
	Key          string `json:"key"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
}

// UsageQueryResultPayload is the response.
type UsageQueryResultPayload struct {
	Period string     `json:"period"`
	Rows   []UsageRow `json:"rows"`
}

// -------------------------------------------------------------------
// Network and agents (operator-aspect WS extension §4.5; admin-gated)
// -------------------------------------------------------------------

// NetworkRestartPayload — restart whole network or a specific aspect.
// Empty Target = restart-all. Operator/Frame role only.
type NetworkRestartPayload struct {
	Target string `json:"target,omitempty"`
}

// NetworkShutdownPayload — graceful shutdown across the network.
type NetworkShutdownPayload struct {
	GracePeriodS int `json:"grace_period_s,omitempty"`
}

// NetworkMaintenancePayload — toggle maintenance mode (suppress
// non-admin frames except status reads).
type NetworkMaintenancePayload struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// AgentStartPayload — bring up an aspect (empty = "all").
type AgentStartPayload struct {
	AspectID string `json:"aspect_id,omitempty"`
}

// AgentSayPayload — direct prompt injection bypassing chat. Used by
// the operator dashboard "say to agent" affordance.
type AgentSayPayload struct {
	AspectID string `json:"aspect_id"`
	Content  string `json:"content"`
}
