package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/context/tree"
	"github.com/nexus-cw/nexus/runtime/providers"
)

// handleFrame is the wsclient Handler for uncorrelated inbound frames.
// Correlated frames (register.ack, etc.) are consumed by Request calls
// and never reach here. This is where upstream-initiated frames land:
// turn, hand.invoke, shutdown, etc.
//
// Crucially, long-running handlers MUST NOT block this function — it
// runs inside the wsclient read goroutine, and blocking stalls all
// subsequent frame reads (ping/pong, shutdown, further turns) for
// the duration. Dispatch to a fresh goroutine instead.
func (a *Agent) handleFrame(env frames.Envelope) {
	switch env.Kind {
	case frames.KindTurn:
		// Provider.Invoke can run for minutes. Dispatching in a
		// goroutine keeps the read loop responsive. Side effect:
		// concurrent turns are now possible, which is fine — the
		// session tree's mutex serialises tree mutations.
		go a.handleTurnFrame(env)
	case frames.KindShutdown:
		a.log.Info("shutdown frame received", "in_reply_to", env.InReplyTo)
		// Shutdown behaviour (graceful wind-down, partial commits) is
		// part 7 scope. For now: log and let Run's ctx-cancel drive
		// exit.
	default:
		a.log.Info("frame kind not yet handled by agent", "kind", env.Kind)
	}
}

// handleTurnFrame processes a turn request from upstream. Dispatches
// through the provider, appends user + assistant entries to the local
// session tree, sends a turn.result frame correlating to the request.
func (a *Agent) handleTurnFrame(env frames.Envelope) {
	var req frames.TurnPayload
	if err := frames.PayloadAs(env, &req); err != nil {
		a.respondTurnError(env, fmt.Sprintf("turn payload invalid: %v", err))
		return
	}
	if req.Prompt == "" {
		a.respondTurnError(env, "empty prompt")
		return
	}

	// Bound the whole turn with a generous timeout — model calls can
	// run for a minute or two, but an unbounded tree-write + provider
	// invoke is a liability.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := a.dispatchTurn(ctx, req)
	if err != nil {
		a.respondTurnError(env, err.Error())
		return
	}

	resp, err := frames.NewResponse(frames.KindTurnResult, env.ID, result)
	if err != nil {
		a.log.Error("build turn.result frame failed", "err", err)
		return
	}
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	if err := a.ws.Send(sendCtx, resp); err != nil {
		a.log.Error("send turn.result failed", "err", err)
	}
}

// respondTurnError sends a turn.error-shaped response so the upstream
// can correlate and surface the failure.
func (a *Agent) respondTurnError(req frames.Envelope, msg string) {
	errEnv, err := frames.NewResponse(frames.Kind("turn.error"), req.ID, map[string]string{"error": msg})
	if err != nil {
		a.log.Error("build turn.error frame failed", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.ws.Send(ctx, errEnv); err != nil {
		a.log.Error("send turn.error failed", "err", err)
	}
}

// dispatchTurn runs a single user-turn through the provider and
// records both the user and assistant entries in the session tree.
// Returns the wire-shaped TurnResultPayload.
func (a *Agent) dispatchTurn(ctx context.Context, req frames.TurnPayload) (frames.TurnResultPayload, error) {
	// 1. Append user turn to tree.
	userEntry, err := a.tree.Append(ctx, tree.Entry{
		Kind: tree.KindTurnUser,
		Payload: map[string]any{
			"text": req.Prompt,
		},
	})
	if err != nil {
		return frames.TurnResultPayload{}, fmt.Errorf("append user turn: %w", err)
	}

	// 2. Replay active branch. Drop the trailing user entry we just
	//    appended — provider gets it separately via Prompt.
	branch, err := a.tree.Replay(ctx)
	if err != nil {
		return frames.TurnResultPayload{}, fmt.Errorf("replay: %w", err)
	}
	if n := len(branch); n > 0 && branch[n-1].ID == userEntry.ID {
		branch = branch[:n-1]
	}

	providerContext := convertToProviderEntries(branch)

	// 3. Invoke provider.
	result, err := a.cfg.Provider.Invoke(ctx, providers.InvokeRequest{
		Context:       providerContext,
		Prompt:        req.Prompt,
		SystemPrompt:  req.SystemPrompt,
		Model:         req.Model,
		ThinkingLevel: req.ThinkingLevel,
		MaxTokens:     req.MaxTokens,
	})
	if err != nil {
		return frames.TurnResultPayload{}, fmt.Errorf("provider invoke: %w", err)
	}

	// 4. Append assistant turn.
	assistantEntry, err := a.tree.Append(ctx, tree.Entry{
		Kind: tree.KindTurnAssistant,
		Payload: map[string]any{
			"text":        result.Output,
			"stop_reason": string(result.StopReason),
			"tokens":      result.Tokens,
		},
	})
	if err != nil {
		return frames.TurnResultPayload{}, fmt.Errorf("append assistant turn: %w", err)
	}

	return frames.TurnResultPayload{
		Output:     result.Output,
		StopReason: string(result.StopReason),
		Tokens: frames.TokenUsage{
			Input:  result.Tokens.Input,
			Output: result.Tokens.Output,
			Total:  result.Tokens.Total,
		},
		EntryIDs: []string{userEntry.ID, assistantEntry.ID},
	}, nil
}

// projectEntryUpward is the tree.AppendHook that forwards each local
// entry as a session.entry.appended frame. Runs on the caller's
// goroutine (after the tree mutex is released) so it MUST be quick —
// we fire the Send on a fresh goroutine with a bounded ctx. Transport
// spec §5.2/§8 positions this as read-only observability: if the
// frame never reaches Nexus (disconnected, slow upstream, etc.) the
// aspect keeps working because the authoritative state is the local
// JSONL.
func (a *Agent) projectEntryUpward(entry tree.Entry) {
	env, err := frames.New(frames.KindSessionEntryAppended, frames.SessionEntryAppendedPayload{
		Aspect:    a.cfg.Aspect.Name,
		SessionID: a.sessionID,
		EntryID:   entry.ID,
		ParentID:  entry.ParentID,
		EntryKind: string(entry.Kind),
		TS:        entry.TS,
		Payload:   entry.Payload,
	})
	if err != nil {
		a.log.Warn("build session.entry.appended frame failed", "err", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.ws.Send(ctx, env); err != nil {
			// Best-effort only — don't retry, don't log at warn level
			// for expected disconnect cases.
			a.log.Debug("session.entry.appended send failed (will not retry)",
				"entry", entry.ID, "err", err)
		}
	}()
}

// convertToProviderEntries converts tree entries to the provider's
// normalised entry type. They share shape but live in different
// packages; this is the seam.
func convertToProviderEntries(entries []tree.Entry) []providers.Entry {
	out := make([]providers.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, providers.Entry{
			ID:       e.ID,
			ParentID: e.ParentID,
			Kind:     providers.EntryKind(e.Kind),
			TS:       e.TS,
			Payload:  e.Payload,
		})
	}
	return out
}
