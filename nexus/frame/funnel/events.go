// Lifecycle events emitted by the funnel as work progresses. Per
// Lock 5 of the aspect-funnel architecture (agent-network/docs/
// 2026-05-02-aspect-funnel-architecture.md).
//
// The funnel emits Events at semantic boundaries: turn start/end,
// compaction start/end, post-hoc filter judgment, provider retry.
// Sinks render them — for the in-process Frame, into the dashboard
// activity strip; for out-of-process aspects (F1.2.5), into outbound
// WS frames addressed to Nexus.
//
// Events are ephemeral telemetry. They are not chat posts, not stored
// in the message log, and emission failure must never break the
// deliberation loop.

package funnel

import (
	"context"
	"time"

	"github.com/nexus-cw/bridle"
)

// EventType is the canonical identifier for a lifecycle event. Sinks
// switch on this; new types are added by appending here, not by
// inventing strings at the call site.
type EventType string

const (
	// EventTurnStart fires immediately before bridle.RunTurn. Carries
	// turn id, the round number within the deliberation, and the
	// cumulative-token estimate going into the turn.
	EventTurnStart EventType = "turn.start"

	// EventTurnToolCall fires when bridle returns tool calls for the
	// funnel to handle within a turn. Carries tool name and call count
	// for that round. Multiple firings per turn are normal.
	EventTurnToolCall EventType = "turn.tool_call"

	// EventTurnEnd fires after bridle.RunTurn returns its final output.
	// Carries usage and wall-clock duration. Always paired with a prior
	// EventTurnStart for the same turn id.
	EventTurnEnd EventType = "turn.end"

	// EventCompactStart fires before the summarize bridle call kicks
	// off. Carries the trigger reason, current token count, and target
	// post-summary count. Operator-visible event — long compactions
	// look like hangs without it.
	EventCompactStart EventType = "compact.start"

	// EventCompactEnd fires after compaction's summarize turn completes.
	// Carries before/after token counts and wall-clock duration. Always
	// paired with a prior EventCompactStart.
	EventCompactEnd EventType = "compact.end"

	// EventFilterJudging fires before the post-hoc filter runs against
	// a turn's natural reply. Reserved for F1.1 — kept here so the
	// taxonomy is complete at the F1.2 wire-up.
	EventFilterJudging EventType = "filter.judging"

	// EventProviderRetry fires on retryable provider errors (rate
	// limit, transient 5xx). Carries attempt count, error class label,
	// backoff duration. Reserved — bridle owns retry today; funnel
	// surfaces it once bridle exposes the hook.
	EventProviderRetry EventType = "provider.retry"
)

// Event is the envelope all lifecycle events share. Payload carries
// the event-specific fields; sinks dispatch on Type.
//
// AspectID identifies which Frame/aspect emitted the event; sinks
// fanning out to multiple aspects need it for routing. EmittedAt is
// set by the funnel at Emit time, not the sink, so chronological
// order is preserved across asynchronous delivery paths.
type Event struct {
	Type      EventType `json:"type"`
	AspectID  string    `json:"aspect_id"`
	EmittedAt time.Time `json:"emitted_at"`
	Payload   any       `json:"payload"`
}

// TurnStartPayload accompanies EventTurnStart.
type TurnStartPayload struct {
	TurnID         string `json:"turn_id"`
	Round          int    `json:"round"`
	ContextTokens  int    `json:"context_tokens"`
}

// TurnToolCallPayload accompanies EventTurnToolCall.
type TurnToolCallPayload struct {
	TurnID   string `json:"turn_id"`
	ToolName string `json:"tool_name"`
	Count    int    `json:"count"`
}

// TurnEndPayload accompanies EventTurnEnd.
type TurnEndPayload struct {
	TurnID     string             `json:"turn_id"`
	Usage      bridle.Usage       `json:"usage"`
	StopReason bridle.StopReason  `json:"stop_reason"`
	StepCount  int                `json:"step_count"`
	Duration   time.Duration      `json:"duration"`
}

// CompactReason names what triggered compaction. Soft = threshold
// crossed during normal operation; hard = forced, e.g. provider
// auto-compact would otherwise fire. v1 only emits soft (the funnel
// proactively compacts to stay below the provider's auto-trigger);
// hard is reserved for the day we surface a manual operator command.
type CompactReason string

const (
	CompactReasonSoft CompactReason = "soft_threshold"
	CompactReasonHard CompactReason = "hard_threshold"
)

// CompactStartPayload accompanies EventCompactStart.
type CompactStartPayload struct {
	Reason       CompactReason `json:"reason"`
	TokensBefore int           `json:"tokens_before"`
	TargetTokens int           `json:"target_tokens"`
}

// CompactEndPayload accompanies EventCompactEnd. TokensAfter is the
// summarize turn's output tokens, which become the new tail's budget.
type CompactEndPayload struct {
	TokensBefore int           `json:"tokens_before"`
	TokensAfter  int           `json:"tokens_after"`
	Duration     time.Duration `json:"duration"`
}

// FilterJudgingPayload accompanies EventFilterJudging.
type FilterJudgingPayload struct {
	TurnID string `json:"turn_id"`
}

// ProviderRetryPayload accompanies EventProviderRetry.
type ProviderRetryPayload struct {
	Attempt    int           `json:"attempt"`
	ErrorClass string        `json:"error_class"`
	Backoff    time.Duration `json:"backoff"`
}

// EventSink consumes lifecycle events. Implementations must be
// goroutine-safe — the funnel emits from whatever goroutine the
// deliberation runs on, and tests will fan out concurrent events.
//
// Emit MUST NOT return errors that propagate into the deliberation
// loop. Sink failure is a telemetry concern, not a correctness
// concern; a broken dashboard or missing WS connection should not
// cause an aspect to drop a message. Sinks that need to surface
// errors should log internally.
//
// Emit SHOULD be non-blocking: a slow sink starves the funnel, which
// breaks the very thing lifecycle events exist to prevent (the
// "looks like a hang" problem). Implementations buffering to a
// channel is the right pattern; flushing synchronously to the
// network is wrong.
type EventSink interface {
	Emit(ctx context.Context, e Event)
}

// NoopSink discards all events. Used as the default when a Frame is
// constructed without a real sink wired (tests, the embedded Frame
// before the dashboard reads from the funnel, etc.). Always safe.
type NoopSink struct{}

// Emit drops the event on the floor.
func (NoopSink) Emit(_ context.Context, _ Event) {}
