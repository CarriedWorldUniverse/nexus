package funnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// markSeenLocked adds id to the seen-set, advancing the FIFO eviction
// queue if the cap is reached. Caller MUST hold f.mu. Persistence to
// disk is fired AFTER the lock is released — markSeenLocked snapshots
// the order slice and queues a write via the persistence path.
//
// No-op when the id is already tracked (idempotent re-marks don't
// disturb FIFO order, matching "first-seen wins" semantics).
//
// NEX-96 — broker delivers at-least-once per Lock 6; the seen-set
// is the funnel-side idempotency record that turns this into
// effectively exactly-once for the deliberation path.
func (f *Funnel) markSeenLocked(id int64) {
	if id <= 0 {
		return
	}
	if _, exists := f.seenMsgIDs[id]; exists {
		return
	}
	f.seenMsgIDs[id] = struct{}{}
	f.seenOrder = append(f.seenOrder, id)
	cap := f.cfg.IdempotencyCap
	if cap <= 0 {
		cap = 1000
	}
	for len(f.seenOrder) > cap {
		evict := f.seenOrder[0]
		f.seenOrder = f.seenOrder[1:]
		delete(f.seenMsgIDs, evict)
	}
	// Snapshot the slice for persistence outside the lock. Caller
	// (Deliberate) drops f.mu shortly after this returns; we kick a
	// goroutine that persists the snapshot independent of the
	// calling goroutine so disk I/O doesn't block subsequent
	// Receive / Deliberate calls.
	if f.cfg.IdempotencyFile == "" {
		return
	}
	snapshot := append([]int64(nil), f.seenOrder...)
	go f.persistSnapshot(snapshot)
}

// loadSeenMsgIDs hydrates the seen-set from disk on funnel startup.
// Called by New under construction; not goroutine-safe (single caller
// before any Receive can land). Empty cfg.IdempotencyFile → no-op;
// missing file → no-op (cold start). Parse errors are returned so
// the caller can decide whether to log + continue or abort.
//
// On-disk format: a JSON array of int64 msg_ids, FIFO order (oldest
// first). The persisted slice always reflects the post-eviction
// state — restart respects the same FIFO cap that runtime enforces.
func (f *Funnel) loadSeenMsgIDs() error {
	if f.cfg.IdempotencyFile == "" {
		return nil
	}
	buf, err := os.ReadFile(f.cfg.IdempotencyFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("funnel idempotency: read %s: %w", f.cfg.IdempotencyFile, err)
	}
	if len(buf) == 0 {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal(buf, &ids); err != nil {
		return fmt.Errorf("funnel idempotency: parse %s: %w", f.cfg.IdempotencyFile, err)
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := f.seenMsgIDs[id]; exists {
			continue
		}
		f.seenMsgIDs[id] = struct{}{}
		f.seenOrder = append(f.seenOrder, id)
	}
	return nil
}

// persistSnapshot writes a snapshot of the seen-set order to disk.
// Atomic via temp-file + rename so a crash mid-write can't leave a
// corrupt file. Failures are logged but never propagate — best-effort.
//
// Called by markSeenLocked in a goroutine (outside the funnel mutex)
// so disk I/O doesn't block the deliberation path. Multiple concurrent
// persists are tolerated — the latest write wins; intermediate writes
// may be overwritten but the on-disk state always reflects SOME valid
// past snapshot, never a torn slice.
func (f *Funnel) persistSnapshot(snapshot []int64) {
	if f.cfg.IdempotencyFile == "" {
		return
	}
	buf, err := json.Marshal(snapshot)
	if err != nil {
		f.log.Warn("funnel idempotency: marshal snapshot", "err", err)
		return
	}
	tmp := f.cfg.IdempotencyFile + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		f.log.Warn("funnel idempotency: write tmp", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, f.cfg.IdempotencyFile); err != nil {
		f.log.Warn("funnel idempotency: rename", "from", tmp, "to", f.cfg.IdempotencyFile, "err", err)
		// best-effort cleanup of the orphaned tmp file
		_ = os.Remove(tmp)
	}
}
