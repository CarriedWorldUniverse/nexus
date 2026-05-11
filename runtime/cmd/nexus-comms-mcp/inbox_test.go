// Inbox unit tests for nexus-comms-mcp.
//
// These tests cover the buffer's reconnect-replay contract — the
// property that makes Part 3's design work end-to-end:
//
//   1. add() updates highestID monotonically — so on reconnect the
//      next register frame can request replay since the last seen msg.
//   2. drainAfter() removes items but does NOT reset highestID —
//      because read_chat clears the buffer but the watermark must
//      survive so reconnect still asks the broker only for newer rows.
//   3. seedHighest() lets the caller pre-seed the watermark from
//      out-of-band state (--since-msg-id flag, future persistence).
//   4. FIFO eviction at capacity, with the dropped counter surfaced
//      so callers know they fell behind.
//
// Why these properties matter: MCP is poll-on-read, not push. The
// model calls read_chat between turns, drains the buffer, but
// chat.deliver frames keep arriving from the broker in the background.
// If a WS drop happens while the buffer is empty, highest() being 0
// would re-replay history we already saw; highest() being correct
// means register asks the broker only for newer rows. The drain-
// preserves-highest invariant is the one that's easy to break and
// breaks the whole replay design when it does.

package main

import (
	"sync"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// TestInboxAdd_TracksHighestID asserts that add() walks highestID up
// monotonically, including the out-of-order arrival case (broker
// shouldn't, but defense-in-depth: any add with a lower ID must not
// regress the watermark).
func TestInboxAdd_TracksHighestID(t *testing.T) {
	b := newInboxBuffer(10)

	if got := b.highest(); got != 0 {
		t.Fatalf("fresh buffer highest = %d, want 0", got)
	}

	b.add(frames.ChatDeliverPayload{ID: 5, From: "anvil", Content: "first"})
	if got := b.highest(); got != 5 {
		t.Fatalf("after add(id=5) highest = %d, want 5", got)
	}

	b.add(frames.ChatDeliverPayload{ID: 7, From: "anvil", Content: "second"})
	if got := b.highest(); got != 7 {
		t.Fatalf("after add(id=7) highest = %d, want 7", got)
	}

	// Out-of-order — must NOT regress.
	b.add(frames.ChatDeliverPayload{ID: 3, From: "anvil", Content: "out of order"})
	if got := b.highest(); got != 7 {
		t.Fatalf("after add(id=3) highest = %d, want 7 (no regression)", got)
	}
}

// TestInboxDrainAfter_PreservesHighest is the load-bearing invariant
// for reconnect-replay. After draining, highest() still returns the
// last-observed msg_id so the next register-on-reconnect requests
// replay from there.
func TestInboxDrainAfter_PreservesHighest(t *testing.T) {
	b := newInboxBuffer(10)
	for _, id := range []int{10, 11, 12} {
		b.add(frames.ChatDeliverPayload{ID: id, From: "wren"})
	}

	beforeHigh := b.highest()
	if beforeHigh != 12 {
		t.Fatalf("before drain highest = %d, want 12", beforeHigh)
	}

	items, dropped := b.drainAfter(0)
	if len(items) != 3 {
		t.Fatalf("drainAfter(0) returned %d items, want 3", len(items))
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (no overflow)", dropped)
	}

	// THE invariant — without this, every reconnect re-replays history
	// we already saw.
	if got := b.highest(); got != 12 {
		t.Fatalf("after drain highest = %d, want 12 (drain must NOT reset watermark)", got)
	}

	// Buffer should be empty after drainAfter(0).
	again, _ := b.drainAfter(0)
	if len(again) != 0 {
		t.Fatalf("second drain returned %d items, want 0", len(again))
	}

	// And highest is still 12.
	if got := b.highest(); got != 12 {
		t.Fatalf("after empty drain highest = %d, want 12", got)
	}
}

// TestInboxDrainAfter_SinceFiltersAndKeeps verifies that drainAfter
// with a since_id cursor returns only newer items and leaves older
// ones in the buffer for a different caller / cursor.
func TestInboxDrainAfter_SinceFiltersAndKeeps(t *testing.T) {
	b := newInboxBuffer(10)
	for _, id := range []int{100, 101, 102, 103} {
		b.add(frames.ChatDeliverPayload{ID: id})
	}

	items, _ := b.drainAfter(101)
	if len(items) != 2 {
		t.Fatalf("drainAfter(101) returned %d items, want 2 (102, 103)", len(items))
	}
	for _, it := range items {
		if it.ID <= 101 {
			t.Errorf("drainAfter(101) returned id=%d (should be > 101)", it.ID)
		}
	}

	// 100 and 101 remain — verify with a second drainAfter(0).
	remaining, _ := b.drainAfter(0)
	if len(remaining) != 2 {
		t.Fatalf("after partial drain, remaining = %d, want 2", len(remaining))
	}
	for _, it := range remaining {
		if it.ID > 101 {
			t.Errorf("remaining item id=%d should have been drained earlier", it.ID)
		}
	}
}

// TestInboxAdd_EvictsOldestWhenFull asserts FIFO eviction with the
// dropped counter ticking up. Capacity 3 with 5 inserts → final
// contents are [3, 4, 5] and dropped == 2.
func TestInboxAdd_EvictsOldestWhenFull(t *testing.T) {
	b := newInboxBuffer(3)
	for _, id := range []int{1, 2, 3, 4, 5} {
		b.add(frames.ChatDeliverPayload{ID: id})
	}

	items, dropped, snapHigh := b.snapshot()
	if len(items) != 3 {
		t.Fatalf("buffer size = %d, want 3 (capacity)", len(items))
	}
	if dropped != 2 {
		t.Errorf("dropped counter = %d, want 2 (1 and 2 evicted)", dropped)
	}
	if snapHigh != 5 {
		t.Errorf("snapshot highest = %d, want 5", snapHigh)
	}
	wantIDs := []int{3, 4, 5}
	for i, it := range items {
		if it.ID != wantIDs[i] {
			t.Errorf("items[%d].ID = %d, want %d", i, it.ID, wantIDs[i])
		}
	}

	// Critical: even though items 1 and 2 were evicted, highest is
	// still 5 (the most recent add). And — eviction does NOT regress
	// highest below 5 even though we lost the lower-id rows.
	if got := b.highest(); got != 5 {
		t.Errorf("after eviction highest = %d, want 5", got)
	}
}

// TestInboxDrainAfter_ResetsDropped verifies that the dropped counter
// is reported once per drain call, then reset. Two-call sequence:
// first drain reports drop count, second drain reports 0 if no new
// drops happened in between.
func TestInboxDrainAfter_ResetsDropped(t *testing.T) {
	b := newInboxBuffer(2)
	// 3 adds at cap=2 → 1 dropped (id 10), buffer has [11, 12]
	for _, id := range []int{10, 11, 12} {
		b.add(frames.ChatDeliverPayload{ID: id})
	}

	_, drop1 := b.drainAfter(0)
	if drop1 != 1 {
		t.Errorf("first drain dropped = %d, want 1", drop1)
	}

	_, drop2 := b.drainAfter(0)
	if drop2 != 0 {
		t.Errorf("second drain (no new drops) dropped = %d, want 0", drop2)
	}
}

// TestInboxSeedHighest_DoesNotRegress asserts seedHighest only moves
// the watermark forward — a fresh process passing an old --since-msg-id
// can't accidentally lower the cursor below what's already been seen.
func TestInboxSeedHighest_DoesNotRegress(t *testing.T) {
	b := newInboxBuffer(10)
	b.add(frames.ChatDeliverPayload{ID: 50})
	if b.highest() != 50 {
		t.Fatalf("setup: highest = %d, want 50", b.highest())
	}

	// Lower seed must not regress.
	b.seedHighest(10)
	if got := b.highest(); got != 50 {
		t.Errorf("after seedHighest(10) highest = %d, want 50 (no regression)", got)
	}

	// Higher seed advances.
	b.seedHighest(200)
	if got := b.highest(); got != 200 {
		t.Errorf("after seedHighest(200) highest = %d, want 200", got)
	}
}

// TestInboxConcurrency_AddAndDrain runs adds and drains concurrently
// to surface any race the -race flag would catch. Not asserting
// ordering — just that the data structure stays internally consistent
// and the final highest matches the max id added.
//
// Useful because production has the WS read goroutine writing while
// MCP tool goroutines call drainAfter/snapshot. A torn slice or
// missed lock here would manifest as flaky tests.
func TestInboxConcurrency_AddAndDrain(t *testing.T) {
	b := newInboxBuffer(50)
	const N = 200

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: add ids 1..N as fast as possible.
	go func() {
		defer wg.Done()
		for i := 1; i <= N; i++ {
			b.add(frames.ChatDeliverPayload{ID: i})
		}
	}()

	// Reader: drain repeatedly while writes happen. Drained items
	// are discarded — we're only testing the data structure, not
	// delivery semantics.
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			b.drainAfter(0)
		}
	}()

	wg.Wait()

	// Whatever's left in the buffer plus what was drained should sum
	// to N items — but we discarded drained items, so we can't check
	// that. We CAN check that highest reached N (last add wins the
	// monotonic update).
	if got := b.highest(); got != N {
		t.Errorf("after concurrent add+drain highest = %d, want %d", got, N)
	}
}
