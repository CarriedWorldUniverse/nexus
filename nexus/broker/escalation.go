// Operator escalation relay (ToolRunner P3c).
//
// The broker is a STATELESS relay for escalation. It holds no
// pending-escalation map: the aspect's own wsclient.Request holds the
// blocked channel, correlated by the request envelope's ID. The broker
// only forwards frames and enforces identity.
//
// Two directions:
//
//   - escalation.request (aspect → broker): a native-API aspect's funnel
//     paused a tool call its policy marked "ask a human" and issued a
//     correlated Request. The broker verifies the payload's aspect
//     matches the connection's authenticated identity (mirrors the
//     dispatch identity check), then fans the request out to every
//     connected operator so agora can surface it. The broker does NOT
//     respond — the operator does, asynchronously.
//
//   - escalation.decision (operator → broker): the operator answered.
//     The decision carries the target aspect and the original request id
//     (payload.RequestID). The broker authorises the operator, looks up
//     the aspect's live connection, and forwards the decision frame with
//     InReplyTo=RequestID so the aspect's blocked wsclient.Request
//     resolves and the funnel hook wakes.
//
// Wire note: the operator sends escalation.decision WITHOUT an envelope
// InReplyTo (RequestID lives in the payload). If the operator set the
// envelope InReplyTo instead, the serve() read loop would route it
// through dispatcher.routeResponse (matching a broker-side pending
// request) before this handler ever ran — and there is no broker-side
// pending entry, so it would be dropped. Keeping the correlation id in
// the payload and stamping InReplyTo only on the forwarded frame keeps
// the broker stateless and the routing unambiguous.

package broker

import (
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// handleEscalationRequestFrame relays an aspect's escalation.request to
// subscribed operators after an identity check. Reached from the aspect
// frame dispatch switch (operators never send escalation.request).
func (c *wsConn) handleEscalationRequestFrame(env frames.Envelope) {
	// escalation.request is an aspect→broker frame: aspects ASK, operators
	// ANSWER. An operator connection reaching here (it falls through
	// dispatchOperatorFrame to the aspect switch) would otherwise pass the
	// admin identity check and have its own request fanned back to it.
	// Reject explicitly — operators have no funnel to block on.
	if c.auth.Operator {
		c.log.Warn("escalation.request from operator connection ignored")
		c.respondError(env, "escalation.request not accepted from operator connections")
		return
	}
	var payload frames.EscalationRequestPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.respondError(env, "escalation.request payload malformed: "+err.Error())
		return
	}
	if payload.Aspect == "" {
		c.respondError(env, "escalation.request: aspect required")
		return
	}
	// Identity enforcement (mirrors handleDispatchFrame §5.4): the
	// escalating aspect must be the connection's authenticated identity.
	// Admin (Frame) may escalate on behalf of any aspect. Without this an
	// aspect could impersonate another in front of the operator.
	if !c.auth.Admin && c.auth.AgentID != payload.Aspect {
		c.log.Warn("escalation.request identity mismatch",
			"claimed", payload.Aspect, "authenticated", c.auth.AgentID)
		c.respondError(env, "identity_mismatch: caller "+c.auth.AgentID+
			" cannot escalate as "+payload.Aspect)
		return
	}

	// Fan the request out to every connected operator. Escalation is an
	// urgent, low-volume signal, so we push to all operators rather than
	// gate on a dedicated subscription (agora — the consumer — is not
	// built yet; the subscribe.escalation knob is deferred per the P3c
	// design §8). The aspect's own Request holds the pending channel;
	// the broker keeps nothing.
	c.log.Info("escalation.request relayed to operators",
		"aspect", payload.Aspect, "tool", payload.Tool, "request_id", env.ID)
	c.broker.fanOutToOperators(env, func(*wsConn) bool { return true })
}

// handleEscalationDecisionFrame routes an operator's escalation.decision
// back to the originating aspect's connection so its blocked Request
// resolves. Reached from dispatchOperatorFrame (operator-only).
func (c *wsConn) handleEscalationDecisionFrame(env frames.Envelope) {
	var payload frames.EscalationDecisionPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.operatorError(env, "escalation.decision payload malformed: "+err.Error())
		return
	}
	if payload.Aspect == "" {
		c.operatorError(env, "escalation.decision: aspect required")
		return
	}
	if payload.RequestID == "" {
		c.operatorError(env, "escalation.decision: request_id required")
		return
	}
	if payload.Decision != frames.EscalationApprove && payload.Decision != frames.EscalationDeny {
		c.operatorError(env, "escalation.decision: decision must be approve or deny")
		return
	}

	// Look up the target aspect's live connection. Stateless relay: if
	// the aspect has disconnected (turn aborted, restart), there is
	// nothing to deliver to — the aspect's Request already failed on ctx
	// cancel / disconnect, so dropping is correct.
	aspectConn := c.broker.dispatcher.connFor(payload.Aspect)
	if aspectConn == nil {
		c.log.Warn("escalation.decision: target aspect not connected",
			"aspect", payload.Aspect, "request_id", payload.RequestID)
		c.operatorError(env, "aspect "+payload.Aspect+" not connected")
		return
	}

	// Forward the decision to the aspect with InReplyTo set to the
	// original request id so the aspect's wsclient pending map resolves.
	// Stamp the operator identity from the authenticated connection so
	// the model/audit can't be fed a forged operator name.
	payload.Operator = c.auth.AgentID
	fwd, err := frames.NewResponse(frames.KindEscalationDecision, payload.RequestID, payload)
	if err != nil {
		c.log.Error("build escalation.decision forward frame", "err", err)
		c.operatorError(env, "internal error building decision frame")
		return
	}
	c.log.Info("escalation.decision routed to aspect",
		"aspect", payload.Aspect, "decision", payload.Decision, "request_id", payload.RequestID)
	aspectConn.send(fwd)
}
