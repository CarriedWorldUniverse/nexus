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

func startBuilderIdleMonitor(ctx context.Context, timeout time.Duration, progress <-chan string, onStall func(), log *slog.Logger) {
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
}

func (h progressObservabilityHook) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	if h.next != nil {
		h.next.BeginTurn(turnID, label, model, provider, triggerMsg)
	}
}

func (h progressObservabilityHook) OnBridleEvent(ev bridle.Event) {
	switch e := ev.(type) {
	case bridle.ToolCallResult:
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

// builderRealOutputTracker accumulates the CURRENT turn's streamed model
// text (bridle.ModelChunk events) so the verified-completion gate
// (builderAcceptanceGate, main.go) can judge what the model actually
// produced this turn rather than trusting a task_done self-report alone —
// review finding on Unit B pass 1 (NET-24: keel-builder's summary claimed
// success while the model produced nothing matching the required token; a
// model authoring "posted CONVERGED-BETA-OK" as its OWN summary text
// without ever having produced it would pass a summary-only check).
//
// Reset at the top of each turn (BeginTurn) so a stale prior turn's text
// never leaks into the NEXT turn's verification. Mutex-guarded because
// task_done can fire (on the same goroutine, synchronously, from inside
// bridle's tool-call dispatch) while OnBridleEvent is still being called
// for that same turn's remaining tool-call rounds; snapshot() may be read
// concurrently with a WriteString from the tail of the same turn.
type builderRealOutputTracker struct {
	next funnel.ObservabilityHook
	mu   sync.Mutex
	text strings.Builder
}

func (h *builderRealOutputTracker) BeginTurn(turnID, label, model, provider string, triggerMsg int64) {
	h.mu.Lock()
	h.text.Reset()
	h.mu.Unlock()
	if h.next != nil {
		h.next.BeginTurn(turnID, label, model, provider, triggerMsg)
	}
}

func (h *builderRealOutputTracker) OnBridleEvent(ev bridle.Event) {
	if chunk, ok := ev.(bridle.ModelChunk); ok {
		h.mu.Lock()
		h.text.WriteString(chunk.Text)
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

// snapshot returns everything streamed so far in the current turn. Safe to
// call mid-turn (task_done fires before the turn's TurnDone event) — it
// just returns whatever has streamed up to that instant, which is exactly
// the "real output produced so far" signal the acceptance gate needs.
func (h *builderRealOutputTracker) snapshot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.text.String()
}

func gitHead(ctx context.Context, worktree string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		return "", errors.New(strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
