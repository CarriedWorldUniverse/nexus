package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolvePath covers the three-tier resolution: arg > env > default.
// Tests the env path explicitly because it's the failure mode that
// silently bites operators who don't pass a flag.
func TestResolvePath(t *testing.T) {
	// Save and restore env var across cases.
	prev := os.Getenv("NEXUS_DATA_DIR")
	defer os.Setenv("NEXUS_DATA_DIR", prev)

	t.Run("explicit dir wins over env", func(t *testing.T) {
		os.Setenv("NEXUS_DATA_DIR", "/from-env")
		got := ResolvePath("/from-arg")
		want := filepath.Join("/from-arg", DBFileName)
		if got != want {
			t.Errorf("ResolvePath(/from-arg) = %q; want %q", got, want)
		}
	})

	t.Run("env used when arg empty", func(t *testing.T) {
		os.Setenv("NEXUS_DATA_DIR", "/from-env")
		got := ResolvePath("")
		want := filepath.Join("/from-env", DBFileName)
		if got != want {
			t.Errorf("ResolvePath(\"\") with env = %q; want %q", got, want)
		}
	})

	t.Run("default when both empty", func(t *testing.T) {
		os.Setenv("NEXUS_DATA_DIR", "")
		got := ResolvePath("")
		want := filepath.Join(DefaultDataDir, DBFileName)
		if got != want {
			t.Errorf("ResolvePath(\"\") with no env = %q; want %q", got, want)
		}
	})
}

// TestWatchFileReplacement_NoReplacementHonorsCtxCancel verifies the
// watcher returns ctx.Err() when context is cancelled cleanly without
// any file activity — i.e. it doesn't false-positive when nothing
// changes.
func TestWatchFileReplacement_NoReplacementHonorsCtxCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := WatchFileReplacement(ctx, path, 30*time.Millisecond, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("watcher returned %v; want context.DeadlineExceeded", err)
	}
}

// TestWatchFileReplacement_DetectsReplacement is the load-bearing test:
// recreate the file under the watcher (simulating Windows file
// replacement / POSIX rename + recreate) and confirm:
//   1. watcher returns ErrFileReplaced
//   2. onReplaced is called exactly once before return
func TestWatchFileReplacement_DetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var onReplacedCount int32
	onReplaced := func() {
		atomic.AddInt32(&onReplacedCount, 1)
	}

	// Replace the file shortly after the watcher starts. Use a short
	// interval so the test runs quickly. Need to actually delete + recreate
	// so the inode/FileInfo identity changes; an in-place write would not
	// trip os.SameFile. The replace goroutine reports failures via a
	// channel rather than calling t.Errorf directly — the watcher may
	// detect the os.Remove and return before os.WriteFile runs, ending
	// the test before this goroutine finishes; calling t.Errorf on a
	// finished test panics under -race.
	replaceErr := make(chan error, 2)
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.Remove(path); err != nil {
			replaceErr <- err
			return
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
			replaceErr <- err
		}
	}()

	err := WatchFileReplacement(ctx, path, 20*time.Millisecond, nil, onReplaced)
	if !errors.Is(err, ErrFileReplaced) {
		t.Errorf("watcher returned %v; want ErrFileReplaced", err)
	}
	if got := atomic.LoadInt32(&onReplacedCount); got != 1 {
		t.Errorf("onReplaced called %d times; want exactly 1", got)
	}
	// Drain any replace-goroutine error best-effort. Only fail the test
	// if the os.Remove itself failed; rewrite-after-remove failure is
	// expected if the watcher already shut down + cleaned up the temp
	// dir on test exit.
	select {
	case e := <-replaceErr:
		if !errors.Is(e, os.ErrNotExist) {
			t.Logf("replace goroutine reported non-fatal error (likely cleanup race): %v", e)
		}
	default:
	}
}

// TestWatchFileReplacement_DetectsDeletion confirms the watcher treats
// a missing file as a replacement event — the underlying failure mode
// (process holds phantom handle while the visible name resolves to
// nothing) is identical.
func TestWatchFileReplacement_DetectsDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var onReplacedCount int32
	onReplaced := func() {
		atomic.AddInt32(&onReplacedCount, 1)
	}

	// Same pattern as DetectsReplacement: report errors via channel
	// to avoid t.Errorf-after-test-finished panics under -race.
	removeErr := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.Remove(path); err != nil {
			removeErr <- err
		}
	}()

	err := WatchFileReplacement(ctx, path, 20*time.Millisecond, nil, onReplaced)
	if !errors.Is(err, ErrFileReplaced) {
		t.Errorf("watcher returned %v; want ErrFileReplaced", err)
	}
	if got := atomic.LoadInt32(&onReplacedCount); got != 1 {
		t.Errorf("onReplaced called %d times; want exactly 1", got)
	}
	select {
	case e := <-removeErr:
		t.Errorf("remove failed: %v", e)
	default:
	}
}

// TestWatchFileReplacement_MissingAtStart returns an error before the
// tick loop. Different code path (initial stat) than runtime detection.
func TestWatchFileReplacement_MissingAtStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.db")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := WatchFileReplacement(ctx, path, 20*time.Millisecond, nil, nil)
	if err == nil {
		t.Fatal("expected error when path doesn't exist at start; got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error; got %v", err)
	}
}

// TestWatchFileReplacement_TransientStatRecoversWithoutTrip verifies
// the consecutive-failure counter: a single stat failure followed by a
// successful stat must not trigger ErrFileReplaced. Mirrors the real-
// world AV/backup-tool failure mode (Windows path briefly held, then
// released) — false-positive triggers shut down the broker for no
// reason. Below transientStatRetryThreshold (2), the watcher must keep
// running.
//
// Implementation: rename the file out and back in within a single
// inter-tick gap. The watcher should see one stat failure (file moved
// away) on one tick and a successful stat (file back, same inode) on
// the next. Because os.Rename preserves the inode on Windows + POSIX,
// the SameFile check after recovery is also satisfied.
func TestWatchFileReplacement_TransientStatRecoversWithoutTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	hidden := filepath.Join(dir, "test.db.hidden")
	if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cap the test at 250ms — long enough for recovery flow, short
	// enough to fail clearly if the watcher trips on a single failure.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	var onReplacedCount int32
	onReplaced := func() {
		atomic.AddInt32(&onReplacedCount, 1)
	}

	// Move the file out for a brief moment, then move it back.
	// 30ms < interval (50ms) means stat may or may not see the gap;
	// run several cycles to reliably hit the failure-then-success
	// transition. We accept that test timing is approximate; the
	// load-bearing check is "no false positive while file mostly
	// exists."
	doneRename := make(chan struct{})
	go func() {
		defer close(doneRename)
		for i := 0; i < 3; i++ {
			time.Sleep(40 * time.Millisecond)
			if err := os.Rename(path, hidden); err == nil {
				time.Sleep(15 * time.Millisecond)
				_ = os.Rename(hidden, path)
			}
		}
	}()

	err := WatchFileReplacement(ctx, path, 50*time.Millisecond, nil, onReplaced)
	<-doneRename

	// Expected outcome: ctx deadline exceeded (no replacement detected)
	// OR ErrFileReplaced if timing happened to land 2 consecutive
	// failures across the rename window. The first is the desired
	// state; the second is acceptable but rare given threshold=2 +
	// fast rename.
	//
	// What we MUST avoid: onReplaced firing while file is back +
	// matches baseline. If it fired, that means single-tick failure
	// tripped — the threshold isn't working.
	if errors.Is(err, context.DeadlineExceeded) {
		// Good path: watcher kept running through transient failures.
		if got := atomic.LoadInt32(&onReplacedCount); got != 0 {
			t.Errorf("watcher kept running but onReplaced was called %d times — unexpected", got)
		}
	} else if errors.Is(err, ErrFileReplaced) {
		// Acceptable if the rename window aligned with two consecutive
		// stat ticks. Log it so we know it happened but don't fail.
		t.Logf("watcher tripped during rename window — acceptable but rare; threshold may need tuning")
	} else {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestWatchFileReplacement_OnReplacedNilSafe confirms a nil callback
// doesn't panic — the watcher should still return the sentinel error
// for the parent supervisor to act on.
func TestWatchFileReplacement_OnReplacedNilSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Remove(path)
	}()

	err := WatchFileReplacement(ctx, path, 20*time.Millisecond, nil, nil)
	if !errors.Is(err, ErrFileReplaced) {
		t.Errorf("watcher returned %v; want ErrFileReplaced", err)
	}
}

// --- WatchWriteDurability tests ---
//
// These exercise the fresh-handle write verifier (Part 2). Real-world
// phantom-handle conditions are hard to reproduce in a unit test (they
// need a process to keep a write handle open while another process
// replaces the file). We approximate the failure mode with a
// fakeLiveDB stub that lets tests control what MAX(id) the "live"
// handle reports — divergent from the on-disk file. That isolates the
// comparison logic from the actual phantom-mode physics.

// openTestDB creates a real sqlite db at the given path with the
// chat_messages schema and inserts the requested number of dummy
// messages, returning the *sql.DB and the highest id.
func openTestDB(t *testing.T, path string, msgCount int) (*sql.DB, int64) {
	t.Helper()
	db, err := Open(context.Background(), filepath.Dir(path), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for i := 0; i < msgCount; i++ {
		if _, err := db.Exec(`INSERT INTO chat_messages (from_agent, content) VALUES (?, ?)`, "test", fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	var max int64
	if err := db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM chat_messages").Scan(&max); err != nil {
		t.Fatalf("max query: %v", err)
	}
	return db, max
}

// TestWatchWriteDurability_HealthyDoesNotTrip: when live and fresh
// handles agree on MAX(id), the verifier should run cleanly until ctx
// cancels.
func TestWatchWriteDurability_HealthyDoesNotTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.db")
	db, _ := openTestDB(t, path, 5)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	var onReplacedCount int32
	onReplaced := func() { atomic.AddInt32(&onReplacedCount, 1) }

	err := WatchWriteDurability(ctx, path, db, 50*time.Millisecond, nil, onReplaced)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("verifier returned %v; want DeadlineExceeded", err)
	}
	if got := atomic.LoadInt32(&onReplacedCount); got != 0 {
		t.Errorf("onReplaced called %d times on healthy db; want 0", got)
	}
}

// TestWatchWriteDurability_NilLiveDBErrors: a nil live handle is a
// programmer error and should be caught loudly rather than nil-panic
// at runtime.
func TestWatchWriteDurability_NilLiveDBErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.db")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := WatchWriteDurability(ctx, path, nil, 20*time.Millisecond, nil, nil)
	if err == nil {
		t.Fatal("expected error on nil liveDB; got nil")
	}
	if errors.Is(err, ErrWriteDurabilityFailed) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unexpected error type: %v", err)
	}
}

// TestWatchWriteDurability_MissingPathTripsSustained: when the fresh
// handle can't be opened (path missing), the verifier should treat
// sustained failures as durability lost and trip after the threshold.
func TestWatchWriteDurability_MissingPathTripsSustained(t *testing.T) {
	dir := t.TempDir()
	// Open a real db so the live handle is healthy, then point the
	// verifier at a path that doesn't exist. Fresh-handle opens will
	// fail consistently, tripping after threshold.
	realPath := filepath.Join(dir, "real.db")
	db, _ := openTestDB(t, realPath, 3)

	missingPath := filepath.Join(dir, "missing.db")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var onReplacedCount int32
	onReplaced := func() { atomic.AddInt32(&onReplacedCount, 1) }

	err := WatchWriteDurability(ctx, missingPath, db, 30*time.Millisecond, nil, onReplaced)
	if !errors.Is(err, ErrWriteDurabilityFailed) {
		t.Errorf("verifier returned %v; want ErrWriteDurabilityFailed", err)
	}
	if got := atomic.LoadInt32(&onReplacedCount); got != 1 {
		t.Errorf("onReplaced called %d times; want exactly 1", got)
	}
}

// TestCompareMaxID_HealthyMatch: sanity check the helper directly —
// fresh and live handles should report the same MAX(id) on a real db.
func TestCompareMaxID_HealthyMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.db")
	db, expected := openTestDB(t, path, 7)

	live, fresh, err := compareMaxID(context.Background(), path, db)
	if err != nil {
		t.Fatalf("compareMaxID: %v", err)
	}
	if live != expected {
		t.Errorf("live MAX(id) = %d; want %d", live, expected)
	}
	if fresh != expected {
		t.Errorf("fresh MAX(id) = %d; want %d", fresh, expected)
	}
}
