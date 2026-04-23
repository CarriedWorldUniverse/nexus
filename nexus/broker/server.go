// Package broker serves the Nexus HTTP API: registration endpoints
// (spec §4.1) and the live-roster query. TLS and bearer-token auth are
// middleware applied in the server constructor.
//
// This package is transport-only — it translates HTTP requests into
// roster operations and returns JSON responses. Business logic lives
// in nexus/roster.
package broker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// Config configures a Broker.
type Config struct {
	Addr               string        // host:port, e.g. ":7888"
	AuthToken          string        // bearer token required on all endpoints
	HeartbeatIntervalS int           // value returned to aspects on register
	StaleAfter         time.Duration // aspect becomes "stale" after this gap
	Logger             *slog.Logger
}

// Broker owns the HTTP server and its roster.
type Broker struct {
	cfg    Config
	roster *roster.Roster
	srv    *http.Server
	log    *slog.Logger
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
	return &Broker{cfg: cfg, roster: r, log: cfg.Logger}
}

// ListenAndServe blocks serving the broker until the context is cancelled.
// v1 uses plain HTTP for local dev; TLS wiring lands alongside the first
// real aspect invocation.
func (b *Broker) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("POST /aspects/register", b.auth(http.HandlerFunc(b.handleRegister)))
	mux.Handle("POST /aspects/heartbeat", b.auth(http.HandlerFunc(b.handleHeartbeat)))
	mux.Handle("POST /aspects/deregister", b.auth(http.HandlerFunc(b.handleDeregister)))
	mux.Handle("GET /aspects", b.auth(http.HandlerFunc(b.handleList)))
	mux.HandleFunc("GET /health", b.handleHealth)

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

// auth rejects any request that doesn't carry the configured bearer token.
// Health is left unauthenticated so process supervisors can poll it.
func (b *Broker) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		given := header[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(given), []byte(b.cfg.AuthToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *Broker) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req schemas.RegisterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRegister(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	state, displacedSession, err := b.roster.Register(&req)
	if err != nil {
		switch {
		case errors.Is(err, roster.ErrAlreadyRegistered):
			writeError(w, http.StatusConflict, "aspect already registered with a different session")
		case errors.Is(err, roster.ErrPortConflict):
			writeError(w, http.StatusConflict, "port in use by another live aspect")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if displacedSession != "" {
		b.log.Warn("aspect re-registered, displacing prior session",
			"name", state.Name,
			"prior_session", displacedSession,
			"new_session", state.SessionID,
		)
	}

	b.log.Info("aspect registered",
		"name", state.Name,
		"port", state.Port,
		"context_mode", state.ContextMode,
		"provider", state.Provider,
	)

	writeJSON(w, http.StatusCreated, schemas.RegisterResponse{
		Status:             "registered",
		HeartbeatIntervalS: b.cfg.HeartbeatIntervalS,
		StaleAfterS:        int(b.cfg.StaleAfter.Seconds()),
	})
}

func (b *Broker) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req schemas.HeartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" || req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "name and session_id required")
		return
	}
	// Always server-stamp liveness — an aspect (or a compromised aspect) could
	// post a future timestamp and trick the reaper into never marking it stale.
	at := time.Now().UTC()

	if err := b.roster.Heartbeat(req.Name, req.SessionID, at); err != nil {
		switch {
		case errors.Is(err, roster.ErrNotRegistered):
			writeError(w, http.StatusNotFound, "aspect not registered; call /aspects/register")
		case errors.Is(err, roster.ErrSessionMismatch):
			writeError(w, http.StatusConflict, "session id does not match current registration")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (b *Broker) handleDeregister(w http.ResponseWriter, r *http.Request) {
	var req schemas.DeregisterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" || req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "name and session_id required")
		return
	}
	if err := b.roster.Deregister(req.Name, req.SessionID); err != nil {
		if errors.Is(err, roster.ErrSessionMismatch) {
			writeError(w, http.StatusConflict, "session id does not match current registration")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	b.log.Info("aspect deregistered", "name", req.Name, "reason", req.Reason)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if req.Port <= 0 || req.Port > 65535 {
		return errors.New("port must be 1–65535")
	}
	return nil
}

// decodeJSON is deliberately tolerant of unknown fields. The wire protocol
// is expected to evolve (enrichment fields on heartbeat, new metadata on
// register) and rejecting unknown fields makes rolling upgrades impossible.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
