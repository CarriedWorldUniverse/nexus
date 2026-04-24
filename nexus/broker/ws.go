// Package broker's WS surface. Handles the /connect endpoint where
// aspects (and, later, Outposts) open a persistent WebSocket and
// exchange frames per transport spec v0.1.
package broker

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/nexus/sessions"
)

// wsConn tracks per-connection state on the broker side: who's
// registered on this socket, when they connected. One wsConn lives as
// long as one WebSocket does; its lifecycle drives deregistration
// when the socket closes.
type wsConn struct {
	id           string // short log correlator assigned on accept
	conn         *websocket.Conn
	broker       *Broker
	registeredAs string // aspect name once register frame is accepted; empty until then
	sessionID    string
	connectedAt  time.Time
	log          *slog.Logger
	mu           sync.Mutex // serialises writes to conn
}

// Maximum time we'll spend reading one frame before tearing down the
// connection. Prevents a slowloris-style attacker from tying up a
// goroutine forever. Extends on every successful read.
const readFrameTimeout = 5 * time.Minute

// Maximum size of a single frame's JSON payload. Generous but
// bounded.
const maxFrameBytes = 1 << 20 // 1 MiB

// handleConnect serves the WS upgrade at /connect. Authenticates via
// Authorization: Bearer header (same token as the HTTP API), upgrades
// to WS, spawns a goroutine per connection that reads frames and
// dispatches them to kind handlers.
func (b *Broker) handleConnect(w http.ResponseWriter, r *http.Request) {
	// Auth runs BEFORE upgrade so bad tokens get a clean 401, not a
	// half-open WS that immediately closes.
	if !b.authCheckHeader(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin check is relaxed because we authenticate via
		// bearer token. Without this, aspects dialing from another
		// host (the whole point of the distributed topology) would
		// be rejected.
		InsecureSkipVerify: true,
	})
	if err != nil {
		b.log.Warn("ws accept failed", "err", err)
		return
	}
	wsc.SetReadLimit(maxFrameBytes)

	connID := newConnID()
	c := &wsConn{
		id:          connID,
		conn:        wsc,
		broker:      b,
		connectedAt: time.Now().UTC(),
		log:         b.log.With("conn_id", connID),
	}

	// The WS connection outlives the HTTP request, so we can't use
	// r.Context() — it cancels as soon as handleConnect returns.
	// Use the broker-lifetime context so graceful shutdown tears
	// down detached serve goroutines instead of waiting on the OS
	// to close TCP. Fall back to Background if tests didn't go
	// through ListenAndServe (direct handler wiring).
	parentCtx := b.ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	go c.serve(parentCtx)
}

// authCheckHeader extracts the bearer token from the upgrade request
// and compares to the configured token in constant time.
func (b *Broker) authCheckHeader(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	given := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(given), []byte(b.cfg.AuthToken)) == 1
}

// serve runs the per-connection read loop. Exits when the connection
// closes or the parent context is cancelled. On exit, deregisters
// any aspect bound to this connection so the roster reflects the
// disconnect immediately.
func (c *wsConn) serve(parentCtx context.Context) {
	defer c.cleanup()

	c.log.Info("ws connection accepted")

	for {
		ctx, cancel := context.WithTimeout(parentCtx, readFrameTimeout)
		msgType, data, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			c.log.Info("ws connection closed", "err", err)
			return
		}
		if msgType != websocket.MessageText {
			c.log.Warn("ws received non-text frame, closing", "type", msgType)
			_ = c.conn.Close(websocket.StatusUnsupportedData, "text frames only")
			return
		}

		env, err := frames.Decode(data)
		if err != nil {
			c.log.Warn("frame decode failed", "err", err)
			// Tolerate garbage: don't close the connection for one
			// bad frame. An attacker spamming garbage gets ignored;
			// a legitimate peer with a bug recovers on its next send.
			continue
		}

		// Route correlated responses FIRST — even if the Kind is
		// unknown to us. A response to a request we sent out (e.g.
		// a turn.error with a kind the current build doesn't list in
		// IsKnown) must still reach the waiting caller. Forward-compat
		// cuts both ways: server-generated kinds like "turn.error"
		// are response-shape, not canonical, and pending routing is
		// the right hook. We unblock the caller, who then decides
		// what to do with the unexpected kind.
		if env.InReplyTo != "" {
			if c.broker.dispatcher.routeResponse(env) {
				continue
			}
			// Fell through — no pending waiter. Log and drop.
			c.log.Info("uncorrelated response frame dropped",
				"kind", env.Kind, "in_reply_to", env.InReplyTo)
			continue
		}

		if !frames.IsKnown(env.Kind) {
			c.log.Warn("unknown frame kind, dropping", "kind", env.Kind)
			continue // forward-compat per spec §5.3
		}

		c.dispatch(env)
	}
}

// dispatch routes a decoded non-response frame to the appropriate
// handler by kind. Response frames are routed to the dispatcher in
// the read loop before we ever get here.
func (c *wsConn) dispatch(env frames.Envelope) {
	switch env.Kind {
	case frames.KindRegister:
		c.handleRegisterFrame(env)
	case frames.KindDeregister:
		c.handleDeregisterFrame(env)
	case frames.KindOutpostRegister:
		c.handleOutpostRegisterFrame(env)
	case frames.KindOutpostDeregister:
		c.handleOutpostDeregisterFrame(env)
	case frames.KindSessionEntryAppended:
		c.handleSessionEntryAppended(env)
	default:
		c.log.Info("frame kind not yet handled", "kind", env.Kind)
	}
}

// handleOutpostRegisterFrame accepts a connecting Outpost. v1 just
// acks; per-Outpost state (roster integration, routing tables) is
// bundled into the broker's dispatcher in part 7.
func (c *wsConn) handleOutpostRegisterFrame(env frames.Envelope) {
	var payload frames.OutpostRegisterPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.respondError(env, "outpost.register payload missing: "+err.Error())
		return
	}
	if payload.OutpostID == "" {
		c.respondError(env, "outpost_id required")
		return
	}
	c.log.Info("outpost registered", "outpost_id", payload.OutpostID)
	ack, _ := frames.NewResponse(frames.KindOutpostRegisterAck, env.ID, frames.OutpostRegisterAckPayload{
		HeartbeatIntervalS: c.broker.cfg.HeartbeatIntervalS,
	})
	c.send(ack)
}

// handleOutpostDeregisterFrame logs graceful shutdown from an
// Outpost. The aspect-deregister cascade happens as each local
// aspect's WS (tunnelled through the Outpost) closes.
func (c *wsConn) handleOutpostDeregisterFrame(env frames.Envelope) {
	var payload frames.OutpostDeregisterPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.respondError(env, "outpost.deregister payload missing: "+err.Error())
		return
	}
	c.log.Info("outpost deregistered", "outpost_id", payload.OutpostID, "reason", payload.Reason)
}

// handleSessionEntryAppended persists the forwarded entry into the
// Nexus-side projection table. Aspects fire these as a best-effort
// observability signal (transport spec §8); dropped frames don't
// break the aspect, so we also don't respond. If the broker was
// constructed without a Projection (tests), log-and-drop.
func (c *wsConn) handleSessionEntryAppended(env frames.Envelope) {
	if c.broker.cfg.Projection == nil {
		return
	}
	var payload frames.SessionEntryAppendedPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.log.Warn("session.entry.appended payload malformed", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.broker.cfg.Projection.WriteEntry(ctx, sessions.Entry{
		Aspect:    payload.Aspect,
		SessionID: payload.SessionID,
		EntryID:   payload.EntryID,
		ParentID:  payload.ParentID,
		EntryKind: payload.EntryKind,
		EntryTS:   payload.TS.Format(time.RFC3339Nano),
		Payload:   payload.Payload,
	}); err != nil {
		c.log.Warn("session projection write failed",
			"aspect", payload.Aspect, "entry", payload.EntryID, "err", err)
	}
}

// handleRegisterFrame processes the first-frame-after-connect register
// frame from an aspect. Also handles the Outpost-forwarded register
// (same frame shape, just with a via_outpost field).
func (c *wsConn) handleRegisterFrame(env frames.Envelope) {
	// Decode as the extended ForwardedRegisterPayload which embeds
	// the base RegisterRequest. Plain aspect registers simply leave
	// via_outpost empty; Outpost-forwarded ones carry the stamp.
	var payload frames.ForwardedRegisterPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.respondError(env, "register payload missing or malformed: "+err.Error())
		return
	}
	if err := validateRegister(&payload.RegisterRequest); err != nil {
		c.respondError(env, err.Error())
		return
	}
	if payload.ViaOutpost != "" {
		c.log.Info("aspect registering via outpost",
			"name", payload.Name, "via", payload.ViaOutpost)
	}

	state, displacedSession, err := c.broker.roster.Register(&payload.RegisterRequest)
	if err != nil {
		switch {
		case errors.Is(err, roster.ErrAlreadyRegistered):
			c.respondError(env, "aspect already registered with a different session")
		case errors.Is(err, roster.ErrPortConflict):
			c.respondError(env, "port in use by another live aspect")
		default:
			c.respondError(env, err.Error())
		}
		return
	}

	c.registeredAs = state.Name
	c.sessionID = state.SessionID
	c.broker.dispatcher.bind(state.Name, c)

	if displacedSession != "" {
		c.log.Warn("aspect re-registered, displacing prior session",
			"name", state.Name,
			"prior_session", displacedSession,
			"new_session", state.SessionID)
	}
	c.log.Info("aspect registered via ws",
		"name", state.Name,
		"context_mode", state.ContextMode,
		"provider", state.Provider)

	ack, _ := frames.NewResponse(frames.KindRegisterAck, env.ID, frames.RegisterAckPayload{
		HeartbeatIntervalS: c.broker.cfg.HeartbeatIntervalS,
		StaleAfterS:        int(c.broker.cfg.StaleAfter.Seconds()),
	})
	c.send(ack)
}

// handleDeregisterFrame processes a graceful-shutdown deregister frame.
func (c *wsConn) handleDeregisterFrame(env frames.Envelope) {
	var payload frames.DeregisterPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.respondError(env, "deregister payload missing or malformed: "+err.Error())
		return
	}
	if payload.Name == "" || payload.SessionID == "" {
		c.respondError(env, "name and session_id required")
		return
	}
	if err := c.broker.roster.Deregister(payload.Name, payload.SessionID); err != nil {
		if errors.Is(err, roster.ErrSessionMismatch) {
			c.respondError(env, "session id does not match current registration")
			return
		}
		c.respondError(env, err.Error())
		return
	}

	c.log.Info("aspect deregistered via ws", "name", payload.Name, "reason", payload.Reason)
	// Unbind from dispatcher + clear the binding so cleanup() doesn't
	// try to deregister again.
	c.broker.dispatcher.unbind(payload.Name, c)
	c.registeredAs = ""
	c.sessionID = ""

	ack, _ := frames.NewResponse(frames.KindDeregister, env.ID, nil)
	c.send(ack)
}

// respondError sends a structured error response correlated to the
// incoming request. The kind becomes "<req.kind>.error" so clients
// distinguish error responses from legitimate same-kind responses.
func (c *wsConn) respondError(req frames.Envelope, msg string) {
	errKind := frames.Kind(string(req.Kind) + ".error")
	errPayload := map[string]string{"error": msg}
	env, err := frames.NewResponse(errKind, req.ID, errPayload)
	if err != nil {
		c.log.Error("failed to construct error response", "err", err)
		return
	}
	c.send(env)
}

// send serialises and writes a frame. Per-connection write mutex
// prevents torn frames if multiple goroutines end up writing in the
// future (not currently — all sends come from the serve goroutine —
// but cheap to hold the invariant).
func (c *wsConn) send(env frames.Envelope) {
	raw, err := frames.Encode(env)
	if err != nil {
		c.log.Error("frame encode failed", "kind", env.Kind, "err", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.log.Warn("frame write failed", "kind", env.Kind, "err", err)
	}
}

// cleanup runs when the connection goroutine exits. If an aspect was
// registered on this socket, deregister it so the roster reflects the
// disconnect, and unbind from the dispatcher.
func (c *wsConn) cleanup() {
	if c.registeredAs != "" && c.sessionID != "" {
		c.broker.dispatcher.unbind(c.registeredAs, c)
		if err := c.broker.roster.Deregister(c.registeredAs, c.sessionID); err != nil && !errors.Is(err, roster.ErrNotRegistered) {
			c.log.Warn("deregister on disconnect failed",
				"name", c.registeredAs, "err", err)
		} else {
			c.log.Info("aspect deregistered on disconnect", "name", c.registeredAs)
		}
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "connection ended")
}

// newConnID returns a short id for logging connection lifetimes. We
// use a frames ULID prefix — uniqueness isn't strictly required for a
// log correlator but the monotonic generator is handy and already in
// the codebase.
func newConnID() string {
	env, _ := frames.NewRequest(frames.KindRegister, nil)
	return env.ID[:10]
}
