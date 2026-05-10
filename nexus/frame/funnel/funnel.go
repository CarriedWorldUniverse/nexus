// Package funnel is the Frame's deliberation engine — the layer that
// owns context-window management, comms-inbox folding, summarization
// triggers, and the deliberation loop itself. The Frame consumes the
// funnel; the funnel consumes bridle.
//
// Three-layer stack (per operator #8555):
//
//	bridle (one-turn driver) ← imported by
//	funnel (deliberation, context, comms, compaction) ← used by
//	Frame (operator identity, admin REST, chat routing)
//
// Funnel-shape contract (per #81 lock):
//   - Receive comms (operator/aspect chat) into an inbox.
//   - When deliberation runs: triage decides engage/dismiss; on engage,
//     bridle.RunTurn drives one or more turns with the comms folded in.
//   - send_comms is a tool the model can call mid-turn — outbound chat
//     goes through ToolRunner, not through a special-cased completion path.
//   - At end of deliberation, log-decision turn decides whether the turn
//     becomes thread history (appended to SessionTail) or is dropped.
//   - Mid-turn comms accumulate in the inbox-as-array; folded into the
//     next turn's prompt.
//
// Compaction: see docs/2026-05-01-funnel-compaction-design.md.
// Cumulative token tracking, summarization-turn at threshold, fresh
// SessionTail with summary-as-first-message, counter reset.
//
// v1 scope: deliberation loop + compaction trigger + send_comms tool +
// hard-rules triage. Cheap-model triage (#5.7) deferred to v2.
package funnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/route"
)

// CompactionPolicy tunes the funnel's context-window management.
// Per anvil's design (00c6dd9), the funnel proactively summarizes
// before hitting the provider's auto-compact threshold.
type CompactionPolicy struct {
	// ThresholdTokens is the cumulative input+output token count at
	// which the funnel runs a summarize-turn. Default 150_000 — leaves
	// 40k headroom under claude-code's empirically-observed 191k
	// auto-trigger.
	ThresholdTokens int

	// SummarizationModel is the model to use for the cheap summarize
	// turn. Empty falls back to the funnel's primary model.
	SummarizationModel string

	// MaxSummaryTokens caps the summary output. Default 4096 — small
	// enough that the post-summary SessionTail is a tiny fraction of
	// the threshold.
	MaxSummaryTokens int
}

// DefaultCompactionPolicy returns sensible v1 defaults.
func DefaultCompactionPolicy() CompactionPolicy {
	return CompactionPolicy{
		ThresholdTokens:  150_000,
		MaxSummaryTokens: 4_096,
	}
}

// Config wires the funnel's dependencies. All fields except the
// optional ones are required.
type Config struct {
	// Identity & framing
	AspectID     string // the Frame's name (operator-chosen)
	SystemPrompt string // composed from NEXUS.md/SOUL.md/PRIMER.md by the caller

	// SystemPromptFn, when non-nil, is consulted on every turn instead
	// of SystemPrompt. Lets the caller swap the prompt at runtime
	// (e.g., Frame personality refresh per spec §11) without rebuilding
	// the funnel. SystemPrompt remains as a static fallback for callers
	// who don't need refresh.
	SystemPromptFn func() string

	// bridle — the one-turn driver
	Harness *bridle.Harness

	// Provider selection
	Provider bridle.ProviderID
	Model    string
	MCP      *bridle.MCPClientConfig // optional; nil = no MCP-loaded tools
	Tools    []bridle.ToolDef        // explicit in-process tool defs (incl. send_comms)
	Runner   bridle.ToolRunner       // executes Tools

	// ChatGateway is the chat-posting seam used to auto-post the model's
	// natural reply at end-of-turn when the post-hoc filter approves
	// (Filter.ShouldPost). Optional — when nil, the funnel still runs
	// turns but FinalText doesn't reach chat. Production wiring sets
	// this to the same ChatGateway the CommsRunner uses, so the
	// auto-post and explicit send_chat tool calls converge on the same
	// path (Broker.HandleChatSend → persistence + fan-out).
	//
	// Without this, providers that don't expose custom tools to the
	// model (e.g. claudecode in subprocess mode) have no way to surface
	// model output: the model produces FinalText, the filter approves,
	// but nobody acts on the decision.
	ChatGateway ChatGateway

	// Compaction
	Compaction CompactionPolicy

	// MaxStepsPerTurn caps tool-call rounds inside a single bridle turn.
	// 0 = unlimited (bridle's default).
	MaxStepsPerTurn int

	// Routing — used by the Frame to decide what reaches the funnel.
	// Not consumed inside Deliberate, but stored here so callers have
	// one place to find the participation index.
	Threads *route.ThreadIndex

	// Events receives lifecycle events emitted as the funnel works
	// (turn start/end, compaction start/end, filter judgments). Per
	// Lock 5. Nil falls back to NoopSink — emission is always safe to
	// call.
	Events EventSink

	// Filter judges each turn's natural reply for meaningfulness
	// before it can post (Lock 1.3 / Lock 3 post-hoc filter). Nil
	// falls back to AlwaysPostFilter — every non-empty reply goes
	// through, matching the v1 §6.5 Frame harness behavior.
	Filter OutputFilter

	// UsageRecorder records per-turn token usage for forensics
	// (Lock 4 attribution per operator #9254/#9258). Called after
	// each successful turn with the bridle.Usage from the result
	// and the chat msg_id that triggered the deliberation. Nil
	// means no recording — the funnel still runs, the operator
	// just can't query "where did the tokens go" later.
	//
	// The recorder is fire-and-forget at this seam — errors are
	// logged but don't fail the deliberation. Forensics can't
	// block the chat path.
	UsageRecorder UsageRecorder

	// PostTurn runs after each successful provider turn, before the
	// next deliberation begins. Concrete implementation: the rewriter
	// runner (nexus/frame/funnel/rewriter), which distills the just-
	// completed turn's tail in the session jsonl. Synchronous —
	// distillation must complete before the next --resume so we don't
	// race claude-code on the file. Returns whether the funnel should
	// rotate the session id (after sustained distillation failure).
	// Nil = no post-turn work; default behavior matches Nexus pre-
	// rewriter.
	PostTurn PostTurnHook

	// Pulser fires chat-visible status pulses before long ops
	// (compaction always; long tool chains and provider retries
	// once F1.4 wires them). Per Lock 5 of the architecture: the
	// funnel — not the aspect author — must announce long work, so
	// silence-during-work is distinguishable from stuck/crashed.
	// Nil falls back to NoopPulser (lifecycle events still fire via
	// Events; Pulser is the human-visible chat layer).
	Pulser StatusPulser

	Logger *slog.Logger
}

// Funnel is the deliberation engine. One Funnel per Frame; the Frame
// owns its lifetime.
type Funnel struct {
	cfg Config
	log *slog.Logger

	mu sync.Mutex // guards inbox, sessionTail, cumulativeTokens, sessionHandle

	// inbox holds comms that arrived since the last deliberation. Folded
	// into the next bridle.RunTurn call. Drained at deliberation start.
	inbox []bridle.InboxItem

	// triggeringMsgID is the chat msg_id that prompted the next
	// deliberation. Set by ReceiveWithMsgID; consumed and cleared
	// by Deliberate so each turn's UsageRecorder.Record call gets
	// the correct attribution. Zero means "no chat trigger" — the
	// recorder writes MsgID=0, which the usage table stores as NULL.
	triggeringMsgID int64

	// sessionTail accumulates events across turns. Compacted when
	// cumulativeTokens crosses the threshold.
	sessionTail []bridle.SessionEvent

	// cumulativeTokens tracks total input+output across turns since the
	// last compaction. Reset to 0 on compact.
	cumulativeTokens int

	// sessionHandle is the bridle session id used for resume on
	// subprocess-stream providers. Rotated on compaction.
	sessionHandle bridle.SessionHandle
}

// New constructs a Funnel from cfg. Returns an error if required fields
// are missing.
func New(cfg Config) (*Funnel, error) {
	if cfg.AspectID == "" {
		return nil, errors.New("funnel: AspectID required")
	}
	if cfg.Harness == nil {
		return nil, errors.New("funnel: Harness required")
	}
	if cfg.Provider == "" {
		return nil, errors.New("funnel: Provider required")
	}
	if cfg.Model == "" {
		return nil, errors.New("funnel: Model required")
	}
	if cfg.Runner == nil {
		return nil, errors.New("funnel: Runner required")
	}
	if cfg.Compaction.ThresholdTokens == 0 {
		cfg.Compaction = DefaultCompactionPolicy()
	}
	if cfg.Events == nil {
		cfg.Events = NoopSink{}
	}
	if cfg.Filter == nil {
		cfg.Filter = AlwaysPostFilter{}
	}
	if cfg.Pulser == nil {
		cfg.Pulser = NoopPulser{}
	}
	if cfg.UsageRecorder == nil {
		cfg.UsageRecorder = NoopUsageRecorder{}
	}
	if cfg.PostTurn == nil {
		cfg.PostTurn = NoopPostTurn{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Funnel{
		cfg: cfg,
		log: cfg.Logger,
		// New=true so the first deliberation tells the provider this is
		// a fresh session for this ID. Flipped to false after the first
		// turn returns successfully (and again on every compaction-driven
		// session rotation).
		sessionHandle: bridle.SessionHandle{ID: newSessionID(), New: true},
	}, nil
}

// Receive enqueues an inbound comm for the next deliberation. Mid-turn
// comms-inbox-as-array per #81: anything received during a running
// deliberation accumulates and folds into the next turn.
func (f *Funnel) Receive(item bridle.InboxItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inbox = append(f.inbox, item)
}

// ReceiveWithMsgID is Receive plus Lock 4 attribution: stores the
// chat msg_id that triggered this deliberation so the funnel's
// UsageRecorder can attribute the resulting turn's tokens back to
// the originating chat message (operator #9254/#9258 forensics).
//
// If multiple Receive calls land before Deliberate runs, the LATEST
// one wins — that's the message most-recently visible to the model
// and the closest fit for "what triggered this turn" attribution.
// Earlier messages are still folded into the inbox; their token
// cost gets attributed to the latest msgID. Acceptable: the operator
// query is "where did the tokens go" and a clustered deliberation
// gets credited to the trigger that closed the latency window.
func (f *Funnel) ReceiveWithMsgID(item bridle.InboxItem, msgID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Stamp MsgID onto the item so the deliberation prompt + the
	// triage tool can reference it. Zero means the caller didn't
	// supply one (e.g. synthetic injection); triage contract treats
	// MsgID==0 as not-applicable per bridle.InboxItem docs.
	item.MsgID = msgID
	f.inbox = append(f.inbox, item)
	if msgID > 0 {
		f.triggeringMsgID = msgID
	}
}

// InboxLen reports the current inbox depth. Useful for observability.
func (f *Funnel) InboxLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inbox)
}

// Deliberate runs one full deliberation cycle: drain inbox → check
// compaction threshold → bridle.RunTurn → post-hoc filter judges
// the natural reply (Lock 1.3 / Lock 3). Returns the bridle.TurnResult
// of the primary turn (compaction's summarize turn isn't surfaced)
// alongside the FilterDecision the funnel made about whether the
// reply should post.
//
// Callers consult FilterDecision.ShouldPost to decide whether to
// surface result.FinalText to chat. F1.2/F1.4 wire the actual posting
// path; today's caller in cmd/nexus/main.go just respects the
// decision implicitly by not having a posting path yet.
//
// Returns ErrEmptyInbox if no comms are pending and userMessage is
// empty — a no-op deliberation isn't useful.
func (f *Funnel) Deliberate(ctx context.Context, userMessage string) (DeliberateResult, error) {
	f.mu.Lock()
	if len(f.inbox) == 0 && userMessage == "" {
		f.mu.Unlock()
		return DeliberateResult{}, ErrEmptyInbox
	}

	// Drain inbox under lock. Mid-deliberation Receive calls will
	// accumulate into the next cycle's inbox.
	pending := make([]bridle.InboxItem, len(f.inbox))
	copy(pending, f.inbox)
	f.inbox = f.inbox[:0]

	// Capture and clear the triggering chat msg_id for Lock 4
	// usage attribution. Subsequent ReceiveWithMsgID calls during
	// this deliberation queue into the next cycle's inbox; their
	// msg_ids will attribute to the next turn.
	triggerMsgID := f.triggeringMsgID
	f.triggeringMsgID = 0

	// Check compaction threshold before running the turn. If we'd cross
	// it, summarize first and rotate the session.
	threshold := f.cfg.Compaction.ThresholdTokens
	shouldCompact := f.cumulativeTokens >= threshold

	tail := append([]bridle.SessionEvent(nil), f.sessionTail...)
	session := f.sessionHandle
	f.mu.Unlock()

	if shouldCompact {
		if err := f.compact(ctx, tail); err != nil {
			f.log.Warn("funnel: compaction failed; continuing without it", "err", err)
			// Don't fail the deliberation — proceed with the existing
			// tail and let the provider's auto-compact handle it if
			// the threshold is also crossed there.
		} else {
			// Refresh local view post-compaction.
			f.mu.Lock()
			tail = append([]bridle.SessionEvent(nil), f.sessionTail...)
			session = f.sessionHandle
			f.mu.Unlock()
		}
	}

	systemPrompt := f.cfg.SystemPrompt
	if f.cfg.SystemPromptFn != nil {
		systemPrompt = f.cfg.SystemPromptFn()
	}
	req := bridle.TurnRequest{
		AspectID:     f.cfg.AspectID,
		SystemPrompt: systemPrompt,
		Session:      session,
		SessionTail:  tail,
		UserMessage:  userMessage,
		Inbox:        pending,
		Tools:        f.cfg.Tools,
		MCP:          f.cfg.MCP,
		Provider:     f.cfg.Provider,
		Model:        f.cfg.Model,
		MaxSteps:     f.cfg.MaxStepsPerTurn,
	}

	turnID := newTurnID()
	turnStart := time.Now()
	f.emit(ctx, Event{
		Type: EventTurnStart,
		Payload: TurnStartPayload{
			TurnID:        turnID,
			Round:         1,
			ContextTokens: estimateContextTokens(tail, pending, userMessage),
		},
	})

	sink := &collectSink{}
	result, err := f.cfg.Harness.RunTurn(ctx, req, f.cfg.Runner, sink)
	// turn.end must fire whether the turn succeeded or errored — the
	// Lock 5 spec promises every turn.start has a paired turn.end.
	// Without this, dashboards listening for paired events would
	// register every provider error as a stuck turn.
	f.emit(ctx, Event{
		Type: EventTurnEnd,
		Payload: TurnEndPayload{
			TurnID:     turnID,
			Usage:      result.Usage,
			StopReason: result.StopReason,
			StepCount:  result.StepCount,
			Duration:   time.Since(turnStart),
		},
	})

	// Lock 4 usage attribution. Always recorded (success and error
	// paths) so a turn that errored still has its partial usage
	// captured — billing apportions to errored turns too. Errors
	// from the recorder are logged but never fail the deliberation.
	if recErr := f.cfg.UsageRecorder.Record(ctx, triggerMsgID, turnID, f.cfg.AspectID, f.cfg.Model, result.Usage); recErr != nil {
		f.log.Warn("funnel: usage record failed",
			"err", recErr, "turn_id", turnID, "msg_id", triggerMsgID)
	}

	if err != nil {
		// Error path skips the cumulative-token update and the post-hoc
		// filter — neither has anything meaningful to do with a turn
		// that didn't produce a normal completion. The turn.end event
		// above already fired with whatever Usage the provider returned
		// (often zero, but some SDKs report partial usage on timeout).
		// F1.4 token-attribution work should NOT rely on
		// cumulativeTokens being precise across error retries — this
		// is the right place to look if attribution numbers ever
		// disagree with the provider's billing.
		//
		// Flip sessionHandle.New=false even on error. The provider may
		// have written the underlying session jsonl (claudecode does
		// this once `--session-id` is accepted, even if a later step
		// fails), so the next turn MUST resume rather than try to
		// create the same id again. Without this flip, every error
		// pins the session in the "new" state and subsequent turns
		// fail with "Session ID already in use" forever.
		f.mu.Lock()
		f.sessionHandle.New = false
		f.mu.Unlock()
		return DeliberateResult{TurnResult: result}, err
	}

	// Append the turn's session delta + update cumulative tokens. If
	// the v2 log-decision turn lands, this is where it'd gate the append.
	// Also flip sessionHandle.New to false: the provider has now created
	// the underlying session (e.g. claudecode wrote the jsonl), so future
	// turns should resume rather than re-create.
	f.mu.Lock()
	f.sessionTail = append(f.sessionTail, result.SessionDelta...)
	f.cumulativeTokens += result.Usage.InputTokens + result.Usage.OutputTokens
	f.sessionHandle.New = false
	f.mu.Unlock()

	// Post-turn hook — distills the just-completed turn's tail in
	// claude-code's session jsonl before we hit --resume on the next
	// turn. Synchronous; the rewriter's atomic temp-rename is safe
	// because no provider call is in flight here. If sustained
	// distillation failures cross the runner's threshold, rotate the
	// session id to a fresh one rather than continue racking up
	// errors against a session we can't compress.
	f.cfg.PostTurn.AfterTurn(ctx)
	if f.cfg.PostTurn.ShouldResetSession() {
		// Reset shape mirrors compaction: rotate session id to a
		// fresh one AND clear sessionTail + cumulativeTokens. The
		// rewriter requested this because it couldn't compress the
		// existing jsonl; carrying the same large sessionTail into
		// the new session would defeat the purpose (next turn would
		// inherit the bloat and the rewriter would fail again on the
		// new file). Better to start fully clean.
		f.mu.Lock()
		oldID := f.sessionHandle.ID
		oldTail := len(f.sessionTail)
		oldTokens := f.cumulativeTokens
		f.sessionHandle = bridle.SessionHandle{ID: newSessionID(), New: true}
		f.sessionTail = nil
		f.cumulativeTokens = 0
		newID := f.sessionHandle.ID
		f.mu.Unlock()
		f.cfg.PostTurn.AcknowledgeReset()
		f.log.Warn("funnel: rotated session after sustained rewriter failures",
			"old_session", oldID, "new_session", newID,
			"discarded_tail_events", oldTail, "discarded_tokens", oldTokens)
	}

	// Post-hoc filter judges the natural reply. Lock 5's
	// EventFilterJudging fires before the call so dashboards can
	// distinguish "filter is running" from "filter result back."
	f.emit(ctx, Event{
		Type:    EventFilterJudging,
		Payload: FilterJudgingPayload{TurnID: turnID},
	})
	decision := f.runFilter(ctx, FilterInput{
		FinalText: result.FinalText,
		AspectID:  f.cfg.AspectID,
		TurnID:    turnID,
	})

	f.log.Info("funnel: turn complete",
		"aspect", f.cfg.AspectID,
		"steps", result.StepCount,
		"tool_calls", len(result.ToolCalls),
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cumulative", f.cumulativeTokens,
		"stop_reason", result.StopReason,
		"filter_post", decision.ShouldPost,
		"filter_reason", decision.Reason)

	// Auto-post the model's natural reply when the filter approves and a
	// gateway is wired. This closes the gap left when providers don't
	// expose chat tools to the model (claudecode's subprocess mode):
	// without this, the model produces FinalText, the filter says
	// ShouldPost, but nobody calls SendChat — the reply never reaches
	// chat. ReplyTo threads the post under the message that triggered
	// the deliberation when one exists; non-triggered turns post
	// top-level.
	if decision.ShouldPost && f.cfg.ChatGateway != nil {
		text := strings.TrimSpace(result.FinalText)
		if text != "" {
			if msgID, err := f.cfg.ChatGateway.SendChat(ctx, text, triggerMsgID, ""); err != nil {
				f.log.Warn("funnel: auto-post failed",
					"aspect", f.cfg.AspectID,
					"trigger_msg_id", triggerMsgID,
					"err", err)
			} else {
				f.log.Info("funnel: auto-posted",
					"aspect", f.cfg.AspectID,
					"msg_id", msgID,
					"reply_to", triggerMsgID,
					"chars", len(text))
			}
		}
	}

	return DeliberateResult{TurnResult: result, Filter: decision}, nil
}

// DeliberateResult is the funnel-level outcome of one deliberation
// cycle: the bridle TurnResult plus the post-hoc filter's decision
// about whether the natural reply should post to chat. Per Lock 1.3
// / Lock 3 of the architecture.
//
// Callers consult Filter.ShouldPost to decide whether to surface
// TurnResult.FinalText. F1.4 (comms tool surface) wires the actual
// posting path and consumes this directly.
type DeliberateResult struct {
	TurnResult bridle.TurnResult
	Filter     FilterDecision
}

// compact runs a summarize turn, rolls the session, and replaces the
// SessionTail with a single summary record. Cumulative token counter
// resets. See docs/2026-05-01-funnel-compaction-design.md.
//
// Single-caller assumption: compact assumes the calling Deliberate
// loop serializes itself. Two concurrent Deliberate calls would race
// here. v1 has one caller (the Frame's main loop), and that's the
// invariant. If Deliberate ever fans out, this needs a guard.
func (f *Funnel) compact(ctx context.Context, tail []bridle.SessionEvent) error {
	if len(tail) == 0 {
		// Nothing to compact.
		return nil
	}

	tokensBefore := f.snapshotCumulative()
	compactStart := time.Now()

	// Pulse the chat surface BEFORE the lifecycle event fires so the
	// human-visible signal precedes the machine-readable one. Per
	// Lock 5 the funnel must announce long ops before they start —
	// silence-during-compaction was the exact failure mode operators
	// kept reading as "stuck" in agent-network.
	f.pulse(ctx, StatusPulse{
		Kind:              PulseKindCompact,
		Reason:            "compacting context — summarizing prior session before next turn",
		EstimatedDuration: estimatedCompactDuration,
	})

	f.emit(ctx, Event{
		Type: EventCompactStart,
		Payload: CompactStartPayload{
			Reason:       CompactReasonSoft,
			TokensBefore: tokensBefore,
			TargetTokens: f.cfg.Compaction.MaxSummaryTokens,
		},
	})

	model := f.cfg.Compaction.SummarizationModel
	if model == "" {
		model = f.cfg.Model
	}

	summarizePrompt := summarizationPrompt
	req := bridle.TurnRequest{
		AspectID:     f.cfg.AspectID,
		SystemPrompt: summarizePrompt,
		// Fresh session for the summarize turn so it doesn't pollute
		// the main session JSONL.
		Session:     bridle.SessionHandle{ID: newSessionID(), New: true},
		SessionTail: tail,
		UserMessage: "Summarize this session into a compact briefing the model can use to continue.",
		Provider:    f.cfg.Provider,
		Model:       model,
		MaxSteps:    1, // pure text; one round is enough
	}

	sink := &collectSink{}
	result, err := f.cfg.Harness.RunTurn(ctx, req, f.cfg.Runner, sink)
	if err != nil {
		return err
	}
	if result.FinalText == "" {
		return errors.New("funnel: summarize turn produced empty result")
	}

	// Mirror the claude-code two-record shape per the compaction design:
	// (1) system compact_boundary; (2) user message with isCompactSummary.
	// We use bridle.SessionEvent's plain shape for portability — provider-
	// specific compact_boundary metadata is left as future work.
	summary := bridle.SessionEvent{
		Role:    bridle.RoleUser,
		Content: result.FinalText,
	}

	f.mu.Lock()
	f.sessionTail = []bridle.SessionEvent{summary}
	f.cumulativeTokens = result.Usage.OutputTokens // the summary itself counts toward the next budget
	// New session minted by compaction — flag as fresh so the provider
	// creates the underlying session rather than trying to resume an id
	// it has never seen.
	f.sessionHandle = bridle.SessionHandle{ID: newSessionID(), New: true}
	f.mu.Unlock()

	f.emit(ctx, Event{
		Type: EventCompactEnd,
		Payload: CompactEndPayload{
			TokensBefore: tokensBefore,
			TokensAfter:  result.Usage.OutputTokens,
			Duration:     time.Since(compactStart),
		},
	})

	f.log.Info("funnel: compaction complete",
		"summary_tokens", result.Usage.OutputTokens,
		"new_session", f.sessionHandle.ID)
	return nil
}

// SessionTail returns a snapshot of the current session events.
// Useful for observability / dashboard display. Read-only — caller
// must not mutate.
func (f *Funnel) SessionTail() []bridle.SessionEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bridle.SessionEvent, len(f.sessionTail))
	copy(out, f.sessionTail)
	return out
}

// CumulativeTokens reports total input+output across all turns since
// the last compaction. Useful for dashboards and tests asserting the
// compaction trigger.
func (f *Funnel) CumulativeTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cumulativeTokens
}

// SessionID returns the current bridle session handle. Rotates on
// compaction.
func (f *Funnel) SessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionHandle.ID
}

// ErrEmptyInbox is returned by Deliberate when there's nothing to
// deliberate on (no inbox items AND empty user message).
var ErrEmptyInbox = errors.New("funnel: empty inbox + empty user message; nothing to deliberate")

// summarizationPrompt is the system prompt used during compaction's
// cheap summarize turn. Optimized for "produce a faithful, compact
// briefing" rather than continuing the deliberation. Per anvil's
// design (00c6dd9).
const summarizationPrompt = `You are a session summarization assistant. The session below is being compacted to fit within context limits. Your job: produce a compact briefing that captures:
- The current goal/task being worked on
- Key decisions made and their rationale
- Open questions and pending work
- Anything the next turn needs to continue without re-reading prior history

Be terse. Strip pleasantries. Preserve only what the model needs to continue. Output the briefing as a single message, no preamble.`

// newSessionID mints a UUIDv4 session id for bridle's --session-id /
// --resume threading. claude-code's CLI requires a UUID for --resume
// (rejects timestamped strings); UUIDv4 is the safe lowest-common-
// denominator for all bridle providers.
//
// Pre-fix this returned a time-based string (YYYYMMDDTHHMMSS.uuuuuuZ-XX),
// which the claude-code provider's RunTurn would pass to `claude --resume`
// and the CLI rejected with "not a UUID and does not match any session
// title." Operator F2.6 smoke surfaced this — fixed during the test run.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("funnel: crypto/rand failed: " + err.Error())
	}
	// RFC 4122 v4 bits: 4-bit version 0x4 in byte 6 high nibble, and
	// the 2-bit variant 0b10 in byte 8 high bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randHex is a tiny helper for the session-id suffix. Not exported.
// Failure is impossible in practice (crypto/rand), so panic on the
// rare case keeps callers free of unexpected error returns.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("funnel: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NullRunner is a ToolRunner that returns an empty JSON object for
// every call. Used when the Frame has no in-process tools registered —
// the model still gets a coherent (if useless) tool response so the
// turn can complete cleanly. Replace with a real runner once send_comms
// and other tools are wired.
type NullRunner struct{}

func (NullRunner) Run(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// collectSink is a no-op EventSink. v1 funnel doesn't act on bridle
// events directly — the TurnResult carries enough for deliberation
// flow. Future: route ModelChunk to a UI streaming channel, hook
// AfterToolCall for spend caps, etc.
type collectSink struct{}

func (collectSink) Emit(_ bridle.Event) {}

// emitTimeout caps how long emit() waits for a sink before logging
// and moving on. A blocking sink (e.g. a slow channel reader, a
// blocked WS write) must not stall deliberation — that's the exact
// "looks like a hang" failure Lock 5 was built to prevent.
const emitTimeout = 100 * time.Millisecond

// emit is the single internal entrypoint for lifecycle events. It
// stamps AspectID + EmittedAt so call sites can stay terse, recovers
// from sink panics so a misbehaving sink can never break the
// deliberation loop, and bounds Emit's wall-clock cost so a slow or
// blocked sink can't stall a turn.
//
// Sinks that need long-running work should buffer to a channel and
// return; the funnel does not wait for downstream delivery.
func (f *Funnel) emit(ctx context.Context, e Event) {
	e.AspectID = f.cfg.AspectID
	e.EmittedAt = time.Now()
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				f.log.Warn("funnel: event sink panicked; suppressing",
					"event", e.Type, "panic", r)
			}
			close(done)
		}()
		f.cfg.Events.Emit(ctx, e)
	}()
	select {
	case <-done:
	case <-time.After(emitTimeout):
		f.log.Warn("funnel: event sink slow; abandoning emit",
			"event", e.Type, "timeout", emitTimeout)
	case <-ctx.Done():
		f.log.Warn("funnel: context cancelled during emit", "event", e.Type)
	}
}

// snapshotCumulative reads the cumulative token count under the
// funnel's lock. Used by event payload construction so the count
// reflects the moment the event fires, not whatever the loop later
// updates it to.
func (f *Funnel) snapshotCumulative() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cumulativeTokens
}

// newTurnID mints a unique id for a single bridle.RunTurn invocation.
// Format mirrors session ids (timestamp + random suffix) — they're
// ordered, debuggable, and collision-free for a single Frame's
// lifetime.
func newTurnID() string {
	return "turn-" + time.Now().UTC().Format("20060102T150405.000000Z") + "-" + randHex(3)
}

// filterTimeout caps how long the funnel waits for a filter's Judge
// to return. CheapModelFilter sets its own ~1.5s internal cap; a
// custom filter that ignores ctx and blocks would otherwise stall
// the deliberation indefinitely. 2s gives the cheap-model filter
// some headroom while still bounding the worst case.
const filterTimeout = 2 * time.Second

// runFilter wraps OutputFilter.Judge with a goroutine + timeout so a
// blocking or misbehaving filter cannot stall deliberation. Mirrors
// the safety pattern around emit() — the filter is observability-
// adjacent and must not hold up the chat path.
//
// On timeout, fail open (ShouldPost=true). Same reasoning as
// CheapModelFilter's internal failure path: suppressing real content
// because telemetry hung is worse than the noise of letting a thin
// reply through.
func (f *Funnel) runFilter(ctx context.Context, in FilterInput) FilterDecision {
	ch := make(chan FilterDecision, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				f.log.Warn("funnel: filter panicked; failing open",
					"panic", r, "turn_id", in.TurnID)
				ch <- FilterDecision{ShouldPost: true}
			}
		}()
		ch <- f.cfg.Filter.Judge(ctx, in)
	}()
	select {
	case d := <-ch:
		return d
	case <-time.After(filterTimeout):
		f.log.Warn("funnel: filter timed out; failing open",
			"timeout", filterTimeout, "turn_id", in.TurnID)
		return FilterDecision{ShouldPost: true}
	case <-ctx.Done():
		f.log.Warn("funnel: context cancelled during filter; failing open",
			"turn_id", in.TurnID)
		return FilterDecision{ShouldPost: true}
	}
}

// estimateContextTokens approximates input tokens for a TurnStart
// payload — we don't have a tokenizer here and we don't want to drag
// one in just for a telemetry estimate. Rough heuristic: 4 chars per
// token, summed over tail content + inbox + user message.
//
// The real number lands in TurnEnd via bridle.Usage. This estimate
// exists so dashboard panels can show a "going in at ~X tokens" hint
// before a slow turn completes.
func estimateContextTokens(tail []bridle.SessionEvent, inbox []bridle.InboxItem, userMessage string) int {
	chars := len(userMessage)
	for _, ev := range tail {
		chars += len(ev.Content)
	}
	for _, item := range inbox {
		chars += len(item.Content)
	}
	return chars / 4
}
