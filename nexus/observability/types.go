package observability

import (
	"encoding/json"
	"time"
)

// FrameKind discriminates the Payload union carried by Frame.
type FrameKind string

const (
	FrameTurn           FrameKind = "turn"
	FrameChat           FrameKind = "chat"
	FramePresence       FrameKind = "presence"
	FrameFilterDecision FrameKind = "filter_decision"
)

// Frame is the wire-format unit consumed by clients. Sequence is
// monotonic per-aspect (one Grouper per aspect) so reconnecting
// subscribers can request "give me everything since seq=N" and
// dedupe against the tail.
type Frame struct {
	Kind     FrameKind       `json:"kind"`
	Aspect   string          `json:"aspect"`
	Sequence int64           `json:"seq"`
	TS       time.Time       `json:"ts"`
	RunID    string          `json:"run_id,omitempty"` // dispatch run that emitted this frame
	Payload  json.RawMessage `json:"payload"`
}

// TurnStatus is the lifecycle marker on a TurnFrame.
type TurnStatus string

const (
	TurnInFlight TurnStatus = "in_flight"
	TurnComplete TurnStatus = "complete"
	TurnErrored  TurnStatus = "errored"
)

// TurnFrame is a complete snapshot of the in-flight or finished
// deliberation turn. Each emission carries the full event list so
// renderers can replace-by-turn_id rather than reconcile deltas.
type TurnFrame struct {
	TurnID string `json:"turn_id"`
	// Label distinguishes which kind of bridle turn this is. Documented
	// values: "main" (the operator-addressed deliberation turn),
	// "compact" (mid-deliberation context summarization), "filter-judge"
	// (post-turn meaningfulness evaluation). Freeform string so new wrap
	// sites can land without an interface bump. Empty string is treated
	// as "main" by renderers — back-compat default.
	Label      string      `json:"label,omitempty"`
	Status     TurnStatus  `json:"status"`
	Started    time.Time   `json:"started"`
	Ended      *time.Time  `json:"ended,omitempty"`
	TriggerMsg int64       `json:"trigger_msg,omitempty"`
	Model      string      `json:"model,omitempty"`
	Provider   string      `json:"provider,omitempty"`
	Events     []TurnEvent `json:"events"`
	Usage      *UsageStats `json:"usage,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// TurnEventKind discriminates the per-step entries inside a TurnFrame.
type TurnEventKind string

const (
	TurnEventText         TurnEventKind = "text"
	TurnEventToolCall     TurnEventKind = "tool_call"
	TurnEventOrphanResult TurnEventKind = "tool_result_orphan"
	TurnEventStep         TurnEventKind = "step"
)

// TurnEvent is one entry in a TurnFrame's Events slice. Only the
// field appropriate to Kind is populated; the others stay zero.
type TurnEvent struct {
	Kind TurnEventKind `json:"kind"`
	Text string        `json:"text,omitempty"`
	Tool *ToolCall     `json:"tool,omitempty"`
	Step int           `json:"step,omitempty"`
}

// ToolCall is the paired view of a model-issued tool call. Result
// is nil while the call is in flight; once the bridle ToolCallResult
// arrives, Result is populated. Artifact is pre-computed at
// ToolCallStart time for the editing tools so renderers don't have
// to re-parse Input on every frame.
type ToolCall struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Input            json.RawMessage `json:"input"`
	Result           *ToolResult     `json:"result,omitempty"`
	Artifact         *Artifact       `json:"artifact,omitempty"`
	ArtifactParseErr string          `json:"artifact_parse_err,omitempty"`
}

// ToolResult is the renderer-friendly summary of a tool's output.
// Preview is the truncated head (≤200 chars). Full is optionally
// populated when an operator explicitly requested the unredacted
// value — Phase A always leaves it empty; Phase B+ may set it.
type ToolResult struct {
	Preview string `json:"preview"`
	Full    string `json:"full,omitempty"`
	IsError bool   `json:"is_error"`
}

// ArtifactKind discriminates artifact shapes.
type ArtifactKind string

const (
	ArtifactFileEdit     ArtifactKind = "file_edit"
	ArtifactFileWrite    ArtifactKind = "file_write"
	ArtifactMultiEdit    ArtifactKind = "multi_edit"
	ArtifactNotebookEdit ArtifactKind = "notebook_edit"
)

// Artifact captures the structured intent of a file-mutating tool
// call so renderers can show a diff instead of raw JSON.
type Artifact struct {
	Kind     ArtifactKind `json:"kind"`
	FilePath string       `json:"file_path"`
	OldText  string       `json:"old_text,omitempty"`
	NewText  string       `json:"new_text,omitempty"`
	Edits    []EditPair   `json:"edits,omitempty"`
}

// EditPair is one swap inside a MultiEdit artifact.
type EditPair struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

// Direction labels a ChatFrame as inbound (addressed to this
// aspect) or outbound (sent by it).
type Direction string

const (
	DirectionInbound  Direction = "inbound"
	DirectionOutbound Direction = "outbound"
)

// ChatFrame is a single chat message viewed from this aspect's
// vantage point. Independent of turn boundaries — chats can arrive
// between turns or during one.
type ChatFrame struct {
	MsgID     int64     `json:"msg_id"`
	From      string    `json:"from"`
	Content   string    `json:"content"`
	ReplyTo   int64     `json:"reply_to,omitempty"`
	Topic     string    `json:"topic,omitempty"`
	Direction Direction `json:"direction"`
	CreatedAt time.Time `json:"created_at"`
}

// PresenceFrame marks a transition in the aspect's WS connection
// state. Reason is a short human-readable tag ("registered",
// "ws_closed", ...).
type PresenceFrame struct {
	Connected bool   `json:"connected"`
	Reason    string `json:"reason,omitempty"`
}

// FilterDecisionFrame is the structured verdict from the funnel's
// post-hoc output filter for a completed main turn. Emitted via
// Grouper.OnFilterDecision (the funnel.FilterDecisionRenderer
// interface) so renderers see a typed frame instead of the legacy
// synthetic "filter-decision" TurnFrame fabricated by the pre-
// FilterDecisionRenderer fallback path.
//
// MainTurnID pairs the verdict to its parent main turn; renderers
// SHOULD attach the decision to that turn's snapshot rather than
// rendering it as a standalone row, but the choice is theirs.
type FilterDecisionFrame struct {
	MainTurnID string `json:"main_turn_id"`
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	ShouldPost bool   `json:"should_post"`
	Reason     string `json:"reason,omitempty"`
	Class      string `json:"class,omitempty"`
}

// UsageStats is the renderer-side view of bridle.Usage plus the
// turn's wall-clock duration. Duration is in nanoseconds to match
// time.Duration's JSON encoding.
type UsageStats struct {
	InputTokens              int           `json:"input_tokens"`
	OutputTokens             int           `json:"output_tokens"`
	CacheReadInputTokens     int           `json:"cache_read,omitempty"`
	CacheCreationInputTokens int           `json:"cache_create,omitempty"`
	Duration                 time.Duration `json:"duration_ns"`
	CostUSD                  float64       `json:"cost_usd,omitempty"`
}
