// Package broker serves the Nexus WS and HTTP surface. Per transport
// spec v0.1 §10, the bulk of inter-component traffic runs over the
// WS endpoint at /connect (see ws.go). This file keeps the HTTP bits
// that remain legitimately HTTP: /health (external monitoring) and
// /api/aspects (dashboard convenience — authoritative roster state
// is the WS-driven in-memory map).
//
// Business logic lives in nexus/roster; this package is transport.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nexus-cw/nexus/nexus/handqueue"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/nexus/sessions"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// Config configures a Broker.
type Config struct {
	Addr string // host:port, e.g. ":7888"

	// AuthToken is the LEGACY shared bearer token. Pre-drift-C, every
	// caller used this single token and the broker trusted whoever
	// presented it. Post-drift-C the broker resolves per-aspect tokens
	// via the TokenStore (hand-dispatch v0.1 §5.3, §5.4); AuthToken is
	// retained as a back-compat shim — when set, presenting it
	// resolves to the Frame identity (admin=true). Leave empty in new
	// deployments; populate per-aspect tokens via TokenStore instead.
	AuthToken string

	// Tokens carries the per-aspect bearer tokens and admin flags
	// resolved from agent_tokens. Required for per-aspect identity
	// resolution; if nil, the broker constructs an empty store and
	// only the legacy AuthToken (if set) will authenticate. The
	// caller (cmd/nexus) is responsible for calling
	// ReconcileAgentTokens / ReconcileFrameToken before
	// ListenAndServe.
	Tokens *TokenStore

	HeartbeatIntervalS int           // value returned to aspects on register
	StaleAfter         time.Duration // aspect becomes "stale" after this gap
	Logger             *slog.Logger

	// Projection receives session.entry.appended frames from aspects.
	// Optional — if nil, the broker logs and drops session-projection
	// frames instead of persisting (useful for tests that don't need
	// a DB).
	Projection *sessions.Projection

	// HandQueue dispatches `dispatch` frames. Optional — if nil,
	// the broker responds with a dispatch.error indicating no
	// dispatcher configured. (Field name is legacy; the queue
	// implements the generic dispatch protocol.)
	HandQueue *handqueue.Queue

	// Admin wires the embedded Frame's admin-action callbacks (#79
	// lock — REST-only admin surface). When nil, the /api/admin/*
	// endpoints are not registered. P5 supplies these from the
	// EmbeddedFrame; pre-§6.5 deployments without a Frame leave Admin
	// nil and lose the admin surface (correct — no Frame = no admin).
	Admin *AdminCallbacks
}

// Broker owns the HTTP server and its roster.
type Broker struct {
	cfg    Config
	roster *roster.Roster
	srv    *http.Server
	log    *slog.Logger

	// ctx drives the lifetime of WS goroutines. Set in ListenAndServe
	// from the caller's context; cancelled when ListenAndServe returns
	// so detached WS serve-goroutines tear down during graceful
	// shutdown (not just when the OS drops the TCP connection).
	ctx       context.Context
	ctxCancel context.CancelFunc

	// dispatcher is the server-side request/response API: tracks
	// which wsConn holds each aspect name, and delivers correlated
	// response frames. Used by SendTurn (and later SendHand etc).
	dispatcher *Dispatcher

	// adminOps tracks in-flight long-running admin operations
	// (shutdown/compact/rewind). Lazily allocated by registerAdmin.
	adminOps *adminOpStore
}

func New(cfg Config, r *roster.Roster) *Broker {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HeartbeatIntervalS == 0 {
		cfg.HeartbeatIntervalS = 10
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = 30 * time.Second
	}
	// Always have a usable TokenStore. If the caller didn't provide
	// one (older test paths), construct an empty store. Legacy
	// AuthToken — when set — is registered as the master-fallback so
	// pre-drift-C tests and callers continue to authenticate as the
	// Frame identity (admin=true) without per-aspect minting.
	if cfg.Tokens == nil {
		cfg.Tokens = NewTokenStore()
	}
	if cfg.AuthToken != "" {
		cfg.Tokens.SetLegacyMaster(cfg.AuthToken)
	}
	return &Broker{cfg: cfg, roster: r, log: cfg.Logger, dispatcher: newDispatcher()}
}

// ListenAndServe blocks serving the broker until the context is cancelled.
// v1 uses plain HTTP for local dev; TLS wiring lands alongside the first
// real aspect invocation.
func (b *Broker) ListenAndServe(ctx context.Context) error {
	b.ctx, b.ctxCancel = context.WithCancel(ctx)
	defer b.ctxCancel()

	mux := http.NewServeMux()
	// WS surface per transport spec v0.1 — see ws.go. Auth is checked
	// inside handleConnect before upgrade so bad tokens get clean 401s.
	mux.HandleFunc("GET /connect", b.handleConnect)
	// HTTP surface that stays per spec §10: dashboard convenience +
	// external monitoring.
	mux.Handle("GET /api/aspects", b.auth(http.HandlerFunc(b.handleList)))
	mux.HandleFunc("GET /health", b.handleHealth)

	// Admin REST surface (#79 lock). Registered only when a Frame is
	// embedded and supplies AdminCallbacks. Per spec §3.3, admin ops
	// belong to the Frame because the Frame IS the Nexus.
	b.registerAdmin(mux)

	b.srv = &http.Server{
		Addr:              b.cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		b.log.Info("broker listening", "addr", b.cfg.Addr)
		if err := b.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return b.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// auth rejects any request whose bearer token does not resolve to a
// known identity. The resolved TokenInfo is stashed on the request
// context so handlers downstream can read it via authUserFromContext.
// Health is left unauthenticated so process supervisors can poll it.
func (b *Broker) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ExtractBearer(r.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		info, ok := b.cfg.Tokens.ResolveToken(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r.WithContext(withAuthUser(r.Context(), info)))
	})
}

// authUserCtxKey is the unexported context key under which the
// resolved TokenInfo is stored. Exported only via withAuthUser /
// AuthUserFromContext helpers below.
type authUserCtxKey struct{}

// withAuthUser returns a copy of ctx carrying the TokenInfo.
func withAuthUser(ctx context.Context, info TokenInfo) context.Context {
	return context.WithValue(ctx, authUserCtxKey{}, info)
}

// AuthUserFromContext extracts the TokenInfo a previous auth pass
// installed on the request context. Returns (zero, false) if absent.
// Drift D's override handlers will use this to gate admin-only ops.
func AuthUserFromContext(ctx context.Context) (TokenInfo, bool) {
	v, ok := ctx.Value(authUserCtxKey{}).(TokenInfo)
	return v, ok
}


func (b *Broker) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"aspects": b.roster.List(),
	})
}

func (b *Broker) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func validateRegister(req *schemas.RegisterRequest) error {
	if req.Name == "" {
		return errors.New("name required")
	}
	if req.SessionID == "" {
		return errors.New("session_id required")
	}
	switch req.ContextMode {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		return errors.New("context_mode must be one of: global, thread, stateless")
	}
	if req.Provider == "" {
		return errors.New("provider required")
	}
	// Port used to be required (HTTP-era: broker needed it to route
	// back to the aspect). Under the WS transport, aspects dial out
	// and have no inbound listener, so port is advisory metadata
	// only. Validated for range if provided.
	if req.Port < 0 || req.Port > 65535 {
		return errors.New("port must be 0–65535 (0 means no inbound listener)")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
