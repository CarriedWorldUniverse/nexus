package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMaterialiseMCP_WritesProfile(t *testing.T) {
	dir := t.TempDir()
	const profile = `{"mcpServers":{"github":{"command":"node","env":{"TOKEN":"ghp-test"}}}}`

	if err := materialiseMCP(dir, profile, slog.Default()); err != nil {
		t.Fatalf("materialiseMCP: %v", err)
	}

	got, err := readMCPProfile(dir)
	if err != nil {
		t.Fatalf("readMCPProfile: %v", err)
	}

	// Normalise both sides through json.Unmarshal+Marshal so ordering
	// differences don't fail the test.
	var wantJSON, gotJSON any
	if err := json.Unmarshal([]byte(profile), &wantJSON); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &gotJSON); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	wantBytes, _ := json.Marshal(wantJSON)
	gotBytes, _ := json.Marshal(gotJSON)
	if string(wantBytes) != string(gotBytes) {
		t.Errorf("mismatch:\n want %s\n got  %s", string(wantBytes), string(gotBytes))
	}

	// The file should be readable only by owner (0600).
	fi, err := os.Stat(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("stat .mcp.json: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
		t.Errorf("permissions = %#o, want 0600", fi.Mode().Perm())
	}
}

func TestMaterialiseMCP_EmptyProfileIsNoOp(t *testing.T) {
	dir := t.TempDir()

	// Pre-create a .mcp.json — empty profile must leave it alone.
	const manual = `{"mcpServers":{"manual":{}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(manual), 0644); err != nil {
		t.Fatalf("write manual .mcp.json: %v", err)
	}

	if err := materialiseMCP(dir, "", slog.Default()); err != nil {
		t.Fatalf("materialiseMCP with empty profile: %v", err)
	}

	got, err := readMCPProfile(dir)
	if err != nil {
		t.Fatalf("readMCPProfile: %v", err)
	}
	if got != manual {
		t.Errorf("manual .mcp.json was modified — got %q, want %q", got, manual)
	}
}

func TestMaterialiseMCP_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()

	// Write a stale profile first.
	const stale = `{"mcpServers":{"old":{}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(stale), 0644); err != nil {
		t.Fatalf("write stale .mcp.json: %v", err)
	}

	const fresh = `{"mcpServers":{"new":{"command":"claude"}}}`
	if err := materialiseMCP(dir, fresh, slog.Default()); err != nil {
		t.Fatalf("materialiseMCP: %v", err)
	}

	got, err := readMCPProfile(dir)
	if err != nil {
		t.Fatalf("readMCPProfile: %v", err)
	}

	var wantJSON, gotJSON any
	json.Unmarshal([]byte(fresh), &wantJSON)
	json.Unmarshal([]byte(got), &gotJSON)
	wantBytes, _ := json.Marshal(wantJSON)
	gotBytes, _ := json.Marshal(gotJSON)
	if string(wantBytes) != string(gotBytes) {
		t.Errorf("mismatch:\n want %s\n got  %s", string(wantBytes), string(gotBytes))
	}
}

func TestMaterialiseMCP_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()

	err := materialiseMCP(dir, `not json`, slog.Default())
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	// Verify .mcp.json was NOT created.
	if _, statErr := os.Stat(filepath.Join(dir, ".mcp.json")); statErr == nil {
		t.Error("malformed profile should not create .mcp.json")
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".mcp.json.tmp")); statErr == nil {
		t.Error("malformed profile should clean up tmpfile")
	}
}

func TestMaterialiseMCP_AtomicNoPartialFile(t *testing.T) {
	dir := t.TempDir()

	// Write a pre-existing .mcp.json so we know it's preserved on failure.
	const existing = `{"mcpServers":{"existing":{}}}`
	existingPath := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(existingPath, []byte(existing), 0644); err != nil {
		t.Fatalf("write existing .mcp.json: %v", err)
	}

	// Make the tmpfile path a read-only directory so WriteFile fails.
	// This simulates a disk-full or permission error mid-write.
	tmpPath := filepath.Join(dir, ".mcp.json.tmp")
	if err := os.Mkdir(tmpPath, 0444); err != nil {
		t.Fatalf("mkdir tmp path: %v", err)
	}

	err := materialiseMCP(dir, `{"mcpServers":{"should-not-land":{}}}`, slog.Default())
	if err == nil {
		t.Fatal("expected error when tmpfile write fails, got nil")
	}

	// Existing .mcp.json must be untouched.
	got, readErr := readMCPProfile(dir)
	if readErr != nil {
		t.Fatalf("readMCPProfile: %v", readErr)
	}
	if got != existing {
		t.Errorf("existing .mcp.json was corrupted: got %q, want %q", got, existing)
	}
}
