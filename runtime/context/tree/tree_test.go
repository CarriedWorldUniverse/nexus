package tree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestTree(t *testing.T) *Tree {
	t.Helper()
	tt, err := Open(t.TempDir(), "session-test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return tt
}

func TestFreshSession(t *testing.T) {
	tt := openTestTree(t)

	head, err := tt.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head != "" {
		t.Errorf("Head() on fresh session = %q, want empty", head)
	}

	entries, err := tt.Replay(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Replay on fresh session returned %d entries, want 0", len(entries))
	}
}

func TestAppendAndReplay(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()

	e1, err := tt.Append(ctx, Entry{
		Kind:    KindTurnUser,
		Payload: map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if e1.ID == "" {
		t.Error("Append didn't assign ID")
	}
	if e1.TS.IsZero() {
		t.Error("Append didn't assign TS")
	}
	if e1.ParentID != "" {
		t.Errorf("first entry ParentID = %q, want empty", e1.ParentID)
	}

	e2, err := tt.Append(ctx, Entry{
		Kind:    KindTurnAssistant,
		Payload: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if e2.ParentID != e1.ID {
		t.Errorf("e2 ParentID = %q, want %q", e2.ParentID, e1.ID)
	}

	head, _ := tt.Head()
	if head != e2.ID {
		t.Errorf("Head = %q, want e2.ID %q", head, e2.ID)
	}

	branch, err := tt.Replay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(branch) != 2 {
		t.Fatalf("Replay len = %d, want 2", len(branch))
	}
	if branch[0].ID != e1.ID || branch[1].ID != e2.ID {
		t.Errorf("Replay order wrong: got %v, want [e1, e2]", []string{branch[0].ID, branch[1].ID})
	}
}

func TestFork(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()

	// Build a linear chain: A -> B -> C
	a, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "A"}})
	b, _ := tt.Append(ctx, Entry{Kind: KindTurnAssistant, Payload: map[string]any{"text": "B"}})
	c, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "C"}})

	// Fork at B — go back to B, then append D. Tree: A-B-C and A-B-D.
	if err := tt.Fork(ctx, b.ID); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	d, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "D"}})
	if d.ParentID != b.ID {
		t.Errorf("D's parent = %q, want B %q", d.ParentID, b.ID)
	}

	branch, _ := tt.Replay(ctx)
	if len(branch) != 3 {
		t.Fatalf("post-fork branch len = %d, want 3 (A-B-D)", len(branch))
	}
	if branch[0].ID != a.ID || branch[1].ID != b.ID || branch[2].ID != d.ID {
		t.Errorf("wrong branch order: %s %s %s", branch[0].ID, branch[1].ID, branch[2].ID)
	}

	// Switch back to the C branch — A-B-C.
	if err := tt.SetHead(ctx, c.ID); err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	branch, _ = tt.Replay(ctx)
	if len(branch) != 3 {
		t.Fatalf("C-branch len = %d, want 3", len(branch))
	}
	if branch[2].ID != c.ID {
		t.Errorf("C-branch tip = %s, want c.ID %s", branch[2].ID, c.ID)
	}

	// All() returns every entry, across both branches.
	all, _ := tt.All(ctx)
	if len(all) != 4 {
		t.Errorf("All len = %d, want 4 (A, B, C, D)", len(all))
	}
}

func TestSetHeadMissingID(t *testing.T) {
	tt := openTestTree(t)
	err := tt.SetHead(context.Background(), "01HNOTEXIST")
	if err == nil {
		t.Error("expected error for unknown id")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want 'not found'", err)
	}
}

func TestAppendRequiresKind(t *testing.T) {
	tt := openTestTree(t)
	_, err := tt.Append(context.Background(), Entry{})
	if err == nil {
		t.Error("expected error for missing Kind")
	}
}

func TestAppendPreservesExplicitTS(t *testing.T) {
	tt := openTestTree(t)
	want := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	e, err := tt.Append(context.Background(), Entry{
		Kind: KindSystemPrompt,
		TS:   want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !e.TS.Equal(want) {
		t.Errorf("TS = %v, want %v", e.TS, want)
	}
}

func TestAppendPersists(t *testing.T) {
	dir := t.TempDir()
	tt, _ := Open(dir, "persist")
	ctx := context.Background()
	e1, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "hello"}})

	// Reopen and re-read.
	tt2, err := Open(dir, "persist")
	if err != nil {
		t.Fatal(err)
	}
	head, _ := tt2.Head()
	if head != e1.ID {
		t.Errorf("head after reopen = %q, want %q", head, e1.ID)
	}
	branch, _ := tt2.Replay(ctx)
	if len(branch) != 1 {
		t.Fatalf("branch after reopen len = %d, want 1", len(branch))
	}
	if branch[0].Payload["text"] != "hello" {
		t.Errorf("payload lost across reopen: %v", branch[0].Payload)
	}
}

func TestSidecarAtomicity(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()
	e, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "x"}})

	// After Append, the .tmp sidecar should have been renamed, not left behind.
	if _, err := os.Stat(tt.SidecarPath() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("leftover .tmp sidecar: %v", err)
	}
	if _, err := os.Stat(tt.SidecarPath()); err != nil {
		t.Errorf("sidecar missing after Append: %v", err)
	}
	_ = e
}

func TestJSONLFormat(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()
	_, _ = tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "a"}})
	_, _ = tt.Append(ctx, Entry{Kind: KindTurnAssistant, Payload: map[string]any{"text": "b"}})

	raw, err := os.ReadFile(tt.JSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	// JSONL must be one entry per line, no array brackets.
	text := string(raw)
	if strings.HasPrefix(text, "[") {
		t.Error("JSONL should not be a JSON array")
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Errorf("line count = %d, want 2", len(lines))
	}
}

func TestAppendExplicitParentBranches(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()

	a, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "A"}})
	b, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "B"}})

	// Explicit ParentID bypasses the default "head = parent" rule.
	c, err := tt.Append(ctx, Entry{
		ParentID: a.ID,
		Kind:     KindTurnUser,
		Payload:  map[string]any{"text": "C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ParentID != a.ID {
		t.Errorf("C.ParentID = %q, want A %q", c.ParentID, a.ID)
	}

	// Head moved to C because Append always advances head.
	head, _ := tt.Head()
	if head != c.ID {
		t.Errorf("head = %q, want c.ID %q", head, c.ID)
	}

	// Replay from C's branch should be A -> C (not A -> B -> C).
	branch, _ := tt.Replay(ctx)
	if len(branch) != 2 {
		t.Fatalf("explicit-parent branch len = %d, want 2", len(branch))
	}
	if branch[0].ID != a.ID || branch[1].ID != c.ID {
		t.Errorf("branch = [%s, %s], want [A, C]", branch[0].ID, branch[1].ID)
	}
	_ = b
}

func TestCompactionEntryKind(t *testing.T) {
	// Just a shape check — the Compaction implementation arrives in
	// part 6; for now verify that emitting a KindCompaction entry
	// with a typical payload works.
	tt := openTestTree(t)
	ctx := context.Background()
	_, _ = tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "a"}})

	c, err := tt.Append(ctx, Entry{
		Kind: KindCompaction,
		Payload: map[string]any{
			"firstKeptEntryId": "xyz",
			"summary":          "summary text",
			"tokensBefore":     1000,
			"tokensAfter":      200,
			"model":            "claude-opus-4-7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != KindCompaction {
		t.Errorf("kind = %q, want %q", c.Kind, KindCompaction)
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()

	src := map[string]any{
		"text":   "payload content",
		"number": float64(42), // JSON numbers unmarshal as float64
		"nested": map[string]any{"key": "value"},
	}
	e, _ := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: src})

	// Find it back via findEntry.
	found, ok, err := tt.findEntry(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("entry not found after round-trip")
	}
	if found.Payload["text"] != "payload content" {
		t.Errorf("text lost: %v", found.Payload["text"])
	}
	if found.Payload["number"].(float64) != 42 {
		t.Errorf("number lost: %v", found.Payload["number"])
	}
}

func TestFilesLandInExpectedDir(t *testing.T) {
	dir := t.TempDir()
	tt, err := Open(dir, "mysess")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tt.Append(context.Background(), Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "x"}})

	if _, err := os.Stat(filepath.Join(dir, "mysess.jsonl")); err != nil {
		t.Errorf("jsonl not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mysess.head.json")); err != nil {
		t.Errorf("sidecar not found: %v", err)
	}
}

// Regression for issue #39: every Tree method that takes a
// context.Context must honor it. A cancelled ctx must short-circuit
// the call rather than completing as if cancellation didn't happen.
func TestTreeMethodsHonorContext(t *testing.T) {
	tt := openTestTree(t)

	// Seed an entry under a live ctx so subsequent reads have something
	// to find.
	seedID := ""
	{
		e, err := tt.Append(context.Background(), Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "seed"}})
		if err != nil {
			t.Fatalf("seed Append: %v", err)
		}
		seedID = e.ID
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := tt.Append(ctx, Entry{Kind: KindTurnUser}); !errors.Is(err, context.Canceled) {
		t.Errorf("Append err = %v, want context.Canceled", err)
	}
	if err := tt.SetHead(ctx, seedID); !errors.Is(err, context.Canceled) {
		t.Errorf("SetHead err = %v, want context.Canceled", err)
	}
	if _, err := tt.Replay(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Replay err = %v, want context.Canceled", err)
	}
	if _, err := tt.All(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("All err = %v, want context.Canceled", err)
	}
}

// TestAppendRollsBackJSONLOnWriteHeadFailure verifies the post-PR
// rollback: if writeHead fails after the JSONL write+fsync, the JSONL
// is truncated back to its pre-write size so an orphan entry doesn't
// leak forever.
func TestAppendRollsBackJSONLOnWriteHeadFailure(t *testing.T) {
	tt := openTestTree(t)
	ctx := context.Background()

	// First Append succeeds and creates the sidecar.
	if _, err := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "first"}}); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	jsonlPath := strings.TrimSuffix(tt.SidecarPath(), ".head.json") + ".jsonl"
	preSize := mustSize(t, jsonlPath)

	// Force writeHead to fail by replacing the sidecar with a non-empty
	// directory of the same name — os.Rename(tmp, sidecar) then returns
	// ENOTDIR/EISDIR/ENOTEMPTY depending on platform.
	if err := os.Remove(tt.SidecarPath()); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	if err := os.Mkdir(tt.SidecarPath(), 0o755); err != nil {
		t.Fatalf("mkdir sidecar-as-dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tt.SidecarPath(), "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	if _, err := tt.Append(ctx, Entry{Kind: KindTurnUser, Payload: map[string]any{"text": "doomed"}}); err == nil {
		t.Fatal("Append succeeded; expected writeHead failure")
	}

	postSize := mustSize(t, jsonlPath)
	if postSize != preSize {
		t.Errorf("JSONL size after failed Append: got %d, want %d (rollback missed an orphan line)", postSize, preSize)
	}
}

func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
