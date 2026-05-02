package wsasp

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Cursor persistence is the load-bearing Lock 6 invariant on the
// aspect side. These tests exercise it without standing up a real
// WS connection — the wsclient itself has its own coverage.

func TestNewClient_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no URL", Config{AspectName: "anvil", OnDeliver: noopDeliver}},
		{"no AspectName", Config{URL: "wss://x", OnDeliver: noopDeliver}},
		{"no OnDeliver", Config{URL: "wss://x", AspectName: "anvil"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.cfg); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestClient_AdvanceCursorPersists(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")

	c, err := NewClient(Config{
		URL:        "wss://example/connect",
		AspectName: "anvil",
		CursorFile: cursorFile,
		OnDeliver:  noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.advanceCursor(42)

	data, err := os.ReadFile(cursorFile)
	if err != nil {
		t.Fatalf("cursor file not written: %v", err)
	}
	if got, _ := strconv.ParseInt(string(data), 10, 64); got != 42 {
		t.Errorf("persisted cursor = %s, want 42", string(data))
	}
}

func TestClient_AdvanceCursorMonotonic(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", CursorFile: cursorFile, OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.advanceCursor(50)
	c.advanceCursor(20) // older — must not regress
	c.advanceCursor(60)

	data, _ := os.ReadFile(cursorFile)
	if got, _ := strconv.ParseInt(string(data), 10, 64); got != 60 {
		t.Errorf("cursor regressed: got %s, want 60", string(data))
	}
}

func TestClient_LoadCursorReadsPersistedValue(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")
	if err := os.WriteFile(cursorFile, []byte("99999"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", CursorFile: cursorFile, OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cursor != 99999 {
		t.Errorf("cursor = %d, want 99999", c.cursor)
	}
}

func TestClient_LoadCursorMissingFileColdStarts(t *testing.T) {
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a",
		CursorFile: "/nonexistent/dir/cursor",
		OnDeliver:  noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cursor != 0 {
		t.Errorf("missing cursor file should cold-start at 0: got %d", c.cursor)
	}
}

func TestClient_LoadCursorBadValueColdStarts(t *testing.T) {
	dir := t.TempDir()
	cursorFile := filepath.Join(dir, "cursor")
	os.WriteFile(cursorFile, []byte("not a number"), 0o600)

	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", CursorFile: cursorFile, OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cursor != 0 {
		t.Errorf("garbled cursor file should cold-start at 0: got %d", c.cursor)
	}
}

func TestClient_NoCursorFileNoPersist(t *testing.T) {
	// Empty CursorFile means no persistence — advanceCursor still
	// updates in-memory but doesn't write anywhere.
	c, err := NewClient(Config{
		URL: "wss://x", AspectName: "a", OnDeliver: noopDeliver,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.advanceCursor(100)
	if c.cursor != 100 {
		t.Errorf("in-memory cursor not updated: got %d", c.cursor)
	}
}

func TestCursorFileForAspect(t *testing.T) {
	got := CursorFileForAspect("/aspects/anvil")
	want := filepath.Join("/aspects/anvil", "cursor")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func noopDeliver(_ DeliveredMessage) {}
