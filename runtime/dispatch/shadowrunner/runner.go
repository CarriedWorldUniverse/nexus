package shadowrunner

import (
	"context"
	"log/slog"
	"time"
)

// DrainFunc runs one drain (in production: invoke `claude -p <orchestrate>`).
// Returning an error is logged but NOT fatal — the next heartbeat re-derives
// ledger truth (level-triggered), so a failed/partial drain self-heals.
type DrainFunc func(context.Context) error

// Config configures the Runner. Heartbeat is the unconditional resync drain
// interval (the level-triggered backstop); zero defaults to 12m.
type Config struct {
	Heartbeat time.Duration
	Log       *slog.Logger
}

// Runner is the croft-resident loop: a heartbeat ticker + coalescing workqueue
// driving a stateless drain. It holds no work state (only the workqueue's
// in-flight/pending bit); all work-state lives in ledger.
type Runner struct {
	cfg   Config
	q     *Workqueue
	drain DrainFunc
	wake  chan struct{} // external triggers (event source, Runner v2)
}

func New(cfg Config, drain DrainFunc) *Runner {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 12 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Runner{cfg: cfg, q: NewWorkqueue(), drain: drain, wake: make(chan struct{}, 1)}
}

// Trigger pokes the loop from an external event source (non-blocking).
func (r *Runner) Trigger() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run drives the loop until ctx is cancelled. Drains run synchronously on this
// goroutine; coalescing guarantees at most one in flight.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Heartbeat)
	defer t.Stop()
	r.cfg.Log.Info("shadow-runner: started", "heartbeat", r.cfg.Heartbeat)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.onTrigger(ctx)
		case <-r.wake:
			r.onTrigger(ctx)
		}
	}
}

// onTrigger applies coalescing: only the idle->running transition runs a drain
// (synchronously); triggers during a drain set the pending bit and this same
// goroutine re-drains on completion until pending clears.
func (r *Runner) onTrigger(ctx context.Context) {
	if !r.q.Trigger() {
		return // a drain is already running; pending bit set
	}
	for {
		if err := r.drain(ctx); err != nil {
			r.cfg.Log.Error("shadow-runner: drain error (will resync next wake)", "err", err)
		}
		if !r.q.Done() {
			return
		}
		r.cfg.Log.Info("shadow-runner: pending trigger — re-draining")
	}
}
