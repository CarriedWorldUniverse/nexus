// Package broker's WS surface. Handles the /connect endpoint where
// aspects (and, later, Outposts) open a persistent WebSocket and
// exchange frames per transport spec v0.1.
package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/handqueue"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/sessions"
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

	// subs guards the operator subscription flags below. Only meaningful
	// on operator connections (auth.Operator true); on aspect conns the
	// flags stay false and the fields are unread. Subscription state is
	// pure in-memory: WS close = subs gone, reconnect = SPA replays its
	// subscribes (per dashboard-ws-port spec §6.2). subMu protects the
	// flags + topic filter; readers under RLock are the fan-out loops in
	// chat_send.go / register/cleanup; writers under Lock are the
	// subscribe/unsubscribe handlers in operator_subs.go.
	subMu                  sync.RWMutex
	subscribedChat         bool
	subscribedRoster       bool
	subscribedAspectStatus bool

	// host is the source IP (without port) extracted from RemoteAddr
	// at accept time. Used to release the per-IP connection slot in
	// cleanup; immutable after construction.
	host string

	// badFrameCount is the run of consecutive frame-decode failures
	// on this connection. Resets to zero on every successful decode.
	// When it reaches Config.MaxConsecutiveBadFrames the connection
	// is closed (#34). Single-reader (the serve loop) so no lock.
	badFrameCount int
}

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
	// Reserve against the connection caps (#25). Reject before WS
	// upgrade if the global or per-IP limit is reached so an attacker
	// can't exhaust file descriptors / goroutines by opening
	// connections faster than they close.
	reserved, host := b.reserveConn(r.RemoteAddr)
	if !reserved {
		b.log.Warn("connection cap reached; refusing /connect",
			"remote", r.RemoteAddr,
			"agent_id", authInfo.AgentID)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	// Per #31: every legacy-master /connect emits a WARN with source +
	// resolved identity so operators see migration progress. Connect
	// events are rare (aspects connect once, stay connected), so the
	// noise is bounded; once all aspects have rotated to per-aspect
	// tokens, AllowLegacyMaster gets flipped off and the line goes
	// silent.
	if authInfo.ViaLegacy {
		b.log.Warn("legacy master token resolve",
			"remote", r.RemoteAddr,
			"resolved_as", authInfo.AgentID,
			"hint", "rotate to per-aspect tokens; then set NEXUS_ALLOW_LEGACY_MASTER=0")
	}

	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin allowlist gates browser-based callers (Origin header
		// present). Non-browser aspects (Go ws clients) don't send an
		// Origin and connect freely; bearer-token auth still gates
		// them at line 49 above. UI surfaces are aspects too — they
		// authenticate the same way, and their Origin must match this
		// list. Pre-#22 the broker set InsecureSkipVerify=true, which
		// allowed any browser-tab JS on the same machine to dial the
		// broker if it could reach the local socket. See Config.OriginPatterns.
		OriginPatterns: b.cfg.OriginPatterns,
	})
	if err != nil {
		b.log.Warn("ws accept failed", "err", err)
		b.releaseConn(host)
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
		host:        host,
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
	// Operator connections register here (not via the register frame
	// — operators don't go through that path, they have no aspect
	// identity to bind into the dispatcher). Cleanup unbinds.
	if c.auth.Operator {
		b.bindOperator(c)
	}
	go c.serve(parentCtx)
}

// resolveUpgradeAuth extracts the bearer from the upgrade request and
// resolves it to a TokenInfo. Two paths in order:
//
//  1. TokenStore lookup. Per-aspect tokens (minted via
//     ReconcileAgentTokens) resolve to the aspect's identity; the
//     legacy shared AuthToken — if configured — resolves to the
//     Frame identity (admin=true) for back-compat with pre-drift-C
//     callers.
//
//  2. Operator JWT verification (5b2). When the TokenStore misses
//     and OperatorLogin is configured, try jwt.Verify against the
//     SessionSigningSecret. On valid signature + sub:"operator",
//     return a TokenInfo with AgentID:"operator", Admin:true,
//     Operator:true. This is the path the dashboard SPA uses after
//     a passkey login — the JWT is short-lived (1h TTL) and signed
//     by the same secret as aspect JWTs.
//
//     Per the Kfv forward-risk invariant in operator_login.go:
//     operator JWTs carry Kfv:0. This code path does NOT enforce
//     any Kfv check. Adding Kfv enforcement here in the future MUST
//     guard the check with `claims.Sub != "operator"` — operators
//     have no keyfile rotation, the passkey IS the long-term
//     credential.
//
// Returns (info, true) on success; (zero, false) on missing, unknown,
// or unverifiable token.
func (b *Broker) resolveUpgradeAuth(r *http.Request) (TokenInfo, bool) {
	// Authorization header is the primary source. Browser WebSocket API
	// can't set custom headers on the upgrade, so fall back to a `token`
	// query parameter for browser-driven operator clients (SPA, smoke-
	// test page). Aspect binaries always use the header.
	token := ExtractBearer(r.Header.Get("Authorization"))
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token != "" {
		if info, ok := b.cfg.Tokens.ResolveToken(token); ok {
			return info, true
		}
		// JWT fallback for operator tokens.
		if info, ok := b.tryVerifyOperatorJWT(token); ok {
			return info, true
		}
		// JWT fallback for aspect keyfile-issued tokens. Aspects use
		// /api/aspect/validate to exchange their keyfile for a session
		// JWT (same signing secret, sub=aspect_name). Without this
		// path the keyfile flow only works for the operator login —
		// aspect WS upgrades 401. Surfaced on 2026-05-11 cutover when
		// anvil's first agentfunnel boot couldn't dial.
		if info, ok := b.tryVerifyAspectJWT(token); ok {
			return info, true
		}
	}
	// Operator auth bypass (dev-only knob). Token-presenting paths above
	// already covered aspect-token connections; if we got here without
	// a token (or with one that didn't resolve), accept as operator
	// when the bypass is on. Logged at INFO so the trail is visible.
	if b.cfg.OperatorAuthBypass {
		if b.cfg.Logger != nil {
			b.cfg.Logger.Info("operator auth bypass: accepting connection without verified token",
				"remote", r.RemoteAddr, "had_token", token != "")
		}
		return TokenInfo{
			AgentID:  "operator",
			Admin:    true,
			Operator: true,
		}, true
	}
	return TokenInfo{}, false
}

// tryVerifyOperatorJWT attempts to verify the bearer as an operator
// JWT. Returns (info, true) on a valid operator JWT; (zero, false)
// on any failure (no secret configured, bad signature, expired,
// non-operator sub). Failures are silent — the WS handler treats
// the result the same as a TokenStore miss and returns 401.
func (b *Broker) tryVerifyOperatorJWT(token string) (TokenInfo, bool) {
	ol := b.cfg.OperatorLogin
	if ol == nil || len(ol.SessionSigningSecret) == 0 {
		return TokenInfo{}, false
	}
	now := time.Now()
	if ol.Now != nil {
		now = ol.Now()
	}
	claims, err := jwt.Verify(ol.SessionSigningSecret, token, now)
	if err != nil {
		return TokenInfo{}, false
	}
	if claims.Sub != "operator" {
		return TokenInfo{}, false
	}
	return TokenInfo{
		AgentID:  "operator",
		Admin:    true,
		Operator: true,
	}, true
}

// tryVerifyAspectJWT attempts to verify the bearer as an aspect
// session JWT (issued by /api/aspect/validate from a keyfile).
// Returns (info, true) when the JWT signs against the operator-shared
// signing secret AND sub names a known aspect. The aspect doesn't
// get the Operator/Admin flags — those are for the operator session;
// aspects get AgentID=<aspect_name> and dispatch through the aspect
// switch in ws.go.
//
// Failures are silent; the WS handler treats this as a TokenStore
// miss and returns 401, same as any other unrecognized bearer.
func (b *Broker) tryVerifyAspectJWT(token string) (TokenInfo, bool) {
	ol := b.cfg.OperatorLogin
	if ol == nil || len(ol.SessionSigningSecret) == 0 {
		return TokenInfo{}, false
	}
	now := time.Now()
	if ol.Now != nil {
		now = ol.Now()
	}
	claims, err := jwt.Verify(ol.SessionSigningSecret, token, now)
	if err != nil {
		return TokenInfo{}, false
	}
	if claims.Sub == "" || claims.Sub == "operator" {
		// "operator" goes through the operator path; empty sub is
		// malformed.
		return TokenInfo{}, false
	}
	// Sub names an aspect. Trust the signature (same secret only the
	// validate endpoint can sign with) — no extra DB lookup. If the
	// aspect was retired post-mint, the next keyfile re-validation
	// will fail; this WS upgrade remains valid for the JWT's TTL,
	// which is the design per docs/2026-05-08-nexus-resident-personality-spec.
	return TokenInfo{
		AgentID:  claims.Sub,
		Admin:    false,
		Operator: false,
	}, true
}

// serve runs the per-connection read loop. Exits when the connection
// closes or the parent context is cancelled. On exit, deregisters
// any aspect bound to this connection so the roster reflects the
// disconnect immediately.
func (c *wsConn) serve(parentCtx context.Context) {
	defer c.cleanup()

	c.log.Info("ws connection accepted")

	// Liveness detection: send a server-side ping every 30s. coder/
	// websocket handles pong correlation internally; if the peer doesn't
	// reply within the per-ping timeout, Ping returns an error and we
	// close. This catches dead half-open connections that no longer
	// flow data either direction. The read loop below blocks on data
	// frames (no per-frame timeout) — idle aspects that register and
	// wait for dispatch don't trip a deadline just because they're
	// quiet.
	pingCtx, pingCancel := context.WithCancel(parentCtx)
	defer pingCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				if err := c.conn.Ping(ctx); err != nil {
					cancel()
					c.log.Info("ws ping failed; closing", "err", err)
					_ = c.conn.Close(websocket.StatusGoingAway, "ping failed")
					return
				}
				cancel()
			}
		}
	}()

	for {
		// Read with parent context only — no per-frame deadline. Idle
		// aspects don't trip a timeout; ping goroutine above catches
		// dead connections.
		msgType, data, err := c.conn.Read(parentCtx)
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
			c.badFrameCount++
			c.log.Warn("frame decode failed",
				"err", err,
				"consecutive", c.badFrameCount)
			// Tolerate isolated garbage frames so a legitimate peer
			// with a single buggy send recovers on its next frame.
			// But cap the run: an attacker streaming malformed 1 MiB
			// frames (#34) gets cut off after MaxConsecutiveBadFrames
			// so they can't burn CPU + bandwidth indefinitely.
			if c.badFrameCount >= c.broker.cfg.MaxConsecutiveBadFrames {
				c.log.Warn("bad-frame cap reached; closing connection",
					"count", c.badFrameCount)
				_ = c.conn.Close(websocket.StatusPolicyViolation, "too many bad frames")
				return
			}
			continue
		}
		c.badFrameCount = 0

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
//
// INVARIANT — operator-only frame kinds: the kinds dispatched by
// dispatchOperatorFrame (roster.list, chat.list, chat.replies,
// chat.reactions.fetch, knowledge.{list,search,store}, aspect.say)
// are accepted ONLY from operator connections. An aspect connection
// sending one of these kinds falls through this switch's default
// branch and is silently dropped (logged at info level). This is
// intentional: aspects access knowledge + chat reads via the
// bridle MCP tool surface (Crossing Parts 3+4), not direct WS
// frames. The dispatchOperatorFrame call below preempts the switch
// when c.auth.Operator is true; aspects never reach the operator
// handler. If a future kind ever needs both an operator AND aspect
// path, add an explicit case in this switch — don't rely on
// fall-through to the operator dispatch.
func (c *wsConn) dispatch(env frames.Envelope) {
	// Dashboard SPA frames first — fires only when the connection
	// resolved as an operator (c.auth.Operator true). Aspects fall
	// through to the existing switch below.
	if c.dispatchOperatorFrame(env) {
		return
	}
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
	case frames.KindChatReaction:
		c.handleChatReactionFrame(env)
	case frames.KindAnnounceFile:
		c.handleAnnounceFileFrame(env)
	case frames.KindShareFile:
		c.handleShareFileFrame(env)
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
	// Per ChatReadPayload doc: "request for a specific message or thread".
	// Fall back to MsgID when ThreadID is unset — ListThread walks descendants
	// from the given node, so a leaf returns just itself, a root returns the
	// whole subtree. Either is a meaningful read.
	rootID := p.ThreadID
	if rootID <= 0 {
		rootID = int64(p.MsgID)
	}
	if rootID <= 0 {
		empty, _ := frames.NewResponse(frames.KindChatReadResult, env.ID, frames.ChatReadResultPayload{})
		c.send(empty)
		return
	}

	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	const maxMessages = 200 // bounded so a thread with 5k entries doesn't slurp them all
	msgs, err := store.ListThread(ctx, rootID, p.SinceID, maxMessages)
	if err != nil {
		c.log.Warn("chat.read: store error",
			"thread", rootID, "since", p.SinceID, "err", err)
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
		// errors. HardCeilingError + QueueFullError carry the §6.3
		// fields the caller needs to decide whether to retry, fan out
		// to spillover, or surface to the operator.
		var hc *handqueue.HardCeilingError
		var qfe *handqueue.QueueFullError
		switch {
		case errors.As(err, &hc):
			c.sendHardCeiling(env, payload.DispatchID, hc)
		case errors.As(err, &qfe):
			// queue_full is a distinct caller-facing condition from
			// hard_ceiling: pending FIFO is at MaxQueueDepth. Surface
			// depth/limit so the caller can backpressure.
			c.sendDispatchError(env, payload.DispatchID, "queue_full",
				fmt.Sprintf("queue depth %d at limit %d", qfe.Depth, qfe.Limit))
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
// hands it to the canonical Broker.HandleChatSend path: persist →
// route → fan out → trigger Frame deliberation. Fire-and-forget: the
// broker does not ack chat.send per the transport spec (chat is
// best-effort). Errors are logged.
func (c *wsConn) handleChatSendFrame(env frames.Envelope) {
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

	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := c.broker.HandleChatSend(ctx, from, payload.Content,
		int64(payload.ReplyTo), payload.Topic); err != nil {
		c.log.Warn("chat.send: handler error", "err", err, "from", from)
	}
}

// handleChatReactionFrame toggles an emoji reaction on a chat message.
// Accepts the frame from any authenticated connection (operator or
// aspect). Reactions are toggle-semantics — same (msg, reactor, emoji)
// triple twice = react, then unreact. The reactor is taken from the
// authenticated connection identity, never trusting the payload's
// `from` field for cross-identity spoofing protection. Fire-and-forget:
// no response frame; downstream observers learn via reaction.applied
// fan-out (when wired).
func (c *wsConn) handleChatReactionFrame(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		c.log.Warn("chat.reaction: no chat store configured")
		return
	}
	var p frames.ChatReactionPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.log.Warn("chat.reaction payload malformed", "err", err, "from", c.registeredAs)
		return
	}
	if p.MsgID <= 0 || p.Emoji == "" {
		c.log.Warn("chat.reaction missing required fields",
			"msg_id", p.MsgID, "emoji", p.Emoji, "from", c.registeredAs)
		return
	}
	reactor := c.registeredAs
	if reactor == "" {
		reactor = p.From // fall back only if the connection has no name (test paths)
	}
	if reactor == "" {
		c.log.Warn("chat.reaction: no reactor identity")
		return
	}
	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	reacted, err := store.ToggleReaction(ctx, int64(p.MsgID), reactor, p.Emoji)
	if err != nil {
		c.log.Warn("chat.reaction: store error",
			"msg_id", p.MsgID, "reactor", reactor, "emoji", p.Emoji, "err", err)
		return
	}

	// Broadcast the full current reactions for this msg to subscribed
	// operators. Without this fan-out the SPA only sees reaction state
	// via chat.reactions.fetch (called on page load and reconnect) —
	// reactions emitted by other clients during a live session never
	// reach the UI until the next refetch. Surfaced 2026-05-12 when
	// plumb reacted from little-blue and the dashboard never showed it.
	all, fetchErr := store.GetReactions(ctx, []int64{int64(p.MsgID)})
	if fetchErr != nil {
		c.log.Warn("chat.reaction: GetReactions after toggle",
			"msg_id", p.MsgID, "err", fetchErr)
		return
	}
	current := all[int64(p.MsgID)]
	rows := make([]frames.ReactionRow, 0, len(current))
	for _, r := range current {
		rows = append(rows, frames.ReactionRow{Aspect: r.Aspect, Emoji: r.Emoji})
	}
	op := "removed"
	if reacted {
		op = "added"
	}
	c.broker.broadcastChatReactionUpdate(frames.ChatReactionUpdatePayload{
		MsgID:     p.MsgID,
		Reactor:   reactor,
		Emoji:     p.Emoji,
		Op:        op,
		Reactions: rows,
	})
}

// handleAnnounceFileFrame creates a chat post announcing a file plus
// a linked shared_files row. Responds with file.result carrying the
// new chat msg_id so the caller can reference the announce in
// subsequent frames (replies, reactions). The sender is taken from
// the authenticated connection — payload.From is informational only.
func (c *wsConn) handleAnnounceFileFrame(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		c.respondError(env, "chat store not configured")
		return
	}
	var p frames.AnnounceFilePayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.respondError(env, "announce_file payload malformed: "+err.Error())
		return
	}
	if p.Path == "" {
		c.respondError(env, "announce_file: path required")
		return
	}
	from := c.registeredAs
	if from == "" {
		from = p.From
	}
	if from == "" {
		c.respondError(env, "announce_file: no sender identity")
		return
	}
	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	msgID, _, err := store.AnnounceSharedFile(ctx, from, p.Path, p.Description)
	if err != nil {
		c.respondError(env, "announce_file: store error: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindFileResult, env.ID, frames.FileResultPayload{
		MsgID: msgID,
	})
	c.send(resp)
}

// handleShareFileFrame records a direct file share to recipients
// without producing a chat post. Responds with file.result carrying
// the new share_id. Sender taken from the authenticated connection.
func (c *wsConn) handleShareFileFrame(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		c.respondError(env, "chat store not configured")
		return
	}
	var p frames.ShareFilePayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.respondError(env, "share_file payload malformed: "+err.Error())
		return
	}
	if p.Path == "" {
		c.respondError(env, "share_file: path required")
		return
	}
	from := c.registeredAs
	if from == "" {
		from = p.From
	}
	if from == "" {
		c.respondError(env, "share_file: no sender identity")
		return
	}
	ctx := c.broker.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	shareID, err := store.ShareFile(ctx, from, p.Path, p.Recipients)
	if err != nil {
		c.respondError(env, "share_file: store error: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindFileResult, env.ID, frames.FileResultPayload{
		ShareID: shareID,
	})
	c.send(resp)
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

	// #21: derive home from broker discovery, not from the payload.
	// payload.Home is informational only; if it disagrees with the
	// discovered home, the discovered home wins and we log. If the
	// aspect name isn't in the discovery map, fall back to payload
	// (legacy deployments without --aspect-dir) but warn.
	if c.broker.cfg.AspectHomes != nil {
		if discovered, ok := c.broker.cfg.AspectHomes[payload.Name]; ok {
			if payload.Home != "" && payload.Home != discovered {
				c.log.Warn("register payload.home overridden by broker discovery",
					"name", payload.Name,
					"payload_home", payload.Home,
					"discovered_home", discovered)
			}
			payload.Home = discovered
		} else {
			c.log.Warn("register: aspect not in discovery map; trusting payload.home (legacy path)",
				"name", payload.Name, "payload_home", payload.Home)
		}
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

	// Push a roster.update to subscribed operators. The dashboard's
	// Status / Agents views render the row from this delta without
	// having to re-fetch the full roster on every aspect connect.
	c.broker.broadcastRosterUpdate(frames.RosterUpdatePayload{
		Aspect:       state.Name,
		Status:       state.Status,
		Capabilities: state.Capabilities,
		Model:        state.Model,
		Provider:     state.Provider,
		ContextMode:  string(state.ContextMode),
		Reason:       "connect",
	})

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
	// Identity enforcement: a caller may only deregister itself, unless
	// admin (Frame role). Without this check, any authenticated peer
	// could DoS another aspect by guessing/observing its session_id —
	// session_ids leak via projection rows and prior register acks.
	// Mirrors the dispatch identity check at handleDispatchFrame; the
	// admin carve-out is deliberate parity with that handler and will
	// be retired together when the per-aspect please-dispatch relay
	// model lands (PR-A3).
	if !c.auth.Admin && c.auth.AgentID != payload.Name {
		c.respondError(env, "identity_mismatch: caller "+c.auth.AgentID+
			" cannot deregister "+payload.Name)
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
	// Operator unbind first — independent of aspect-side roster
	// bookkeeping. unbindOperator is a no-op when c was never
	// bound (non-operator conn).
	if c.auth.Operator {
		c.broker.unbindOperator(c)
	}
	if c.registeredAs != "" && c.sessionID != "" {
		c.broker.dispatcher.unbind(c.registeredAs, c)
		deregErr := c.broker.roster.Deregister(c.registeredAs, c.sessionID)
		if deregErr != nil && !errors.Is(deregErr, roster.ErrNotRegistered) {
			c.log.Warn("deregister on disconnect failed",
				"name", c.registeredAs, "err", deregErr)
		} else {
			c.log.Info("aspect deregistered on disconnect", "name", c.registeredAs)
		}
		// Push roster.update reason="disconnect" to subscribed
		// operators so the dashboard removes the row from Status /
		// Agents without polling the roster.
		c.broker.broadcastRosterUpdate(frames.RosterUpdatePayload{
			Aspect: c.registeredAs,
			Status: "down",
			Reason: "disconnect",
		})
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "connection ended")
	// Release the connection-cap slots reserved at handleConnect.
	c.broker.releaseConn(c.host)
}

// newConnID returns a short id for logging connection lifetimes. We
// use a frames ULID prefix — uniqueness isn't strictly required for a
// log correlator but the monotonic generator is handy and already in
// the codebase.
func newConnID() string {
	env, _ := frames.NewRequest(frames.KindRegister, nil)
	return env.ID[:10]
}
