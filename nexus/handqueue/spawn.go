package handqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/handexec"
)

// AspectHomeResolver looks up the filesystem path for an aspect by
// name. The dispatcher gets this from its in-memory roster.
type AspectHomeResolver interface {
	HomeFor(aspect string) (string, bool)
}

// AspectHomeResolverFunc adapts a plain function to AspectHomeResolver.
type AspectHomeResolverFunc func(aspect string) (string, bool)

// HomeFor implements AspectHomeResolver.
func (f AspectHomeResolverFunc) HomeFor(aspect string) (string, bool) {
	return f(aspect)
}

// SpawnExecutor runs a dispatch by spawning a harness subprocess in
// dispatch mode. The harness binary path is configurable so tests can
// point at a mock; production wires it to the same binary that runs
// the aspect (one binary, two modes per transport spec §2.2).
//
// Per hand-dispatch v0.1: the spawned worker boots loaded with the
// dispatching aspect's home directory — its NEXUS.md, SOUL.md, PRIMER
// frame the persona for this single fresh-context turn. There is no
// per-named-hand lookup; slots are interchangeable, persona inherits
// from the dispatcher.
type SpawnExecutor struct {
	// HarnessPath is the absolute path to the harness executable.
	// If empty, defaults to looking up "harness" on PATH via
	// exec.LookPath semantics.
	HarnessPath string

	// HomeResolver maps aspect name → home folder on this host.
	HomeResolver AspectHomeResolver

	// Env entries passed to the child, in addition to the parent's.
	// Typically carries NEXUS_UPSTREAM / NEXUS_OUTPOST / NEXUS_TOKEN
	// so the worker can query knowledge etc. via WS if/when that lands.
	ExtraEnv []string
}

// Execute spawns a harness subprocess, pipes the request as JSON on
// stdin, reads the DispatchResultPayload JSON on stdout, returns it.
func (s *SpawnExecutor) Execute(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
	home, ok := s.HomeResolver.HomeFor(req.Aspect)
	if !ok {
		return frames.DispatchResultPayload{}, fmt.Errorf("aspect %q not locally resolvable", req.Aspect)
	}
	harness := s.HarnessPath
	if harness == "" {
		harness = "harness"
	}

	cmd := exec.CommandContext(ctx, harness, "-home", home, "-hand")
	if len(s.ExtraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), s.ExtraEnv...)
	}

	stdinReq := handexec.Request{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: req.DispatchID,
		Payload:    req.Payload,
	}
	stdinBytes, err := json.Marshal(stdinReq)
	if err != nil {
		return frames.DispatchResultPayload{}, fmt.Errorf("marshal stdin: %w", err)
	}
	cmd.Stdin = bytes.NewReader(stdinBytes)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return frames.DispatchResultPayload{
			Aspect:     req.Aspect,
			Thread:     req.Thread,
			DispatchID: req.DispatchID,
			Error:      fmt.Sprintf("%s (stderr: %s)", err, truncate(stderr.String(), 500)),
		}, err
	}

	// Parse the last non-empty line of stdout as the response
	// envelope — handexec writes one JSON object, but the harness
	// may also log to stdout during startup depending on provider
	// implementation; defensive parsing.
	raw := lastJSONLine(stdout.Bytes())
	if len(raw) == 0 {
		return frames.DispatchResultPayload{
			Aspect:     req.Aspect,
			Thread:     req.Thread,
			DispatchID: req.DispatchID,
			Error:      "harness produced no JSON response on stdout",
		}, fmt.Errorf("empty response")
	}
	var result frames.DispatchResultPayload
	if err := json.Unmarshal(raw, &result); err != nil {
		return frames.DispatchResultPayload{
			Aspect:     req.Aspect,
			Thread:     req.Thread,
			DispatchID: req.DispatchID,
			Error:      fmt.Sprintf("parse response: %v", err),
		}, err
	}
	// Fill in identity fields in case the harness didn't; the
	// dispatcher always knows what it dispatched.
	if result.Aspect == "" {
		result.Aspect = req.Aspect
	}
	if result.Thread == "" {
		result.Thread = req.Thread
	}
	if result.DispatchID == "" {
		result.DispatchID = req.DispatchID
	}
	return result, nil
}

// lastJSONLine returns the last line of data that starts with '{'.
// Handles the case where the harness logs to stdout before emitting
// its JSON envelope.
func lastJSONLine(data []byte) []byte {
	var best []byte
	start := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			line := bytes.TrimSpace(data[start:i])
			if len(line) > 0 && line[0] == '{' {
				best = line
			}
			start = i + 1
		}
	}
	return best
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
