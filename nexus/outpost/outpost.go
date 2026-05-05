// Package outpost implements the per-host relay process. Accepts
// local aspect WS connections, multiplexes them onto a single
// upstream WS to Nexus (or another Outpost in future deployments),
// forwards frames bidirectionally.
//
// Stateless beyond its in-memory aspect table: Nexus owns all
// persistent state (transport spec §8). On upstream reconnect, the
// Outpost replays `outpost.register` then forwards register frames
// for each currently-connected aspect so Nexus re-hydrates its
// roster.
//
// Hand dispatch queue + spawn mechanics arrive in part 7. This part
// is relay-only.
package outpost

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/wsclient"
)

// Config configures an Outpost.
type Config struct {
	// ListenAddr is where aspects connect locally, e.g. ":7950".
	ListenAddr string

	// UpstreamURL is the Nexus (or parent-Outpost) /connect endpoint.
	UpstreamURL string

	// AuthToken is the bearer token sent on the upward WS dial to
	// Nexus / parent Outpost (Authorization: Bearer).
	AuthToken string

	// AspectTokens maps inbound aspect-name → per-aspect bearer token
	// (#33). Each local aspect MUST authenticate with its own token,
	// not the upstream AuthToken. Pre-fix the same shared token
	// authenticated everything, so any aspect that reached the
	// outpost could register under any Name on Nexus.
	//
	// Empty map (or nil) falls back to legacy shared-token mode,
	// where AuthToken authenticates inbound aspects too — operators
	// migrating from the old setup can leave this nil during the
	// transition; new deployments should populate it.
	AspectTokens map[string]string

	// OutpostID identifies this Outpost to the Nexus. Usually the
	// hostname; must be stable across reconnects.
	OutpostID string

	// TLSCertFile / TLSKeyFile point at the PEM-encoded server cert
	// and key used by the local aspect-facing listener. Required —
	// the outpost has no plain-HTTP path. Operator decision (#9667).
	TLSCertFile string
	TLSKeyFile  string

	// OriginPatterns is the WebSocket Origin allowlist for inbound
	// /connect upgrades from local aspects. Mirrors broker.Config.OriginPatterns:
	// non-browser aspects (Go ws clients) connect freely, browser-based
	// callers must match this list. Empty list (default) = no browser
	// origins accepted.
	OriginPatterns []string

	// Logger is optional.
	Logger *slog.Logger
}

// Outpost is the running relay instance.
type Outpost struct {
	cfg Config
	log *slog.Logger

	// upstream is the WS client to Nexus.
	upstream *wsclient.Client

	// srv is the local aspect-facing HTTP server (for WS upgrades).
	srv *http.Server

	mu sync.Mutex
	// aspects holds the aspects currently connected to us. The key
	// is the registered aspect name; the value carries the local
	// connection + last seen register payload (for re-sync on
	// upstream reconnect).
	aspects map[string]*localAspect

	// inFlight maps an envelope ID we forwarded upstream → the local
	// aspect that originated it. When the broker's correlated
	// response arrives (InReplyTo == that ID) we route it back to
	// the originating aspect. Without this map the response would
	// fall through to handleDownstreamFrame's default case and the
	// aspect's Request would time out (#20).
	inFlight map[string]string
}

// localAspect tracks a locally-connected aspect's state.
type localAspect struct {
	conn            *websocket.Conn
	registerPayload frames.ForwardedRegisterPayload
	mu              sync.Mutex // serialises writes to conn
}

// New constructs an Outpost.
func New(cfg Config) (*Outpost, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("outpost: ListenAddr required")
	}
	if cfg.UpstreamURL == "" {
		return nil, errors.New("outpost: UpstreamURL required")
	}
	if cfg.OutpostID == "" {
		return nil, errors.New("outpost: OutpostID required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	o := &Outpost{
		cfg:      cfg,
		log:      log,
		aspects:  make(map[string]*localAspect),
		inFlight: make(map[string]string),
	}

	// Upstream WS client. Handler receives non-correlated inbound
	// frames from Nexus — these are downward-flowing frames destined
	// for local aspects (turn, hand.invoke, shutdown, etc.).
	ws, err := wsclient.New(wsclient.Config{
		URL:       cfg.UpstreamURL,
		AuthToken: cfg.AuthToken,
		Handler:   wsclient.HandlerFunc(o.handleDownstreamFrame),
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("outpost: upstream wsclient: %w", err)
	}
	o.upstream = ws
	return o, nil
}

// Run brings the Outpost up: starts the local listener, drives the
// upstream WS client (which dials and reconnects). Blocks until ctx
// is cancelled. Deregisters on clean shutdown.
func (o *Outpost) Run(ctx context.Context) error {
	if o.cfg.TLSCertFile == "" || o.cfg.TLSKeyFile == "" {
		return errors.New("outpost: TLSCertFile and TLSKeyFile required " +
			"(run `nexus cert init` to provision, then point --tls-cert / --tls-key at them)")
	}
	// Start the local aspect-facing listener.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /connect", o.handleConnect)
	o.srv = &http.Server{
		Addr:              o.cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	listenErrCh := make(chan error, 1)
	go func() {
		o.log.Info("outpost listening", "addr", o.cfg.ListenAddr)
		if err := o.srv.ListenAndServeTLS(o.cfg.TLSCertFile, o.cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
			listenErrCh <- err
		}
	}()

	// Run upstream + register loops under a derived context so any
	// shutdown trigger (caller ctx, listener error, upstream error)
	// cancels both goroutines. Without this, a listener-error branch
	// would call srv.Shutdown but block on <-upstreamErrCh until the
	// upstream's reconnect loop independently exited.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Upstream register-on-connect loop.
	registerDone := make(chan struct{})
	go o.upstreamRegisterLoop(runCtx, registerDone)

	// Upstream connect + reconnect.
	upstreamErrCh := make(chan error, 1)
	go func() {
		upstreamErrCh <- o.upstream.Run(runCtx)
	}()

	upstreamExited := false
	select {
	case <-runCtx.Done():
		o.log.Info("outpost stopping", "reason", runCtx.Err())
	case err := <-listenErrCh:
		o.log.Error("local listener failed", "err", err)
	case err := <-upstreamErrCh:
		o.log.Error("upstream exited", "err", err)
		upstreamExited = true
	}

	// Cancel the derived ctx so upstream + register loops exit if
	// they haven't already.
	runCancel()

	// Graceful shutdown: close aspect connections, deregister
	// upstream, stop listener.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = o.srv.Shutdown(shutdownCtx)

	_ = o.sendOutpostDeregister(shutdownCtx)

	if !upstreamExited {
		<-upstreamErrCh
	}
	<-registerDone
	return nil
}

// -------------------------------------------------------------------
// Upstream: Outpost → Nexus
// -------------------------------------------------------------------

// upstreamRegisterLoop watches for upstream connection transitions
// and sends outpost.register + replays all local aspect registrations
// each time a fresh connection comes up.
func (o *Outpost) upstreamRegisterLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	lastConnected := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		connected := o.upstream.Connected()
		if connected && !lastConnected {
			if err := o.sendOutpostRegister(ctx); err != nil {
				o.log.Warn("outpost.register failed", "err", err)
			} else {
				// Replay every aspect we're currently holding.
				o.mu.Lock()
				toReplay := make([]frames.ForwardedRegisterPayload, 0, len(o.aspects))
				for _, la := range o.aspects {
					toReplay = append(toReplay, la.registerPayload)
				}
				o.mu.Unlock()
				for _, payload := range toReplay {
					env, _ := frames.New(frames.KindRegister, payload)
					sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					if err := o.upstream.Send(sendCtx, env); err != nil {
						o.log.Warn("re-sync aspect register failed",
							"aspect", payload.Name, "err", err)
					}
					cancel()
				}
				o.log.Info("upstream reconnected; replayed aspects",
					"count", len(toReplay))
			}
		}
		lastConnected = connected
	}
}

// sendOutpostRegister tells Nexus "I'm an outpost, here I am."
func (o *Outpost) sendOutpostRegister(ctx context.Context) error {
	payload := frames.OutpostRegisterPayload{
		OutpostID: o.cfg.OutpostID,
		StartedAt: time.Now().UTC(),
	}
	env, err := frames.NewRequest(frames.KindOutpostRegister, payload)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := o.upstream.Request(reqCtx, env)
	if err != nil {
		return err
	}
	if resp.Kind != frames.KindOutpostRegisterAck {
		return fmt.Errorf("outpost.register: got kind %q", resp.Kind)
	}
	o.log.Info("registered with upstream", "outpost_id", o.cfg.OutpostID)
	return nil
}

// sendOutpostDeregister sends graceful shutdown to upstream.
func (o *Outpost) sendOutpostDeregister(ctx context.Context) error {
	env, err := frames.NewRequest(frames.KindOutpostDeregister, frames.OutpostDeregisterPayload{
		OutpostID: o.cfg.OutpostID,
		Reason:    "graceful shutdown",
	})
	if err != nil {
		return err
	}
	return o.upstream.Send(ctx, env)
}

// handleDownstreamFrame receives frames from upstream that aren't
// the outpost's own correlated responses (those go to wsclient's
// Request callers via pending). Routes by InReplyTo (correlated
// reply to a local aspect's request) or by Envelope.TargetAspect
// (unsolicited turn/dispatch from broker). Closes #20.
func (o *Outpost) handleDownstreamFrame(env frames.Envelope) {
	// Correlated reply: look up the originating aspect via inFlight.
	if env.InReplyTo != "" {
		o.mu.Lock()
		owner, ok := o.inFlight[env.InReplyTo]
		if ok {
			delete(o.inFlight, env.InReplyTo)
		}
		o.mu.Unlock()
		if ok {
			o.deliverToAspect(owner, env)
			return
		}
		// Fell through — log but don't broadcast; an uncorrelated
		// reply with no waiter is benign.
		o.log.Info("downstream reply with no in-flight owner; dropping",
			"in_reply_to", env.InReplyTo, "kind", env.Kind)
		return
	}

	// Unsolicited downstream: broker sets Envelope.TargetAspect for
	// turn / dispatch frames going to a specific aspect via outpost.
	if env.TargetAspect != "" {
		o.deliverToAspect(env.TargetAspect, env)
		return
	}

	// Lifecycle: shutdown broadcasts to every local aspect.
	if env.Kind == frames.KindShutdown {
		o.log.Info("shutdown frame from upstream; broadcasting to local aspects")
		o.broadcastShutdown(env)
		return
	}

	o.log.Info("downstream frame with no target; dropping",
		"kind", env.Kind, "id", env.ID)
}

// deliverToAspect writes a frame to the named local aspect's
// connection. Logs and drops if the aspect isn't connected — the
// broker's send semantics already cover absent aspects via
// ErrAspectNotConnected on its side; this is best-effort delivery.
func (o *Outpost) deliverToAspect(name string, env frames.Envelope) {
	o.mu.Lock()
	la, ok := o.aspects[name]
	o.mu.Unlock()
	if !ok {
		o.log.Warn("downstream frame for unknown aspect; dropping",
			"aspect", name, "kind", env.Kind, "id", env.ID)
		return
	}
	raw, err := frames.Encode(env)
	if err != nil {
		o.log.Error("encode downstream frame failed", "err", err)
		return
	}
	la.mu.Lock()
	defer la.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := la.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		o.log.Warn("write to aspect failed",
			"aspect", name, "kind", env.Kind, "err", err)
	}
}

// broadcastShutdown forwards a shutdown frame to every local aspect.
func (o *Outpost) broadcastShutdown(env frames.Envelope) {
	raw, err := frames.Encode(env)
	if err != nil {
		o.log.Error("encode shutdown failed", "err", err)
		return
	}
	o.mu.Lock()
	conns := make([]*localAspect, 0, len(o.aspects))
	for _, la := range o.aspects {
		conns = append(conns, la)
	}
	o.mu.Unlock()
	for _, la := range conns {
		la.mu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = la.conn.Write(ctx, websocket.MessageText, raw)
		cancel()
		la.mu.Unlock()
	}
}

// -------------------------------------------------------------------
// Downstream: aspects → Outpost
// -------------------------------------------------------------------

// handleConnect is the aspect-facing WS handler. Auth via Bearer
// header, upgrade, serve frames. Resolves the inbound identity
// from AspectTokens (preferred) or falls back to the shared
// AuthToken in legacy mode.
func (o *Outpost) handleConnect(w http.ResponseWriter, r *http.Request) {
	identity, ok := o.resolveInboundAuth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// See broker.handleConnect for the rationale; same Origin
		// allowlist policy applied here.
		OriginPatterns: o.cfg.OriginPatterns,
	})
	if err != nil {
		o.log.Warn("ws accept failed", "err", err)
		return
	}
	wsc.SetReadLimit(1 << 20)

	go o.serveAspect(wsc, identity)
}

// resolveInboundAuth checks the bearer header against the
// per-aspect AspectTokens map first, falling back to the shared
// AuthToken (legacy mode). Returns the resolved aspect name (empty
// in legacy mode) and whether auth succeeded. The constant-time
// compare on the prefix prevents trivial timing leaks; the body
// compares iterate the (small) per-aspect map identically to the
// broker's TokenStore pattern so a hit/miss has the same shape.
func (o *Outpost) resolveInboundAuth(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || subtle.ConstantTimeCompare([]byte(header[:len(prefix)]), []byte(prefix)) != 1 {
		return "", false
	}
	given := header[len(prefix):]
	givenBytes := []byte(given)

	// Per-aspect tokens (preferred path, #33).
	var hitAspect string
	var found int
	for aspect, tok := range o.cfg.AspectTokens {
		if subtle.ConstantTimeCompare(givenBytes, []byte(tok)) == 1 {
			hitAspect = aspect
			found = 1
		}
	}
	if found == 1 {
		return hitAspect, true
	}

	// Legacy shared-token fallback. Only honored when AspectTokens
	// is unset, so operators that have migrated can't have an
	// attacker reach back to legacy auth.
	if len(o.cfg.AspectTokens) == 0 && o.cfg.AuthToken != "" &&
		subtle.ConstantTimeCompare(givenBytes, []byte(o.cfg.AuthToken)) == 1 {
		return "", true // empty identity = legacy mode, register payload trusted as before
	}
	return "", false
}

// serveAspect is the per-aspect read loop. Pulls frames, identifies
// the aspect on first register, forwards everything upstream.
//
// `identity` is the resolved aspect name from AspectTokens auth
// (empty in legacy shared-token mode). When non-empty, register
// frames are rejected if payload.Name disagrees — closes #33 (an
// aspect with a stolen / shared token can't register under another
// aspect's name).
func (o *Outpost) serveAspect(wsc *websocket.Conn, identity string) {
	var aspectName string
	ctx := context.Background()

	defer func() {
		if aspectName != "" {
			o.mu.Lock()
			delete(o.aspects, aspectName)
			// Drop any in-flight correlation entries that point at
			// this aspect — the broker's response will arrive but
			// we have nowhere to deliver it.
			for id, owner := range o.inFlight {
				if owner == aspectName {
					delete(o.inFlight, id)
				}
			}
			o.mu.Unlock()
			o.log.Info("aspect disconnected from outpost", "aspect", aspectName)
		}
		_ = wsc.Close(websocket.StatusNormalClosure, "connection ended")
	}()

	for {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		msgType, data, err := wsc.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		if msgType != websocket.MessageText {
			continue
		}
		env, err := frames.Decode(data)
		if err != nil {
			o.log.Warn("aspect frame decode failed", "err", err)
			continue
		}

		// Register frames establish the aspect identity on this
		// connection and get stamped with via_outpost before
		// forwarding.
		if env.Kind == frames.KindRegister {
			var payload frames.RegisterPayload
			if err := frames.PayloadAs(env, &payload); err != nil {
				o.log.Warn("register payload parse failed", "err", err)
				continue
			}
			// #33: when running with per-aspect tokens, the connection's
			// resolved identity MUST match the register payload's name.
			// In legacy shared-token mode (identity == ""), payload.Name
			// is still trusted — same as before this PR — for
			// back-compat with deployments that haven't migrated.
			if identity != "" && payload.Name != identity {
				o.log.Warn("outpost: identity mismatch on register",
					"connection_identity", identity,
					"payload_name", payload.Name)
				_ = wsc.Close(websocket.StatusPolicyViolation,
					"register payload name does not match auth identity")
				return
			}
			aspectName = payload.Name
			forwarded := frames.ForwardedRegisterPayload{
				RegisterRequest: payload.RegisterRequest,
				ViaOutpostStamp: frames.ViaOutpostStamp{ViaOutpost: o.cfg.OutpostID},
			}
			o.mu.Lock()
			o.aspects[aspectName] = &localAspect{
				conn:            wsc,
				registerPayload: forwarded,
			}
			// Track the register envelope so the broker's register.ack
			// can be routed back to this aspect (#20). Also covers
			// re-registers on reconnect. Guard on env.ID != "" — a
			// malformed register with no ID would otherwise stomp on
			// a phantom inFlight[""] entry.
			if env.ID != "" {
				o.inFlight[env.ID] = aspectName
			}
			o.mu.Unlock()

			// Re-envelope with stamped payload, keep the same ID so
			// the ack correlates back to the aspect.
			outEnv, _ := frames.NewRequest(frames.KindRegister, forwarded)
			outEnv.ID = env.ID
			sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
			if err := o.upstream.Send(sendCtx, outEnv); err != nil {
				o.log.Warn("forward register upstream failed", "err", err)
			}
			sendCancel()
			continue
		}

		// Track inFlight only for kinds the broker actually responds
		// to. Fire-and-forget frames (chat.send, knowledge.store,
		// session.entry.appended, etc.) emit no correlated response,
		// so tracking them would accumulate one entry per emitted
		// frame for the lifetime of the connection. Reviewer-flagged
		// (PR-H#3) unbounded-growth class.
		if env.ID != "" && aspectName != "" && isRequestKind(env.Kind) {
			o.mu.Lock()
			o.inFlight[env.ID] = aspectName
			o.mu.Unlock()
		}

		// Forward verbatim. Nexus identifies the source aspect via
		// the wsConn mapping on its side (the register-bind we
		// forwarded above).
		sendCtx, sendCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := o.upstream.Send(sendCtx, env); err != nil {
			o.log.Warn("forward aspect frame upstream failed",
				"kind", env.Kind, "err", err)
		}
		sendCancel()
	}
}

// isRequestKind reports whether the broker is expected to send a
// correlated response for an aspect-originated frame of this kind.
// inFlight tracking is gated on this so fire-and-forget kinds (chat,
// knowledge, session entries, activity pulses) don't accumulate
// entries for the lifetime of the connection. Register frames take
// the explicit branch above and are tracked separately.
func isRequestKind(kind frames.Kind) bool {
	switch kind {
	case frames.KindTurn,
		frames.KindDispatch,
		frames.KindDeregister:
		return true
	}
	return false
}
