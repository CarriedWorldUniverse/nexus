package main

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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

// bridleToolFlightCounter tracks the number of in-flight bridle tool calls
// during a builder turn. Incremented on ToolCallStart, decremented on
// ToolCallResult (success or error — both mark the end of in-flight). The
// idle monitor consults this counter before stalling: if tools are in-flight
// the model is still working, so the timer resets rather than firing.
//
// Uses atomic.Int32 (not int64) because the count per turn is bounded by the
// model's tool budget; int32 gives us the same race-free semantics on every
// platform with one fewer byte of contention. Zero value is valid — a
// builder that never calls tools keeps the counter at zero and the idle
// monitor behaves exactly as before this change.
type bridleToolFlightCounter struct {
	count atomic.Int32
}

// Increment records a tool call start. Safe to call from any goroutine.
func (c *bridleToolFlightCounter) Increment() {
	c.count.Add(1)
}

// Decrement records a tool call result. Safe to call from any goroutine.
// Mirrors Increment — paired per-tool, one Decrement per Increment.
func (c *bridleToolFlightCounter) Decrement() {
	c.count.Add(-1)
}

// Active reports whether any tools are currently in-flight. Returns false
// for a zero-value counter (never incremented), so callers can treat a nil
// or empty counter the same way without special-casing.
func (c *bridleToolFlightCounter) Active() bool {
	return c.count.Load() > 0
}

func startBuilderIdleMonitor(ctx context.Context, timeout time.Duration, progress <-chan string, flightCounter *bridleToolFlightCounter, onStall func(), log *slog.Logger) {
	if timeout <= 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	lastReason := "startup"
	for {
		select {
		case <-ctx.Done():
			return
		case reason := <-progress:
			if reason != "" {
				lastReason = reason
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			// Tool still in-flight: the model is working, just slowly.
			// Reset the timer rather than stalling — per NET-49 the idle
			// clock only counts time with zero in-flight tools AND no
			// other progress signal on the progress channel.
			if flightCounter != nil && flightCounter.Active() {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
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
	next          funnel.ObservabilityHook
	progress      builderProgressFunc
	flightCounter *bridleToolFlightCounter
}

func (h progressObservabilityHook) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	if h.next != nil {
		h.next.BeginTurn(turnID, label, model, provider, triggerMsg)
	}
}

func (h progressObservabilityHook) OnBridleEvent(ev bridle.Event) {
	switch e := ev.(type) {
	case bridle.ToolCallStart:
		if h.flightCounter != nil {
			h.flightCounter.Increment()
		}
	case bridle.ToolCallResult:
		if h.flightCounter != nil {
			h.flightCounter.Decrement()
		}
		if e.Err == "" {
			h.progress("tool_call")
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
