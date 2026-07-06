// M1 Unit 5 — the worker-status heartbeat (PHASE2-DESIGN §5): every
// dispatched worker emits one machine-readable status shape at boot,
// each main-turn boundary, and on a ~60s wall-clock ticker. Bridle has
// no mid-turn heartbeat hook (confirmed by the wave/audit that scoped
// this unit), so the ticker below is funnel-owned — agentfunnel starts
// it itself rather than threading a callback through bridle/funnel.
//
// Emission is always best-effort: a failed send is logged and dropped,
// never surfaced as an error the caller has to handle, retry, or
// escalate. The worker's real job (the deliberation loop) must never
// stall or crash because a heartbeat frame didn't make it to the
// broker — the next tick or turn boundary supersedes a missed one.
package main

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// heartbeatInterval is the ~60s wall-clock cadence from PHASE2-DESIGN §5.
const heartbeatInterval = 60 * time.Second

// workerStatusSender is the minimal surface workerStatusEmitter needs.
// Implemented by *wsasp.Client.SendWorkerStatus; declared as an
// interface so tests can inject a fake sender without a live WS
// connection (mirrors obsforward.Sender's shape).
type workerStatusSender interface {
	SendWorkerStatus(ctx context.Context, p frames.WorkerStatusPayload) error
}

// turnMetricsTracker wraps a funnel.ObservabilityHook, folding a running
// count of completed main-deliberation turns and cumulative token usage
// into the pass-through — the two heartbeat fields (`turns`,
// `tokens_used`) that funnel.ObservabilityHook's own interface has no
// query surface for. Every call is forwarded unchanged to the wrapped
// hook, so inserting this tracker changes nothing about existing
// observability forwarding (obsforward.WSForwarder, the builder-progress
// wrapper, etc) — it's a transparent decorator.
//
// Only "main" label turns (or an empty label, which today's callers
// never actually send but is treated the same as main for safety)
// increment the turn counter — filter-judge and compact sub-turns are
// bookkeeping noise the operator dashboard shouldn't count as
// deliberation progress. Token usage accumulates across ALL turns
// (main + judge + compact) since that's genuinely-spent cost.
type turnMetricsTracker struct {
	next funnel.ObservabilityHook

	mu         sync.Mutex
	turns      int
	tokensUsed int
	curLabel   string

	// onMainTurnEnd fires after a "main" turn's EndTurn, letting the
	// caller emit a turn-boundary heartbeat (PHASE2-DESIGN §5: "each
	// turn boundary"). Optional — nil is a no-op.
	onMainTurnEnd func()
}

func newTurnMetricsTracker(next funnel.ObservabilityHook, onMainTurnEnd func()) *turnMetricsTracker {
	return &turnMetricsTracker{next: next, onMainTurnEnd: onMainTurnEnd}
}

func (t *turnMetricsTracker) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	t.mu.Lock()
	t.curLabel = label
	t.mu.Unlock()
	if t.next != nil {
		t.next.BeginTurn(turnID, label, model, provider, triggerMsg)
	}
}

func (t *turnMetricsTracker) OnBridleEvent(ev bridle.Event) {
	if done, ok := ev.(bridle.TurnDone); ok {
		t.mu.Lock()
		if t.curLabel == "" || t.curLabel == "main" {
			t.turns++
		}
		t.tokensUsed += done.Result.Usage.InputTokens + done.Result.Usage.OutputTokens
		t.mu.Unlock()
	}
	if t.next != nil {
		t.next.OnBridleEvent(ev)
	}
}

func (t *turnMetricsTracker) EndTurn() {
	t.mu.Lock()
	label := t.curLabel
	t.mu.Unlock()
	if t.next != nil {
		t.next.EndTurn()
	}
	if (label == "" || label == "main") && t.onMainTurnEnd != nil {
		t.onMainTurnEnd()
	}
}

// Snapshot returns the running totals so far.
func (t *turnMetricsTracker) Snapshot() (turns, tokensUsed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turns, t.tokensUsed
}

// workerStatusEmitter assembles and best-effort-sends the worker.status
// heartbeat. Constructed once at boot with the fields that never change
// for this process's lifetime (agent identity, role/personality/
// work-item, cli/image version); the fields that DO change per-emission
// (binding, auth health, turn/token counters) are read fresh from the
// injected closures on every Emit call.
type workerStatusEmitter struct {
	sender      workerStatusSender
	agent       string
	role        string
	personality string
	workItemID  string
	cliVersion  string
	imageTag    string
	startedAt   time.Time

	// binding returns the worker's currently-resolved provider/model
	// (funnel.Binding) — read from the same atomic binding cache the
	// funnel's BindingFn uses, so a mid-run rebind (NEX-335 admin
	// provider-binding edit) is reflected on the next heartbeat.
	binding func() funnel.Binding

	// authState reports whether the current frontier credential is
	// healthy and when it expires. Wired to the aspect's own session
	// JWT state today (sessionState.Snapshot) — the CLAUDE_CODE_OAUTH_TOKEN
	// almanac-sourced secret from PHASE2-DESIGN §6/§7 is a later build
	// unit; this seam reports the equivalent "is my auth about to die"
	// signal with what agentfunnel already tracks.
	authState func() (ok bool, expiresAt time.Time)

	metrics *turnMetricsTracker
	log     *slog.Logger

	state atomic.Value // string: spawning|running|blocked|awaiting_gate|done|failed
}

func newWorkerStatusEmitter(
	sender workerStatusSender,
	agent, role, personality, workItemID, cliVersion, imageTag string,
	startedAt time.Time,
	binding func() funnel.Binding,
	authState func() (bool, time.Time),
	metrics *turnMetricsTracker,
	log *slog.Logger,
) *workerStatusEmitter {
	if log == nil {
		log = slog.Default()
	}
	e := &workerStatusEmitter{
		sender: sender, agent: agent, role: role, personality: personality,
		workItemID: workItemID, cliVersion: cliVersion, imageTag: imageTag,
		startedAt: startedAt, binding: binding, authState: authState,
		metrics: metrics, log: log,
	}
	e.state.Store("spawning")
	return e
}

// Emit sends one heartbeat carrying the given lifecycle state.
// Best-effort: a send failure is logged at debug (heartbeats are
// routine; a miss during a reconnect is expected, not alarming) and
// otherwise ignored — Emit never returns an error.
func (e *workerStatusEmitter) Emit(ctx context.Context, state string) {
	e.state.Store(state)
	p := frames.WorkerStatusPayload{
		Agent:         e.agent,
		Role:          e.role,
		Personality:   e.personality,
		WorkItemID:    e.workItemID,
		State:         state,
		CLIVersion:    e.cliVersion,
		ImageTag:      e.imageTag,
		LastHeartbeat: time.Now().UTC(),
		StartedAt:     e.startedAt,
	}
	if e.binding != nil {
		b := e.binding()
		p.Provider = string(b.Provider)
		p.Model = b.Model
	}
	if e.authState != nil {
		p.AuthOk, p.TokenExpiresAt = e.authState()
	}
	if e.metrics != nil {
		p.Turns, p.TokensUsed = e.metrics.Snapshot()
	}
	if e.sender == nil {
		return
	}
	if err := e.sender.SendWorkerStatus(ctx, p); err != nil {
		e.log.Debug("agentfunnel: worker.status heartbeat send failed (best-effort)",
			"err", err, "agent", e.agent, "state", state)
	}
}

// detectCLIVersion best-effort shells out to `<path or claude> --version`
// to populate the heartbeat's `cli_version` field (PHASE2-DESIGN §7 —
// "which pod is on an old CLI" as a queryable field, not log
// archaeology). A missing/erroring binary (native-API providers that
// never invoke claude-code at all, e.g. a bare openai/ollama worker)
// is normal, not a failure — logged at debug and reported as "".
func detectCLIVersion(claudePath string, log *slog.Logger) string {
	bin := claudePath
	if bin == "" {
		bin = "claude"
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		if log != nil {
			log.Debug("agentfunnel: cli_version detection skipped", "bin", bin, "err", err)
		}
		return ""
	}
	return strings.TrimSpace(string(out))
}

// StartHeartbeat runs a funnel-owned ~interval ticker that re-emits the
// current lifecycle state until ctx is cancelled, returning immediately
// (the ticker runs in its own goroutine). Callers should Emit the boot
// state ("running", or "spawning" if still initialising) before calling
// this — the first tick fires after one full interval, not immediately.
func (e *workerStatusEmitter) StartHeartbeat(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state, _ := e.state.Load().(string)
				if state == "" {
					state = "running"
				}
				e.Emit(ctx, state)
			}
		}
	}()
}
