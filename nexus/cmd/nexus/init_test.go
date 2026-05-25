package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runInitInTemp invokes runInitSubcommand against a fresh temp dir
// with stdout captured. Returns (tempDir, capturedStdout, exitCode).
func runInitInTemp(t *testing.T, extraArgs ...string) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	args := append([]string{"--data-dir", dir}, extraArgs...)

	// Capture stdout while runInitSubcommand runs.
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	code := runInitSubcommand(args)

	w.Close()
	os.Stdout = origStdout
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return dir, buf.String(), code
}

func TestInit_FreshDirCreatesExpectedLayout(t *testing.T) {
	dir, out, code := runInitInTemp(t)
	if code != 0 {
		t.Fatalf("exit code %d; output:\n%s", code, out)
	}

	// Required artifacts on disk.
	for _, want := range []string{
		"nexus.db",
		"ledger.db",
		"sample.mcp.json",
		"keyfiles",
		"activity",
		"sessions",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing artifact %q: %v", want, err)
		}
	}

	// Output should include the token banner + next-steps.
	for _, want := range []string{
		"data-dir:",
		"broker.db schema ready",
		"ledger.db schema ready",
		"sample.mcp.json",
		"Operator admin token",
		"Next steps",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestInit_QuietPrintsOnlyToken(t *testing.T) {
	_, out, code := runInitInTemp(t, "--quiet")
	if code != 0 {
		t.Fatalf("exit code %d; output:\n%s", code, out)
	}
	trimmed := strings.TrimSpace(out)
	lines := strings.Split(trimmed, "\n")
	if len(lines) != 1 {
		t.Errorf("expected single line of output (the token); got %d lines:\n%s", len(lines), out)
	}
	// Token is hex; sanity-check it's non-empty + only hex chars.
	tok := lines[0]
	if len(tok) < 32 {
		t.Errorf("token looks too short: %q", tok)
	}
	for _, r := range tok {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("token contains non-hex char %q in %q", r, tok)
			break
		}
	}
}

func TestInit_IdempotentRunReturnsSameToken(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--data-dir", dir, "--quiet"}

	captureRun := func() string {
		origStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		code := runInitSubcommand(args)
		w.Close()
		os.Stdout = origStdout
		if code != 0 {
			t.Fatalf("init returned non-zero: %d", code)
		}
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		return strings.TrimSpace(buf.String())
	}

	first := captureRun()
	second := captureRun()
	if first != second {
		t.Errorf("re-run produced different token:\n first:  %s\n second: %s", first, second)
	}
}

func TestInit_MCPSampleHasExpectedShape(t *testing.T) {
	dir, _, _ := runInitInTemp(t, "--quiet")
	mcp, err := os.ReadFile(filepath.Join(dir, "sample.mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(mcp)
	for _, want := range []string{
		`"mcpServers"`,
		`"nexus-comms"`,
		`"nexus-jira"`,
		`"nexus-imap"`,
		"{{ NEXUS_BIN_DIR }}",
		"{{ DATA_DIR }}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sample.mcp.json missing %q\n---\n%s", want, body)
		}
	}
}
