// Package handexec implements hand-mode execution for the harness
// binary. Spawned as a fresh subprocess by the dispatcher; reads the
// invocation from the process args/env; invokes the configured
// provider; writes a HandResultPayload JSON envelope to stdout;
// exits. Per transport spec §6.3.
//
// One-shot lifecycle by design — no reconnect loops, no persistent
// state. Any hand-owned credentials live in the aspect home and are
// loaded per-invocation.
package handexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/providers"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// Request is the JSON shape the harness hand-mode reads from stdin.
// (Arguments could carry it too, but JSON on stdin keeps args clean.)
type Request struct {
	HandName string         `json:"hand_name"`
	ThreadID string         `json:"thread_id,omitempty"`
	Invoker  string         `json:"invoker"`
	Input    map[string]any `json:"input"`
}

// Run reads a Request from stdin, executes the hand against the
// aspect's configured provider, writes a HandResultPayload to
// stdout as JSON, returns.
//
// On error, writes a minimal HandResultPayload with Error set, plus
// returns an error so the caller process can exit non-zero.
func Run(ctx context.Context, aspectHome string, aspect schemas.AspectConfig, provider providers.Provider) error {
	req, err := readRequest(os.Stdin)
	if err != nil {
		return writeAndReturnError("read stdin", err)
	}

	hand, ok := findHand(aspect, req.HandName)
	if !ok {
		return writeAndReturnError("unknown hand", fmt.Errorf("hand %q not declared in aspect.json", req.HandName))
	}

	// Build the provider invocation. Hand's system_prompt + the
	// caller-supplied input, serialised as a prompt. Tools are
	// wired into the provider call once the runtime's tool
	// allowlist layer arrives (later part); for now we rely on the
	// aspect's declared tool list from aspect.json and let the
	// provider negotiate.
	promptBytes, err := json.Marshal(req.Input)
	if err != nil {
		return writeAndReturnError("marshal input", err)
	}

	result, err := provider.Invoke(ctx, providers.InvokeRequest{
		Prompt:       string(promptBytes),
		SystemPrompt: hand.SystemPrompt,
	})
	if err != nil {
		return writeAndReturnError("provider.Invoke", err)
	}

	// Parse the provider's string output back into a map if it's
	// JSON-shaped; otherwise wrap it in {text: ...}.
	output := parseOutput(result.Output)

	resp := frames.HandResultPayload{
		TargetAspect: aspect.Name,
		HandName:     req.HandName,
		ThreadID:     req.ThreadID,
		Output:       output,
		Tokens: frames.TokenUsage{
			Input:  result.Tokens.Input,
			Output: result.Tokens.Output,
			Total:  result.Tokens.Total,
		},
	}

	// Model name comes from the provider's result if provided;
	// otherwise leave empty and let the dispatcher default in the
	// audit log.
	return writeResponse(resp)
}

func readRequest(r io.Reader) (Request, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Request{}, err
	}
	if len(raw) == 0 {
		return Request{}, errors.New("empty stdin")
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return Request{}, fmt.Errorf("parse request: %w", err)
	}
	if req.HandName == "" {
		return Request{}, errors.New("hand_name required")
	}
	return req, nil
}

func findHand(cfg schemas.AspectConfig, name string) (schemas.HandConfig, bool) {
	for _, h := range cfg.Hands {
		if h.Name == name {
			return h, true
		}
	}
	return schemas.HandConfig{}, false
}

// parseOutput attempts to decode the provider string as JSON into a
// map. If that fails, returns {text: <string>} as a fallback.
func parseOutput(s string) map[string]any {
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err == nil && out != nil {
		return out
	}
	return map[string]any{"text": s}
}

// writeResponse writes the HandResultPayload as a single-line JSON
// envelope to stdout. The dispatcher reads stdout; stderr carries
// the harness's regular log output (which the dispatcher should
// forward to its own logs).
func writeResponse(resp frames.HandResultPayload) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	_, err = os.Stdout.Write(append(raw, '\n'))
	return err
}

// writeAndReturnError emits a HandResultPayload with Error set,
// returns the underlying error so the caller process can exit non-
// zero. Keeps the dispatcher's parse path consistent even on
// failures.
func writeAndReturnError(label string, err error) error {
	// Payload has TargetAspect/HandName unset on error — the
	// dispatcher already knows which hand it dispatched.
	_ = writeResponse(frames.HandResultPayload{
		Error: fmt.Sprintf("%s: %v", label, err),
	})
	return fmt.Errorf("%s: %w", label, err)
}
