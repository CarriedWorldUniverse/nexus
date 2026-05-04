// Package handexec implements dispatch-mode execution for the harness
// binary. Spawned as a fresh subprocess by the dispatcher; reads the
// dispatch request from stdin; invokes the configured provider; writes
// a DispatchResultPayload JSON envelope to stdout; exits.
//
// Per hand-dispatch v0.1: workers are interchangeable subprocess slots
// drawn from a shared pool. Each worker boots loaded with the
// dispatching aspect's identity framing (NEXUS.md / SOUL.md / PRIMER
// from the aspect home). There are no per-aspect named hands and no
// per-hand config — the persona inherited from the dispatching aspect
// IS the slot's persona for the duration of one dispatch.
//
// One-shot lifecycle by design — no reconnect loops, no persistent
// state. Any aspect-owned credentials live in the aspect home and are
// loaded per-invocation.
//
// NOTE (§6.5): identity-bundle loading (NEXUS.md / SOUL.md / PRIMER →
// composed system prompt) lives behind the Frame harness work. v0.1
// keeps the spawn machinery in place but currently invokes the
// provider with the dispatch payload directly; the identity-framing
// composition lands when the Frame harness ports identity loaders here.
//
// The package directory name (`handexec`, parent `nexus/handqueue`)
// is legacy and kept per spec §9 amnesty — only identifiers and types
// inside have moved to the generic vocabulary.
package handexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/providers"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// Request is the JSON shape the harness dispatch-mode reads from
// stdin. Mirrors the body fields of frames.DispatchPayload.
type Request struct {
	Aspect     string         `json:"aspect"`
	Thread     string         `json:"thread,omitempty"`
	DispatchID string         `json:"dispatch_id,omitempty"`
	Payload    map[string]any `json:"payload"`
}

// Run reads a Request from stdin, executes a single dispatch turn
// against the aspect's configured provider, writes a
// DispatchResultPayload to stdout as JSON, returns.
//
// On error, writes a minimal DispatchResultPayload with Error set,
// plus returns an error so the caller process can exit non-zero.
//
// `aspectHome` is the on-disk home for the dispatching aspect — the
// identity-loading layer (§6.5) consumes this; for now we accept it
// but only forward the dispatch payload to the provider.
func Run(ctx context.Context, aspectHome string, aspect schemas.AspectConfig, provider providers.Provider) error {
	req, err := readRequest(os.Stdin)
	if err != nil {
		return writeAndReturnError(req, "read stdin", err)
	}

	// TODO(§6.5): Frame harness composes the identity-framing system
	// prompt from aspectHome/NEXUS.md, aspectHome/SOUL.md, and
	// aspectHome/PRIMER.md, then prepends to req.Payload before
	// invoking the provider. The dispatcher already passes:
	//   - aspectHome (this function arg) as the worker's cwd via
	//     SpawnExecutor's cmd.Dir.
	//   - NEXUS_TOKEN env carrying the dispatching aspect's bearer
	//     token, so any callbacks (post results, knowledge) auth as
	//     that aspect.
	// Drift D establishes the spawning machinery + identity-passing;
	// §6.5 wires prompt-composition on top.
	//
	// The dispatch payload is forwarded to the provider as the prompt
	// body. The identity-framing system prompt is composed by the
	// Frame harness layer (§6.5) and not implemented here yet.
	promptBytes, err := json.Marshal(req.Payload)
	if err != nil {
		return writeAndReturnError(req, "marshal payload", err)
	}

	result, err := provider.Invoke(ctx, providers.InvokeRequest{
		Prompt: string(promptBytes),
	})
	if err != nil {
		return writeAndReturnProviderError(req, err)
	}

	output := parseOutput(result.Output)

	resp := frames.DispatchResultPayload{
		Aspect:     aspect.Name,
		Thread:     req.Thread,
		DispatchID: req.DispatchID,
		Output:     output,
		Tokens: frames.TokenUsage{
			Input:  result.Tokens.Input,
			Output: result.Tokens.Output,
			Total:  result.Tokens.Total,
		},
	}
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
	if req.Aspect == "" {
		return Request{}, errors.New("aspect required")
	}
	return req, nil
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

// writeResponse writes the DispatchResultPayload as a single-line JSON
// envelope to stdout. The dispatcher reads stdout; stderr carries
// the harness's regular log output (which the dispatcher should
// forward to its own logs).
func writeResponse(resp frames.DispatchResultPayload) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	_, err = os.Stdout.Write(append(raw, '\n'))
	return err
}

// writeAndReturnError emits a DispatchResultPayload with Error set,
// returns the underlying error so the caller process can exit non-
// zero. Keeps the dispatcher's parse path consistent even on failures.
//
// Used for harness-internal failures (stdin parse, payload marshal)
// where the error text is generated locally and safe to surface.
// Provider errors must go through writeAndReturnProviderError so they
// are mapped to opaque codes — provider error strings can carry prompt
// fragments, request bodies, or partial credentials and must not
// round-trip across the dispatch boundary.
func writeAndReturnError(req Request, label string, err error) error {
	_ = writeResponse(frames.DispatchResultPayload{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: req.DispatchID,
		Error:      fmt.Sprintf("%s: %v", label, err),
	})
	return fmt.Errorf("%s: %w", label, err)
}

// writeAndReturnProviderError redacts a provider error before writing
// the dispatch result, then returns the rich error to the local
// process so it lands in stderr / process logs. The dispatch payload
// receives only the opaque code from providerErrorCode — upstream
// error strings (prompt fragments, account ids, masked-but-partial
// credentials in 401 echoes) never cross the dispatch boundary.
func writeAndReturnProviderError(req Request, err error) error {
	code := providerErrorCode(err)
	// Local-only log of the rich error for operator debugging.
	slog.Error("dispatch provider error",
		"aspect", req.Aspect,
		"thread", req.Thread,
		"dispatch_id", req.DispatchID,
		"code", code,
		"err", err)
	_ = writeResponse(frames.DispatchResultPayload{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: req.DispatchID,
		Error:      code,
	})
	return fmt.Errorf("provider.Invoke: %w", err)
}

// providerErrorCode maps a provider error to a small set of opaque
// codes safe to send across the dispatch boundary. Recipients pattern-
// match on these codes; rich detail stays in local logs.
func providerErrorCode(err error) string {
	switch {
	case errors.Is(err, providers.ErrAuth):
		return "provider_auth"
	case errors.Is(err, providers.ErrRateLimit):
		return "provider_rate_limit"
	case errors.Is(err, providers.ErrContextWindow):
		return "provider_context_window"
	case errors.Is(err, providers.ErrTimeout):
		return "provider_timeout"
	case errors.Is(err, providers.ErrUnsupported):
		return "provider_unsupported"
	case errors.Is(err, providers.ErrProvider):
		return "provider_error"
	default:
		return "provider_internal"
	}
}
