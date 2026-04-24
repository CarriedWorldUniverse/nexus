// Package agent implements the single per-aspect runtime (agent.exe).
// Reads an aspect's home folder, registers with Nexus, heartbeats,
// serves an inbound turn endpoint that the Nexus (or tests) can POST
// to, dispatches through the provider, writes entries to the session
// tree. Ties together parts 1–6 into a working aspect.
//
// Scope note: comms-broker-style routing is NOT implemented here.
// The agent exposes a simple HTTP turn endpoint at `/turn` as the
// dispatch surface for v1; the broader comms layer (kind:"hand"
// on a shared chat bus) lands in a later part.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nexus-cw/nexus/runtime/context/tree"
	"github.com/nexus-cw/nexus/runtime/providers"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// Config bundles the runtime dependencies. All fields are required
// unless noted.
type Config struct {
	Home       string            // absolute path to the aspect home folder
	Aspect     schemas.AspectConfig
	Provider   providers.Provider // chat provider (e.g. claude-api)
	NexusURL   string            // base URL, e.g. http://localhost:7888
	AuthToken  string            // bearer token for Nexus auth
	Logger     *slog.Logger      // nil falls back to slog default
	HTTPClient *http.Client      // nil means http.DefaultClient
	ListenAddr string            // e.g. ":7904"; zero-port (":0") works for tests
}

// Agent is the running runtime instance.
type Agent struct {
	cfg       Config
	log       *slog.Logger
	client    *http.Client
	sessionID string
	tree      *tree.Tree

	mu         sync.Mutex
	registered bool
	srv        *http.Server
	listenURL  string
	serveErrCh chan error
}

// New constructs an Agent. Does no I/O — call Start() to kick off
// registration and the heartbeat loop.
func New(cfg Config) (*Agent, error) {
	if cfg.Home == "" {
		return nil, errors.New("agent: Home required")
	}
	if cfg.Aspect.Name == "" {
		return nil, errors.New("agent: Aspect.Name required")
	}
	if cfg.Provider == nil {
		return nil, errors.New("agent: Provider required")
	}
	if cfg.NexusURL == "" {
		return nil, errors.New("agent: NexusURL required")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	// One session id per agent-process lifetime. Stays stable across
	// heartbeat retries so the Nexus roster ties this process to its
	// registration entry.
	sessionID := fmt.Sprintf("%s-%d-%d", cfg.Aspect.Name, os.Getpid(), time.Now().UnixNano())

	// Session tree lives under <home>/session/ per context-mode
	// conventions. Global mode: single session. Thread/stateless:
	// runtime creates per-thread files as they're needed; for now
	// we only serve the global-mode path — thread sessions arrive
	// when dispatch gets richer.
	sessionDir := filepath.Join(cfg.Home, "session")
	tr, err := tree.Open(sessionDir, "global")
	if err != nil {
		return nil, fmt.Errorf("agent: open session tree: %w", err)
	}

	// Warn-only on non-global context modes: the runtime doesn't
	// fully serve thread/stateless yet. Onboarding aspects with those
	// modes declared should work for their "global-shaped" usage and
	// we pick up the extras later.
	if cfg.Aspect.ContextMode != "" && cfg.Aspect.ContextMode != schemas.ContextGlobal {
		log.Warn("agent: context mode not fully served in v1",
			"mode", cfg.Aspect.ContextMode,
			"note", "treating as global-scoped session")
	}

	return &Agent{
		cfg:        cfg,
		log:        log,
		client:     client,
		sessionID:  sessionID,
		tree:       tr,
		serveErrCh: make(chan error, 1),
	}, nil
}

// SessionID returns the unique id for this agent-process lifetime.
func (a *Agent) SessionID() string { return a.sessionID }

// Tree returns the session tree — useful for tests + dashboard.
func (a *Agent) Tree() *tree.Tree { return a.tree }

// ListenURL returns the address the turn endpoint is listening on.
// Only valid after Start returns.
func (a *Agent) ListenURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listenURL
}

// -------------------------------------------------------------------
// Lifecycle
// -------------------------------------------------------------------

// Start brings the agent up: starts the turn server, registers with
// Nexus, launches the heartbeat loop. Blocks until ctx is cancelled
// or a fatal error occurs. Deregisters on exit.
func (a *Agent) Start(ctx context.Context) error {
	if err := a.startTurnServer(); err != nil {
		return fmt.Errorf("agent.Start: turn server: %w", err)
	}

	heartbeatInterval, err := a.register(ctx)
	if err != nil {
		// Tear down the server we just started — registration failed.
		_ = a.shutdownTurnServer(context.Background())
		return fmt.Errorf("agent.Start: register: %w", err)
	}
	a.log.Info("agent registered",
		"name", a.cfg.Aspect.Name,
		"session", a.sessionID,
		"nexus", a.cfg.NexusURL,
		"listen", a.listenURL,
		"heartbeat_s", heartbeatInterval)

	// Heartbeat loop. Runs until ctx done; any failure is logged but
	// not fatal (transient broker restarts shouldn't kill the agent).
	// The serveErrCh path catches the async turn-server crash case:
	// if the listener dies, registration is stale and we must exit.
	ticker := time.NewTicker(time.Duration(heartbeatInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.log.Info("agent stopping", "reason", ctx.Err())
			return a.shutdown()
		case err := <-a.serveErrCh:
			a.log.Error("turn server crashed — shutting down agent", "err", err)
			// Best-effort deregister so Nexus doesn't keep routing
			// to a dead endpoint.
			_ = a.shutdown()
			return fmt.Errorf("agent: turn server crashed: %w", err)
		case <-ticker.C:
			if err := a.heartbeat(ctx); err != nil {
				a.log.Warn("heartbeat failed", "err", err)
			}
		}
	}
}

func (a *Agent) shutdown() error {
	// Best-effort deregister + server shutdown. Use a fresh bounded
	// context so the parent cancellation doesn't immediately kill
	// this.
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var errs []error
	if err := a.deregister(bg); err != nil {
		a.log.Warn("deregister failed", "err", err)
		errs = append(errs, err)
	}
	if err := a.shutdownTurnServer(bg); err != nil {
		a.log.Warn("turn server shutdown failed", "err", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// -------------------------------------------------------------------
// Registration protocol
// -------------------------------------------------------------------

func (a *Agent) register(ctx context.Context) (int, error) {
	req := schemas.RegisterRequest{
		Name:         a.cfg.Aspect.Name,
		ContextMode:  a.cfg.Aspect.ContextMode,
		Provider:     a.cfg.Aspect.Provider,
		Port:         a.cfg.Aspect.Port,
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		Capabilities: a.cfg.Aspect.Capabilities,
		Home:         a.cfg.Home,
		SessionID:    a.sessionID,
		Metadata:     a.cfg.Aspect.Metadata,
		Hands:        a.cfg.Aspect.Hands,
	}

	var resp schemas.RegisterResponse
	if err := a.postJSON(ctx, "/aspects/register", req, &resp); err != nil {
		return 0, err
	}
	if resp.Status != "registered" {
		return 0, fmt.Errorf("register: unexpected status %q", resp.Status)
	}

	a.mu.Lock()
	a.registered = true
	a.mu.Unlock()

	interval := resp.HeartbeatIntervalS
	if interval <= 0 {
		interval = 15
	}
	return interval, nil
}

func (a *Agent) heartbeat(ctx context.Context) error {
	req := schemas.HeartbeatRequest{
		Name:      a.cfg.Aspect.Name,
		SessionID: a.sessionID,
		At:        time.Now().UTC(),
	}
	return a.postJSON(ctx, "/aspects/heartbeat", req, nil)
}

func (a *Agent) deregister(ctx context.Context) error {
	a.mu.Lock()
	registered := a.registered
	a.mu.Unlock()
	if !registered {
		return nil
	}
	req := schemas.DeregisterRequest{
		Name:      a.cfg.Aspect.Name,
		SessionID: a.sessionID,
		Reason:    "graceful shutdown",
	}
	return a.postJSON(ctx, "/aspects/deregister", req, nil)
}

// postJSON is a tiny JSON-over-HTTP helper with bearer auth.
func (a *Agent) postJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.NexusURL+path, newByteReader(raw))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.AuthToken)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("%s: decode response: %w", path, err)
		}
	}
	return nil
}
