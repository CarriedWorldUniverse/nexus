// Package broker's WS surface. Handles the /connect endpoint where
// aspects (and, later, Outposts) open a persistent WebSocket and
// exchange frames per transport spec v0.1.
package broker

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/nexus/handqueue"
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

	// auth holds the TokenInfo resolved from the WS upgrade's bearer
	// token. Set once at accept time; never mutated for the life of
	// the connection. Used by dispatch handlers (and Drift D override
	// handlers) to enforce identity-mismatch and admin checks per
	// hand-dispatch v0.1 §5.3 and §5.4.
	auth TokenInfo
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
	// half-open WS that immediately closes. Resolve the bearer token
	// to a TokenInfo (per-aspect identity + admin flag) and stash on
	// the wsConn so subsequent frames can enforce identity per
	// hand-dispatch v0.1 §5.4.
	authInfo, ok := b.resolveUpgradeAuth(r)
	if !ok {
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
		log:         b.log.With("conn_id", connID, "agent_id", authInfo.AgentID),
		auth:        authInfo,
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

// resolveUpgradeAuth extracts the bearer from the upgrade request and
// resolves it to a TokenInfo via the broker's TokenStore. Returns
// (info, true) on a successful resolve; (zero, false) on missing or
// unknown token. Per-aspect tokens (minted via ReconcileAgentTokens)
// resolve to the aspect's identity; the legacy shared AuthToken — if
// configured — resolves to the Frame identity (admin=true) for
// back-compat with pre-drift-C callers.
func (b *Broker) resolveUpgradeAuth(r *http.Request) (TokenInfo, bool) {
	token := ExtractBearer(r.Header.Get("Authorization"))
	if token == "" {
		return TokenInfo{}, false
	}
	return b.cfg.Tokens.ResolveToken(token)
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
	case frames.KindDispatch:
		c.handleDispatchFrame(env)
	case frames.KindChatSend:
		c.handleChatSendFrame(env)
	case frames.KindChatRead:
		c.handleChatReadFrame(env)
	default:
		c.log.Info("frame kind not yet handled", "kind", env.Kind)
	}
}

// handleChatReadFrame answers a chat.read request — Lock 2 pull path.
// Aspects use this to fetch context they weren't pushed, without
// triggering a fresh deliberation cycle on the broker side.
//
// Server returns a chat.read.result with the messages oldest-first.
// SinceID gives pagination — if the caller has already seen messages
// up to N in the thread, sending SinceID=N returns only newer rows.
func (c *wsConn) handleChatReadFrame(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		// No store → empty result rather than hard error. The aspect
		// can read this as "nothing here" without crashing on a
		// malformed reply.
		empty, _ := frames.NewResponse(frames.KindChatReadResult, env.ID, frames.ChatReadResultPayload{})
		c.send(empty)
		return
	}
	var p frames.ChatReadPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("chat.read payload malformed", "err", err, "from", c.registeredAs)
		empty, _ := frames.NewResponse(frames.KindChatReadResult, env.ID, frames.ChatReadResultPayload{})
		c.send(empty)
		return
	}
	if p.ThreadID <= 0 {
		// chat.read without a thread is a no-op. The funnel calls this
		// with a thread id known via prior chat.deliver; calling
		// without one is most likely a model bug, not a real read
		// request — return empty and move on.
		empty, _ := frames.NewResponse(frames.KindChatReadResult, env.ID, frames.ChatReadResultPayload{})
		c.send(empty)
		return
	}

	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	const maxMessages = 200 // bounded so a thread with 5k entries doesn't slurp them all
	msgs, err := store.ListThread(ctx, p.ThreadID, p.SinceID, maxMessages)
	if err != nil {
		c.log.Warn("chat.read: store error",
			"thread", p.ThreadID, "since", p.SinceID, "err", err)
		empty, _ := frames.NewResponse(frames.KindChatReadResult, env.ID, frames.ChatReadResultPayload{})
		c.send(empty)
		return
	}

	out := make([]frames.ChatDeliverPayload, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, frames.ChatDeliverPayload{
			ID:         int(m.ID),
			From:       m.From,
			Content:    m.Content,
			ReplyTo:    int(m.ReplyTo),
			ReceivedAt: m.CreatedAt.UTC().Format(time.RFC3339),
			// Reason left empty — aspect knows it's a pull, not a push,
			// from the frame's response correlation. Replay=false
			// (this is a synchronous read, not Lock 6 replay).
		})
	}
	resp, _ := frames.NewResponse(frames.KindChatReadResult, env.ID, frames.ChatReadResultPayload{
		Messages: out,
	})
	c.send(resp)
}

// handleDispatchFrame enqueues the job on the broker's HandQueue,
// awaits the result, and writes a dispatch.result frame back to the
// requester correlated by the request's ID.
//
// Runs in a goroutine to avoid stalling the per-connection read loop
// during long-running dispatch execution (same pattern as the aspect's
// turn handler).
func (c *wsConn) handleDispatchFrame(env frames.Envelope) {
	go c.executeDispatch(env)
}

func (c *wsConn) executeDispatch(env frames.Envelope) {
	if c.broker.cfg.HandQueue == nil {
		c.sendDispatchError(env, "", "no_dispatcher", "broker has no HandQueue configured")
		return
	}

	var payload frames.DispatchPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.sendDispatchError(env, "", "bad_request", "dispatch payload malformed: "+err.Error())
		return
	}
	if payload.Aspect == "" {
		c.sendDispatchError(env, payload.DispatchID, "bad_request", "aspect required")
		return
	}

	// Identity enforcement per hand-dispatch v0.1 §5.4: caller's
	// resolved identity must match the dispatch's aspect field.
	// Frame (admin=true) is allowed to dispatch on behalf of any
	// aspect — Frame's coordination role can spawn workers under any
	// identity, mirroring agent-network's enforceIdentity carve-out
	// for the operator role. Aspects can only dispatch as themselves.
	if !c.auth.Admin && c.auth.AgentID != payload.Aspect {
		c.sendIdentityMismatch(env, payload.DispatchID, c.auth.AgentID, payload.Aspect)
		return
	}

	// The dispatcher owns the per-dispatch deadline (spec §5.5). Submit
	// blocks until the worker exits or the dispatcher's timeout fires.
	// We use Background here so the broker's caller context doesn't
	// abort an in-flight dispatch (per spec §6.4: caller cancellation
	// does not abort a dispatch — only Frame override or the timeout).
	result, err := c.broker.cfg.HandQueue.Submit(context.Background(), payload)
	if err != nil {
		// Translate structured queue errors into structured dispatch
		// errors. HardCeilingError carries the §6.3 fields.
		var hc *handqueue.HardCeilingError
		switch {
		case errors.As(err, &hc):
			c.sendHardCeiling(env, payload.DispatchID, hc)
		case errors.Is(err, handqueue.ErrQueueShutdown):
			c.sendDispatchError(env, payload.DispatchID, "shutdown", err.Error())
		default:
			// Dispatcher timeouts surface as a generic queue error
			// here; the worker-side timeout already posted a
			// structured timeout result on the originating thread.
			c.sendDispatchError(env, payload.DispatchID, "queue_error", err.Error())
		}
		return
	}

	resp, rerr := frames.NewResponse(frames.KindDispatchResult, env.ID, result)
	if rerr != nil {
		c.log.Error("build dispatch.result frame failed", "err", rerr)
		return
	}
	c.send(resp)
}

// sendIdentityMismatch is the structured 403 for a dispatch where the
// caller's resolved identity doesn't match the payload's aspect (per
// hand-dispatch v0.1 §5.4). The expected/claimed split is included in
// the reason text so debugging callers can see both sides without
// having to consult the broker logs.
func (c *wsConn) sendIdentityMismatch(req frames.Envelope, dispatchID, expected, claimed string) {
	c.log.Warn("dispatch identity mismatch",
		"expected", expected, "claimed", claimed, "dispatch_id", dispatchID)
	reason := "caller identity " + expected + " cannot dispatch as " + claimed
	c.sendDispatchError(req, dispatchID, "identity_mismatch", reason)
}

// sendHardCeiling is the §6.3 structured error: dispatch_rejected with
// reason=hard_ceiling, plus active / soft_cap / limit fields.
func (c *wsConn) sendHardCeiling(req frames.Envelope, dispatchID string, hc *handqueue.HardCeilingError) {
	c.log.Warn("dispatch hard_ceiling rejection",
		"dispatch_id", dispatchID, "active", hc.Active, "limit", hc.Limit)
	errPayload := frames.DispatchErrorPayload{
		DispatchID: dispatchID,
		Reason:     "hard_ceiling",
		Code:       "hard_ceiling",
		Active:     hc.Active,
		SoftCap:    hc.SoftCap,
		Limit:      hc.Limit,
	}
	env, err := frames.NewResponse(frames.KindDispatchError, req.ID, errPayload)
	if err != nil {
		c.log.Error("build dispatch.error frame failed", "err", err)
		return
	}
	c.send(env)
}

// sendDispatchError writes a dispatch.error response correlated to
// the dispatch request.
func (c *wsConn) sendDispatchError(req frames.Envelope, dispatchID, code, reason string) {
	errPayload := frames.DispatchErrorPayload{
		DispatchID: dispatchID,
		Reason:     reason,
		Code:       code,
	}
	env, err := frames.NewResponse(frames.KindDispatchError, req.ID, errPayload)
	if err != nil {
		c.log.Error("build dispatch.error frame failed", "err", err)
		return
	}
	c.send(env)
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

// handleChatSendFrame receives a chat.send frame from an aspect and
// routes it to the ChatRouter (P6 deliberation funnel). Fire-and-forget:
// the broker does not ack chat.send per the transport spec (chat is
// best-effort). When no ChatRouter is configured, log and drop (same
// behaviour as the previous "not yet handled" default).
func (c *wsConn) handleChatSendFrame(env frames.Envelope) {
	if c.broker.cfg.ChatRouter == nil || c.broker.cfg.ChatRouter.RouteChat == nil {
		c.log.Info("chat.send: no router configured, dropping", "from", c.registeredAs)
		return
	}
	var payload frames.ChatSendPayload
	if err := frames.PayloadAs(env, &payload); err != nil {
		c.log.Warn("chat.send payload malformed", "err", err, "from", c.registeredAs)
		return
	}

	// Assign the sender from the authenticated connection if the
	// payload doesn't carry one (aspects always set From, but defensive).
	from := payload.From
	if from == "" {
		from = c.registeredAs
	}

	// Derive a stable int64 msg id from the frame's ULID. ULIDs are
	// 26-char Crockford-base32 strings; the first ~10 chars are the
	// monotonic timestamp, so a string-prefix scheme would make
	// consecutive messages collide on their first 8 bytes. FNV-64a
	// over the full ULID gives proper distribution at negligible cost.
	var msgID int64
	if env.ID != "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte(env.ID))
		// hash sum is uint64; cast to int64 — sign doesn't matter for
		// equality checks, which is the only thing ThreadIndex does.
		msgID = int64(h.Sum64())
	}

	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go c.broker.cfg.ChatRouter.RouteChat(ctx, msgID, from, payload.Content,
		int64(payload.ReplyTo), payload.Topic)
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

	// Lock 6 replay: if the aspect's register frame carried a non-zero
	// since_msg_id, query chat history for messages addressed to this
	// aspect since the cursor and emit each as its own chat.deliver
	// with Replay=true. Goroutine so the read loop isn't blocked.
	// Failures are logged and do not propagate — replay is best-effort
	// per Lock 6's graceful-degradation framing.
	if payload.SinceMsgID > 0 && c.broker.cfg.Replayer != nil {
		go c.replayAddressedSince(state.Name, int64(payload.SinceMsgID))
	}
}

// replayAddressedSince walks the chat history forward from the
// supplied cursor and emits a chat.deliver frame for each message the
// recipient policy says should have been delivered to this aspect.
// Lock 6: replay frames carry Replay=true; otherwise identical shape
// to live delivery. ReceivedAt is the message's original ingestion
// time so the model can age-check on receipt.
func (c *wsConn) replayAddressedSince(aspect string, since int64) {
	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	msgs, err := c.broker.cfg.Replayer.AddressedSince(ctx, aspect, since)
	if err != nil {
		c.log.Warn("lock-6 replay failed",
			"aspect", aspect, "since", since, "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}
	c.log.Info("lock-6 replay starting",
		"aspect", aspect, "since", since, "count", len(msgs))
	for _, m := range msgs {
		env, err := frames.New(frames.KindChatDeliver, frames.ChatDeliverPayload{
			ID:         int(m.ID),
			From:       m.From,
			Content:    m.Content,
			ReplyTo:    int(m.ReplyTo),
			ReceivedAt: m.CreatedAt.UTC().Format(time.RFC3339),
			Reason:     "replay",
			Replay:     true,
		})
		if err != nil {
			c.log.Warn("lock-6 replay: build frame", "err", err)
			continue
		}
		c.send(env)
	}
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
