// Package handqueue implements the dispatcher's worker-execution queue
// per hand-dispatch v0.1 §2-§3: a fairness-scheduled FIFO queue with
// per-aspect active-worker tracking, soft-cap N, hard-ceiling H, and
// spillover for idle aspects.
//
// The package name is legacy (per spec §9 directory-rename amnesty);
// identifiers + types inside use the generic protocol vocabulary
// (dispatch / worker) rather than the deployment-specific surface
// vocabulary (hand / summon).
//
// Model:
//   - One in-process FIFO list of pending dispatches.
//   - Per-aspect set of active worker IDs (map[aspect]map[workerID]struct{}).
//   - On arrival:
//     - if total active workers < SoftCap N → spawn immediately.
//     - else if calling aspect has zero active workers → spillover spawn
//       (still bounded by HardCeiling H).
//     - else → enqueue at FIFO tail.
//     - if at H regardless → reject ErrHardCeiling.
//   - On worker release:
//     - if queue empty → nothing.
//     - else scan queue head-first preferring an item from an idle
//       aspect (no active workers); else FIFO head. Spawn for picked.
//
// v1 scope: single-host execution.
package handqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nexus-cw/nexus/nexus/frames"
)

// Executor runs a single dispatch and returns the result payload.
// Implementations may take arbitrarily long; the Queue applies the
// per-dispatch deadline via ctx and kills the worker on expiry by
// cancelling ctx (subprocess executors should honor it via
// exec.CommandContext).
type Executor interface {
	Execute(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error)
}

// ExecutorFunc adapts a plain function to the Executor interface.
type ExecutorFunc func(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error)

// Execute implements Executor.
func (f ExecutorFunc) Execute(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
	return f(ctx, req)
}

// Default deadlines per spec §5.5.
const (
	DefaultDeadline = 30 * time.Minute
	MaxDeadline     = 2 * time.Hour
)

// Config tunes the queue.
type Config struct {
	// MaxConcurrent is the soft cap N — steady-state max-concurrent
	// worker count. Default 3 per spec §2.1. Spillover may push the
	// active count past N up to HardCeiling H for idle aspects.
	MaxConcurrent int

	// HardCeiling is the absolute cap H. Past H, dispatch arrivals
	// reject with ErrHardCeiling. If unset (or < MaxConcurrent),
	// defaults to MaxConcurrent + 1 (a defensive minimum); production
	// callers should pass roster_size + 1 per spec §2.1.
	HardCeiling int

	// DefaultDeadline overrides the spec default of 30 minutes per
	// dispatch when set; use 0 for the spec default.
	DefaultDeadline time.Duration

	// MaxDeadline overrides the spec hard maximum of 2 hours; use 0
	// for the spec default. Caller-supplied deadline_secs above this
	// is silently capped, not errored.
	MaxDeadline time.Duration

	// MaxQueueDepth caps the number of dispatches in the pending FIFO
	// (#25). Pre-cap, q.pending was unbounded — an authenticated peer
	// flooding dispatches faster than workers drained could grow the
	// queue (and the goroutines waiting on Submit) without limit. New
	// arrivals past this depth reject with ErrQueueFull. Default
	// from defaultMaxQueueDepth when zero.
	MaxQueueDepth int

	// Executor runs jobs. Required.
	Executor Executor

	// Logger is optional.
	Logger *slog.Logger

	// Now is the time source; tests inject deterministic clocks.
	// Default time.Now.
	Now func() time.Time
}

// Queue is the fairness-scheduled FIFO dispatcher.
type Queue struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu sync.Mutex
	// pending is the FIFO list of waiting dispatches.
	pending []*pendingItem
	// activeByAspect maps aspect → set of active worker IDs for that
	// aspect. An aspect with an empty (or absent) set is "idle" for
	// fairness purposes.
	activeByAspect map[string]map[string]struct{}
	// activeCount is the global count of currently-running workers
	// across all aspects (sum of |activeByAspect[*]|). Maintained
	// alongside the per-aspect map for O(1) cap checks.
	activeCount int
	// nextWorkerSeq is the monotonic worker-ID generator within this
	// dispatcher process. Combined with the start timestamp it makes
	// audit logs tractable.
	nextWorkerSeq uint64

	// shutdown
	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	wg           sync.WaitGroup
}

// pendingItem is one waiting dispatch.
type pendingItem struct {
	req         frames.DispatchPayload
	respCh      chan jobResult
	submittedAt time.Time
}

// jobResult is what runs the Submit side. Buffered (capacity 1) so the
// worker can post without ever blocking even after Submit's caller has
// timed out and walked away.
type jobResult struct {
	payload frames.DispatchResultPayload
	err     error
}

// Errors returned through Submit / via DispatchErrorPayload at the
// broker layer.
var (
	// ErrQueueShutdown is returned by Submit if Shutdown has been called.
	ErrQueueShutdown = errors.New("handqueue: shutdown")
	// ErrHardCeiling is returned when a dispatch arrives while the
	// dispatcher is at HardCeiling H. Per spec §6.3 the broker maps
	// this to a structured DispatchErrorPayload with code=hard_ceiling.
	ErrHardCeiling = errors.New("handqueue: hard_ceiling")
	// ErrQueueFull is returned when a dispatch arrives while the
	// pending FIFO already has MaxQueueDepth entries (#25). Caller
	// should backpressure or surface to the dispatching aspect.
	ErrQueueFull = errors.New("handqueue: queue_full")
)

// HardCeilingError carries the structured fields the broker needs to
// build a §6.3 DispatchErrorPayload (active / soft_cap / limit).
// errors.As-friendly.
type HardCeilingError struct {
	Active  int
	SoftCap int
	Limit   int
}

// Error implements error.
func (e *HardCeilingError) Error() string {
	return fmt.Sprintf("handqueue: hard_ceiling reached (active=%d soft_cap=%d limit=%d)", e.Active, e.SoftCap, e.Limit)
}

// Is so errors.Is(err, ErrHardCeiling) works.
func (e *HardCeilingError) Is(target error) bool {
	return target == ErrHardCeiling
}

// QueueFullError carries the structured fields for the §6.3
// dispatch error payload when MaxQueueDepth is reached.
type QueueFullError struct {
	Depth int
	Limit int
}

// Error implements error.
func (e *QueueFullError) Error() string {
	return fmt.Sprintf("handqueue: queue_full (depth=%d limit=%d)", e.Depth, e.Limit)
}

// Is so errors.Is(err, ErrQueueFull) works.
func (e *QueueFullError) Is(target error) bool {
	return target == ErrQueueFull
}

// New constructs a Queue.
func New(cfg Config) (*Queue, error) {
	if cfg.Executor == nil {
		return nil, errors.New("handqueue: Executor required")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}
	if cfg.HardCeiling < cfg.MaxConcurrent {
		cfg.HardCeiling = cfg.MaxConcurrent + 1
	}
	if cfg.MaxQueueDepth <= 0 {
		cfg.MaxQueueDepth = 256 // generous default; tunable per deployment
	}
	if cfg.DefaultDeadline <= 0 {
		cfg.DefaultDeadline = DefaultDeadline
	}
	if cfg.MaxDeadline <= 0 {
		cfg.MaxDeadline = MaxDeadline
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	q := &Queue{
		cfg:            cfg,
		log:            cfg.Logger,
		now:            cfg.Now,
		activeByAspect: make(map[string]map[string]struct{}),
		shutdownCh:     make(chan struct{}),
	}
	return q, nil
}

// Submit submits a dispatch and blocks until the executor returns, the
// queue shuts down, or ctx cancels. Per fairness rules:
//   - Spawns immediately if total active < N.
//   - Spawns spillover if calling aspect has no active worker, up to H.
//   - Else enqueues at FIFO tail.
//   - Rejects with *HardCeilingError if at H.
func (q *Queue) Submit(ctx context.Context, req frames.DispatchPayload) (frames.DispatchResultPayload, error) {
	respCh := make(chan jobResult, 1)
	item := &pendingItem{req: req, respCh: respCh, submittedAt: q.now()}

	q.mu.Lock()
	select {
	case <-q.shutdownCh:
		q.mu.Unlock()
		return frames.DispatchResultPayload{}, ErrQueueShutdown
	default:
	}

	aspect := req.Aspect
	aspectActive := len(q.activeByAspect[aspect])

	switch {
	case q.activeCount < q.cfg.MaxConcurrent:
		// Under soft cap → immediate spawn.
		q.spawnLocked(item)
		q.mu.Unlock()
	case aspectActive == 0 && q.activeCount < q.cfg.HardCeiling:
		// Spillover: idle aspect; allow above N but under H.
		q.spawnLocked(item)
		q.mu.Unlock()
	case aspectActive == 0 && q.activeCount >= q.cfg.HardCeiling:
		// At hard ceiling AND aspect is idle: this arrival would have
		// been spillover but there's no headroom. Reject per §6.3 —
		// the aspect can't enqueue (per §3 "the aspect never enqueues
		// if they have nothing in flight"; spillover is the only path
		// for an idle aspect, and it's blocked by H).
		err := &HardCeilingError{
			Active:  q.activeCount,
			SoftCap: q.cfg.MaxConcurrent,
			Limit:   q.cfg.HardCeiling,
		}
		q.mu.Unlock()
		q.log.Warn("dispatch rejected: hard_ceiling",
			"aspect", aspect,
			"dispatch_id", req.DispatchID,
			"active", err.Active,
			"limit", err.Limit)
		return frames.DispatchResultPayload{}, err
	default:
		// Aspect already has active worker and pool is at/above soft
		// cap: enqueue tail, after the depth cap check (#25).
		if len(q.pending) >= q.cfg.MaxQueueDepth {
			err := &QueueFullError{
				Depth: len(q.pending),
				Limit: q.cfg.MaxQueueDepth,
			}
			q.mu.Unlock()
			q.log.Warn("dispatch rejected: queue_full",
				"aspect", aspect,
				"dispatch_id", req.DispatchID,
				"depth", err.Depth,
				"limit", err.Limit)
			return frames.DispatchResultPayload{}, err
		}
		q.pending = append(q.pending, item)
		q.log.Debug("dispatch enqueued",
			"aspect", aspect,
			"dispatch_id", req.DispatchID,
			"queue_depth", len(q.pending),
			"active", q.activeCount)
		q.mu.Unlock()
	}

	// Wait for completion or caller cancel. The respCh is buffered, so
	// even if the caller walks away, the worker post never blocks.
	select {
	case res := <-respCh:
		return res.payload, res.err
	case <-ctx.Done():
		return frames.DispatchResultPayload{}, ctx.Err()
	}
}

// spawnLocked picks a fresh worker ID, records the dispatch as active
// for its aspect, and launches a goroutine to run the executor.
// Caller MUST hold q.mu.
func (q *Queue) spawnLocked(item *pendingItem) {
	q.nextWorkerSeq++
	workerID := fmt.Sprintf("w-%d-%d", q.now().UnixNano(), q.nextWorkerSeq)
	aspect := item.req.Aspect
	bucket, ok := q.activeByAspect[aspect]
	if !ok {
		bucket = make(map[string]struct{})
		q.activeByAspect[aspect] = bucket
	}
	bucket[workerID] = struct{}{}
	q.activeCount++

	deadline := q.resolveDeadline(item.req)
	q.log.Debug("dispatch spawn",
		"aspect", aspect,
		"dispatch_id", item.req.DispatchID,
		"worker_id", workerID,
		"deadline_s", int(deadline.Seconds()),
		"active", q.activeCount,
		"soft_cap", q.cfg.MaxConcurrent,
		"limit", q.cfg.HardCeiling)

	q.wg.Add(1)
	go q.runWorker(workerID, item, deadline)
}

// resolveDeadline picks the per-dispatch timeout per spec §5.5:
//   - If payload.deadline_secs absent or <=0 → DefaultDeadline.
//   - Else min(payload.deadline_secs, MaxDeadline) — silent cap.
func (q *Queue) resolveDeadline(req frames.DispatchPayload) time.Duration {
	if req.Payload == nil {
		return q.cfg.DefaultDeadline
	}
	raw, ok := req.Payload["deadline_secs"]
	if !ok {
		return q.cfg.DefaultDeadline
	}
	var secs float64
	switch v := raw.(type) {
	case float64:
		secs = v
	case int:
		secs = float64(v)
	case int64:
		secs = float64(v)
	default:
		return q.cfg.DefaultDeadline
	}
	if secs <= 0 {
		return q.cfg.DefaultDeadline
	}
	d := time.Duration(secs * float64(time.Second))
	if d > q.cfg.MaxDeadline {
		d = q.cfg.MaxDeadline
	}
	return d
}

// runWorker is the goroutine that actually invokes the Executor. On
// return — whether normal completion, error, or context-cancelled
// timeout — it releases the slot and runs the fairness scan to advance
// the queue.
//
// The worker context is independent of any caller context: even if the
// Submit caller walks away, the worker keeps running until completion
// or its own deadline. This matches spec §6.4 (caller cancellation
// does not abort the dispatch — only Frame's `kill_worker` override
// or the dispatcher timeout does).
func (q *Queue) runWorker(workerID string, item *pendingItem, deadline time.Duration) {
	defer q.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	payload, err := q.cfg.Executor.Execute(ctx, item.req)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if timedOut {
		q.log.Warn("dispatcher timeout fired",
			"aspect", item.req.Aspect,
			"dispatch_id", item.req.DispatchID,
			"worker_id", workerID,
			"deadline_s", int(deadline.Seconds()))
		// Per spec §5.5: dispatcher kills worker (already happens via
		// ctx cancel through exec.CommandContext) and posts a
		// `timeout` notice. We surface a structured DispatchResult
		// with Error=timeout so the broker forwards it on the
		// originating thread. This replaces whatever partial result
		// the executor returned.
		payload = frames.DispatchResultPayload{
			Aspect:     item.req.Aspect,
			Thread:     item.req.Thread,
			DispatchID: item.req.DispatchID,
			Error:      fmt.Sprintf("timeout: dispatch exceeded deadline of %ds", int(deadline.Seconds())),
		}
		// Surface as a non-nil err so the broker wraps as
		// dispatch.error rather than dispatch.result. The broker
		// already maps queue errors → dispatch.error in ws.go.
		err = fmt.Errorf("dispatcher timeout after %s", deadline)
	}

	item.respCh <- jobResult{payload: payload, err: err}

	// Release slot + fairness scan.
	q.mu.Lock()
	q.releaseLocked(item.req.Aspect, workerID)
	q.advanceLocked()
	q.mu.Unlock()
}

// releaseLocked removes a worker from the active set. Caller MUST hold q.mu.
func (q *Queue) releaseLocked(aspect, workerID string) {
	if bucket, ok := q.activeByAspect[aspect]; ok {
		if _, exists := bucket[workerID]; exists {
			delete(bucket, workerID)
			q.activeCount--
		}
		if len(bucket) == 0 {
			delete(q.activeByAspect, aspect)
		}
	}
}

// advanceLocked runs the fairness scan after a worker release per
// spec §3: pick the first queued item whose aspect has no active
// workers; else the FIFO head. Spawn ONE worker for it.
//
// One-per-release is intentional: each release frees exactly one
// slot, so we spawn exactly one in its place. Spawning more would
// re-introduce the spike above N that the soft cap exists to prevent.
//
// Caller MUST hold q.mu.
func (q *Queue) advanceLocked() {
	if len(q.pending) == 0 {
		return
	}
	if q.activeCount >= q.cfg.HardCeiling {
		return
	}
	idx := q.pickQueueIndexLocked()
	picked := q.pending[idx]
	q.pending = append(q.pending[:idx], q.pending[idx+1:]...)
	q.spawnLocked(picked)
}

// pickQueueIndexLocked returns the index of the next item to dispatch
// per the fairness rule. Caller MUST hold q.mu and have len(q.pending) > 0.
func (q *Queue) pickQueueIndexLocked() int {
	for i, item := range q.pending {
		if len(q.activeByAspect[item.req.Aspect]) == 0 {
			return i
		}
	}
	return 0
}

// Stats returns a snapshot of dispatcher state. Used by Frame and
// tests to inspect the pool without poking internals.
type Stats struct {
	ActiveTotal   int
	SoftCap       int
	HardCeiling   int
	QueueDepth    int
	ActiveByAspect map[string]int
}

// Stats returns a snapshot of current dispatcher state.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	byAspect := make(map[string]int, len(q.activeByAspect))
	for a, set := range q.activeByAspect {
		byAspect[a] = len(set)
	}
	return Stats{
		ActiveTotal:    q.activeCount,
		SoftCap:        q.cfg.MaxConcurrent,
		HardCeiling:    q.cfg.HardCeiling,
		QueueDepth:     len(q.pending),
		ActiveByAspect: byAspect,
	}
}

// Shutdown stops accepting new dispatches and waits for in-flight
// workers to finish. Pending items still in the FIFO queue have their
// respCh closed with ErrQueueShutdown so blocked Submit callers
// unblock.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.shutdownOnce.Do(func() {
		q.mu.Lock()
		close(q.shutdownCh)
		// Drain pending: every queued Submit gets an error response so
		// it unblocks instead of waiting forever.
		for _, item := range q.pending {
			item.respCh <- jobResult{err: ErrQueueShutdown}
		}
		q.pending = nil
		q.mu.Unlock()
	})
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
