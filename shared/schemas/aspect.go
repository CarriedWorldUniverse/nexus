// Package schemas defines the on-the-wire shapes shared between the Nexus
// process and the agent runtime. These mirror the JSON structures described
// in docs/2026-04-22-nexus-registration-spec.md §3 and §4.
package schemas

import "time"

// ContextMode declares how the runtime persists and replays session state
// for a given aspect. See spec §2.2.
type ContextMode string

const (
	ContextGlobal    ContextMode = "global"
	ContextThread    ContextMode = "thread"
	ContextStateless ContextMode = "stateless"
)

// Role declares whether an aspect home defines a regular aspect or the
// Nexus's Frame. See frame-role spec §3 — exactly one Frame per Nexus.
//
// On-disk values: "aspect" or "frame". Empty (field absent) is treated
// as RoleAspect for backwards compatibility with pre-Frame aspect.json
// files. Anything else is unknown — callers should surface as a typo
// rather than silently coerce, since an unknown role likely means the
// operator tried to configure something and misspelled it.
type Role string

const (
	RoleAspect   Role = "aspect"
	RoleFrame    Role = "frame"
	RoleOperator Role = "operator" // human principal driving the dashboard SPA
)

// AspectConfig is the on-disk shape of aspect.json. See spec §3.
//
// Per hand-dispatch v0.1 §8: per-aspect named-hand declarations are
// removed. Workers spawned by the dispatcher inherit the dispatching
// aspect's identity from its home (NEXUS.md/SOUL.md/PRIMER) at boot;
// there is no `hands[]` array on aspect.json any more.
type AspectConfig struct {
	Name           string         `json:"name"`
	Role           Role           `json:"role,omitempty"` // empty = RoleAspect (back-compat)
	ContextMode    ContextMode    `json:"context_mode"`
	Provider       string         `json:"provider"`
	ProviderConfig map[string]any `json:"provider_config"`
	Port           int            `json:"port"`
	Capabilities   []string       `json:"capabilities"`
	NexusURLEnv    string         `json:"nexus_url_env"`
	AuthTokenEnv   string         `json:"auth_token_env"`
	CommsPerms     []string       `json:"commsPerms,omitempty"`
	// Filter selects the post-hoc output triage that decides whether the
	// model's natural reply gets posted to chat. Values:
	//
	//   "cheap"  — hard rules + cheap-model judgment (default; full triage)
	//   "hard"   — substring/prefix self-suppress only (no extra model call)
	//   "always" — only suppress empty replies (today's pre-triage default)
	//   "off"    — post every non-empty reply unmodified (alias of "always")
	//
	// Empty string falls back to "cheap". Per-aspect because some aspects
	// (e.g. forge training reports) legitimately produce content the cheap
	// model misjudges and need a looser filter.
	Filter string `json:"filter,omitempty"`

	// FilterProvider lets the operator pick a separate (typically cheaper)
	// provider for the CheapModelFilter judgment call. Empty falls back
	// to the aspect's main Provider — which is the right default when
	// the Frame is subscription-auth claudecode (no extra creds needed).
	// Set this to e.g. "claude-api" with FilterProviderConfig.model =
	// "claude-haiku-4-5" for a cheap-tier judge, or to an entirely
	// different stack (ollama, openai) for non-Claude deployments.
	FilterProvider string `json:"filter_provider,omitempty"`

	// FilterProviderConfig mirrors ProviderConfig but for the filter
	// judge — typically just {"model": "..."}. Empty model falls back
	// to "claude-haiku-4-5" when FilterProvider is a Claude flavor,
	// otherwise to the aspect's main Model.
	FilterProviderConfig map[string]any `json:"filter_provider_config,omitempty"`

	// FilterCredential names a provider credential (in the broker's
	// credential store) whose env overlay (ANTHROPIC_API_KEY +
	// ANTHROPIC_BASE_URL for Anthropic-shape) is injected into the
	// judge subprocess at spawn. Lets the operator route the cheap-
	// judge to a separate auth domain (DeepSeek's Anthropic-compatible
	// endpoint, an API-key Anthropic account, etc) without affecting
	// the main deliberation provider. Empty = filter inherits the
	// main provider's auth (subscription claudecode, process-env keys).
	//
	// Pairs naturally with FilterProvider="claude-code" + bare judge:
	// the bare subprocess wants explicit API-key auth, which this
	// credential supplies. NEX-103 tracks per-kind dispatch through
	// ProviderEnvResolver; until then this is a static per-aspect knob.
	FilterCredential string `json:"filter_credential,omitempty"`

	// Rewriter configures the per-turn session-jsonl rewriter (see
	// nexus/frame/funnel/rewriter). Only meaningful for claude-code-
	// backed aspects — direct-API providers don't replay a jsonl, so
	// distillation is moot. Empty/absent → enabled by default for
	// claude-code, disabled for everyone else.
	Rewriter *RewriterConfig `json:"rewriter,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`
}

// RewriterConfig controls the per-turn jsonl rewriter. All fields are
// optional; zero-values resolve to the spec defaults
// (G:/My Drive/nexus/general/specs/2026-05-10-jsonl-rewriter-spec.md).
//
//   - Enabled: nil = default-on for claude-code-backed aspects,
//     default-off otherwise. Explicit *bool lets the operator override
//     either way.
//   - ToolResultThreshold: bytes; tool_result content above this is
//     distilled. Default 1000.
//   - AssistantTextThreshold: bytes; assistant text above this is
//     distilled. Default 500.
//   - DistillerProvider: provider id for the haiku call. Empty falls
//     back to the Frame's main provider when it's a Claude flavor;
//     otherwise the operator must set it explicitly so we don't
//     accidentally hammer a non-Claude main model.
//   - DistillerModel: model id. Empty falls back to claude-haiku-4-5
//     for Claude providers, the Frame's main model otherwise.
type RewriterConfig struct {
	Enabled                *bool  `json:"enabled,omitempty"`
	ToolResultThreshold    int    `json:"tool_result_threshold,omitempty"`
	AssistantTextThreshold int    `json:"assistant_text_threshold,omitempty"`
	DistillerProvider      string `json:"distiller_provider,omitempty"`
	DistillerModel         string `json:"distiller_model,omitempty"`
}

// EffectiveRole returns the role with empty-string normalized to RoleAspect.
// Use this rather than reading c.Role directly so back-compat is uniform.
// Unknown role strings (e.g. typos) pass through unchanged — callers
// should check Known() to distinguish "valid role" from "unknown string."
func (c AspectConfig) EffectiveRole() Role {
	if c.Role == "" {
		return RoleAspect
	}
	return c.Role
}

// Known reports whether r is one of the recognized role values for an
// on-disk aspect.json. False means the role string was not the empty
// string AND not in the known set — likely a typo. Callers should
// surface this loudly rather than coerce.
//
// RoleOperator is intentionally NOT included here: operators are never
// instantiated from disk — they're minted at login from a passkey-
// unlocked keyfile (dashboard-ws-port spec §2.2). If an aspect.json
// declares role: "operator" the broker treats it as unknown so the
// operator boundary stays uncrossable from the filesystem.
func (r Role) Known() bool {
	switch r {
	case RoleAspect, RoleFrame:
		return true
	default:
		return false
	}
}

// IsRuntimeIdentity reports whether r is a recognized identity at
// runtime — including identities like RoleOperator that exist only on
// live connections. Use this when validating tokens/JWTs/registers.
func (r Role) IsRuntimeIdentity() bool {
	switch r {
	case RoleAspect, RoleFrame, RoleOperator:
		return true
	default:
		return false
	}
}

// RegisterRequest is the body of POST /aspects/register. See spec §4.2.
type RegisterRequest struct {
	Name         string         `json:"name"`
	ContextMode  ContextMode    `json:"context_mode"`
	Provider     string         `json:"provider"`
	Port         int            `json:"port"`
	PID          int            `json:"pid"`
	StartedAt    time.Time      `json:"started_at"`
	Model        string         `json:"model,omitempty"`
	Capabilities []string       `json:"capabilities"`
	Home         string         `json:"home"`
	SessionID    string         `json:"session_id"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// RegisterResponse is returned to an aspect after successful registration.
type RegisterResponse struct {
	Status             string `json:"status"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
	StaleAfterS        int    `json:"stale_after_s"`
}

// HeartbeatRequest is the body of POST /aspects/heartbeat.
type HeartbeatRequest struct {
	Name      string    `json:"name"`
	SessionID string    `json:"session_id"`
	At        time.Time `json:"at"`
}

// DeregisterRequest is the body of POST /aspects/deregister.
type DeregisterRequest struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

// AspectState is the live-roster entry for a registered aspect.
// Static fields are set on register; dynamic fields are filled by the
// enrichment fiber (spec §2.5).
type AspectState struct {
	// Static — set on registration, immutable for session lifetime.
	Name         string         `json:"name"`
	ContextMode  ContextMode    `json:"context_mode"`
	Provider     string         `json:"provider"`
	Port         int            `json:"port"`
	PID          int            `json:"pid"`
	StartedAt    time.Time      `json:"started_at"`
	Model        string         `json:"model,omitempty"`
	Capabilities []string       `json:"capabilities"`
	Home         string         `json:"home"`
	SessionID    string         `json:"session_id"`
	Metadata     map[string]any `json:"metadata,omitempty"`

	// Dynamic — refreshed by heartbeats and enrichment fiber.
	LastHeartbeat time.Time      `json:"last_heartbeat"`
	Status        string         `json:"status"` // "live" | "stale" | "down"
	Enrichment    map[string]any `json:"enrichment,omitempty"`
}
