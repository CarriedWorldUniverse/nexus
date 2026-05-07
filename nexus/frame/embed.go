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
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
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

	// personality is the latest fetched personality bundle. Behind a
	// mutex so RefreshPersonality can swap it from a goroutine while
	// the funnel reader (via SystemPrompt) reads concurrently.
	mu          sync.RWMutex
	personality *aspects.Personality
	store       aspects.Store // retained for RefreshPersonality
}

// SystemPrompt returns the composed personality prompt for the Frame's
// funnel.Config.SystemPromptFn. Empty when no aspect_personalities row
// exists yet (Frame still functions; operator runs `nexus personality
// edit <frame>` to populate). Safe for concurrent reads with
// RefreshPersonality.
//
// Per spec §11: keel reads its personality from
// `aspect_personalities WHERE aspect_name='keel'` rather than from
// CLAUDE.md/SOUL.md/PRIMER.md on disk.
//
// Resolution order:
//
//  1. If `composed` is non-empty (Part 7's renderer will populate it),
//     return it directly. This is the canonical path once Part 7 ships.
//  2. Otherwise — and this is the Part 6 → Part 7 gap — concatenate
//     NexusMD + SoulMD + PrimerMD with section separators. Without
//     this concat the Frame would run with operational scope (NexusMD)
//     but no voice/values (SoulMD) or network primer (PrimerMD).
//     Note that PersonalitySet invalidates `composed` on every write
//     (Part 2), so this fallback is the active path until a renderer
//     populates the cache.
//  3. If everything is empty, return "" — Frame still boots, just
//     prompt-less.
func (e *EmbeddedFrame) SystemPrompt() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.personality == nil {
		return ""
	}
	if e.personality.Composed != "" {
		return e.personality.Composed
	}
	parts := make([]string, 0, 3)
	if e.personality.NexusMD != "" {
		parts = append(parts, e.personality.NexusMD)
	}
	if e.personality.SoulMD != "" {
		parts = append(parts, e.personality.SoulMD)
	}
	if e.personality.PrimerMD != "" {
		parts = append(parts, e.personality.PrimerMD)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// RefreshPersonality re-fetches the Frame's personality row and swaps
// it atomically. Used by Part 7's in-process refresh path (operator
// runs `nexus personality edit keel` → REST handler invokes this on
// the EmbeddedFrame, the next deliberation cycle picks up the new
// SystemPrompt). Returns ErrNoPersonality if no row exists for the
// Frame's name (caller decides whether to ignore — Frame can run
// without one).
//
// Per spec §11: "A way to receive personality refresh (in-process
// callback, no WS frame)." This is that callback.
func (e *EmbeddedFrame) RefreshPersonality(ctx context.Context) error {
	if e.store == nil {
		return errors.New("frame: RefreshPersonality requires PersonalityStore at Embed time")
	}
	p, err := e.store.PersonalityGet(ctx, e.Aspect.Name)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.personality = p
	e.mu.Unlock()
	return nil
}

// EmbedConfig threads dependencies into Embed without growing a long
// argument list. Roster, TokenStore, Detected required; DB, Logger,
// PersonalityStore optional.
type EmbedConfig struct {
	Detected   *FrameAspect
	Roster     *roster.Roster
	TokenStore *broker.TokenStore
	DB         *sql.DB
	Logger     *slog.Logger

	// PersonalityStore is the aspect_personalities backend (Part 2).
	// When non-nil, Embed fetches the Frame's personality row and
	// stashes it on the returned EmbeddedFrame so EmbeddedFrame.SystemPrompt
	// can serve it to the funnel. When nil, SystemPrompt returns "" —
	// callers can still set it explicitly via funnel.Config.SystemPrompt
	// elsewhere (legacy path).
	//
	// Per spec §11: keel reads its personality from
	// aspect_personalities, not from on-disk markdown.
	PersonalityStore aspects.Store
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

	// Spec §11: load personality from aspect_personalities WHERE
	// aspect_name = <frame name>. Missing row is allowed — Frame still
	// boots, just runs prompt-less until the operator populates one.
	var personality *aspects.Personality
	if cfg.PersonalityStore != nil {
		p, perr := cfg.PersonalityStore.PersonalityGet(ctx, name)
		switch {
		case perr == nil:
			personality = p
		case errors.Is(perr, aspects.ErrNotFound):
			cfg.Logger.Warn("frame: no personality row found — running with empty SystemPrompt until populated",
				"name", name,
				"hint", "run: nexus personality edit "+name)
		default:
			return nil, fmt.Errorf("frame: load personality: %w", perr)
		}
	}

	cfg.Logger.Info("frame: embedded as in-process aspect",
		"name", name, "session", sessionID, "pid", pid,
		"home", cfg.Detected.Path, "context_mode", cfg.Detected.Config.ContextMode,
		"provider", cfg.Detected.Config.Provider, "model", registerReq.Model,
		"personality_loaded", personality != nil,
		"personality_version", personalityVersion(personality))

	return &EmbeddedFrame{
		Aspect:      *cfg.Detected,
		AdminToken:  token,
		SessionID:   sessionID,
		State:       state,
		personality: personality,
		store:       cfg.PersonalityStore,
	}, nil
}

// personalityVersion is a small helper for the structured-log line —
// avoids dereferencing a possibly-nil personality pointer at the call
// site.
func personalityVersion(p *aspects.Personality) int64 {
	if p == nil {
		return 0
	}
	return p.Version
}

// Heartbeat refreshes the Frame's last-seen so the roster's stale
// reaper doesn't mark it down. Caller should run this on a ticker as
// long as the Nexus process is alive. Pointer receiver — EmbeddedFrame
// holds a sync.RWMutex so it must never be copied.
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
