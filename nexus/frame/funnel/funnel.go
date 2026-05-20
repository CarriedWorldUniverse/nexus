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
	"strconv"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/route"
	"github.com/google/uuid"
)

// CompactionPolicy tunes the funnel's context-window management.
// Per anvil's design (00c6dd9), the funnel proactively summarizes
// before hitting the provider's auto-compact threshold.
type CompactionPolicy struct {
	// ThresholdTokens is the cumulative input+output token count at
	// which the funnel runs a summarize-turn. Default 125_000 — keeps
	// the working context in the operator's 125K-150K target window.
	// cumulativeTokens counts only UNCACHED input + output; the cached
	// portion of the prefix (often 30-70K once the conversation warms
	// up) is on top of this number. Setting the trigger at 125K leaves
	// headroom for the cached prefix without overshooting claude-code's
	// empirically-observed 191k auto-compact.
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
		ThresholdTokens:  125_000,
		MaxSummaryTokens: 4_096,
	}
}

// ContextMode selects how the funnel derives session ids for
// bridle.TurnRequest.Session. Part of nexus task #226: each chat
// thread should keep its own claude-code jsonl so SessionTail doesn't
// bleed across threads.
type ContextMode string

const (
	// ContextGlobal keeps one session per funnel lifetime, rotated on
	// compaction. The historical behaviour — appropriate for the in-
	// process Frame and any aspect that genuinely deliberates across
	// all incoming chat as one stream of consciousness.
	ContextGlobal ContextMode = "global"

	// ContextThreadIsolated derives a deterministic per-thread session
	// id from (aspect_id, thread_root_msg_id) via uuid_v5. Each chat
	// thread therefore keeps its own jsonl on the claude-code side, no
	// cross-thread SessionTail bleed. Falls back to the funnel's
	// global handle when an inbox item arrives with ThreadRoot == 0
	// (synthetic / non-chat trigger / pre-#226 row).
	ContextThreadIsolated ContextMode = "thread"

	// ContextStateless mints a fresh uuid_v4 per turn — every turn
	// starts cold, no resume, no SessionTail-on-disk reuse. Intended
	// for eval harnesses, smoke tests, and aspects that genuinely
	// don't want continuity. Compaction is a no-op under this mode
	// because there's nothing accumulating to compact.
	ContextStateless ContextMode = "stateless"
)

// sessionNamespace is the uuid_v5 namespace under which thread-
// isolated session ids are derived. Fixed value, never changed —
// reshuffling the namespace would orphan every existing per-thread
// jsonl on disk and force a network-wide cold start.
var sessionNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("nexus.funnel.session.v1"))

// SessionResolver maps (ContextMode, thread root) to a bridle session
// handle, remembering which ids it has already minted so subsequent
// resolutions of the same id return New=false (the provider should
// --resume rather than re-create). Safe for concurrent use.
type SessionResolver struct {
	mu       sync.Mutex
	aspectID string
	mode     ContextMode
	known    map[string]bool

	// globalHandle is the handle used by ContextGlobal — and the
	// fallback returned by ContextThreadIsolated when an item arrives
	// with ThreadRoot == 0. Mutates on compaction-driven rotation
	// (RotateGlobal). Unused under ContextStateless.
	globalHandle bridle.SessionHandle
}

// NewSessionResolver constructs a resolver. AspectID seeds the per-
// thread uuid_v5 derivation so two aspects in the same thread still
// get different sessions on disk. Empty aspectID is accepted but the
// resulting ids will collide across funnels — production wiring must
// pass the real aspect id.
func NewSessionResolver(aspectID string, mode ContextMode) *SessionResolver {
	if mode == "" {
		mode = ContextGlobal
	}
	r := &SessionResolver{
		aspectID: aspectID,
		mode:     mode,
		known:    make(map[string]bool),
	}
	// Global/thread modes both lean on globalHandle (latter as the
	// no-thread fallback). Stateless skips it — every Resolve call
	// mints a fresh id and never touches globalHandle.
	if mode != ContextStateless {
		r.globalHandle = bridle.SessionHandle{ID: newSessionID(), New: true}
	}
	return r
}

// Resolve returns the session handle to use for a turn whose
// triggering inbox item has the given ThreadRoot. threadRoot==0 means
// "no thread context" (synthetic / non-chat / legacy) and degrades
// to the global handle even under ContextThreadIsolated.
func (r *SessionResolver) Resolve(threadRoot int64) bridle.SessionHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.mode {
	case ContextStateless:
		id := newSessionID()
		// Stateless ids are by definition new; we don't track them.
		return bridle.SessionHandle{ID: id, New: true}
	case ContextThreadIsolated:
		if threadRoot == 0 {
			return r.globalHandle
		}
		id := uuid.NewSHA1(sessionNamespace, []byte(r.aspectID+":"+strconv.FormatInt(threadRoot, 10))).String()
		isNew := !r.known[id]
		r.known[id] = true
		return bridle.SessionHandle{ID: id, New: isNew}
	default: // ContextGlobal
		return r.globalHandle
	}
}

// MarkResumed flips the New flag off for a session id the provider
// has acknowledged. Called by Deliberate after the first successful
// turn against a given session id so subsequent turns --resume rather
// than try to re-create. Idempotent.
func (r *SessionResolver) MarkResumed(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known[id] = true
	if r.globalHandle.ID == id {
		r.globalHandle.New = false
	}
}

// RotateGlobal mints a fresh global handle. Used by compaction in
// ContextGlobal mode (and the no-thread fallback path in
// ContextThreadIsolated). No-op under ContextStateless — there is no
// persistent handle to rotate.
func (r *SessionResolver) RotateGlobal() bridle.SessionHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode == ContextStateless {
		// Return a fresh handle without storing; caller can use it
		// once but it's not retained.
		return bridle.SessionHandle{ID: newSessionID(), New: true}
	}
	r.globalHandle = bridle.SessionHandle{ID: newSessionID(), New: true}
	return r.globalHandle
}

// GlobalHandle returns the current global handle (without rotating
// it). Exposed for tests + observability.
func (r *SessionResolver) GlobalHandle() bridle.SessionHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.globalHandle
}

// Mode reports the resolver's configured mode.
func (r *SessionResolver) Mode() ContextMode {
	return r.mode
}

// Config wires the funnel's dependencies. All fields except the
// optional ones are required.
type Config struct {
	// Identity & framing
	AspectID     string // the Frame's name (operator-chosen)
	SystemPrompt string // composed from NEXUS.md/SOUL.md/PRIMER.md by the caller

	// AspectHome is the filesystem directory the aspect "lives in" —
	// passed to bridle as TurnRequest.Cwd to anchor subprocess providers
	// (currently claude-code). claude-code derives both its session
	// jsonl path and its .mcp.json discovery from process cwd, so this
	// is what determines per-aspect identity isolation when multiple
	// aspects share a Harness or when nexus.exe inherits a cwd from
	// its launcher. Empty falls through to the parent process's cwd —
	// fine for tests (stubfunnel etc.), wrong for production aspects.
	AspectHome string

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

	// ChatGateway is the chat-posting seam used by the default
	// NexusChatReturnHandler to auto-post the model's natural reply at
	// end-of-turn when the post-hoc filter approves. Optional — when
	// nil, the default return handler is a no-op and FinalText doesn't
	// reach chat. Production wiring sets this to the same ChatGateway
	// the CommsRunner uses, so the auto-post and explicit send_chat
	// tool calls converge on the same path (Broker.HandleChatSend →
	// persistence + fan-out).
	//
	// As of NEX-82 this field is consulted only to build the default
	// Return handler when Return is left nil. Callers that pass an
	// explicit Return handler can leave ChatGateway nil — the handler
	// owns its own posting surface.
	ChatGateway ChatGateway

	// StreamTextToChat, when true, posts each assistant text block to
	// chat as it streams from the provider rather than buffering and
	// posting once at turn-end. Gives the operator live visibility into
	// a turn's progress. Tool-use events stay on the activity feed.
	// When enabled, the auto-post in NexusChatReturnHandler is
	// suppressed (text was already streamed). Requires ChatGateway.
	StreamTextToChat bool

	// Return is the post-deliberation routing seam (NEX-82). The
	// engine calls Return.OnTurnStart at turn pickup and Return.Handle
	// at turn end. Implementations decide what to do with the result
	// given the trigger context (chat reply for nexus, panel-state for
	// agora, etc.).
	//
	// nil = funnel.New picks a default based on ChatGateway:
	//   - ChatGateway non-nil → NexusChatReturnHandler{Gateway: ChatGateway}
	//     (the pre-NEX-82 behavior: 👀/👍/🙊 reactions + auto-post).
	//   - ChatGateway nil    → NoopReturnHandler (headless paths —
	//     operator REST eval, funnel_test in-memory).
	//
	// Explicit Return overrides the default. agora-side composes its
	// own Source-tagged ReturnHandler to route chat-vs-tty.
	Return ReturnHandler

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

	// Triage persists per-msg-id triage decisions per the inbox-triage
	// contract (docs/2026-05-10-funnel-triage-contract.md). After every
	// deliberation the funnel reconciles the inbox against persisted
	// decisions; any msg_id the model failed to triage gets a synthetic
	// skip row tagged "no_triage_emitted" so the operator's 1:1 view
	// can audit "did the aspect see this msg, and what did it decide?"
	//
	// Nil disables enforcement (no synthetic skips). The triage tool
	// still runs but with no persistence, matching legacy aspect.exe /
	// agentfunnel callers that haven't migrated yet.
	Triage chat.TriageStore

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

	// ObservabilityHook receives bridle's raw event stream plus per-
	// turn boundary calls (BeginTurn / EndTurn). When set, the funnel
	// wraps every Harness.RunTurn call site (main turn, compact, and
	// the post-hoc filter judge) to forward events. Nil disables
	// observability — the pre-Phase-E no-op path. Production wiring
	// passes broker's observability.Hub.GrouperFor(aspectID).
	//
	// Dual-scoping with Events is intentional; see ObservabilityHook
	// interface comment and docs/2026-05-12-funnel-observability-audit.md.
	ObservabilityHook ObservabilityHook

	// ContextMode controls how session ids are derived per turn (task
	// #226 session isolation). Defaults to ContextGlobal — one session
	// per funnel lifetime, rotated on compaction. Set to
	// ContextThreadIsolated for funnel-driven aspects (agentfunnel,
	// out-of-process) so each chat thread keeps its own jsonl. See
	// ContextMode docs.
	ContextMode ContextMode

	// ProviderEnvResolver is consulted on every TurnRequest to fetch
	// the auth/routing env the provider should use for THAT turn
	// (task #218 credential-store routing). Returns (env, true) when a
	// credential is resolved; (nil, false) when the funnel should fall
	// through to the provider's own defaults (subscription claude-code,
	// process-env API keys, --bare-style flags).
	//
	// kind names the call-site that's about to fire — main, compact,
	// or filter — so the resolver can route different lanes to
	// different credentials (e.g. main turn against operator's
	// Anthropic credit pool, judge against DeepSeek-via-Anthropic-shape
	// credential). Nil = no resolution; provider runs unchanged.
	ProviderEnvResolver ProviderEnvResolver

	// IdempotencyFile, if set, is the path where the funnel persists
	// its seen-msg_id set so duplicate-delivery dropping survives
	// process restart (NEX-96). Empty disables persistence — in-memory
	// only; acceptable for short-lived test funnels. Production funnels
	// should set this alongside the wsasp cursor file so at-least-once
	// delivery + funnel idempotency = effectively exactly-once.
	IdempotencyFile string

	// IdempotencyCap caps the in-memory seen-msg_id set. Older entries
	// are evicted FIFO when the cap is reached. 0 = default (1000),
	// which covers any plausible disconnect-window backlog without
	// unbounded memory.
	IdempotencyCap int

	Logger *slog.Logger
}

// ProviderEnvResolver returns the per-turn auth/routing env the
// funnel should overlay onto bridle.TurnRequest.ProviderEnv. Implementations
// usually wrap nexus/credentials.Store.ResolveDefaultForAspect plus a
// fallback policy (which shape to use, when to return no env so the
// provider falls through to subscription/process-env).
type ProviderEnvResolver interface {
	// Resolve returns the env map for the upcoming turn. kind is one
	// of "main", "compact", "filter". aspectID is the funnel's
	// configured AspectID. Returning (nil, false, nil) means "no env
	// overlay, let provider use its own auth"; (env, true, nil) means
	// "overlay these onto ProviderEnv"; non-nil err propagates as a
	// turn error.
	Resolve(ctx context.Context, aspectID, kind string) (env map[string]string, ok bool, err error)
}

// Funnel is the deliberation engine. One Funnel per Frame; the Frame
// owns its lifetime.
type Funnel struct {
	cfg Config
	log *slog.Logger

	mu sync.Mutex // guards inbox, sessionTail, cumulativeTokens, sessionHandle, seenMsgIDs

	// inbox holds comms that arrived since the last deliberation. Folded
	// into the next bridle.RunTurn call. Drained at deliberation start.
	inbox []bridle.InboxItem

	// seenMsgIDs records msg_ids the funnel has already deliberated on
	// (NEX-96). Broker delivers at-least-once per Lock 6; on reconnect
	// with a stale cursor, already-handled messages can re-arrive.
	// Receive checks this set and drops duplicates rather than appending
	// them to the inbox. Bounded FIFO via seenOrder; persisted to
	// cfg.IdempotencyFile so the guarantee survives restart.
	seenMsgIDs map[int64]struct{}
	seenOrder  []int64

	// triggeringMsgID is the chat msg_id that prompted the next
	// deliberation. Set by ReceiveWithMsgID; consumed and cleared
	// by Deliberate so each turn's UsageRecorder.Record call gets
	// the correct attribution. Zero means "no chat trigger" — the
	// recorder writes MsgID=0, which the usage table stores as NULL.
	triggeringMsgID int64

	// triggeringFrom / triggeringContent mirror triggeringMsgID — the
	// From and Content of the InboxItem that closed the latency window.
	// Passed into FilterInput so the cheap-judge can evaluate the
	// candidate AS A REPLY TO the trigger (not in isolation). Without
	// this context, short substantive replies routinely got mislabeled
	// as scratch. Cleared by Deliberate alongside triggeringMsgID.
	triggeringFrom    string
	triggeringContent string

	// sessionTail accumulates events across turns. Compacted when
	// cumulativeTokens crosses the threshold.
	sessionTail []bridle.SessionEvent

	// cumulativeTokens tracks total input+output across turns since the
	// last compaction. Reset to 0 on compact.
	cumulativeTokens int

	// sessionHandle is the bridle session id used for resume on
	// subprocess-stream providers. Under ContextGlobal it persists
	// across turns and rotates on compaction. Under other modes it's
	// overwritten per-Deliberate via the resolver — the field stays
	// for legacy SessionID() observability callers.
	sessionHandle bridle.SessionHandle

	// resolver owns the per-Deliberate session id derivation. Always
	// non-nil after New; see ContextMode.
	resolver *SessionResolver

	// goalDoD is the Definition of Done for the current goal-pursuit
	// turn (NEX-210). Set by GoalLoop via SetDoD before Deliberate;
	// read and cleared by Deliberate when constructing FilterInput.
	// Empty when no goal is active.
	goalDoD string
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
	// Default ReturnHandler wiring (NEX-82). Pre-split callers wired
	// ChatGateway and the funnel internalised the 👀/auto-post calls;
	// preserve that by building the default NexusChatReturnHandler
	// when Return is nil and ChatGateway is present. Headless callers
	// (no gateway, no explicit handler) get the noop. Explicit Return
	// values override both — agora's two-channel handler lands here.
	if cfg.Return == nil {
		if cfg.ChatGateway != nil {
			cfg.Return = &NexusChatReturnHandler{
				Gateway:          cfg.ChatGateway,
				AspectID:         cfg.AspectID,
				Logger:           cfg.Logger,
				SuppressAutoPost: cfg.StreamTextToChat,
			}
		} else {
			cfg.Return = NoopReturnHandler{}
		}
	}

	if cfg.IdempotencyCap == 0 {
		cfg.IdempotencyCap = 1000
	}

	resolver := NewSessionResolver(cfg.AspectID, cfg.ContextMode)
	f := &Funnel{
		cfg:           cfg,
		log:           cfg.Logger,
		resolver:      resolver,
		sessionHandle: resolver.GlobalHandle(),
		seenMsgIDs:    make(map[int64]struct{}),
	}
	// Hydrate the seen-set from disk if a persistence file is configured.
	// Best-effort: parse failure logs + continues with an empty set
	// (degrades to in-memory only for this process; production wiring
	// catches this in observability + the operator can re-mint the file).
	if err := f.loadSeenMsgIDs(); err != nil {
		f.log.Warn("funnel: idempotency hydrate failed",
			"path", cfg.IdempotencyFile, "err", err)
	}
	return f, nil
}

// Receive enqueues an inbound comm for the next deliberation. Mid-turn
// comms-inbox-as-array per #81: anything received during a running
// deliberation accumulates and folds into the next turn.
//
// Drops items whose MsgID is already in the seen-set (NEX-96 idempotency
// guard). Broker delivers at-least-once per Lock 6; if a reconnect with a
// stale cursor re-pushes already-deliberated messages, the funnel skips
// them rather than re-running the turn.
func (f *Funnel) Receive(item bridle.InboxItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item.MsgID > 0 {
		if _, seen := f.seenMsgIDs[item.MsgID]; seen {
			f.log.Debug("funnel: dropping duplicate inbox item",
				"aspect", f.cfg.AspectID, "msg_id", item.MsgID)
			return
		}
	}
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
	// NEX-96 idempotency: drop already-deliberated msg_ids.
	if msgID > 0 {
		if _, seen := f.seenMsgIDs[msgID]; seen {
			f.log.Debug("funnel: dropping duplicate inbox item (with-msgid)",
				"aspect", f.cfg.AspectID, "msg_id", msgID)
			return
		}
	}
	f.inbox = append(f.inbox, item)
	if msgID > 0 {
		f.triggeringMsgID = msgID
		// Capture from + content for the filter judge (latest wins,
		// matching triggeringMsgID's LATEST semantics — same message
		// is the one credited with the turn's tokens and the one the
		// judge evaluates the candidate against).
		f.triggeringFrom = item.From
		f.triggeringContent = item.Content
	}
}

// SetDoD stores the Definition of Done for the next deliberation
// (NEX-210). Read and cleared by Deliberate when constructing the
// FilterInput for the post-turn judge. Safe for concurrent use —
// the goal-loop calls this from the same goroutine that calls
// Deliberate, but observability readers may access it.
func (f *Funnel) SetDoD(dod string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.goalDoD = dod
}

// takeDoD reads and clears the goal DoD. Called by Deliberate once per turn.
func (f *Funnel) takeDoD() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.goalDoD
	f.goalDoD = ""
	return d
}

// ReceiveSynthetic enqueues a synthetic inbox item — one that didn't
// originate from a chat message (MsgID=0). Used by the goal-loop
// (NEX-210) to inject continuation briefs, and by other internal
// producers that need to stimulate a deliberation without a real
// chat trigger. Preserves ThreadRoot for per-thread session isolation.
func (f *Funnel) ReceiveSynthetic(item bridle.InboxItem) {
	item.MsgID = 0 // synthetic — not a real chat message
	f.Receive(item)
}

// InboxLen reports the current inbox depth. Useful for observability.
func (f *Funnel) InboxLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inbox)
}

// Deliberate runs one full deliberation cycle for EXACTLY ONE inbox
// message (FIFO head-pop) → check compaction threshold → bridle.RunTurn
// → post-hoc filter judges the natural reply. Returns the bridle.TurnResult
// alongside the FilterDecision.
//
// Per #224 (operator 2026-05-13): the inbox is a FIFO queue and each
// Deliberate call pops exactly one head item. Cross-thread context
// folding (the prior "drain all into one prompt" behavior) corrupts
// reasoning and breaks the thread-targeting invariant — one turn's
// reasoning happens in the context of ONE msg_id, producing ONE reply,
// threaded under ONE parent. Cross-thread bursts naturally serialize
// into N turns.
//
// Callers that want to fully drain a burst should loop:
//
//	for {
//	    _, err := f.Deliberate(ctx, "")
//	    if errors.Is(err, ErrEmptyInbox) { break }
//	    // handle other errors
//	}
//
// Returns ErrEmptyInbox if no comms are pending and userMessage is
// empty — a no-op deliberation isn't useful.
//
// Callers consult FilterDecision.ShouldPost to decide whether to
// surface result.FinalText to chat. The funnel's auto-post path uses
// the popped item's msg_id as reply_to, so threading is intrinsic.
func (f *Funnel) Deliberate(ctx context.Context, userMessage string) (DeliberateResult, error) {
	f.mu.Lock()
	if len(f.inbox) == 0 && userMessage == "" {
		f.mu.Unlock()
		return DeliberateResult{}, ErrEmptyInbox
	}

	// FIFO pop: take the HEAD item only. Remaining items stay in
	// f.inbox for the next Deliberate call. Per #224, this preserves
	// the thread context invariant — each turn handles one msg in
	// isolation, queue depth is invisible to the model.
	var pending []bridle.InboxItem
	if len(f.inbox) > 0 {
		head := f.inbox[0]
		// Shift remaining items down. Copy-then-truncate so the slice's
		// backing array doesn't pin freed item references.
		copy(f.inbox, f.inbox[1:])
		f.inbox = f.inbox[:len(f.inbox)-1]
		pending = []bridle.InboxItem{head}

		// NEX-96 idempotency: record the popped msg_id so any future
		// duplicate-delivery (broker re-push after reconnect with stale
		// cursor) gets dropped at Receive rather than re-deliberated.
		// markSeenLocked maintains FIFO eviction at IdempotencyCap.
		if head.MsgID > 0 {
			f.markSeenLocked(head.MsgID)
		}
	}

	// Trigger context comes from the popped item (not from the legacy
	// "latest Receive wins" path — that conflated multiple msgs into one
	// trigger). When userMessage drives the turn with no inbox item,
	// trigger fields stay zero/empty.
	var triggerMsgID, triggerThreadRoot int64
	var triggerFrom, triggerContent string
	if len(pending) > 0 {
		triggerMsgID = pending[0].MsgID
		triggerFrom = pending[0].From
		triggerContent = pending[0].Content
		triggerThreadRoot = pending[0].ThreadRoot
	}
	// Clear the cached "latest Receive" fields — they're vestigial under
	// FIFO semantics but kept until ReceiveWithMsgID is refactored to
	// stop writing them. Reset on every Deliberate for safety.
	f.triggeringMsgID = 0
	f.triggeringFrom = ""
	f.triggeringContent = ""

	// Build the typed TurnTrigger from the popped item (or zero-value
	// for non-inbox-driven turns). Single source of truth for the
	// return-side context; both OnTurnStart and Handle receive it.
	var trigger TurnTrigger
	if len(pending) > 0 {
		trigger = triggerFromInboxItem(pending[0])
	}

	// NEX-82: Return.OnTurnStart fires the "picking it up" pulse.
	// Default impl (NexusChatReturnHandler) writes 👀 on the trigger
	// msg via ChatGateway. Noop for headless callers. Errors are
	// logged but never abort the turn — failed start-pulse shouldn't
	// kill substantive work.
	if err := f.cfg.Return.OnTurnStart(ctx, trigger); err != nil {
		f.log.Debug("funnel: return handler OnTurnStart failed",
			"aspect", f.cfg.AspectID, "trigger_msg_id", trigger.MsgID, "err", err)
	}

	// Check compaction threshold before running the turn. If we'd cross
	// it, summarize first and rotate the session.
	threshold := f.cfg.Compaction.ThresholdTokens
	shouldCompact := f.cumulativeTokens >= threshold

	tail := append([]bridle.SessionEvent(nil), f.sessionTail...)
	f.mu.Unlock()

	// Resolve the session for this turn via the per-mode resolver
	// (task #226.4). Under ContextGlobal this returns the funnel-wide
	// handle (rotated by compaction); under ContextThreadIsolated it
	// returns a deterministic per-thread id; under ContextStateless a
	// fresh uuid per turn. triggerThreadRoot==0 falls back to the
	// global handle even in thread-isolated mode, so non-chat
	// triggers (synthetic injections, userMessage-only turns) stay on
	// the global session rather than minting a useless one-off.
	session := f.resolver.Resolve(triggerThreadRoot)
	// Keep the legacy field in sync for SessionID()/observability
	// callers that haven't migrated. In thread-isolated mode this
	// reflects "the last session we deliberated against," which is
	// the most useful observability answer for that mode.
	f.mu.Lock()
	f.sessionHandle = session
	f.mu.Unlock()

	if shouldCompact {
		if err := f.compact(ctx, tail); err != nil {
			f.log.Warn("funnel: compaction failed; continuing without it", "err", err)
			// Don't fail the deliberation — proceed with the existing
			// tail and let the provider's auto-compact handle it if
			// the threshold is also crossed there.
		} else {
			// Refresh local view post-compaction. compact() rotates the
			// resolver's global handle, so re-resolve to pick up the
			// fresh id for this turn.
			f.mu.Lock()
			tail = append([]bridle.SessionEvent(nil), f.sessionTail...)
			f.mu.Unlock()
			session = f.resolver.Resolve(triggerThreadRoot)
			f.mu.Lock()
			f.sessionHandle = session
			f.mu.Unlock()
		}
	}

	systemPrompt := f.cfg.SystemPrompt
	if f.cfg.SystemPromptFn != nil {
		systemPrompt = f.cfg.SystemPromptFn()
	}
	// Toolkit-awareness blurb for the claude-code provider only. The
	// provider passes our prompt as --append-system-prompt, layered on
	// top of Anthropic's default. The default frames the assistant and
	// describes its toolkit; the personality bundle frames identity and
	// role. Neither tells claude-code which network it's embedded in
	// or that the Skill ecosystem is available — without this nudge,
	// aspects answer "what tools do you have?" with silence even though
	// the tools are right there. Other providers (claude-api, openai,
	// ollama) have no Anthropic default to layer onto; the blurb would
	// be load-bearing-as-instruction not as-augmentation, so skip it.
	if f.cfg.Provider == "claudecode" {
		systemPrompt = appendToolkitBlurb(systemPrompt)
	}
	providerEnv, err := f.resolveProviderEnv(ctx, "main")
	if err != nil {
		f.log.Warn("funnel: provider env resolution failed; falling through to provider defaults", "err", err)
	}
	req := bridle.TurnRequest{
		AspectID:           f.cfg.AspectID,
		AppendSystemPrompt: systemPrompt,
		Session:            session,
		SessionTail:        tail,
		UserMessage:        userMessage,
		Inbox:              pending,
		Tools:              f.cfg.Tools,
		MCP:                f.cfg.MCP,
		Provider:           f.cfg.Provider,
		Model:              f.cfg.Model,
		MaxSteps:           f.cfg.MaxStepsPerTurn,
		Cwd:                f.cfg.AspectHome,
		ProviderEnv:        providerEnv,
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

	// Phase E: bracket the turn with ObservabilityHook so Grouper sees
	// BeginTurn/events/EndTurn. turnSink falls back to collectSink when
	// the hook is nil, preserving the pre-Phase-E no-op path.
	//
	// NOT deferred: the post-hoc filter judge (below) calls BeginTurn
	// on the same Grouper. A pending defer here would land the filter
	// inside this turn's lifetime and trigger the Grouper's protocol-
	// violation recovery (force-close main as errored). Close explicitly
	// immediately after RunTurn so the main TurnFrame settles cleanly.
	if f.cfg.ObservabilityHook != nil {
		f.cfg.ObservabilityHook.BeginTurn(turnID, "main", f.cfg.Model, string(f.cfg.Provider), triggerMsgID)
	}
	sink := turnSink(f.cfg.ObservabilityHook)
	// When streaming text to chat, prepend a streamingChatSink so each
	// ModelChunk posts to chat before the observability hook sees it.
	if f.cfg.StreamTextToChat && f.cfg.ChatGateway != nil {
		streamSink := &streamingChatSink{
			gateway:  f.cfg.ChatGateway,
			replyTo:  trigger.MsgID,
			aspectID: f.cfg.AspectID,
		}
		sink = multiSink{streamSink, sink}
	}
	// Tag the context with the turn_id so the triage tool runner
	// persists rows under the right turn. Required when Triage is
	// wired; harmless otherwise.
	turnCtx := WithTurnID(ctx, turnID)
	result, err := f.cfg.Harness.RunTurn(turnCtx, req, f.cfg.Runner, sink)
	if f.cfg.ObservabilityHook != nil {
		f.cfg.ObservabilityHook.EndTurn()
	}
	// turn.end must fire whether the turn succeeded or errored — the
	// Lock 5 spec promises every turn.start has a paired turn.end.
	// Without this, dashboards listening for paired events would
	// register every provider error as a stuck turn.
	// Map result.StopReason to an ErrorClass label. Today the only
	// non-clean StopReason that produces a recoverable partial turn is
	// process_exit (bridle #219). Add to this mapping when new
	// recoverable error classes appear.
	var errorClass string
	if result.StopReason == bridle.StopReasonProcessExit {
		errorClass = "subprocess_exit_partial"
	}
	f.emit(ctx, Event{
		Type: EventTurnEnd,
		Payload: TurnEndPayload{
			TurnID:     turnID,
			Usage:      result.Usage,
			StopReason: result.StopReason,
			StepCount:  result.StepCount,
			Duration:   time.Since(turnStart),
			ErrorClass: errorClass,
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

	// Triage enforcement (inbox-triage contract). For every inbox
	// msg_id we sent into this turn, check whether the model called
	// triage(); if not, emit a synthetic skip row with reason
	// "no_triage_emitted" so the operator's 1:1 view shows a complete
	// audit trail. Pre-fix bug: model produced one reply that acked
	// the latest probe and silently dropped the earlier inbox items
	// — uninhabitable as a substrate.
	//
	// Runs on both success and error paths: the model may have
	// triaged some items before erroring; we still want those rows,
	// and the untriaged ones still need synthetic skips so the
	// reconciliation invariant holds regardless of provider outcome.
	if f.cfg.Triage != nil {
		f.reconcileTriage(ctx, turnID, pending)
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
		// Surface partial assistant text before cleanup (NEX-239).
		// When the provider exits non-zero after producing text blocks
		// (claude-code exit-1, stream timeout), the partial result is
		// recoverable and should reach chat. Skip the filter judge —
		// partial results default to ShouldPost so the text surfaces
		// rather than being silently dropped.
		if result.FinalText != "" {
			partial := DeliberateResult{
				TurnResult: result,
				Filter:     FilterDecision{ShouldPost: true},
			}
			if hErr := f.cfg.Return.Handle(ctx, partial, trigger); hErr != nil {
				f.log.Debug("funnel: partial-result Handle failed",
					"aspect", f.cfg.AspectID,
					"trigger_msg_id", trigger.MsgID,
					"err", hErr)
			}
		}

		// Flip the resolver's "known" set for this session id even on
		// error. The provider may have written the underlying session
		// jsonl (claudecode does this once `--session-id` is accepted,
		// even if a later step fails), so the next turn MUST resume
		// rather than try to create the same id again. Without this
		// flip, every error pins the session in the "new" state and
		// subsequent turns fail with "Session ID already in use".
		f.resolver.MarkResumed(session.ID)
		f.mu.Lock()
		f.sessionHandle.New = false
		f.mu.Unlock()
		return DeliberateResult{TurnResult: result}, err
	}

	// Append the turn's session delta + update cumulative tokens. If
	// the v2 log-decision turn lands, this is where it'd gate the append.
	// Also mark the session as resumed in the resolver: the provider has
	// created the underlying session (e.g. claudecode wrote the jsonl),
	// so future turns against the same id resume rather than re-create.
	f.resolver.MarkResumed(session.ID)
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
		fresh := f.resolver.RotateGlobal()
		f.mu.Lock()
		oldID := f.sessionHandle.ID
		oldTail := len(f.sessionTail)
		oldTokens := f.cumulativeTokens
		f.sessionHandle = fresh
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

	// Read and clear the goal DoD for this turn (NEX-210).
	dod := f.takeDoD()

	decision := f.runFilter(ctx, FilterInput{
		FinalText:    result.FinalText,
		AspectID:     f.cfg.AspectID,
		TurnID:       turnID,
		TriggerFrom:  triggerFrom,
		TriggerText:  triggerContent,
		TriggerMsgID: triggerMsgID,
		DoD:          dod,
	})

	// Surface the verdict as a structured Event so non-obs-hook sinks
	// (WS frame relay, future remote dashboards) see it. The local
	// observability hub already renders the judge tile from bridle's
	// BeginTurn/EndTurn pair; this event is for sinks that don't
	// subscribe to that pipeline.
	f.emit(ctx, Event{
		Type: EventFilterJudged,
		Payload: FilterJudgedPayload{
			TurnID:       turnID,
			ShouldPost:   decision.ShouldPost,
			Reason:       decision.Reason,
			FinalTextLen: len(result.FinalText),
		},
	})

	// Synthetic filter-decision turn for the observability stream so
	// EVERY filter outcome surfaces as a frame — including HardRules
	// short-circuits that never invoke the cheap judge. Without this,
	// suppressions from substring/prefix self-suppress and empty-output
	// rejections are invisible to operators: they see EventFilterJudging
	// (filter is running) but no decision, no judge turn, and no auto-
	// post. The synthetic turn carries the verdict, the layer that
	// produced it, and the reason text so the operator can audit
	// suppressions without grepping host logs.
	if f.cfg.ObservabilityHook != nil {
		f.cfg.ObservabilityHook.BeginTurn(turnID+"-decision", "filter-decision", f.cfg.Model, string(f.cfg.Provider), 0)
		f.cfg.ObservabilityHook.OnBridleEvent(bridle.ModelChunk{Text: renderFilterVerdict(decision)})
		f.cfg.ObservabilityHook.OnBridleEvent(bridle.TurnDone{})
		f.cfg.ObservabilityHook.EndTurn()
	}

	f.log.Info("funnel: turn complete",
		"aspect", f.cfg.AspectID,
		"steps", result.StepCount,
		"tool_calls", len(result.ToolCalls),
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cache_read", result.Usage.CacheReadInputTokens,
		"cache_create", result.Usage.CacheCreationInputTokens,
		"cumulative", f.cumulativeTokens,
		"stop_reason", result.StopReason,
		"filter_post", decision.ShouldPost,
		"filter_reason", decision.Reason)

	// NEX-82: hand the result off to Return.Handle. The default
	// NexusChatReturnHandler does what the inline pre-split code did:
	// resolve-emoji (👀 toggle-off / 🙊 / 👍 based on filter+text) and
	// auto-post FinalText when filter ShouldPost. agora-side handlers
	// route Source-tagged. Errors are logged but don't fail the turn —
	// Deliberate's caller already has the result; return-side failures
	// are observability concerns.
	deliberate := DeliberateResult{TurnResult: result, Filter: decision}
	if err := f.cfg.Return.Handle(ctx, deliberate, trigger); err != nil {
		f.log.Debug("funnel: return handler Handle failed",
			"aspect", f.cfg.AspectID,
			"trigger_msg_id", trigger.MsgID,
			"err", err)
	}

	return deliberate, nil
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
	compactEnv, err := f.resolveProviderEnv(ctx, "compact")
	if err != nil {
		f.log.Warn("funnel: provider env resolution failed for compact; falling through to provider defaults", "err", err)
	}
	req := bridle.TurnRequest{
		AspectID:           f.cfg.AspectID,
		AppendSystemPrompt: summarizePrompt,
		// Fresh session for the summarize turn so it doesn't pollute
		// the main session JSONL.
		Session:     bridle.SessionHandle{ID: newSessionID(), New: true},
		SessionTail: tail,
		UserMessage: "Summarize this session into a compact briefing the model can use to continue.",
		Provider:    f.cfg.Provider,
		Model:       model,
		MaxSteps:    1, // pure text; one round is enough
		Cwd:         f.cfg.AspectHome,
		ProviderEnv: compactEnv,
	}

	// Phase E: surface the compact turn under its own label. Not
	// deferred — close immediately after RunTurn so the Grouper's
	// terminal TurnFrame settles before the downstream EventCompactEnd
	// emission and session-tail mutation (mirrors the Deliberate
	// site's reasoning).
	compactTurnID := newTurnID()
	if f.cfg.ObservabilityHook != nil {
		f.cfg.ObservabilityHook.BeginTurn(compactTurnID, "compact", model, string(f.cfg.Provider), 0)
	}
	sink := turnSink(f.cfg.ObservabilityHook)
	result, err := f.cfg.Harness.RunTurn(ctx, req, f.cfg.Runner, sink)
	if f.cfg.ObservabilityHook != nil {
		f.cfg.ObservabilityHook.EndTurn()
	}
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

	fresh := f.resolver.RotateGlobal()
	f.mu.Lock()
	f.sessionTail = []bridle.SessionEvent{summary}
	f.cumulativeTokens = result.Usage.OutputTokens // the summary itself counts toward the next budget
	// New session minted by compaction — flag as fresh so the provider
	// creates the underlying session rather than trying to resume an id
	// it has never seen.
	f.sessionHandle = fresh
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

// resolveProviderEnv consults the configured ProviderEnvResolver and
// returns the env overlay for an upcoming turn of the given kind
// ("main" | "compact" | "filter"). Nil resolver, nil env, or a "no
// default configured" result all flow back as (nil, nil) so the
// caller's TurnRequest leaves ProviderEnv unset and the provider runs
// against whatever auth it would normally use (subscription claude-
// code, process-env API keys, --bare flags). Genuine resolver errors
// (DB failures, decryption failures) propagate so the funnel can log
// them — turns still fire with no env overlay, fail-open rather than
// fail-closed.
func (f *Funnel) resolveProviderEnv(ctx context.Context, kind string) (map[string]string, error) {
	if f.cfg.ProviderEnvResolver == nil {
		return nil, nil
	}
	env, ok, err := f.cfg.ProviderEnvResolver.Resolve(ctx, f.cfg.AspectID, kind)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return env, nil
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

// toolkitBlurb is appended to the system prompt for claude-code-provider
// aspects so they reach for their tools when asked. Without this, the
// personality bundle says "you are X, this is your role" and the
// Anthropic default says "you are an assistant with these tools" — but
// neither one prompts the model to enumerate or invoke them when the
// operator asks "what can you do?" Empty answers are the symptom.
//
// Style: short, present-tense, mention the categories (native tools +
// Skill ecosystem) rather than enumerating every individual tool —
// the toolkit changes; this blurb shouldn't have to.
const toolkitBlurb = `

You operate inside the Nexus network and have full access to your underlying claude-code toolkit: native tools (Bash, Read, Write, Edit, Glob, Grep, Task, WebFetch, WebSearch) plus the Skill ecosystem (invoke via the Skill tool — common skills include brainstorming, executing-plans, systematic-debugging, writing-plans). When the operator asks about your capabilities, enumerate them concretely from your toolkit rather than answering abstractly. When a task suits a skill, use it.`

// appendToolkitBlurb concatenates the toolkit blurb onto a personality
// bundle. Safe to call with empty input.
func appendToolkitBlurb(personality string) string {
	return personality + toolkitBlurb
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

// runFilter calls OutputFilter.Judge and waits for its answer. There
// is no outer timeout — the judge MUST be the authority on whether
// to post.
//
// Previous design raced an outer 2s timeout against the inner judge
// call; in practice the timer almost always won, the funnel "failed
// open" with ShouldPost=true, and the actual judge answer arrived
// hundreds of ms later — too late, post already out. The filter was
// effectively a noop. (Operator 2026-05-12: "i lean to remove the
// timeout as well — waiting an extra 1-2 sec over the noise when you
// have 4-5 agents in the room".)
//
// Per-implementation timeouts (e.g. CheapModelFilter's filterJudgeTimeout)
// still bound how long a single call can hang; if the judge harness
// itself wedges that's a bridle/provider bug to surface, not paper
// over with a fail-open race. Panic protection stays — a panicking
// filter still fails open as the least-bad recovery.
//
// Context cancellation also fails open: ctx-cancelled at this point
// means the turn is tearing down, so suppressing wouldn't matter
// anyway (the deliberation goroutine is already on its way out).
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
// filterDecisionLayer reports which filter layer produced a decision
// based on the Reason. HardRulesFilter sets the canonical
// FilterReason* constants; CheapModelFilter sets either the canonical
// scratch/ramble or a free-form 12-word reason from the judge model.
// Anything not in the canonical hard-rules set is treated as
// cheap_judge so freeform reasons route correctly.
func filterDecisionLayer(reason string, class string) string {
	// NEX-210: classification-driven layers take priority.
	switch class {
	case FilterClassGoalNotMet:
		return "cheap_judge"
	case FilterClassBlocked:
		return "cheap_judge"
	}
	switch reason {
	case FilterReasonEmpty, FilterReasonSelfSuppress:
		return "hard_rules"
	case "":
		return "always_post"
	default:
		return "cheap_judge"
	}
}

// renderFilterVerdict formats the FilterDecision as a single text
// line for the synthetic filter-decision turn frame. Format chosen
// so the dashboard's existing turn-text renderer surfaces it inline
// without needing a new TurnEvent kind.
func renderFilterVerdict(d FilterDecision) string {
	verdict := "suppress"
	if d.ShouldPost {
		verdict = "post"
	}
	reason := d.Reason
	if reason == "" {
		reason = "(none)"
	}
	class := d.Class
	if class == "" {
		if d.ShouldPost {
			class = FilterClassComplete
		} else {
			class = FilterClassScratch
		}
	}
	return "verdict=" + verdict + " class=" + class + " layer=" + filterDecisionLayer(d.Reason, d.Class) + " reason=" + reason
}

// reconcileTriage walks the inbox items the turn ingested and inserts
// synthetic skip rows for any whose msg_id wasn't already triaged by
// the model. Without this the inbox-triage contract collapses to "the
// model triages when it remembers to," which is exactly the bug we're
// closing. Items with MsgID==0 are synthetic/internal (no real chat
// row to triage against) and are skipped here.
//
// Errors from the store are logged, never returned: triage enforcement
// is observability, not correctness — a failed audit row mustn't fail
// a turn that already produced a model reply.
func (f *Funnel) reconcileTriage(ctx context.Context, turnID string, inbox []bridle.InboxItem) {
	// Build the set of msg_ids the funnel sent into this turn.
	want := make(map[int64]struct{}, len(inbox))
	for _, item := range inbox {
		if item.MsgID > 0 {
			want[item.MsgID] = struct{}{}
		}
	}
	if len(want) == 0 {
		return
	}

	// Read what the model actually triaged.
	have, err := f.cfg.Triage.ListByTurn(ctx, turnID)
	if err != nil {
		f.log.Warn("funnel: triage reconcile read failed",
			"err", err, "turn_id", turnID, "aspect", f.cfg.AspectID)
		return
	}
	seen := make(map[int64]struct{}, len(have))
	for _, dec := range have {
		seen[dec.MsgID] = struct{}{}
	}

	// Emit synthetic skips for the difference.
	for msgID := range want {
		if _, ok := seen[msgID]; ok {
			continue
		}
		if _, err := f.cfg.Triage.Record(ctx, chat.TriageDecision{
			AspectName: f.cfg.AspectID,
			MsgID:      msgID,
			TurnID:     turnID,
			Decision:   "skip",
			Reason:     "no_triage_emitted",
		}); err != nil {
			f.log.Warn("funnel: synthetic triage write failed",
				"err", err, "turn_id", turnID, "msg_id", msgID, "aspect", f.cfg.AspectID)
			continue
		}
		f.log.Info("funnel: synthetic triage emitted (model did not call triage())",
			"turn_id", turnID, "msg_id", msgID, "aspect", f.cfg.AspectID)
	}
}

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
