// Keyfile validation HTTP endpoints for the broker.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §5:
//
//   GET  /api/nexus_id          — application-layer identity check
//                                 (agentfunnel verifies envelope.nexus_id
//                                 matches before sending the encrypted
//                                 payload).
//   POST /api/aspect/validate   — keyfile validation handshake. Body
//                                 carries the encrypted_payload; response
//                                 carries the session JWT + personality
//                                 bundle, or an error sentinel.
//
// Both endpoints are intentionally UNAUTHENTICATED at the broker's
// `auth` middleware level: the keyfile IS the authentication for
// /api/aspect/validate, and /api/nexus_id is meant to be queried before
// the caller has any credentials. They sit alongside /health in that
// regard.

package broker

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// KeyfileValidator wires the data /api/aspect/validate needs. cmd/nexus
// builds this from the loaded identity + an aspects.SQLStore. When nil
// in Config.KeyfileValidator, the endpoints are not registered (legacy
// boot mode without keyfile auth — Part 5 will make this required).
type KeyfileValidator struct {
	// NexusID echoed by /api/nexus_id and stamped into JWT iss claims.
	NexusID string

	// ServerEd25519Pubkey + ServerEd25519Privkey decrypt incoming
	// sealed payloads.
	ServerEd25519Pubkey  ed25519.PublicKey
	ServerEd25519Privkey ed25519.PrivateKey

	// SessionSigningSecret signs issued JWTs.
	SessionSigningSecret []byte

	// Store is the aspects backend.
	Store aspects.Store

	// Settings is the nexus_settings backend (Part 9). Validate uses
	// it to populate ValidatedSession.CentralNexusMD/CentralVersion in
	// the response, so agentfunnel can layer the central content
	// above the per-aspect bundle. Optional; nil = legacy shape with
	// per-aspect content only.
	Settings aspects.SettingsStore

	// Credentials is the broker-mediated credential store (NEX-168).
	// When non-nil, /api/aspect/validate resolves the aspect's
	// mcp_profile via Substitute and includes the rendered JSON in the
	// success response (NEX-169). Nil = no profile is emitted (legacy
	// boot mode without the credential store wired); the response's
	// mcp_profile field is "".
	Credentials *credentials.Store

	// JWTTTL is the issued JWT lifetime. Default 1h per spec §6.
	JWTTTL time.Duration
}

// validateRequest is the POST /api/aspect/validate body shape. Single
// field; the keyfile envelope already carries everything else
// (nexus_url, nexus_id) on the agentfunnel side.
type validateRequest struct {
	EncryptedPayload string `json:"encrypted_payload"`
}

// validateResponse mirrors the spec §5 success shape. Personality is
// always emitted (substituting an empty bundle when no row exists) so
// the wire shape is stable for agentfunnel's JSON decoder.
type validateResponse struct {
	OK               bool            `json:"ok"`
	SessionJWT       string          `json:"session_jwt"`
	SessionExpiresAt string          `json:"session_expires_at"`
	Personality      personalityWire `json:"personality"`
	Provider         string          `json:"provider"`
	Model            string          `json:"model"`

	// CentralNexusMD is the network-wide nexus_settings.nexus_md
	// (Part 9). agentfunnel layers it ABOVE Personality.NexusMD in
	// the composed prompt. CentralVersion lets the agent detect
	// changes between re-validations independent of the personality
	// version. Both are zero-valued when Part 9 isn't wired (legacy
	// shape — Personality alone is the prompt).
	CentralNexusMD string `json:"central_nexus_md"`
	CentralVersion int64  `json:"central_version"`

	// MCPProfile is the aspect's resolved MCP-server profile (NEX-169):
	// the stored JSON blob from mcp_profiles.profile with every
	// ${credential:NAME.field} placeholder substituted with the
	// plaintext credential value. agentfunnel materialises this into
	// the aspect's .mcp.json (NEX-170, keel's lane).
	//
	// Empty string when:
	//   - KeyfileValidator.Credentials is nil (legacy boot), OR
	//   - no mcp_profiles row exists for the aspect (operator hasn't
	//     configured one yet — treated identically to the empty profile
	//     case by GetMCPProfile).
	//
	// Substitution failure (malformed placeholder, unknown credential,
	// unknown field) is fatal: identity is verified but the response
	// is unusable, so the handler returns 500 rather than emit a
	// half-resolved profile.
	MCPProfile string `json:"mcp_profile"`
}

// personalityWire is the on-the-wire shape of the personality bundle.
// Mirrors aspects.Personality but keeps JSON field naming under our
// control (the Go struct field names already match, but defining this
// explicitly insulates the wire from future Go-side renames).
type personalityWire struct {
	NexusMD   string `json:"nexus_md"`
	SoulMD    string `json:"soul_md"`
	PrimerMD  string `json:"primer_md"`
	Composed  string `json:"composed"`
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

// nexusIDResponse is the GET /api/nexus_id body. agentfunnel compares
// this against envelope.nexus_id from its keyfile to confirm it dialled
// the right Nexus before sending the encrypted payload.
type nexusIDResponse struct {
	NexusID string `json:"nexus_id"`
}

// errorResponse is the spec §5 rejection shape: always { error: "..." }.
// Specific failures may add fields (e.g. revocation includes
// current_version).
type errorResponse struct {
	Error          string `json:"error"`
	CurrentVersion int64  `json:"current_version,omitempty"`
}

// registerKeyfileEndpoints attaches the spec §5 endpoints to mux. Called
// from ListenAndServe when KeyfileValidator is configured. Both routes
// bypass the auth() middleware: the keyfile is its own credential.
func (b *Broker) registerKeyfileEndpoints(mux *http.ServeMux) {
	v := b.cfg.KeyfileValidator
	if v == nil {
		return
	}
	mux.HandleFunc("GET /api/nexus_id", b.handleNexusID)
	mux.HandleFunc("POST /api/aspect/validate", b.handleAspectValidate)

	// Part 9c: aspect self-edit personality. Uses session JWT (sub
	// claim) for auth, NOT admin bearer. Gated on Store being wired
	// since the aspect row is the write target.
	if v.Store != nil {
		mux.HandleFunc("PUT /api/aspect/personality", b.handleAspectSelfEdit)
	}
}

func (b *Broker) handleNexusID(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil || v.NexusID == "" {
		writeError(w, http.StatusServiceUnavailable, "nexus identity not configured")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nexusIDResponse{NexusID: v.NexusID})
}

func (b *Broker) handleAspectValidate(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil {
		writeError(w, http.StatusServiceUnavailable, "validator not configured")
		return
	}

	// TODO(DoS): this endpoint is unauthenticated and runs NaCl
	// crypto_box_open + DB lookup per request. The 16 KiB body cap
	// bounds per-request memory but not request rate. Tailnet-only
	// deployments are protected by tailnet ACLs; any internet-exposed
	// Nexus needs a per-IP rate limit (e.g. golang.org/x/time/rate
	// at ~10 req/s). Track in #157 follow-up.

	// Cap body size — the encrypted_payload is ~300-500 bytes for a
	// well-formed keyfile; 16 KiB is generous and bounds memory.
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)

	var req validateRequest
	dec := json.NewDecoder(r.Body)
	// DisallowUnknownFields keeps the request body shape pinned to a
	// single field at v1. Forward-compat note for Part 5: adding a
	// field to validateRequest requires a paired Nexus update, since
	// older Nexuses will 400-reject any field beyond encrypted_payload.
	// If field churn becomes likely, drop this and rely on documented
	// stability instead.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body malformed", 0)
		return
	}
	if req.EncryptedPayload == "" {
		writeJSONError(w, http.StatusBadRequest, "encrypted_payload missing", 0)
		return
	}

	cfg := aspects.ValidateConfig{
		Store:                v.Store,
		Settings:             v.Settings,
		NexusID:              v.NexusID,
		ServerEd25519Privkey: v.ServerEd25519Privkey,
		ServerEd25519Pubkey:  v.ServerEd25519Pubkey,
		SessionSigningSecret: v.SessionSigningSecret,
		JWTTTL:               v.JWTTTL,
	}

	sess, err := aspects.Validate(r.Context(), cfg, req.EncryptedPayload)
	if err != nil {
		// Map sentinels per spec §5. Surface the rejection reason on
		// the wire (the spec strings double as machine-readable codes
		// for agentfunnel).
		var rev *aspects.RevokedError
		switch {
		case errors.Is(err, aspects.ErrDecryptionFailed):
			writeJSONError(w, http.StatusUnauthorized, "decryption failed", 0)
		case errors.Is(err, aspects.ErrMalformedPayload):
			writeJSONError(w, http.StatusBadRequest, "payload malformed", 0)
		case errors.Is(err, aspects.ErrUnknownAspect):
			writeJSONError(w, http.StatusNotFound, "unknown aspect", 0)
		case errors.Is(err, aspects.ErrRetired):
			writeJSONError(w, http.StatusForbidden, "retired", 0)
		case errors.As(err, &rev):
			writeJSONError(w, http.StatusForbidden, "revoked", rev.CurrentVersion)
		case errors.Is(err, aspects.ErrKeyMismatch):
			writeJSONError(w, http.StatusForbidden, "key mismatch", 0)
		default:
			b.log.Error("validate: internal error", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// NEX-169: resolve the aspect's mcp_profile (NEX-168 stored blob +
	// Substitute). Gated on the credentials store being wired — a
	// legacy boot without it just emits an empty profile field.
	mcpProfile, mcpErr := resolveMCPProfile(r.Context(), v.Credentials, sess.AspectName)
	if mcpErr != nil {
		// Identity passed but the profile is broken (malformed placeholder,
		// unknown credential, unknown field, or audit-tx failure). Emit a
		// 500 rather than a half-resolved profile: the operator must fix
		// the misconfiguration before the aspect can connect. The aspect
		// name + cause go to logs; the wire body stays opaque so we don't
		// leak credential names to the client.
		b.log.Error("validate: mcp_profile resolve failed",
			"aspect", sess.AspectName, "err", mcpErr)
		writeError(w, http.StatusInternalServerError, "mcp profile resolve failed")
		return
	}

	resp := validateResponse{
		OK:               true,
		SessionJWT:       sess.SessionJWT,
		SessionExpiresAt: sess.ExpiresAt.UTC().Format(time.RFC3339),
		Provider:         sess.Provider,
		Model:            sess.Model,
		Personality:      personalityWireFrom(sess.Personality),
		CentralNexusMD:   sess.CentralNexusMD,
		CentralVersion:   sess.CentralVersion,
		MCPProfile:       mcpProfile,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		b.log.Warn("validate: response encode failed", "err", err)
	}
}

// personalityWireFrom adapts an aspects.Personality (or nil) to the
// wire shape. nil → empty bundle so the JSON shape is stable.
func personalityWireFrom(p *aspects.Personality) personalityWire {
	if p == nil {
		return personalityWire{}
	}
	return personalityWire{
		NexusMD:   p.NexusMD,
		SoulMD:    p.SoulMD,
		PrimerMD:  p.PrimerMD,
		Composed:  p.Composed,
		Version:   p.Version,
		UpdatedAt: p.UpdatedAt,
	}
}

// resolveMCPProfile loads the aspect's stored MCP profile blob and runs
// credential substitution. Returns ("", nil) when:
//   - the credentials store isn't wired (legacy boot), OR
//   - no profile row exists for the aspect.
//
// Any error from Substitute (malformed placeholder, unknown credential,
// unknown field, audit-tx failure) is propagated; the caller maps that
// to a 500 since the response is unusable. NEX-169.
func resolveMCPProfile(ctx context.Context, store *credentials.Store, aspect string) (string, error) {
	if store == nil {
		return "", nil
	}
	profile, err := store.GetMCPProfile(ctx, aspect)
	if err != nil {
		return "", err
	}
	if profile == "" {
		return "", nil
	}
	return store.Substitute(ctx, aspect, profile)
}

// writeJSONError emits the spec §5 error shape. currentVersion is
// included only for the revocation case (>0); zero is omitted via the
// struct tag's omitempty.
func writeJSONError(w http.ResponseWriter, status int, message string, currentVersion int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:          message,
		CurrentVersion: currentVersion,
	})
}
