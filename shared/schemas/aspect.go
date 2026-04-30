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

// AspectConfig is the on-disk shape of aspect.json. See spec §3.
//
// Per hand-dispatch v0.1 §8: per-aspect named-hand declarations are
// removed. Workers spawned by the dispatcher inherit the dispatching
// aspect's identity from its home (NEXUS.md/SOUL.md/PRIMER) at boot;
// there is no `hands[]` array on aspect.json any more.
type AspectConfig struct {
	Name           string         `json:"name"`
	ContextMode    ContextMode    `json:"context_mode"`
	Provider       string         `json:"provider"`
	ProviderConfig map[string]any `json:"provider_config"`
	Port           int            `json:"port"`
	Capabilities   []string       `json:"capabilities"`
	NexusURLEnv    string         `json:"nexus_url_env"`
	AuthTokenEnv   string         `json:"auth_token_env"`
	CommsPerms     []string       `json:"commsPerms,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
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
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Status        string    `json:"status"` // "live" | "stale" | "down"
	Enrichment    map[string]any `json:"enrichment,omitempty"`
}
