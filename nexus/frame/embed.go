package frame

// Frame embedding in normal-mode startup. When Detect returns a
// FrameAspect, Embed instantiates it as an in-process aspect: registers
// it in the broker's roster with a generated session id, reconciles its
// admin-flagged token, and returns an EmbeddedFrame value the rest of
// the Nexus can hold for direct method-call wiring.
//
// Per spec §3.2, the Frame is the Nexus's running self with a name. It
// participates in the roster like any other aspect (so /api/aspects
// lists it, peer aspects see it, dashboard renders it) but doesn't run
// as a subprocess and doesn't hold a peer port. The trust boundary is
// the process boundary — admin operations gate on the in-process
// EmbeddedFrame value, not on token possession.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// EmbeddedFrame is the in-process handle to the running Frame. P6 (the
// deliberation loop) and P8 (chat routing) consume this value to know
// which aspect IS the Frame, what its admin token is for outbound
// broker calls, and what its home folder is for personality reads.
type EmbeddedFrame struct {
	// Aspect is the resolved Frame personality from Detect.
	Aspect FrameAspect

	// AdminToken is the bearer the Frame uses for outbound calls into
	// the broker (e.g., the admin REST endpoints in P7). Reconciled
	// with admin=true via TokenStore at Embed time.
	AdminToken string

	// SessionID is the per-startup session marker for the Frame's
	// roster entry. Regenerated on every Nexus boot — the Frame doesn't
	// persist a session like aspects do because there's no separate
	// process to survive across restarts.
	SessionID string

	// State is the registered roster state. Useful for downstream
	// consumers that want the heartbeat fields without re-querying the
	// roster. Read-only post-Embed; the roster owns updates.
	State *schemas.AspectState
}

// EmbedConfig threads dependencies into Embed without growing a long
// argument list. All fields required.
type EmbedConfig struct {
	Detected   *FrameAspect
	Roster     *roster.Roster
	TokenStore *broker.TokenStore
	DB         *sql.DB
	Logger     *slog.Logger
}

// ErrEmbedRequiresFrame is returned when Embed is called with a nil
// detected frame. Caller should have checked Detect's result before
// calling — Embed does not handle the no-frame case.
var ErrEmbedRequiresFrame = errors.New("frame: Embed requires a detected Frame; check Detect first")

// Embed registers the Frame as an in-process aspect and returns the
// handle. Runs once at startup, after Detect but before normal-mode
// services come up.
//
// NOT idempotent. A second call against the same Roster returns
// ErrAlreadyRegistered (from the underlying Roster.Register) because
// the in-memory roster already holds a live entry under this name. If
// a future code path needs reload semantics it must Deregister the
// existing roster entry before re-Embedding.
func Embed(ctx context.Context, cfg EmbedConfig) (*EmbeddedFrame, error) {
	if cfg.Detected == nil {
		return nil, ErrEmbedRequiresFrame
	}
	if cfg.Roster == nil {
		return nil, errors.New("frame: EmbedConfig.Roster required")
	}
	if cfg.TokenStore == nil {
		return nil, errors.New("frame: EmbedConfig.TokenStore required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	name := cfg.Detected.Name
	if name == "" {
		return nil, errors.New("frame: detected aspect has empty name")
	}

	// Mint or load the admin-flagged Frame token. P7's admin endpoints
	// will gate on this token's admin=true claim.
	token, err := cfg.TokenStore.ReconcileFrameTokenFor(ctx, cfg.DB, name)
	if err != nil {
		return nil, fmt.Errorf("frame: reconcile admin token: %w", err)
	}

	sessionID, err := generateSessionID()
	if err != nil {
		return nil, fmt.Errorf("frame: generate session id: %w", err)
	}

	pid := os.Getpid()
	registerReq := &schemas.RegisterRequest{
		Name:         name,
		ContextMode:  cfg.Detected.Config.ContextMode,
		Provider:     cfg.Detected.Config.Provider,
		Port:         cfg.Detected.Config.Port,
		PID:          pid,
		StartedAt:    time.Now().UTC(),
		Capabilities: cfg.Detected.Config.Capabilities,
		Home:         cfg.Detected.Path,
		SessionID:    sessionID,
		Metadata:     cfg.Detected.Config.Metadata,
	}
	if cfg.Detected.Config.ProviderConfig != nil {
		if m, ok := cfg.Detected.Config.ProviderConfig["model"].(string); ok {
			registerReq.Model = m
		}
	}

	state, displaced, err := cfg.Roster.Register(registerReq)
	if err != nil {
		return nil, fmt.Errorf("frame: register in roster: %w", err)
	}
	if displaced != "" {
		// Should never happen on a fresh boot — log and continue.
		cfg.Logger.Warn("frame: replaced existing roster entry on Embed",
			"name", name, "displaced_session", displaced, "new_session", sessionID)
	}

	cfg.Logger.Info("frame: embedded as in-process aspect",
		"name", name, "session", sessionID, "pid", pid,
		"home", cfg.Detected.Path, "context_mode", cfg.Detected.Config.ContextMode,
		"provider", cfg.Detected.Config.Provider, "model", registerReq.Model)

	return &EmbeddedFrame{
		Aspect:     *cfg.Detected,
		AdminToken: token,
		SessionID:  sessionID,
		State:      state,
	}, nil
}

// Heartbeat refreshes the Frame's last-seen so the roster's stale
// reaper doesn't mark it down. Caller should run this on a ticker as
// long as the Nexus process is alive.
func (e *EmbeddedFrame) Heartbeat(r *roster.Roster, at time.Time) error {
	return r.Heartbeat(e.Aspect.Name, e.SessionID, at)
}

// Name is a small convenience for downstream code that holds a Frame
// pointer and wants the aspect id without reaching through Aspect.Name.
func (e *EmbeddedFrame) Name() string {
	return e.Aspect.Name
}

// generateSessionID mints a fresh hex session marker. Mirrors the
// 32-byte token shape used elsewhere in the broker auth path; collisions
// are infeasible and there's no need to invent a separate scheme.
func generateSessionID() (string, error) {
	return broker.GenerateAgentToken()
}
