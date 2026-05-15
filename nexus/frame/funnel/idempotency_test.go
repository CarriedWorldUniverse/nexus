package funnel

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// TestReceive_DropsDuplicateMsgID verifies NEX-96's primary guard:
// a Receive call with a MsgID already in the seen-set is silently
// dropped rather than appended to the inbox.
func TestReceive_DropsDuplicateMsgID(t *testing.T) {
	f := newIdempotencyFunnel(t, "")

	// First receive accepts.
	f.Receive(bridle.InboxItem{MsgID: 42, Content: "hello"})
	if got := f.InboxLen(); got != 1 {
		t.Fatalf("first receive: inbox len = %d, want 1", got)
	}

	// Simulate Deliberate having popped + marked the msg_id by
	// calling markSeenLocked directly. Real flow has Deliberate doing
	// this; testing the guard in isolation.
	f.mu.Lock()
	f.markSeenLocked(42)
	// Drain inbox to simulate the pop.
	f.inbox = nil
	f.mu.Unlock()

	// Second receive of the same msg_id should be dropped.
	f.Receive(bridle.InboxItem{MsgID: 42, Content: "hello"})
	if got := f.InboxLen(); got != 0 {
		t.Fatalf("duplicate receive: inbox len = %d, want 0", got)
	}

	// A fresh msg_id still gets accepted.
	f.Receive(bridle.InboxItem{MsgID: 43, Content: "world"})
	if got := f.InboxLen(); got != 1 {
		t.Fatalf("fresh receive: inbox len = %d, want 1", got)
	}
}

// TestReceiveWithMsgID_DropsDuplicate verifies the with-msgid variant
// honors the same idempotency guard.
func TestReceiveWithMsgID_DropsDuplicate(t *testing.T) {
	f := newIdempotencyFunnel(t, "")

	f.mu.Lock()
	f.markSeenLocked(99)
	f.mu.Unlock()

	f.ReceiveWithMsgID(bridle.InboxItem{Content: "x"}, 99)
	if got := f.InboxLen(); got != 0 {
		t.Fatalf("with-msgid duplicate: inbox len = %d, want 0", got)
	}
}

// TestReceive_AllowsMsgIDZero verifies that items without a MsgID
// (zero) bypass the idempotency guard. Internal/synthetic items
// shouldn't be deduped — they're not chat-message-derived.
func TestReceive_AllowsMsgIDZero(t *testing.T) {
	f := newIdempotencyFunnel(t, "")

	for i := 0; i < 3; i++ {
		f.Receive(bridle.InboxItem{MsgID: 0, Content: "synthetic"})
	}
	if got := f.InboxLen(); got != 3 {
		t.Fatalf("synthetic receives: inbox len = %d, want 3", got)
	}
}

// TestMarkSeen_FIFOEviction verifies that the seen-set respects its
// cap by evicting oldest entries first.
func TestMarkSeen_FIFOEviction(t *testing.T) {
	f := newIdempotencyFunnel(t, "")
	f.cfg.IdempotencyCap = 3

	f.mu.Lock()
	f.markSeenLocked(10)
	f.markSeenLocked(20)
	f.markSeenLocked(30)
	f.markSeenLocked(40) // evicts 10
	f.mu.Unlock()

	// 10 should be evicted, 20-40 should remain.
	if _, ok := f.seenMsgIDs[10]; ok {
		t.Errorf("expected 10 evicted")
	}
	for _, id := range []int64{20, 30, 40} {
		if _, ok := f.seenMsgIDs[id]; !ok {
			t.Errorf("expected %d retained", id)
		}
	}
	if len(f.seenOrder) != 3 {
		t.Errorf("seenOrder len = %d, want 3", len(f.seenOrder))
	}
}

// TestPersist_SurvivesRestart simulates a funnel persisting its
// seen-set, then a new funnel hydrating from the same file —
// the hydrated funnel must recognize the original msg_ids.
func TestPersist_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idempotency.json")

	// Build with no persistence file so markSeenLocked doesn't kick
	// async writes that would race with our explicit persist call below.
	f1 := newIdempotencyFunnel(t, "")
	f1.mu.Lock()
	f1.markSeenLocked(100)
	f1.markSeenLocked(200)
	f1.markSeenLocked(300)
	snapshot := append([]int64(nil), f1.seenOrder...)
	f1.mu.Unlock()
	// Now point at the path and persist exactly once.
	f1.cfg.IdempotencyFile = path
	f1.persistSnapshot(snapshot)

	// Verify the file got written.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persist file missing: %v", err)
	}

	// New funnel hydrating from the same path.
	f2 := newIdempotencyFunnel(t, path)
	if got := len(f2.seenMsgIDs); got != 3 {
		t.Fatalf("hydrated set size = %d, want 3", got)
	}
	for _, id := range []int64{100, 200, 300} {
		if _, ok := f2.seenMsgIDs[id]; !ok {
			t.Errorf("hydrated set missing %d", id)
		}
	}

	// Duplicate-drop guard should fire post-restart.
	f2.Receive(bridle.InboxItem{MsgID: 100, Content: "replayed"})
	if got := f2.InboxLen(); got != 0 {
		t.Fatalf("post-restart duplicate: inbox len = %d, want 0", got)
	}
}

// TestLoadSeenMsgIDs_MissingFile is a no-op (cold start). New funnel
// with an unwritten path proceeds with an empty seen-set.
func TestLoadSeenMsgIDs_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.json")

	f := newIdempotencyFunnel(t, path)
	if got := len(f.seenMsgIDs); got != 0 {
		t.Fatalf("cold-start set size = %d, want 0", got)
	}
}

// TestLoadSeenMsgIDs_CorruptFile returns error rather than crashing.
// New logs + continues; the funnel proceeds with an empty seen-set.
func TestLoadSeenMsgIDs_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// New itself catches the load error + logs; the funnel still
	// constructs and proceeds. Verify directly here.
	f := newIdempotencyFunnel(t, "")
	f.cfg.IdempotencyFile = path
	err := f.loadSeenMsgIDs()
	if err == nil {
		t.Fatalf("expected parse error from corrupt file, got nil")
	}
}

// TestPersistSnapshot_AtomicWrite verifies the temp-file + rename
// pattern works (no corrupt file visible mid-write).
func TestPersistSnapshot_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.json")

	f := newIdempotencyFunnel(t, path)
	f.persistSnapshot([]int64{1, 2, 3})

	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after persist: %v", err)
	}
	var ids []int64
	if err := json.Unmarshal(buf, &ids); err != nil {
		t.Fatalf("parse persisted file: %v", err)
	}
	if len(ids) != 3 || ids[0] != 1 || ids[2] != 3 {
		t.Errorf("persisted = %v, want [1, 2, 3]", ids)
	}

	// No leftover .tmp file.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file should be cleaned up, got err %v", err)
	}
}

// newIdempotencyFunnel builds a minimal funnel for testing — bypasses the
// full New() validation since these tests don't exercise Harness etc.
// idempotencyFile may be empty to disable persistence.
func newIdempotencyFunnel(t *testing.T, idempotencyFile string) *Funnel {
	t.Helper()
	cfg := Config{
		AspectID:        "test-aspect",
		IdempotencyFile: idempotencyFile,
		IdempotencyCap:  1000,
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	f := &Funnel{
		cfg:        cfg,
		log:        cfg.Logger,
		seenMsgIDs: make(map[int64]struct{}),
	}
	if err := f.loadSeenMsgIDs(); err != nil {
		t.Fatalf("loadSeenMsgIDs: %v", err)
	}
	return f
}
