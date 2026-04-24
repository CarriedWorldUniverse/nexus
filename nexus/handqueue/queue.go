// Package handqueue implements the dispatcher's hand-execution
// queue: FIFO job pool with a max-concurrency cap. Jobs execute by
// spawning a harness subprocess in hand mode. Per transport spec §6.
//
// v1 scope: single-host execution. The dispatcher that owns this
// queue runs on the same machine as the target aspect's home. Cross-
// host routing (send hand.dispatch to a remote Outpost, spawn there)
// arrives when the Outpost gains its own queue in a later iteration.
package handqueue

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/nexus-cw/nexus/nexus/frames"
)

// Executor runs a single hand job and returns the result payload.
// The real implementation spawns a harness subprocess; tests inject
// a mock that returns canned results.
type Executor interface {
	Execute(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error)
}

// ExecutorFunc adapts a plain function to the Executor interface.
type ExecutorFunc func(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error)

// Execute implements Executor.
func (f ExecutorFunc) Execute(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error) {
	return f(ctx, req)
}

// Config tunes the queue.
type Config struct {
	// MaxConcurrent caps how many harness subprocesses run at once.
	// Default 5.
	MaxConcurrent int

	// Executor runs jobs. Required.
	Executor Executor

	// Logger is optional.
	Logger *slog.Logger
}

// Queue is the FIFO job queue with bounded concurrency.
type Queue struct {
	cfg  Config
	log  *slog.Logger
	jobs chan job
	wg   sync.WaitGroup

	// Shutdown sync. Closed once to stop workers after drain.
	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

// job bundles a dispatch request with the response channel callers
// block on.
type job struct {
	req    frames.HandDispatchPayload
	respCh chan jobResult
}

// jobResult is what Run delivers back to Submit's caller.
type jobResult struct {
	payload frames.HandResultPayload
	err     error
}

// ErrQueueShutdown is returned by Submit if Shutdown has been called.
var ErrQueueShutdown = errors.New("handqueue: shutdown")

// New constructs a Queue.
func New(cfg Config) (*Queue, error) {
	if cfg.Executor == nil {
		return nil, errors.New("handqueue: Executor required")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 5
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	q := &Queue{
		cfg:        cfg,
		log:        cfg.Logger,
		jobs:       make(chan job, 64), // bounded inbound buffer
		shutdownCh: make(chan struct{}),
	}
	// Spawn the worker pool.
	for i := 0; i < cfg.MaxConcurrent; i++ {
		q.wg.Add(1)
		go q.worker()
	}
	return q, nil
}

// Submit enqueues a job and blocks until the executor returns or the
// queue shuts down. ctx bounds the wait.
func (q *Queue) Submit(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error) {
	respCh := make(chan jobResult, 1)
	j := job{req: req, respCh: respCh}

	select {
	case <-q.shutdownCh:
		return frames.HandResultPayload{}, ErrQueueShutdown
	case q.jobs <- j:
	case <-ctx.Done():
		return frames.HandResultPayload{}, ctx.Err()
	}

	select {
	case res := <-respCh:
		return res.payload, res.err
	case <-ctx.Done():
		// Job is already queued and may run anyway; caller just
		// stops waiting. Worker discards the result when it comes
		// back (channel has buffer 1, no blocking).
		return frames.HandResultPayload{}, ctx.Err()
	}
}

// worker pulls jobs and executes them. Exits when jobs channel is
// closed (Shutdown).
func (q *Queue) worker() {
	defer q.wg.Done()
	for j := range q.jobs {
		// Each job runs with its own bounded ctx. 10 minutes is
		// generous for a provider call.
		ctx, cancel := context.WithTimeout(context.Background(), 10*60*1e9) // 10 min
		payload, err := q.cfg.Executor.Execute(ctx, j.req)
		cancel()
		j.respCh <- jobResult{payload: payload, err: err}
	}
}

// Shutdown stops accepting new jobs, waits for in-flight ones, and
// returns.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.shutdownOnce.Do(func() {
		close(q.shutdownCh)
		close(q.jobs)
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
