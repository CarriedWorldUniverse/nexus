// Per-aspect observability subscriptions (Phase B).
//
// subscribe.observe enrolls an operator's WS connection in one
// aspect's observability stream; live frames produced by the
// observability Hub fan out to enrolled operators through
// broadcastObserveFrame. Subscription state is per-aspect (a map on
// wsConn), distinct from the global subscribedChat flag — operators
// commonly want to watch one or two aspects in detail without
// drinking the whole firehose.
//
// Tail replay: on subscribe, the broker drains the Hub's retained
// Buffer for that aspect and writes the frames immediately so the
// operator sees recent history before any new frame arrives. SinceSeq
// in the payload narrows replay on reconnect.
//
// Bridle-event wiring is deferred to Phase E (Keel's parallel work).
// v0.1 ships chat-only observability: hooks in chat_send.go drive
// ChatFrame emission per sender + recipient.

package broker

import (
	"encoding/json"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

func (c *wsConn) handleSubscribeObserve(env frames.Envelope) {
	var payload frames.SubscribeObservePayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.log.Warn("subscribe.observe: payload decode failed", "err", err)
		return
	}
	if payload.Aspect == "" {
		c.log.Warn("subscribe.observe: empty aspect", "from", c.registeredAs)
		return
	}

	c.subMu.Lock()
	if c.subscribedObserve == nil {
		c.subscribedObserve = make(map[string]bool)
	}
	c.subscribedObserve[payload.Aspect] = true
	c.subMu.Unlock()

	c.log.Info("observability subscribe", "aspect", payload.Aspect, "since_seq", payload.SinceSeq)

	// Drain the Hub's retained tail before acking so the operator
	// receives history-then-ack-then-live in deterministic order.
	if c.broker != nil && c.broker.observability != nil {
		for _, f := range c.broker.observability.Tail(payload.Aspect, payload.SinceSeq) {
			c.sendObserveFrame(payload.Aspect, f)
		}
	}

	c.ackSubscribe(env)
}

func (c *wsConn) handleUnsubscribeObserve(env frames.Envelope) {
	var payload frames.UnsubscribeObservePayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.log.Warn("unsubscribe.observe: payload decode failed", "err", err)
		return
	}

	c.subMu.Lock()
	if c.subscribedObserve != nil {
		delete(c.subscribedObserve, payload.Aspect)
	}
	c.subMu.Unlock()

	c.log.Info("observability unsubscribe", "aspect", payload.Aspect)
	c.ackSubscribe(env)
}

// sendObserveFrame builds and sends a single observe.frame envelope
// to this connection. Marshal failure is logged and the frame is
// dropped — better one missed frame than a corrupted stream.
func (c *wsConn) sendObserveFrame(aspect string, f observability.Frame) {
	fJSON, err := json.Marshal(f)
	if err != nil {
		c.log.Warn("observe.frame: marshal failed", "aspect", aspect, "err", err)
		return
	}
	env, err := frames.New(frames.KindObserveFrame, frames.ObserveFramePayload{
		Aspect: aspect,
		Frame:  fJSON,
	})
	if err != nil {
		c.log.Warn("observe.frame: build failed", "aspect", aspect, "err", err)
		return
	}
	c.send(env)
}

// broadcastObserveFrame is the Hub's onFrame callback: every Grouper
// emission lands here, the broker walks live operators, and pushes
// the frame to any with this aspect in their subscribedObserve set.
// Mirrors the fanOutToOperators predicate pattern but reads from the
// per-aspect map instead of a single flag.
func (b *Broker) broadcastObserveFrame(aspect string, f observability.Frame) {
	b.opMu.RLock()
	targets := make([]*wsConn, 0, len(b.operators))
	for c := range b.operators {
		c.subMu.RLock()
		if c.subscribedObserve != nil && c.subscribedObserve[aspect] {
			targets = append(targets, c)
		}
		c.subMu.RUnlock()
	}
	b.opMu.RUnlock()

	for _, c := range targets {
		c.sendObserveFrame(aspect, f)
	}
}
