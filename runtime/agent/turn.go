package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/nexus-cw/nexus/runtime/context/tree"
	"github.com/nexus-cw/nexus/runtime/providers"
)

// TurnRequest is the inbound shape for POST /turn. Matches what the
// Nexus (or a test) sends to drive a conversation step.
type TurnRequest struct {
	Prompt       string `json:"prompt"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Model        string `json:"model,omitempty"`
}

// TurnResponse carries the adapter's reply back.
type TurnResponse struct {
	Output     string                   `json:"output"`
	StopReason string                   `json:"stop_reason"`
	Tokens     providers.TokenCounts    `json:"tokens"`
	EntryIDs   []string                 `json:"entry_ids"`
}

// startTurnServer binds the configured listen address and starts a
// goroutine serving POST /turn. Writes the resolved listen URL back
// to the agent so tests and ops can find it.
func (a *Agent) startTurnServer() error {
	addr := a.cfg.ListenAddr
	if addr == "" {
		addr = ":0" // tests expect a ephemeral port
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/turn", a.handleTurn)
	mux.HandleFunc("/healthz", a.handleHealthz)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	a.mu.Lock()
	a.srv = srv
	a.listenURL = "http://" + ln.Addr().String()
	a.mu.Unlock()

	go func() {
		err := srv.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			// Push to the serve-error channel so Start can react —
			// without this, a crashed listener leaves registration
			// stale and Nexus keeps routing to a dead endpoint.
			a.log.Error("turn server exited abnormally", "err", err)
			select {
			case a.serveErrCh <- err:
			default:
				// Channel full or no reader — don't block the goroutine.
			}
		}
	}()
	return nil
}

func (a *Agent) shutdownTurnServer(ctx context.Context) error {
	a.mu.Lock()
	srv := a.srv
	a.srv = nil
	a.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (a *Agent) handleHealthz(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (a *Agent) handleTurn(w http.ResponseWriter, r *http.Request) {
	// Drain on all exit paths so HTTP/1.1 keep-alive can reuse the
	// connection even after decode failures.
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()

	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req TurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, "empty prompt", http.StatusBadRequest)
		return
	}

	resp, err := a.dispatchTurn(r.Context(), req)
	if err != nil {
		a.log.Error("turn dispatch failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// dispatchTurn runs a single user-turn through the provider and
// records both the user and assistant entries in the session tree.
// Returns the entry ids so callers can trace exactly which tree
// nodes the turn produced.
func (a *Agent) dispatchTurn(ctx context.Context, req TurnRequest) (TurnResponse, error) {
	// 1. Append user turn to tree (advances head).
	userEntry, err := a.tree.Append(ctx, tree.Entry{
		Kind: tree.KindTurnUser,
		Payload: map[string]any{
			"text": req.Prompt,
		},
	})
	if err != nil {
		return TurnResponse{}, fmt.Errorf("append user turn: %w", err)
	}

	// 2. Replay active branch for provider context (excludes the user
	//    turn itself — we pass that as InvokeRequest.Prompt).
	branch, err := a.tree.Replay(ctx)
	if err != nil {
		return TurnResponse{}, fmt.Errorf("replay: %w", err)
	}
	// Drop the trailing user entry we just appended — provider gets
	// it separately via Prompt.
	if n := len(branch); n > 0 && branch[n-1].ID == userEntry.ID {
		branch = branch[:n-1]
	}

	providerContext := convertToProviderEntries(branch)

	// 3. Invoke provider.
	result, err := a.cfg.Provider.Invoke(ctx, providers.InvokeRequest{
		Context:      providerContext,
		Prompt:       req.Prompt,
		SystemPrompt: req.SystemPrompt,
		Model:        req.Model,
	})
	if err != nil {
		return TurnResponse{}, fmt.Errorf("provider invoke: %w", err)
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
		return TurnResponse{}, fmt.Errorf("append assistant turn: %w", err)
	}

	return TurnResponse{
		Output:     result.Output,
		StopReason: string(result.StopReason),
		Tokens:     result.Tokens,
		EntryIDs:   []string{userEntry.ID, assistantEntry.ID},
	}, nil
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

// newByteReader wraps a byte slice as an io.Reader. Used by the
// Nexus-bound postJSON helper; extracted here so both files can
// reference it.
func newByteReader(b []byte) io.Reader { return bytes.NewReader(b) }
