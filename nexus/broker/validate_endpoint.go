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
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/nexus-cw/nexus/nexus/aspects"
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
	OK                bool                `json:"ok"`
	SessionJWT        string              `json:"session_jwt"`
	SessionExpiresAt  string              `json:"session_expires_at"`
	Personality       personalityWire     `json:"personality"`
	Provider          string              `json:"provider"`
	Model             string              `json:"model"`
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

	resp := validateResponse{
		OK:               true,
		SessionJWT:       sess.SessionJWT,
		SessionExpiresAt: sess.ExpiresAt.UTC().Format(time.RFC3339),
		Provider:         sess.Provider,
		Model:            sess.Model,
		Personality:      personalityWireFrom(sess.Personality),
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

