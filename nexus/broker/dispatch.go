// Package broker's server-side dispatch surface — the seam where
// other Nexus code (embedded keel frame, later the operator UI, later
// cross-aspect hand routing) can send frames TO connected aspects and
// await their responses.
//
// Connection registry tracks one wsConn per registered aspect name.
// Displaces on re-register per the roster contract.
package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// Dispatcher is the server-side request/response API over WS. The
// broker owns one; callers (keel-embedded, tests, the future UI
// backend) use it to drive turns and hands against connected aspects.
//
// Routing is by aspect name — the registry maps name → the wsConn
// that last registered under that name. Displacement follows the
// roster's session-id rules (see handleRegisterFrame).
type Dispatcher struct {
	mu sync.Mutex

	// connsByAspect maps aspect-name → the connection currently
	// holding that registration.
	connsByAspect map[string]*wsConn

	// pending maps correlation-id → response channel. Turn-result and
	// similar responses correlate back through here.
	pending map[string]chan frames.Envelope
}

// newDispatcher is called once by the Broker on construction.
func newDispatcher() *Dispatcher {
	return &Dispatcher{
		connsByAspect: make(map[string]*wsConn),
		pending:       make(map[string]chan frames.Envelope),
	}
}

// bind records the connection as the active route for this aspect.
// Called from handleRegisterFrame after the roster accepts.
func (d *Dispatcher) bind(name string, c *wsConn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connsByAspect[name] = c
}

// connFor returns the wsConn currently bound to the named aspect, or
// nil if no aspect with that name is connected. Used by the chat
// fan-out path to deliver chat.deliver frames to live aspects.
func (d *Dispatcher) connFor(name string) *wsConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connsByAspect[name]
}

// ConnectedAspects returns the names currently holding a live WS
// registration. Used by the reaper to keep WS-connected aspects out
// of the stale/down sweep — under the WS transport, an open
// connection IS the heartbeat (Lock 2).
func (d *Dispatcher) ConnectedAspects() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.connsByAspect))
	for name := range d.connsByAspect {
		out = append(out, name)
	}
	return out
}

// unbind removes the mapping when the connection goes away. If a
// different connection has already replaced this one (displacement),
// we must not unbind the newer entry.
func (d *Dispatcher) unbind(name string, c *wsConn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, ok := d.connsByAspect[name]; ok && cur == c {
		delete(d.connsByAspect, name)
	}
}

// routeResponse delivers a correlated response frame to the waiting
// caller. Returns true if someone was waiting (so the frame dispatcher
// knows not to double-handle it).
func (d *Dispatcher) routeResponse(env frames.Envelope) bool {
	if env.InReplyTo == "" {
		return false
	}
	d.mu.Lock()
	ch, ok := d.pending[env.InReplyTo]
	if ok {
		delete(d.pending, env.InReplyTo)
	}
	d.mu.Unlock()
	if !ok {
		return false
	}
	ch <- env
	close(ch)
	return true
}

// SendTurn dispatches a turn to the named aspect and waits for the
// correlated turn.result. Returns ErrAspectNotConnected if the aspect
// isn't currently holding a connection. Blocks until the response
// arrives or ctx times out.
func (b *Broker) SendTurn(ctx context.Context, aspect string, req frames.TurnPayload) (frames.TurnResultPayload, error) {
	env, err := frames.NewRequest(frames.KindTurn, req)
	if err != nil {
		return frames.TurnResultPayload{}, fmt.Errorf("build turn frame: %w", err)
	}
	// Stamp the target so an outpost-routed connection can deliver
	// to the right local aspect (#20). Direct-aspect connections
	// ignore the field.
	env.TargetAspect = aspect

	// Register pending response channel before sending so a fast
	// responder can't beat us to the map.
	respCh := make(chan frames.Envelope, 1)
	b.dispatcher.mu.Lock()
	b.dispatcher.pending[env.ID] = respCh
	b.dispatcher.mu.Unlock()
	defer func() {
		b.dispatcher.mu.Lock()
		delete(b.dispatcher.pending, env.ID)
		b.dispatcher.mu.Unlock()
	}()

	b.dispatcher.mu.Lock()
	conn, ok := b.dispatcher.connsByAspect[aspect]
	b.dispatcher.mu.Unlock()
	if !ok {
		return frames.TurnResultPayload{}, ErrAspectNotConnected
	}

	conn.send(env)

	select {
	case <-ctx.Done():
		return frames.TurnResultPayload{}, ctx.Err()
	case resp, open := <-respCh:
		if !open {
			return frames.TurnResultPayload{}, errors.New("broker.SendTurn: connection dropped before response")
		}
		if resp.Kind == frames.Kind("turn.error") {
			var errPayload map[string]string
			_ = frames.PayloadAs(resp, &errPayload)
			return frames.TurnResultPayload{}, fmt.Errorf("turn error: %s", errPayload["error"])
		}
		if resp.Kind != frames.KindTurnResult {
			return frames.TurnResultPayload{}, fmt.Errorf("unexpected response kind %q", resp.Kind)
		}
		var result frames.TurnResultPayload
		if err := frames.PayloadAs(resp, &result); err != nil {
			return frames.TurnResultPayload{}, fmt.Errorf("decode turn.result: %w", err)
		}
		return result, nil
	}
}

// ConnectedAspects returns the names of aspects holding a live WS
// registration right now. Thin pass-through to the Dispatcher; exposed
// on Broker so the reaper goroutine (cmd/nexus) doesn't need to reach
// into broker internals.
func (b *Broker) ConnectedAspects() []string {
	return b.dispatcher.ConnectedAspects()
}

// ErrAspectNotConnected is returned by SendTurn (and future
// SendHand / Send*) when the target aspect doesn't hold a live WS.
var ErrAspectNotConnected = errors.New("aspect not connected")
