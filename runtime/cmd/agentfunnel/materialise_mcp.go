// NEX-170: materialise .mcp.json from the validate response before
// spawning the claude-code subprocess. The MCP profile is the resolved
// JSON blob from NEX-169's credential substitution — already in the
// standard {"mcpServers": {...}} shape — so the write is a validated
// passthrough with atomic replacement.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// materialiseMCP writes the resolved MCP profile to <cwd>/.mcp.json
// atomically (tmpfile + rename). No-op when profile is empty — leaves any
// existing .mcp.json alone so operators can maintain manual overrides
// during the transition.
func materialiseMCP(cwd, profile string, log *slog.Logger) error {
	if profile == "" {
		return nil
	}

	// Validate that the profile is well-formed JSON. The server already
	// guarantees this, but a parse guard here catches wire corruption and
	// prevents writing a garbage file that would break claude-code's
	// .mcp.json discovery on the next startup.
	var parsed any
	if err := json.Unmarshal([]byte(profile), &parsed); err != nil {
		return fmt.Errorf("materialise .mcp.json: profile is not valid JSON: %w", err)
	}

	// Re-marshal with indentation so operators can read the file.
	pretty, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return fmt.Errorf("materialise .mcp.json: re-marshal: %w", err)
	}

	target := filepath.Join(cwd, ".mcp.json")
	tmp := target + ".tmp"

	// Write to tmpfile with restricted permissions — credentials may be
	// embedded in env blocks.
	if err := os.WriteFile(tmp, pretty, 0600); err != nil {
		return fmt.Errorf("materialise .mcp.json: write tmpfile: %w", err)
	}

	if err := os.Rename(tmp, target); err != nil {
		// Best-effort cleanup; don't mask the rename error.
		_ = os.Remove(tmp)
		return fmt.Errorf("materialise .mcp.json: rename: %w", err)
	}

	log.Info("agentfunnel: materialised .mcp.json",
		"path", target,
		"bytes", len(pretty))
	return nil
}

// readMCPProfile is a test helper that returns the content of
// <cwd>/.mcp.json, or "" when the file doesn't exist.
func readMCPProfile(cwd string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}
