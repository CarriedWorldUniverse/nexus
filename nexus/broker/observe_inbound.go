// Inbound observability frames from remote aspects.
//
// The handlers decode each inbound observe.* frame and call the
// matching method on the per-aspect Grouper.
//
// Attribution (keel-cli #236): the aspect identity for the Grouper is
// always taken from c.registeredAs — the wsConn's authenticated
// registration — never from the payload's Aspect field. The payload
// field is logged as advisory when it disagrees but does NOT
// override; this prevents a compromised agent from forging frames
// against another aspect's stream.

package broker

import (
	"encoding/json"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/runtime/obsforward"
)

// inboundObserveAspect resolves the trusted aspect identity for an
// incoming observe.* frame. Returns "" when the connection isn't
// registered (the frame is dropped by the caller). Logs a warning on
// payload/identity divergence — useful for diagnostics, not a fault.
func (c *wsConn) inboundObserveAspect(payloadAspect, frameKind string) string {
	if c.registeredAs == "" {
		c.log.Warn("observe.* from unregistered conn — dropped",
			"kind", frameKind, "payload_aspect", payloadAspect)
		return ""
	}
	if payloadAspect != "" && payloadAspect != c.registeredAs {
		c.log.Warn("observe.* payload aspect mismatch — using registered identity",
			"kind", frameKind, "payload_aspect", payloadAspect, "registered_as", c.registeredAs)
	}
	return c.registeredAs
}

func (c *wsConn) handleObserveBegin(env frames.Envelope) {
	var p frames.ObserveBeginPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("observe.begin: payload decode failed", "err", err)
		return
	}
	aspect := c.inboundObserveAspect(p.Aspect, "observe.begin")
	if aspect == "" {
		return
	}
	if c.broker.observability == nil {
		return
	}
	c.broker.observability.GrouperFor(aspect).BeginTurn(p.TurnID, p.Label, p.Model, p.Provider, p.TriggerMsg)
}

func (c *wsConn) handleObserveEvent(env frames.Envelope) {
	var p frames.ObserveEventPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("observe.event: payload decode failed", "err", err)
		return
	}
	aspect := c.inboundObserveAspect(p.Aspect, "observe.event")
	if aspect == "" {
		return
	}
	if c.broker.observability == nil {
		return
	}
	ev, ok := decodeInboundBridleEvent(p.EventKind, p.Event)
	if !ok {
		// Unknown kind — drop with a debug log. Forward-compat: an old
		// broker getting a new EventKind should not crash, just lose
		// that one event.
		c.log.Debug("observe.event: unknown event_kind, dropped",
			"event_kind", p.EventKind, "aspect", aspect)
		return
	}
	c.broker.observability.GrouperFor(aspect).OnBridleEvent(ev)
}

func (c *wsConn) handleObserveEnd(env frames.Envelope) {
	var p frames.ObserveEndPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("observe.end: payload decode failed", "err", err)
		return
	}
	aspect := c.inboundObserveAspect(p.Aspect, "observe.end")
	if aspect == "" {
		return
	}
	if c.broker.observability == nil {
		return
	}
	c.broker.observability.GrouperFor(aspect).EndTurn()
}

// decodeInboundBridleEvent is the inverse of obsforward.encodeBridleEvent.
// Kept in lockstep with the discriminators declared in obsforward to
// avoid wire-incompat drift. Returns (event, true) on success; (nil,
// false) for unknown kinds or decode failure.
func decodeInboundBridleEvent(kind string, body json.RawMessage) (bridle.Event, bool) {
	switch kind {
	case obsforward.EventKindModelChunk:
		var e bridle.ModelChunk
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, false
		}
		return e, true
	case obsforward.EventKindToolCallStart:
		var e bridle.ToolCallStart
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, false
		}
		return e, true
	case obsforward.EventKindToolCallResult:
		var e bridle.ToolCallResult
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, false
		}
		return e, true
	case obsforward.EventKindStepBoundary:
		var e bridle.StepBoundary
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, false
		}
		return e, true
	case obsforward.EventKindTurnDone:
		var e bridle.TurnDone
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, false
		}
		return e, true
	case obsforward.EventKindTurnError:
		// TurnError.Err is an error interface — the forwarder serialises
		// it as a plain string; we reconstruct a sentinel error so the
		// Grouper's downstream path can stringify uniformly.
		var raw struct {
			Err   string `json:"err"`
			Stage string `json:"stage"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, false
		}
		var revivedErr error
		if raw.Err != "" {
			revivedErr = inboundError(raw.Err)
		}
		return bridle.TurnError{Err: revivedErr, Stage: bridle.TurnErrorStage(raw.Stage)}, true
	default:
		return nil, false
	}
}

// inboundError carries a stringified bridle.TurnError.Err across the
// wire. Type-distinct so a Grouper consumer can detect a forwarded
// error vs. a locally-produced one if it ever needs to. Currently the
// Grouper just calls Err.Error() (grouper.go:135), so the distinction
// is documentary.
type inboundError string

func (e inboundError) Error() string { return string(e) }

// _ silences unused-import lints for observability while keeping the
// option open to grow this file with frame-level helpers that need
// the package — same idiom used elsewhere in nexus/broker.
var _ = observability.FrameKind("")
