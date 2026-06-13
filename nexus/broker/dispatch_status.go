package broker

import (
	"context"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
)

func (c *wsConn) handleDispatchStatusFrame(env frames.Envelope) {
	if c.registeredAs == "" {
		c.log.Warn("dispatch.status from unregistered connection dropped")
		return
	}
	store := c.broker.cfg.RunsStore
	if store == nil {
		return
	}
	var p frames.DispatchStatusPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("dispatch.status payload malformed", "err", err, "from", c.registeredAs)
		return
	}
	p.RunID = strings.TrimSpace(p.RunID)
	if p.RunID == "" {
		c.log.Warn("dispatch.status missing run_id", "from", c.registeredAs)
		return
	}
	at := p.At
	if at.IsZero() {
		at = env.TS
	}
	if at.IsZero() {
		at = time.Now()
	}
	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	run, err := store.Get(ctx, p.RunID)
	if err != nil {
		c.log.Warn("dispatch.status run lookup failed", "run_id", p.RunID, "from", c.registeredAs, "err", err)
		return
	}
	if run.Agent != "" && run.Agent != c.registeredAs {
		c.log.Warn("dispatch.status agent mismatch",
			"run_id", p.RunID,
			"run_agent", run.Agent,
			"from", c.registeredAs)
		return
	}

	switch p.Status {
	case "accepted":
		if err := store.MarkAccepted(ctx, p.RunID, at); err != nil {
			c.log.Warn("dispatch.status accepted record failed", "run_id", p.RunID, "err", err)
			return
		}
		c.emitRunUpdate(ctx, p.RunID)
	case "failed":
		preAccepted := run.Status == runs.StatusSubmitted || run.Status == runs.StatusQueued || run.Status == runs.StatusRunning
		if err := store.MarkDone(ctx, p.RunID, runs.StatusFailed, at, run.PRURL, run.DurationSecs); err != nil {
			c.log.Warn("dispatch.status failed record failed", "run_id", p.RunID, "err", err)
			return
		}
		c.emitRunUpdate(ctx, p.RunID)
		if preAccepted {
			c.log.Error("dispatch: ESCALATION run failed pre-acceptance",
				"run_id", p.RunID,
				"ticket", run.Ticket,
				"agent", run.Agent,
				"reason", p.Reason)
		}
	default:
		c.log.Warn("dispatch.status unknown status", "run_id", p.RunID, "status", p.Status, "from", c.registeredAs)
	}
}

func (c *wsConn) emitRunUpdate(ctx context.Context, runID string) {
	if c.broker == nil || c.broker.cfg.RunsStore == nil {
		return
	}
	if r, err := c.broker.cfg.RunsStore.Get(ctx, runID); err == nil {
		c.broker.broadcastRunsUpdate(r)
	}
}
