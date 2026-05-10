package rewriter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Runner is the funnel-facing wrapper around Rewriter. It tracks
// consecutive failures, implements busy-retry semantics for Windows
// rename collisions, and exposes a "should reset session?" signal
// that the funnel honors after sustained failures.
//
// Lifecycle: one Runner per Frame. The funnel calls AfterTurn between
// each provider turn. AfterTurn runs synchronously — distillation
// must complete before the next --resume, otherwise we'd race
// claude-code on the jsonl.
//
// Failure tracking:
//   - busy errors (ErrSessionFileBusy) are retried up to BusyRetries
//     times with BusyBackoff between attempts. They DON'T count
//     against ConsecutiveFailures since they reflect filesystem
//     contention, not distiller misbehavior.
//   - other errors (read/write/distiller) increment
//     ConsecutiveFailures. After ConsecutiveFailureThreshold reaches
//     occur in a row, ShouldResetSession returns true and the
//     funnel rotates to a fresh session id.
//   - any successful run zeroes ConsecutiveFailures.
type Runner struct {
	rw *Rewriter

	BusyRetries                int           // default 3
	BusyBackoff                time.Duration // default 250ms
	ConsecutiveFailureThreshold int          // default 3

	// distillFn is the function actually invoked by AfterTurn. Tests
	// override it with a deterministic in-memory shim; production
	// leaves it nil and falls through to rw.DistillTail.
	distillFn func(ctx context.Context) (Stats, error)

	mu                   sync.Mutex
	consecutiveFailures  int
	resetRequested       bool

	log *slog.Logger
}

// NewRunner returns a Runner wrapping the given Rewriter. Defaults:
// 3 busy retries × 250ms backoff, 3 consecutive failures → reset.
func NewRunner(rw *Rewriter, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		rw:                          rw,
		BusyRetries:                 3,
		BusyBackoff:                 250 * time.Millisecond,
		ConsecutiveFailureThreshold: 3,
		log:                         log,
	}
}

// AfterTurn satisfies funnel.PostTurnHook. The Stats are logged
// internally; callers that want them should use AfterTurnDetailed.
func (r *Runner) AfterTurn(ctx context.Context) {
	_ = r.AfterTurnDetailed(ctx)
}

// AfterTurnDetailed runs the distillation pass and returns the Stats
// for callers that want them (tests + future telemetry consumers).
// Errors are logged and folded into the failure counter; the funnel
// SHOULD continue regardless. Distillation failure is a degradation
// (heavier context next turn), not a fatal condition.
func (r *Runner) AfterTurnDetailed(ctx context.Context) Stats {
	if r == nil || r.rw == nil {
		return Stats{}
	}
	if r.shouldSkip() {
		// Reset already requested but not yet honored; no point
		// distilling a session we're about to discard.
		r.log.Debug("rewriter runner: skipping AfterTurn — reset pending")
		return Stats{}
	}
	if r.rw != nil {
		r.log.Info("rewriter: AfterTurn invoked", "session_path", r.rw.sessionPath())
	}

	distill := r.distillFn
	if distill == nil {
		distill = r.rw.DistillTail
	}
	var stats Stats
	var lastErr error
	for attempt := 0; attempt <= r.BusyRetries; attempt++ {
		s, err := distill(ctx)
		stats = s
		lastErr = err
		if err == nil {
			r.recordSuccess()
			r.log.Info("rewriter: AfterTurn ok",
				"scanned", s.RecordsScanned,
				"rewritten", s.RecordsRewritten,
				"skipped", s.RecordsSkipped,
				"distiller_errors", s.DistillerErrors,
				"bytes_before", s.BytesBefore,
				"bytes_after", s.BytesAfter,
			)
			return stats
		}
		if errors.Is(err, ErrSessionFileBusy) {
			r.log.Info("rewriter: session file busy, retrying",
				"attempt", attempt+1, "max", r.BusyRetries+1)
			select {
			case <-ctx.Done():
				return stats
			case <-time.After(r.BusyBackoff):
			}
			continue
		}
		// Non-busy error — don't retry, count toward threshold.
		break
	}

	// Sustained busy errors don't count toward the reset threshold:
	// the file being open is filesystem contention, not distiller
	// misbehavior. Log and skip — the next AfterTurn will try again.
	if errors.Is(lastErr, ErrSessionFileBusy) {
		r.log.Warn("rewriter: all busy retries exhausted; deferring to next turn (no failure counted)",
			"retries", r.BusyRetries+1)
		return stats
	}
	// Empty/unparseable session is normal on the very first turn —
	// treat ErrNoBoundary as a no-op rather than a failure so we
	// don't trip the reset threshold during cold start.
	if errors.Is(lastErr, ErrNoBoundary) {
		r.log.Debug("rewriter: session has no parseable records yet; nothing to distill")
		return stats
	}

	r.recordFailure(lastErr)
	return stats
}

// ShouldResetSession reports whether the funnel should rotate to a
// fresh session ID before the next turn. Returns true once
// ConsecutiveFailureThreshold non-busy failures have occurred in a
// row. The funnel is responsible for actually performing the
// rotation; once it does, it MUST call AcknowledgeReset to clear the
// flag.
func (r *Runner) ShouldResetSession() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resetRequested
}

// AcknowledgeReset clears the reset flag and zeroes the failure
// counter. Called by the funnel after it has rotated the session.
func (r *Runner) AcknowledgeReset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetRequested = false
	r.consecutiveFailures = 0
}

func (r *Runner) shouldSkip() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resetRequested
}

func (r *Runner) recordSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecutiveFailures = 0
}

func (r *Runner) recordFailure(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecutiveFailures++
	r.log.Warn("rewriter: AfterTurn failed",
		"err", err,
		"consecutive", r.consecutiveFailures,
		"threshold", r.ConsecutiveFailureThreshold)
	if r.consecutiveFailures >= r.ConsecutiveFailureThreshold {
		r.resetRequested = true
		r.log.Warn("rewriter: consecutive failures crossed threshold — requesting session reset",
			"failures", r.consecutiveFailures)
	}
}
