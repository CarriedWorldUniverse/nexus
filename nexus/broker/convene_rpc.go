// convene_rpc.go — convene.close (facilitator/operator) and convenes.list
// (operator) frame handlers (roundtable spec component 3, P3 plan Task B).
//
// convene.close transitions an open convene to converged|abandoned. Only
// the facilitator (the aspect whose registered identity matches the
// convene's facilitator) or an operator connection may close — a
// participant cannot end the roundtable. The store's Close is idempotent
// (WHERE status='open'), so a re-fired close is harmless. Closing does
// NOTHING to the participants: the idle reaper naps them on the normal
// quiet timeout, the convene record just records the verdict.
//
// convenes.list is an operator watch surface mirroring runs.list.

package broker

import (
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/convene"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

func conveneToPayload(c convene.Convene) frames.ConvenePayload {
	p := frames.ConvenePayload{
		ConveneID:    c.ConveneID,
		RootMsgID:    c.RootMsgID,
		Facilitator:  c.Facilitator,
		Participants: c.Participants,
		Problem:      c.Problem,
		Status:       string(c.Status),
		CreatedAt:    c.CreatedAt.UnixMilli(),
		SummaryMsgID: c.SummaryMsgID,
	}
	if !c.ClosedAt.IsZero() {
		p.ClosedAt = c.ClosedAt.UnixMilli()
	}
	return p
}

// validCloseStatus maps the wire status string to a terminal convene
// status, rejecting anything that isn't converged|abandoned.
func validCloseStatus(s string) (convene.Status, bool) {
	switch convene.Status(s) {
	case convene.StatusConverged:
		return convene.StatusConverged, true
	case convene.StatusAbandoned:
		return convene.StatusAbandoned, true
	default:
		return "", false
	}
}

// NOTE: a broker-side round-cap/TTL auto-close for orphaned-open convenes
// (facilitator dies mid-convene, leaving the record open forever) is deferred
// to P4+. In v1 judging convergence and closing is the facilitating aspect's
// job; the broker does not reap convenes on a timer.
//
// closeConvene is the shared close path. caller is the authenticated
// identity (an aspect's registeredAs, or "operator"); isOperator relaxes
// the facilitator-only authz. Returns a result payload (never panics on a
// missing store/convene — it reports the failure in the payload).
func (b *Broker) closeConvene(c connLike, p frames.ConveneClosePayload, caller string, isOperator bool) frames.ConveneCloseResultPayload {
	store := b.cfg.ConveneStore
	if store == nil {
		return frames.ConveneCloseResultPayload{Message: "convene store not configured"}
	}
	if p.ConveneID == "" {
		return frames.ConveneCloseResultPayload{Message: "convene_id required"}
	}
	status, ok := validCloseStatus(p.Status)
	if !ok {
		return frames.ConveneCloseResultPayload{Message: "status must be converged or abandoned"}
	}

	ctx := b.ctxOrBackground()
	rec, err := store.Get(ctx, p.ConveneID)
	if err != nil || rec.ConveneID == "" {
		return frames.ConveneCloseResultPayload{Message: "convene not found"}
	}
	// Authz: only the facilitator (or an operator) may close.
	if !isOperator && caller != rec.Facilitator {
		b.log.Warn("convene.close rejected: not facilitator",
			"convene_id", p.ConveneID, "caller", caller, "facilitator", rec.Facilitator)
		return frames.ConveneCloseResultPayload{Message: "only the facilitator may close this convene"}
	}

	if err := store.Close(ctx, p.ConveneID, status, time.Now(), p.SummaryMsgID); err != nil {
		b.log.Warn("convene.close store error", "convene_id", p.ConveneID, "err", err)
		return frames.ConveneCloseResultPayload{Message: "close failed: " + err.Error()}
	}
	b.log.Info("convene closed", "convene_id", p.ConveneID, "status", status, "by", caller)
	// Re-read so the reported status reflects the idempotent store (a
	// double-close leaves the terminal state as-is).
	final, _ := store.Get(ctx, p.ConveneID)
	return frames.ConveneCloseResultPayload{OK: true, Status: string(final.Status)}
}

// connLike is the slice of *wsConn the convene RPC path needs for sending
// a response. Narrowed so the shared close path is unit-testable without a
// live WS connection.
type connLike interface {
	send(env frames.Envelope)
}

// handleConveneCloseFrame is the aspect-WS entry: the facilitator aspect
// closes its convene. Authz keys on the connection's registered identity.
func (c *wsConn) handleConveneCloseFrame(env frames.Envelope) {
	var p frames.ConveneClosePayload
	if err := frames.PayloadAs(env, &p); err != nil {
		resp, _ := frames.NewResponse(frames.KindConveneCloseResult, env.ID,
			frames.ConveneCloseResultPayload{Message: "malformed convene.close payload"})
		c.send(resp)
		return
	}
	result := c.broker.closeConvene(c, p, c.registeredAs, c.auth.Operator)
	resp, _ := frames.NewResponse(frames.KindConveneCloseResult, env.ID, result)
	c.send(resp)
}

// handleOperatorConveneClose is the operator-WS entry: an operator closes a
// convene (facilitator authz relaxed).
func (c *wsConn) handleOperatorConveneClose(env frames.Envelope) {
	var p frames.ConveneClosePayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed convene.close payload")
		return
	}
	result := c.broker.closeConvene(c, p, "operator", true)
	resp, _ := frames.NewResponse(frames.KindConveneCloseResult, env.ID, result)
	c.send(resp)
}

// handleOperatorConvenesList answers convenes.list (open + recent).
func (c *wsConn) handleOperatorConvenesList(env frames.Envelope) {
	store := c.broker.cfg.ConveneStore
	if store == nil {
		c.operatorError(env, "convene store not configured")
		return
	}
	var p frames.ConvenesListPayload
	_ = frames.PayloadAs(env, &p)
	ctx, cancel := c.opCtx()
	defer cancel()
	cs, err := store.List(ctx, p.Limit)
	if err != nil {
		c.operatorError(env, "convenes.list: "+err.Error())
		return
	}
	out := make([]frames.ConvenePayload, 0, len(cs))
	for _, cv := range cs {
		out = append(out, conveneToPayload(cv))
	}
	resp, _ := frames.NewResponse(frames.KindConvenesListResult, env.ID,
		frames.ConvenesListResultPayload{Convenes: out})
	c.send(resp)
}
