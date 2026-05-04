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

	// AuthToken is the bearer token used for BOTH directions:
	//   - sent on the upward WS dial as Authorization: Bearer
	//   - required from inbound aspect connections
	// v1 uses one shared token across the whole network; per-
	// component tokens are a v2 hardening step.
	AuthToken string

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
		cfg:     cfg,
		log:     log,
		aspects: make(map[string]*localAspect),
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

// handleDownstreamFrame receives frames from upstream that are NOT
// correlated responses (those go to wsclient's Request callers).
// Forwards to the appropriate local aspect based on frame type.
func (o *Outpost) handleDownstreamFrame(env frames.Envelope) {
	// For now the only downward-flowing frames that identify a
	// target aspect are turn + hand.invoke + shutdown. v1 implements
	// turn routing; part 7 adds hand. Shutdown broadcasts.
	switch env.Kind {
	case frames.KindTurn:
		// The Nexus router puts the target aspect name in the
		// InReplyTo path — actually no, turn isn't a response, it's
		// initiated. We'd route by extracting target from payload,
		// but TurnPayload doesn't carry one (the agent handles the
		// connection it's on). Without a name in the frame the
		// Outpost can't route — this is a spec gap that'll be
		// resolved when dispatch takes shape in part 7.
		o.log.Info("turn frame received from upstream — routing not yet implemented", "id", env.ID)
	case frames.KindShutdown:
		o.log.Info("shutdown frame from upstream; broadcasting to local aspects")
		o.broadcastShutdown(env)
	default:
		o.log.Info("downstream frame kind not handled", "kind", env.Kind)
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
// header, upgrade, serve frames.
func (o *Outpost) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !o.authCheckHeader(r) {
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

	go o.serveAspect(wsc)
}

func (o *Outpost) authCheckHeader(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || subtle.ConstantTimeCompare([]byte(header[:len(prefix)]), []byte(prefix)) != 1 {
		return false
	}
	given := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(given), []byte(o.cfg.AuthToken)) == 1
}

// serveAspect is the per-aspect read loop. Pulls frames, identifies
// the aspect on first register, forwards everything upstream.
func (o *Outpost) serveAspect(wsc *websocket.Conn) {
	var aspectName string
	ctx := context.Background()

	defer func() {
		if aspectName != "" {
			o.mu.Lock()
			delete(o.aspects, aspectName)
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

		// All other frames forward verbatim. Nexus identifies the
		// source aspect via the wsConn mapping on its side — that's
		// the register-bind we just forwarded.
		sendCtx, sendCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := o.upstream.Send(sendCtx, env); err != nil {
			o.log.Warn("forward aspect frame upstream failed",
				"kind", env.Kind, "err", err)
		}
		sendCancel()
	}
}
