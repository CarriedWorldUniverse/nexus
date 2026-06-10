// Operator subscription handlers + fan-out helpers (Crossing 5d).
//
// Three subscription channels:
//
//   - chat       — operator receives chat.deliver for every persisted
//                  message (operator's view is "everything"). Hooked
//                  in chat_send.go after the per-aspect recipient
//                  fan-out.
//   - roster     — operator receives roster.update on aspect connect,
//                  disconnect, status change. Hooked in
//                  handleRegisterFrame and cleanup().
//   - aspect_status — operator receives aspect.status_pulse when an
//                  aspect emits a pulse. Pulse origin (#118) doesn't
//                  exist yet; the subscribe path lands the wire
//                  surface so 5e can render UI without re-touching
//                  this layer when pulses land.
//
// Subscriptions are pure in-memory state on wsConn (subscribedChat /
// subscribedRoster / subscribedAspectStatus). WS close = subs gone;
// SPA replays its subscribes on reconnect (per spec §6.2). All three
// flips are idempotent.

package broker

import (
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// dispatchOperatorSubFrame routes operator-only subscription frames.
// Returns true when the kind was handled. Called from
// dispatchOperatorFrame BEFORE its kind switch so subs land on the
// fast path.
func (c *wsConn) dispatchOperatorSubFrame(env frames.Envelope) bool {
	switch env.Kind {
	case frames.KindSubscribeRoster:
		c.handleSubscribeRoster(env)
	case frames.KindSubscribeChat:
		c.handleSubscribeChat(env)
	case frames.KindSubscribeAspectStatus:
		c.handleSubscribeAspectStatus(env)
	case frames.KindUnsubscribeRoster:
		c.handleUnsubscribeRoster(env)
	case frames.KindUnsubscribeChat:
		c.handleUnsubscribeChat(env)
	case frames.KindUnsubscribeAspectStatus:
		c.handleUnsubscribeAspectStatus(env)
	case frames.KindSubscribeObserve:
		c.handleSubscribeObserve(env)
	case frames.KindUnsubscribeObserve:
		c.handleUnsubscribeObserve(env)
	case frames.KindPing:
		resp, _ := frames.NewResponse(frames.KindPong, env.ID, nil)
		c.send(resp)
	default:
		return false
	}
	return true
}

func (c *wsConn) ackSubscribe(env frames.Envelope) {
	resp, _ := frames.NewResponse(frames.KindSubscribeAck, env.ID, frames.SubscribeAckPayload{
		Kind: string(env.Kind),
	})
	c.send(resp)
}

func (c *wsConn) handleSubscribeRoster(env frames.Envelope) {
	c.subMu.Lock()
	c.subscribedRoster = true
	c.subMu.Unlock()
	c.ackSubscribe(env)
}

func (c *wsConn) handleSubscribeChat(env frames.Envelope) {
	c.subMu.Lock()
	c.subscribedChat = true
	c.subMu.Unlock()
	c.ackSubscribe(env)
}

func (c *wsConn) handleSubscribeAspectStatus(env frames.Envelope) {
	c.subMu.Lock()
	c.subscribedAspectStatus = true
	c.subMu.Unlock()
	c.ackSubscribe(env)
}

func (c *wsConn) handleUnsubscribeRoster(env frames.Envelope) {
	c.subMu.Lock()
	c.subscribedRoster = false
	c.subMu.Unlock()
	c.ackSubscribe(env)
}

func (c *wsConn) handleUnsubscribeChat(env frames.Envelope) {
	c.subMu.Lock()
	c.subscribedChat = false
	c.subMu.Unlock()
	c.ackSubscribe(env)
}

func (c *wsConn) handleUnsubscribeAspectStatus(env frames.Envelope) {
	c.subMu.Lock()
	c.subscribedAspectStatus = false
	c.subMu.Unlock()
	c.ackSubscribe(env)
}

// --- broker-side fan-out helpers ---

// bindOperator registers an operator wsConn for fan-out membership.
// Called from handleConnect when c.auth.Operator is true.
func (b *Broker) bindOperator(c *wsConn) {
	b.opMu.Lock()
	b.operators[c] = struct{}{}
	b.opMu.Unlock()
}

// unbindOperator removes an operator wsConn. Called from cleanup.
// Safe to call when c was never bound (no-op).
func (b *Broker) unbindOperator(c *wsConn) {
	b.opMu.Lock()
	delete(b.operators, c)
	b.opMu.Unlock()
}

// fanOutToOperators iterates live operator connections, invoking
// pred to decide whether the conn should receive `env`. The conn's
// subscription state is read under c.subMu.RLock to keep fan-out
// non-blocking with concurrent subscribe writes.
//
// pred returns true when this conn should receive the frame.
// Typical predicates check the matching subscribed* flag.
//
// WS-safety invariant: coder/websocket v1.8.x permits concurrent
// writes — multiple goroutines may call c.Write simultaneously, and
// Write is independent of Read. The caller's read loop in serve()
// can run concurrently with this fan-out's writes. c.mu inside
// c.send serializes multiple concurrent writers (this fan-out vs.
// any direct send from a frame handler) so a partial-frame
// interleave is impossible.
//
// Lock order: opMu → c.subMu → c.mu (inside c.send). This is the
// only order in the codebase; subscribe handlers take c.subMu →
// c.mu (no opMu involved); aspect handlers don't touch opMu or
// subMu. No inversion path exists.
//
// Send failures (closed conn, slow consumer) are silently dropped:
// the conn's read loop will surface the close on its next Read,
// triggering cleanup which unbinds. Letting one slow operator
// block the fan-out goroutine would stall every other operator's
// view; we accept "dashboard misses a frame, refresh shows truth"
// as the right trade-off for operator UX.
func (b *Broker) fanOutToOperators(env frames.Envelope, pred func(c *wsConn) bool) {
	b.opMu.RLock()
	conns := make([]*wsConn, 0, len(b.operators))
	for c := range b.operators {
		conns = append(conns, c)
	}
	b.opMu.RUnlock()

	for _, c := range conns {
		c.subMu.RLock()
		ok := pred(c)
		c.subMu.RUnlock()
		if !ok {
			continue
		}
		c.send(env)
	}
}

// broadcastChatDeliverToOperators pushes a chat.deliver frame to
// every operator with subscribedChat true. Hooked in chat_send.go's
// fan-out tail. Distinct from the per-aspect fan-out above it: that
// loop pushes to RecipientPolicy-selected aspects; this loop pushes
// to every subscribing operator regardless of recipient policy
// (operator's view is "everything").
func (b *Broker) broadcastChatDeliverToOperators(env frames.Envelope) {
	n := 0
	b.fanOutToOperators(env, func(c *wsConn) bool {
		if c.subscribedChat {
			n++
			return true
		}
		return false
	})
	b.log.Debug("chat.deliver operator fan-out", "subscribers", n)
}

// BroadcastChatReactionUpdate pushes a chat.reaction.update frame to
// every operator with subscribedChat true. Piggy-backs the chat
// subscription rather than introducing a separate subscribe.reactions
// channel: reactions are part of chat in the operator's mental model,
// and an operator who wants chat at all wants its reactions live too.
// Hooked from handleChatReactionFrame after ToggleReaction succeeds.
func (b *Broker) BroadcastChatReactionUpdate(payload frames.ChatReactionUpdatePayload) {
	env, err := frames.New(frames.KindChatReactionUpdate, payload)
	if err != nil {
		b.log.Warn("chat.reaction.update build", "err", err, "msg_id", payload.MsgID)
		return
	}
	b.fanOutToOperators(env, func(c *wsConn) bool {
		return c.subscribedChat
	})
}

// broadcastRosterUpdate pushes a roster.update frame to every
// operator with subscribedRoster true. Called from
// handleRegisterFrame (reason: "connect"), cleanup (reason:
// "disconnect"), and reapStale (reason: "status_change").
func (b *Broker) broadcastRosterUpdate(payload frames.RosterUpdatePayload) {
	env, err := frames.New(frames.KindRosterUpdate, payload)
	if err != nil {
		b.log.Warn("roster.update build", "err", err)
		return
	}
	b.fanOutToOperators(env, func(c *wsConn) bool {
		return c.subscribedRoster
	})
}

// broadcastAspectStatusPulse is the public seam for #118's pulse
// origin (when it lands). Currently no caller fires it; the SPA
// can subscribe today, and the channel just stays quiet. Lands
// here so the dashboard handler doesn't need a follow-up touch
// when pulses go live.
func (b *Broker) broadcastAspectStatusPulse(payload frames.AspectStatusPulsePayload) {
	env, err := frames.New(frames.KindAspectStatusPulse, payload)
	if err != nil {
		b.log.Warn("aspect.status_pulse build", "err", err)
		return
	}
	b.fanOutToOperators(env, func(c *wsConn) bool {
		return c.subscribedAspectStatus
	})
}
