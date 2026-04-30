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
	"log/slog"
	"sync"
	"time"

	"github.com/nexus-cw/bridle"
	"github.com/nexus-cw/nexus/nexus/frame/route"
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

	// bridle — the one-turn driver
	Harness *bridle.Harness

	// Provider selection
	Provider bridle.ProviderID
	Model    string
	MCP      *bridle.MCPClientConfig // optional; nil = no MCP-loaded tools
	Tools    []bridle.ToolDef        // explicit in-process tool defs (incl. send_comms)
	Runner   bridle.ToolRunner       // executes Tools

	// Compaction
	Compaction CompactionPolicy

	// MaxStepsPerTurn caps tool-call rounds inside a single bridle turn.
	// 0 = unlimited (bridle's default).
	MaxStepsPerTurn int

	// Routing — used by the Frame to decide what reaches the funnel.
	// Not consumed inside Deliberate, but stored here so callers have
	// one place to find the participation index.
	Threads *route.ThreadIndex

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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Funnel{
		cfg:           cfg,
		log:           cfg.Logger,
		sessionHandle: bridle.SessionHandle{ID: newSessionID()},
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

// InboxLen reports the current inbox depth. Useful for observability.
func (f *Funnel) InboxLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inbox)
}

// Deliberate runs one full deliberation cycle: drain inbox → check
// compaction threshold → bridle.RunTurn → log-decision turn (deferred
// to v2 — for v1, all turns are kept). Returns the bridle.TurnResult
// of the primary turn (compaction's summarize turn isn't surfaced).
//
// Returns ErrEmptyInbox if no comms are pending and userMessage is
// empty — a no-op deliberation isn't useful.
func (f *Funnel) Deliberate(ctx context.Context, userMessage string) (bridle.TurnResult, error) {
	f.mu.Lock()
	if len(f.inbox) == 0 && userMessage == "" {
		f.mu.Unlock()
		return bridle.TurnResult{}, ErrEmptyInbox
	}

	// Drain inbox under lock. Mid-deliberation Receive calls will
	// accumulate into the next cycle's inbox.
	pending := make([]bridle.InboxItem, len(f.inbox))
	copy(pending, f.inbox)
	f.inbox = f.inbox[:0]

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

	req := bridle.TurnRequest{
		AspectID:     f.cfg.AspectID,
		SystemPrompt: f.cfg.SystemPrompt,
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

	sink := &collectSink{}
	result, err := f.cfg.Harness.RunTurn(ctx, req, f.cfg.Runner, sink)
	if err != nil {
		return result, err
	}

	// Append the turn's session delta + update cumulative tokens. If
	// the v2 log-decision turn lands, this is where it'd gate the append.
	f.mu.Lock()
	f.sessionTail = append(f.sessionTail, result.SessionDelta...)
	f.cumulativeTokens += result.Usage.InputTokens + result.Usage.OutputTokens
	f.mu.Unlock()

	f.log.Info("funnel: turn complete",
		"aspect", f.cfg.AspectID,
		"steps", result.StepCount,
		"tool_calls", len(result.ToolCalls),
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cumulative", f.cumulativeTokens,
		"stop_reason", result.StopReason)

	return result, nil
}

// compact runs a summarize turn, rolls the session, and replaces the
// SessionTail with a single summary record. Cumulative token counter
// resets. See docs/2026-05-01-funnel-compaction-design.md.
func (f *Funnel) compact(ctx context.Context, tail []bridle.SessionEvent) error {
	if len(tail) == 0 {
		// Nothing to compact.
		return nil
	}

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
		Session:     bridle.SessionHandle{ID: newSessionID()},
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
	f.sessionHandle = bridle.SessionHandle{ID: newSessionID()}
	f.mu.Unlock()

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

// newSessionID mints a unique session id for bridle's --session-id /
// --resume threading. Time-based + random suffix; collision-infeasible
// for single-Frame use.
func newSessionID() string {
	return time.Now().UTC().Format("20060102T150405.000000Z") + "-" + randHex(4)
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
