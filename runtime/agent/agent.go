// Package agent implements the per-aspect runtime. Long-running
// process that connects to its upstream (Nexus directly OR a local
// Outpost) via a persistent WebSocket. Handles register/turn frames
// over that WS, writes session entries to a local tree, invokes the
// configured provider for each turn.
//
// v1 scope: register + deregister + turn dispatch over WS. Hand
// dispatch, knowledge frames, session projection land in subsequent
// parts.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/context/tree"
	"github.com/nexus-cw/nexus/runtime/providers"
	"github.com/nexus-cw/nexus/runtime/wsclient"
	"github.com/nexus-cw/nexus/shared/schemas"
)

// Config bundles the runtime dependencies. All fields except Logger
// are required.
type Config struct {
	// Home is the absolute path to the aspect home folder.
	Home string

	// Aspect is the parsed aspect.json.
	Aspect schemas.AspectConfig

	// Provider is the chat provider adapter (e.g. claude-api).
	Provider providers.Provider

	// UpstreamURL is the WS URL to dial. For aspects running on a
	// direct-to-Nexus host this is the Nexus's /connect endpoint;
	// on hosts with a local Outpost, it's the Outpost's listener.
	// Resolution rule: NEXUS_OUTPOST overrides NEXUS_UPSTREAM per
	// transport spec §3.1. The caller (main.go) resolves it and
	// passes the resulting URL here.
	UpstreamURL string

	// UpstreamIsExplicitOutpost is true when the URL was resolved
	// from NEXUS_OUTPOST (not NEXUS_UPSTREAM). Triggers fail-loudly
	// on initial connect failure per transport spec §3.5.
	UpstreamIsExplicitOutpost bool

	// AuthToken is the bearer token sent on the WS upgrade.
	AuthToken string

	// Logger is optional; nil falls back to slog.Default().
	Logger *slog.Logger
}

// Agent is the running runtime instance.
type Agent struct {
	cfg       Config
	log       *slog.Logger
	sessionID string
	tree      *tree.Tree

	ws *wsclient.Client

	mu         sync.Mutex
	registered bool
}

// New constructs an Agent. Does no I/O — call Start() to dial and
// register.
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
	if cfg.UpstreamURL == "" {
		return nil, errors.New("agent: UpstreamURL required")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	// One session id per agent-process lifetime. Stable across
	// reconnects so the Nexus roster ties this process to its
	// registration entry.
	sessionID := fmt.Sprintf("%s-%d-%d", cfg.Aspect.Name, os.Getpid(), time.Now().UnixNano())

	// Session tree lives under <home>/session/. Global mode: single
	// session file. Thread/stateless modes arrive in a later part.
	sessionDir := filepath.Join(cfg.Home, "session")
	tr, err := tree.Open(sessionDir, "global")
	if err != nil {
		return nil, fmt.Errorf("agent: open session tree: %w", err)
	}

	if cfg.Aspect.ContextMode != "" && cfg.Aspect.ContextMode != schemas.ContextGlobal {
		log.Warn("agent: context mode not fully served in v1",
			"mode", cfg.Aspect.ContextMode,
			"note", "treating as global-scoped session")
	}

	a := &Agent{
		cfg:       cfg,
		log:       log,
		sessionID: sessionID,
		tree:      tr,
	}

	ws, err := wsclient.New(wsclient.Config{
		URL:              cfg.UpstreamURL,
		AuthToken:        cfg.AuthToken,
		Handler:          wsclient.HandlerFunc(a.handleFrame),
		Logger:           log,
		FailFirstConnect: cfg.UpstreamIsExplicitOutpost,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: ws client: %w", err)
	}
	a.ws = ws
	return a, nil
}

// SessionID returns the unique id for this agent-process lifetime.
func (a *Agent) SessionID() string { return a.sessionID }

// Tree returns the session tree — useful for tests.
func (a *Agent) Tree() *tree.Tree { return a.tree }

// Start brings the agent up: drives the wsclient Run loop (which
// dials upstream and reconnects on drop), registers on each new
// connection. Blocks until ctx is cancelled or FailFirstConnect
// trips. Deregisters on clean shutdown.
func (a *Agent) Start(ctx context.Context) error {
	// Register-on-connect: watch for the wsclient becoming ready and
	// send the register frame. On reconnect, we register again with
	// the same session id — the roster's displacement logic will
	// either accept the re-register (same session) or reject it
	// (different session, live entry — but we keep the same id, so
	// it's the same-session path).
	registerDone := make(chan struct{})
	go a.registerLoop(ctx, registerDone)

	err := a.ws.Run(ctx)

	// Graceful shutdown: attempt a deregister frame if we were
	// registered. Do it on a fresh context so parent-cancel doesn't
	// immediately kill it.
	a.mu.Lock()
	wasRegistered := a.registered
	a.mu.Unlock()
	if wasRegistered {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.sendDeregister(bg)
	}

	// Signal the register loop to exit (Run has already returned).
	select {
	case <-registerDone:
	default:
	}

	return err
}

// registerLoop sends a register frame each time the wsclient becomes
// ready. Runs until ctx done.
func (a *Agent) registerLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	lastConnected := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		connected := a.ws.Connected()
		if connected && !lastConnected {
			// New connection just came up — register.
			if err := a.sendRegister(ctx); err != nil {
				a.log.Warn("register after connect failed", "err", err)
			}
		}
		lastConnected = connected
	}
}

// sendRegister sends a register frame and waits for the ack. Marks
// the agent as registered on success.
func (a *Agent) sendRegister(ctx context.Context) error {
	req, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
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
		},
	})
	if err != nil {
		return fmt.Errorf("build register frame: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := a.ws.Request(reqCtx, req)
	if err != nil {
		return fmt.Errorf("register request: %w", err)
	}
	if resp.Kind != frames.KindRegisterAck {
		return fmt.Errorf("register: got kind %q, want register.ack", resp.Kind)
	}

	var ack frames.RegisterAckPayload
	if err := frames.PayloadAs(resp, &ack); err != nil {
		// Register succeeded (ack kind) but payload malformed; treat
		// as transient.
		a.log.Warn("register.ack payload malformed", "err", err)
	}

	a.mu.Lock()
	a.registered = true
	a.mu.Unlock()

	a.log.Info("agent registered",
		"name", a.cfg.Aspect.Name,
		"session", a.sessionID,
		"heartbeat_s", ack.HeartbeatIntervalS)
	return nil
}

// sendDeregister sends a graceful deregister frame. Best-effort.
func (a *Agent) sendDeregister(ctx context.Context) error {
	env, err := frames.NewRequest(frames.KindDeregister, frames.DeregisterPayload{
		DeregisterRequest: schemas.DeregisterRequest{
			Name:      a.cfg.Aspect.Name,
			SessionID: a.sessionID,
			Reason:    "graceful shutdown",
		},
	})
	if err != nil {
		return err
	}
	// Fire-and-forget: the connection is probably being torn down;
	// don't bother waiting for the ack.
	return a.ws.Send(ctx, env)
}
