package main

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

const (
	defaultBuilderIdleTimeout = 2 * time.Minute
	builderStalledReason      = "stalled"
	gitProgressPollInterval   = 10 * time.Second
)

type builderProgressFunc func(string)

// builderInFlightTracker counts tool calls currently executing (a
// ToolCallStart seen, its matching ToolCallResult not yet seen). The idle
// monitor suspends its stall timer while the count is > 0: a tool call
// that is still running (e.g. `go build ./...` taking several minutes) is
// itself progress, not idleness. Live evidence: a builder run was killed
// "ESCALATION builder stalled — idle timeout expired, last_progress=
// tool_call" while a long-running tool call was STILL EXECUTING — the old
// monitor only reset on discrete progress *signals* (a completed tool
// call, a chat send, …) and had no notion of "currently busy."
//
// Synchronization: inc/dec are called from the observability event path —
// bridle dispatches ToolCallStart/ToolCallResult synchronously as part of
// the model's tool-call round-trip, on whatever goroutine is driving that
// turn. dec/inFlight are read from the separate idle-monitor goroutine on
// every timer tick. A single mutex guards the counter AND the "did this
// decrement cross from >0 to 0" check together (rather than a bare atomic
// counter) so that a decrement's zero-crossing determination can never be
// computed from a stale read — with a plain atomic, two concurrent
// decrements could each independently observe "count is now 0" after a
// racy load, double-reporting the zero-crossing; here the check and the
// mutation happen under the same lock. The monitor's own read
// (builderInFlightTracker.inFlight) also always takes the lock — the
// select loop re-reads it fresh on every tick rather than caching a
// snapshot from a previous tick, since the whole point is to notice a
// tool completing between ticks.
type builderInFlightTracker struct {
	mu    sync.Mutex
	count int
}

// inc records a ToolCallStart — one more tool call is now in flight.
func (t *builderInFlightTracker) inc() {
	t.mu.Lock()
	t.count++
	t.mu.Unlock()
}

// dec records a ToolCallResult (success or error — both end the tool
// call). Returns true iff this decrement brought the in-flight count from
// >0 down to exactly 0 — the instant the idle monitor's window should
// restart from, per the design (a tool finishing is progress). Never lets
// the count go negative: a ToolCallResult with no matching prior
// ToolCallStart is unexpected but must not panic or corrupt the counter,
// so it's logged as a warning and treated as a no-op.
func (t *builderInFlightTracker) dec(log *slog.Logger) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.count <= 0 {
		if log == nil {
			log = slog.Default()
		}
		log.Warn("agentfunnel: builder in-flight tool counter underflow — ToolCallResult with no matching ToolCallStart")
		t.count = 0
		return false
	}
	t.count--
	return t.count == 0
}

// inFlight reports whether at least one tool call is currently executing.
func (t *builderInFlightTracker) inFlight() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count > 0
}

// startBuilderIdleMonitor watches for builder inactivity. inFlight may be
// nil (treated as "nothing ever in flight," matching the pre-existing
// behavior) for callers that don't track tool concurrency.
func startBuilderIdleMonitor(ctx context.Context, timeout time.Duration, progress <-chan string, inFlight *builderInFlightTracker, onStall func(), log *slog.Logger) {
	if timeout <= 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	lastReason := "startup"
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(timeout)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case reason := <-progress:
			if reason != "" {
				lastReason = reason
			}
			resetTimer()
		case <-timer.C:
			// Re-check suspension on every tick — never cache the
			// in-flight state from a previous iteration. A tool call
			// still running when the window would otherwise expire
			// means this isn't a stall: rearm and keep waiting.
			if inFlight != nil && inFlight.inFlight() {
				log.Debug("agentfunnel: builder idle timer suspended — tool call in flight",
					"idle_timeout", timeout)
				resetTimer()
				continue
			}
			log.Error("agentfunnel: ESCALATION builder stalled — idle timeout expired",
				"reason", builderStalledReason,
				"idle_timeout", timeout,
				"last_progress", lastReason)
			onStall()
			return
		}
	}
}

func newBuilderProgressReporter(ch chan<- string) builderProgressFunc {
	return func(reason string) {
		select {
		case ch <- reason:
		default:
		}
	}
}

type progressObservabilityHook struct {
	next     funnel.ObservabilityHook
	progress builderProgressFunc
	// inFlight, when non-nil, is fed ToolCallStart/ToolCallResult so the
	// idle monitor can suspend its timer for the duration of a tool call
	// rather than only reacting to discrete progress signals. Optional —
	// nil preserves the pre-existing behavior for callers that don't wire
	// it up.
	inFlight *builderInFlightTracker
}

func (h progressObservabilityHook) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	if h.next != nil {
		h.next.BeginTurn(turnID, label, model, provider, triggerMsg)
	}
}

func (h progressObservabilityHook) OnBridleEvent(ev bridle.Event) {
	switch e := ev.(type) {
	case bridle.ToolCallStart:
		if h.inFlight != nil {
			h.inFlight.inc()
		}
	case bridle.ToolCallResult:
		zeroCrossed := false
		if h.inFlight != nil {
			zeroCrossed = h.inFlight.dec(nil)
		}
		if e.Err == "" {
			h.progress("tool_call")
		} else if zeroCrossed {
			// Error results don't otherwise emit a progress signal
			// (TestBuilderIdleMonitorDoesNotResetOnErrorBursts), but a
			// tool finishing — even with an error — that brings the
			// in-flight count back to zero IS the moment the idle
			// window should restart from: the tool is no longer
			// running, so this is genuinely fresh idle time from here.
			h.progress("tool_call_end")
		}
	case bridle.TurnDone:
		h.progress("turn_done")
	}
	if h.next != nil {
		h.next.OnBridleEvent(ev)
	}
}

func (h progressObservabilityHook) EndTurn() {
	if h.next != nil {
		h.next.EndTurn()
	}
}

type progressChatGateway struct {
	funnel.ChatGateway
	progress builderProgressFunc
}

func (g progressChatGateway) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
	msgID, err := g.ChatGateway.SendChat(ctx, content, replyTo, topic)
	if err == nil {
		g.progress("chat_send")
	}
	return msgID, err
}

func (g progressChatGateway) AnnounceFile(ctx context.Context, path, description string) (int64, error) {
	msgID, err := g.ChatGateway.AnnounceFile(ctx, path, description)
	if err == nil {
		g.progress("chat_activity")
	}
	return msgID, err
}

func watchBuilderGitProgress(ctx context.Context, worktree string, interval time.Duration, progress builderProgressFunc, log *slog.Logger) {
	if worktree == "" || interval <= 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	last, err := gitHead(ctx, worktree)
	if err != nil {
		log.Debug("agentfunnel: builder git progress watcher disabled; initial HEAD unavailable", "worktree", worktree, "err", err)
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			head, err := gitHead(ctx, worktree)
			if err != nil {
				log.Debug("agentfunnel: builder git progress HEAD check failed", "worktree", worktree, "err", err)
				continue
			}
			if head != "" && head != last {
				last = head
				progress("git_commit")
			}
		}
	}
}

// builderToolResultCap bounds how much tool-result text
// builderRealOutputTracker accumulates per turn — a single file-read tool
// result can be arbitrarily large and would otherwise blow the acceptance
// judge's context. Tail-kept (strings.TrimLeft-style truncation from the
// front) because recent evidence matters most: the LAST thing the model's
// tools produced this turn is the most relevant signal for "did it actually
// do the work."
const builderToolResultCap = 50_000

// builderRealOutputTracker accumulates the CURRENT turn's streamed model
// text (bridle.ModelChunk events) AND tool-call results (bridle.ToolCallResult
// events) so the verified-completion gate (builderAcceptanceGate, main.go)
// can judge what the model actually produced this turn rather than trusting
// a task_done self-report alone — review finding on Unit B pass 1 (NET-24:
// keel-builder's summary claimed success while the model produced nothing
// matching the required token; a model authoring "posted CONVERGED-BETA-OK"
// as its OWN summary text without ever having produced it would pass a
// summary-only check).
//
// Live evidence NET-46 (2026-07-06): a real PR existed (gh pr create's tool
// output carried the URL) but the judge only ever saw the model's streamed
// TEXT — the tool result itself never reached verification, so genuine
// evidence was invisible and the run was wrongly rejected. Tool results are
// captured here alongside model text so verificationInput can fold in real
// evidence, not just self-report + prose.
//
// Reset at the top of each turn (BeginTurn) so a stale prior turn's text
// never leaks into the NEXT turn's verification. Mutex-guarded because
// task_done can fire (on the same goroutine, synchronously, from inside
// bridle's tool-call dispatch) while OnBridleEvent is still being called
// for that same turn's remaining tool-call rounds; snapshot() may be read
// concurrently with a WriteString from the tail of the same turn.
type builderRealOutputTracker struct {
	next        funnel.ObservabilityHook
	mu          sync.Mutex
	text        strings.Builder
	toolNames   map[string]string // ToolCallStart.ID -> Name, for labeling results
	toolResults string            // capped, tail-kept accumulation of tool results this turn
}

func (h *builderRealOutputTracker) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	h.mu.Lock()
	h.text.Reset()
	h.toolNames = nil
	h.toolResults = ""
	h.mu.Unlock()
	if h.next != nil {
		h.next.BeginTurn(turnID, label, model, provider, triggerMsg)
	}
}

func (h *builderRealOutputTracker) OnBridleEvent(ev bridle.Event) {
	switch e := ev.(type) {
	case bridle.ModelChunk:
		h.mu.Lock()
		h.text.WriteString(e.Text)
		h.mu.Unlock()
	case bridle.ToolCallStart:
		h.mu.Lock()
		if h.toolNames == nil {
			h.toolNames = make(map[string]string)
		}
		h.toolNames[e.ID] = e.Name
		h.mu.Unlock()
	case bridle.ToolCallResult:
		h.mu.Lock()
		name := h.toolNames[e.ID]
		if name == "" {
			name = "tool"
		}
		var entry string
		if e.Err != "" {
			entry = "[TOOL " + name + " ERROR] " + e.Err + "\n"
		} else {
			entry = "[TOOL " + name + " RESULT] " + strings.TrimSpace(string(e.Result)) + "\n"
		}
		h.toolResults = appendCappedTail(h.toolResults, entry, builderToolResultCap)
		h.mu.Unlock()
	}
	if h.next != nil {
		h.next.OnBridleEvent(ev)
	}
}

func (h *builderRealOutputTracker) EndTurn() {
	if h.next != nil {
		h.next.EndTurn()
	}
}

// appendCappedTail appends add to existing and, if the result exceeds cap
// chars, keeps only the TAIL — the most recent evidence, per NET-46's fix
// spec, matters most for verification; an older large tool result is more
// likely superseded than a fresh one.
func appendCappedTail(existing, add string, maxLen int) string {
	combined := existing + add
	if len(combined) <= maxLen {
		return combined
	}
	return combined[len(combined)-maxLen:]
}

// textSnapshot returns everything the model streamed so far in the current
// turn. Safe to call mid-turn (task_done fires before the turn's TurnDone
// event) — it just returns whatever has streamed up to that instant, which
// is exactly the "real output produced so far" signal the acceptance gate
// needs.
func (h *builderRealOutputTracker) textSnapshot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.text.String()
}

// toolResultsSnapshot returns the capped, tail-kept tool-result text
// accumulated so far in the current turn.
func (h *builderRealOutputTracker) toolResultsSnapshot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toolResults
}

// snapshot returns the combined real-evidence signal for the current turn:
// streamed model text plus any tool-call results, labeled so the judge can
// tell them apart. Kept as the single entry point realOutputFor calls so
// verificationInput's contract (one authoritative "real output" string)
// doesn't need to change shape for callers that don't care about the
// text/tool-result split.
func (h *builderRealOutputTracker) snapshot() string {
	text := h.textSnapshot()
	tools := h.toolResultsSnapshot()
	if tools == "" {
		return text
	}
	if text == "" {
		return "TOOL RESULTS THIS TURN:\n" + tools
	}
	return text + "\n\nTOOL RESULTS THIS TURN:\n" + tools
}

func gitHead(ctx context.Context, worktree string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		return "", errors.New(strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
