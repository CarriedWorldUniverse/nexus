// idle_reaper.go — scales quiet wake-on-mention aspects to zero
// (roundtable spec component 1, the scale-down half of napping presence;
// the scale-up half is wake.go).
//
// A wake-on-mention aspect is reaped when ALL of:
//   - no chat to/from it for IdleTimeout (lastChatActivity, stamped
//     inline in HandleChatSend — no ChatStore scans)
//   - no active dispatch run owned by it (RunsStore.ListRunning)
//   - no in-flight turn (the observability Grouper's open-turn state)
//
// Reap = ScaleDeployment(…, 0) + roster.SetNapping. always-on and
// dispatch-only aspects are never touched; so are aspects already
// napping. The sweep runs on a ticker (IdleTimeout/3) started in
// ListenAndServe, gated on the wake controller being configured —
// nothing is ever scaled to zero that chat can't scale back up.

package broker

import (
	"context"
	"log/slog"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// defaultIdleTimeout is how long a wake-on-mention aspect must be quiet
// before it's scaled to zero, when Config.IdleTimeout is unset.
const defaultIdleTimeout = 15 * time.Minute

type idleReaper struct {
	b       *Broker
	scaler  deploymentScaler
	timeout time.Duration
	now     func() time.Time // injectable clock for tests
	log     *slog.Logger
}

func newIdleReaper(b *Broker, scaler deploymentScaler, timeout time.Duration) *idleReaper {
	if timeout <= 0 {
		timeout = defaultIdleTimeout
	}
	return &idleReaper{b: b, scaler: scaler, timeout: timeout, now: time.Now, log: b.log}
}

// run sweeps until ctx cancels. Same shape as the stale-reap ticker in
// cmd/nexus; interval timeout/3 so an aspect naps at most ~1.33× the
// configured quiet window after its last activity.
func (ir *idleReaper) run(ctx context.Context) {
	t := time.NewTicker(ir.timeout / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ir.sweep(ctx)
		}
	}
}

// sweep evaluates every wake-on-mention aspect and scales the quiet ones
// to zero. One ListRunning per sweep, not per aspect.
func (ir *idleReaper) sweep(ctx context.Context) {
	now := ir.now()

	var running map[string]bool
	if ir.b.cfg.RunsStore != nil {
		rs, err := ir.b.cfg.RunsStore.ListRunning(ctx)
		if err != nil {
			// Can't verify the active-run guard — skip the whole sweep
			// rather than risk reaping an aspect mid-run.
			ir.log.Warn("idle reaper: ListRunning failed, sweep skipped", "err", err)
			return
		}
		running = make(map[string]bool, len(rs))
		for _, r := range rs {
			running[r.Agent] = true
		}
	}

	for aspect, policy := range ir.b.cfg.AspectWakePolicy {
		if policy != WakePolicyWakeOnMention {
			continue
		}
		if state, ok := ir.b.roster.Get(aspect); ok && state.Status == roster.StatusNapping {
			continue // already parked
		}
		last, ok := ir.b.lastChatTouch(aspect)
		if !ok {
			// No activity on record (e.g. fresh broker boot) — start the
			// idle clock now instead of reaping with zero evidence.
			ir.b.touchChatActivity(aspect, now)
			continue
		}
		if now.Sub(last) < ir.timeout {
			continue
		}
		if running[aspect] {
			continue
		}
		if ir.b.turnInFlight(aspect) {
			continue
		}

		deployment := ir.b.cfg.AspectDeployment[aspect]
		if deployment == "" {
			deployment = aspect
		}
		if err := ir.scaler.ScaleDeployment(ctx, deployment, 0); err != nil {
			ir.log.Warn("idle reaper: scale-down failed", "aspect", aspect, "deployment", deployment, "err", err)
			continue
		}
		ir.b.roster.SetNapping(aspect)
		ir.log.Info("idle reaper: aspect napping", "aspect", aspect, "deployment", deployment,
			"quiet_for", now.Sub(last).Round(time.Second))
	}
}

// touchChatActivity stamps the aspect's last chat touch. Called from
// HandleChatSend for the sender and every computed recipient, and by the
// reaper itself when it first sights an aspect with no record.
func (b *Broker) touchChatActivity(aspect string, at time.Time) {
	b.lastChatMu.Lock()
	defer b.lastChatMu.Unlock()
	b.lastChatActivity[aspect] = at
}

// lastChatTouch returns the most recent chat touch for the aspect, or
// false if none has been recorded since broker start.
func (b *Broker) lastChatTouch(aspect string) (time.Time, bool) {
	b.lastChatMu.Lock()
	defer b.lastChatMu.Unlock()
	at, ok := b.lastChatActivity[aspect]
	return at, ok
}

// turnInFlight reports whether the aspect has an open observe turn
// (observe.begin without observe.end) — the Grouper tracks that state
// already; GrouperFor lazily creates, and a fresh Grouper reports false.
func (b *Broker) turnInFlight(aspect string) bool {
	if b.observability == nil {
		return false
	}
	return b.observability.GrouperFor(aspect).TurnInFlight()
}
