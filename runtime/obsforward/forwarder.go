// Package obsforward wires a remote aspect's funnel observability hook
// to the broker over the existing aspect WS connection.
//
// Topology: nexus's observability.Hub lives in the broker process. The
// embedded Frame's funnel shares the heap with the Hub and feeds it
// directly (cmd/nexus/main.go). Remote aspects — agentfunnel on
// <operator-host>, dMon, etc — run their funnel in a different process, so
// their bridle events have to traverse the WS to reach the Hub.
//
// This package provides:
//   - WSForwarder: implements funnel.ObservabilityHook by marshalling
//     each call into an observe.begin / observe.event / observe.end
//     frame and pushing through a Sender.
//   - The broker-side decoders live in nexus/broker/observe_inbound.go.
//
// Send failures are logged (best-effort) but never block the funnel —
// observability is consumed for diagnostics, and a stalled WS write
// must not be allowed to wedge a deliberation turn.
package obsforward

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// Sender is the minimal interface WSForwarder needs to push frames.
// Implemented by *wsasp.Client.SendBestEffort (use the agentfunnel-side
// best-effort path so observability frames don't pile up in the
// reconnect-replay buffer and surface minutes stale after a flip).
// Declared as an interface to keep this package decoupled from wsasp
// and to make the unit test trivial — a fake Sender records sent
// frames.
type Sender interface {
	Send(ctx context.Context, env frames.Envelope) error
}

// SenderFunc adapts a function value to Sender so callers can pass
// methods like wsasp.Client.SendBestEffort directly without writing
// a wrapper struct.
type SenderFunc func(ctx context.Context, env frames.Envelope) error

func (f SenderFunc) Send(ctx context.Context, env frames.Envelope) error { return f(ctx, env) }

// sendTimeout caps how long a single Send blocks before the forwarder
// gives up on that one frame and moves on. The funnel goroutine must
// not stall on a wedged WS write.
const sendTimeout = 2 * time.Second

// WSForwarder implements funnel.ObservabilityHook. It is safe to call
// from multiple goroutines — Send on the wsclient is goroutine-safe
// and each method here is a self-contained marshal + send.
type WSForwarder struct {
	sender  Sender
	aspect  string
	log     *slog.Logger
	dropped atomic.Uint64
}

// Dropped returns the number of frames the forwarder has failed to
// send (build or transport errors). Exposed so callers can surface a
// silent-drop rate without scraping debug logs.
func (w *WSForwarder) Dropped() uint64 { return w.dropped.Load() }

// New constructs a forwarder for the named aspect. aspect is included
// in each frame's payload for receiver diagnostics, but the broker
// authoritatively tags events with the wsConn's registered identity
// (per keel-cli's caveat #236) — so a divergence here is harmless.
func New(sender Sender, aspect string, log *slog.Logger) *WSForwarder {
	if log == nil {
		log = slog.Default()
	}
	return &WSForwarder{sender: sender, aspect: aspect, log: log}
}

// BeginTurn forwards an observe.begin frame.
func (w *WSForwarder) BeginTurn(turnID, label, model, provider string, trigger int64) {
	env, err := frames.New(frames.KindObserveBegin, frames.ObserveBeginPayload{
		Aspect:     w.aspect,
		TurnID:     turnID,
		Label:      label,
		Model:      model,
		Provider:   provider,
		TriggerMsg: trigger,
	})
	if err != nil {
		w.dropped.Add(1)
		w.log.Warn("obsforward: build begin frame", "err", err, "turn_id", turnID)
		return
	}
	w.send(env, "begin")
}

// OnBridleEvent marshals one bridle.Event and forwards observe.event.
// Events whose Go type is unknown to this package are skipped (with a
// warn log) rather than panicking — bridle adding a new event type
// should not crash an older agentfunnel.
func (w *WSForwarder) OnBridleEvent(ev bridle.Event) {
	kind, body, err := encodeBridleEvent(ev)
	if err != nil {
		w.dropped.Add(1)
		w.log.Warn("obsforward: encode bridle event", "err", err, "type_kind", kind)
		return
	}
	env, err := frames.New(frames.KindObserveEvent, frames.ObserveEventPayload{
		Aspect:    w.aspect,
		EventKind: kind,
		Event:     body,
	})
	if err != nil {
		w.dropped.Add(1)
		w.log.Warn("obsforward: build event frame", "err", err, "event_kind", kind)
		return
	}
	w.send(env, "event")
}

// EndTurn forwards an observe.end frame.
func (w *WSForwarder) EndTurn() {
	env, err := frames.New(frames.KindObserveEnd, frames.ObserveEndPayload{Aspect: w.aspect})
	if err != nil {
		w.dropped.Add(1)
		w.log.Warn("obsforward: build end frame", "err", err)
		return
	}
	w.send(env, "end")
}

func (w *WSForwarder) send(env frames.Envelope, tag string) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if err := w.sender.Send(ctx, env); err != nil {
		w.dropped.Add(1)
		// Debug level — disconnects are routine (sleep/wake on
		// <operator-host>) and we don't want to spam at warn.
		w.log.Debug("obsforward: send failed", "err", err, "tag", tag)
	}
}

// Bridle event kind discriminators on the wire. Kept stable across
// agentfunnel versions so an old broker can decode a new agentfunnel
// (forward compat) and vice versa (back compat: a broker that sees an
// unknown EventKind drops the event with a warn, no crash).
const (
	EventKindModelChunk     = "model_chunk"
	EventKindToolCallStart  = "tool_call_start"
	EventKindToolCallResult = "tool_call_result"
	EventKindStepBoundary   = "step_boundary"
	EventKindTurnDone       = "turn_done"
	EventKindTurnError      = "turn_error"
)

// encodeBridleEvent marshals one bridle.Event into a discriminator
// string plus a JSON body. TurnError gets special handling because
// its Err field is an error interface — naive json.Marshal would
// emit `{}` and lose the message; we stringify first.
func encodeBridleEvent(ev bridle.Event) (string, json.RawMessage, error) {
	switch e := ev.(type) {
	case bridle.ModelChunk:
		body, err := json.Marshal(e)
		return EventKindModelChunk, body, err
	case bridle.ToolCallStart:
		body, err := json.Marshal(e)
		return EventKindToolCallStart, body, err
	case bridle.ToolCallResult:
		body, err := json.Marshal(e)
		return EventKindToolCallResult, body, err
	case bridle.StepBoundary:
		body, err := json.Marshal(e)
		return EventKindStepBoundary, body, err
	case bridle.TurnDone:
		body, err := json.Marshal(e)
		return EventKindTurnDone, body, err
	case bridle.TurnError:
		errStr := ""
		if e.Err != nil {
			errStr = e.Err.Error()
		}
		body, err := json.Marshal(struct {
			Err   string `json:"err,omitempty"`
			Stage string `json:"stage,omitempty"`
		}{Err: errStr, Stage: string(e.Stage)})
		return EventKindTurnError, body, err
	default:
		return "unknown", nil, errUnknownEventType
	}
}

// errUnknownEventType signals encodeBridleEvent received a bridle.Event
// shape it doesn't recognise. Sentinel so callers can log without a
// fresh allocation on the common path.
var errUnknownEventType = errors.New("obsforward: unknown bridle.Event type")
