// Per-aspect observability subscriptions (Phase B).
//
// subscribe.observe enrolls an operator's WS connection in one
// aspect's observability stream; live frames produced by the
// observability Hub fan out to enrolled operators through
// BroadcastObserveFrame. Subscription state is per-aspect (a map on
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

	c.log.Info("observability subscribe", "aspect", payload.Aspect, "since_seq", payload.SinceSeq)

	// Order matters: drain the tail FIRST (while subscribedObserve is
	// still false for this aspect → BroadcastObserveFrame cannot pick
	// us up as a live target), THEN flip the subscription flag, THEN
	// ack. The earlier flag-first ordering let a concurrent chat.send
	// race a live frame in ahead of the historical tail, breaking the
	// spec's history-then-live ordering.
	//
	// Tradeoff: a frame appended to the Hub buffer AFTER Tail() returns
	// but BEFORE we flip the flag below will be neither replayed nor
	// delivered live to this op. The gap is sub-millisecond. The
	// resync-on-reconnect path (re-subscribe with SinceSeq) covers it;
	// the alternative — holding the Hub's emit chain across this
	// handler — would block all writes for the duration of any
	// subscribe and is worse.
	if c.broker != nil && c.broker.observability != nil {
		for _, f := range c.broker.observability.Tail(payload.Aspect, payload.SinceSeq) {
			c.sendObserveFrame(payload.Aspect, f)
		}
	}

	c.subMu.Lock()
	if c.subscribedObserve == nil {
		c.subscribedObserve = make(map[string]bool)
	}
	c.subscribedObserve[payload.Aspect] = true
	c.subMu.Unlock()

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

// BroadcastObserveFrame is the Hub's onFrame callback: every Grouper
// emission lands here, the broker walks live operators, and pushes
// the frame to any with this aspect in their subscribedObserve set.
// Mirrors the fanOutToOperators predicate pattern but reads from the
// per-aspect map instead of a single flag.
//
// Exported so callers (cmd/nexus/main.go) can chain it with other
// onFrame subscribers — e.g. the jsonlsink persistent writer — by
// composing a closure that calls multiple sinks and re-installing it
// via Hub.SetOnFrame. The broker constructor still installs this
// directly as the default; main.go overrides during wiring.
//
// Invariant: called from Grouper.emit without the Hub lock held —
// must not re-enter Hub.GrouperFor or it'll deadlock with the Hub
// mutex acquisition order. Likewise must not call back into the
// same Grouper (Grouper.mu is held across emit).
func (b *Broker) BroadcastObserveFrame(aspect string, f observability.Frame) {
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
