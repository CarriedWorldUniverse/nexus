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
	"log/slog"
	"net/http"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
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
	MCPProfile string `json:"mcp_profile,omitempty"`

	// ProviderEnv is the aspect's resolved provider-credential env overlay
	// (NEX-332 phase 4): {OPENAI_API_KEY, OPENAI_BASE_URL} or
	// {ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL}, drawn from the aspect's
	// default provider credential in the store. Lets an out-of-process
	// aspect (agentfunnel) construct its native-API provider with the
	// broker-held key — no key in start scripts or env. Empty when: no
	// credentials store, no default for the provider's shape, the provider
	// self-authenticates (claude-code), or the credential's mode forbids
	// fetch (mode=proxy keeps the key inside nexus). The aspect falls back
	// to its own process env when this is absent.
	ProviderEnv map[string]string `json:"provider_env,omitempty"`

	// JudgeProvider/JudgeModel/JudgeEnv carry the EFFECTIVE cheap-judge
	// config (per-aspect override > network default) so an out-of-process
	// aspect builds its judge from the validate response — NOT a separate
	// startup WS round-trip, which raced wsClient.Run and silently timed out
	// (NEX-373). JudgeEnv is the judge credential's {API_KEY, BASE_URL}
	// overlay (same shape + mode-gate as ProviderEnv). All empty when no
	// judge policy is set → the aspect's judge inherits its main provider.
	JudgeProvider string            `json:"judge_provider,omitempty"`
	JudgeModel    string            `json:"judge_model,omitempty"`
	JudgeEnv      map[string]string `json:"judge_env,omitempty"`

	// CompactModel/CompactEnv mirror the judge fields for the compact /
	// summarizer (rewriter) tier. Empty → the aspect's compact inherits.
	CompactModel string            `json:"compact_model,omitempty"`
	CompactEnv   map[string]string `json:"compact_env,omitempty"`
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

	// NEX-571 Task D: the JWT-boot path for hands. A spawned hand holds a
	// broker-minted session JWT (CW_SESSION_JWT) but no keyfile, so it
	// can't run the keyfile validate handshake. This endpoint resolves
	// the SAME persona/provider/config bundle keyed on the JWT's verified
	// sub (derived names fall back to the base aspect). The JWT IS the
	// credential — authenticated like the self-edit / credential.fetch
	// endpoints, not the unauthenticated keyfile path.
	if v.Store != nil {
		mux.HandleFunc("POST /api/aspect/resolve", b.handleAspectResolve)
	}

	// NEX-435: agent credential seam. An agent fetches a scoped, audited
	// credential bundle with its own session JWT — the HTTP counterpart of
	// the WS credential.fetch frame (the cw git-credential-helper uses it).
	// The handler guards on the credential store being wired.
	mux.HandleFunc("POST /api/agent/credential.fetch", b.handleAgentCredentialFetch)

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

	// NEX-332 phase 4: resolve the aspect's default provider credential
	// from the store and deliver it as an env overlay, so an out-of-process
	// aspect constructs its native-API provider with the broker-held key
	// (no key in start scripts/env). Best-effort: any miss falls back to
	// the aspect's own process env.
	// Resolve provider/judge/compact credentials by the PERSONALITY: a pool
	// worker `<personality>-<role>` (and a dotted hand `<parent>.sub-N`) has
	// no aspects row of its own — its provider credential + defaults live on
	// the personality/parent it resolves to (see aspects.PersonalityOf +
	// aspects/lineage.go). PersonalityOf is identity for an ordinary keyfile
	// aspect, so this is safe for those too. Mirrors handleAspectResolve.
	credAspect := aspects.PersonalityOf(sess.AspectName)
	providerEnv := resolveProviderEnv(r.Context(), v.Credentials, credAspect, sess.Provider, b.log)

	// NEX-373: resolve the effective judge + compact config here and deliver
	// it in the validate response, instead of the out-of-process aspect doing
	// a startup WS round-trip (which raced wsClient.Run and timed out).
	judgeProvider, judgeModel, judgeEnv := resolveJudgeConfig(r.Context(), v.Credentials, credAspect, b.log)
	compactModel, compactEnv := resolveCompactConfig(r.Context(), v.Credentials, credAspect, b.log)

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
		ProviderEnv:      providerEnv,
		JudgeProvider:    judgeProvider,
		JudgeModel:       judgeModel,
		JudgeEnv:         judgeEnv,
		CompactModel:     compactModel,
		CompactEnv:       compactEnv,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		b.log.Warn("validate: response encode failed", "err", err)
	}
}

// handleAspectResolve is the JWT-boot persona resolver (NEX-571 Task D).
// A spawned hand authenticates with its broker-minted session JWT (the
// bearer) and asks for the persona/provider/config bundle the keyfile
// validate path would return — without a keyfile. The JWT's sub (a
// derived `<base>.<word>` name for a hand) is the only source of the
// identity; ResolveByName resolves persona/provider/central against the
// BASE aspect (inheritance + persona fallback). The response is
// wire-identical to /api/aspect/validate apart from re-using the
// presented JWT (no fresh mint — the hand already holds a valid token).
func (b *Broker) handleAspectResolve(w http.ResponseWriter, r *http.Request) {
	v := b.cfg.KeyfileValidator
	if v == nil || v.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "validator not configured")
		return
	}

	// The JWT is the credential. Verify against the same signing secret
	// the validate endpoint mints with (the WS upgrade accepts the same
	// token). sub is the verified identity — never a body/query field.
	secret := v.SessionSigningSecret
	if len(secret) == 0 && b.cfg.OperatorLogin != nil {
		secret = b.cfg.OperatorLogin.SessionSigningSecret
	}
	token := ExtractBearer(r.Header.Get("Authorization"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	claims, err := jwt.Verify(secret, token, time.Now())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	if claims.Sub == "" || claims.Sub == "operator" {
		writeError(w, http.StatusUnauthorized, "session token missing aspect sub claim")
		return
	}
	aspectName := claims.Sub

	resolved, err := aspects.ResolveByName(r.Context(), aspects.ResolveConfigByName{
		Store:    v.Store,
		Settings: v.Settings,
	}, aspectName)
	if err != nil {
		switch {
		case errors.Is(err, aspects.ErrUnknownAspect):
			writeJSONError(w, http.StatusNotFound, "unknown aspect", 0)
		case errors.Is(err, aspects.ErrRetired):
			writeJSONError(w, http.StatusForbidden, "retired", 0)
		default:
			b.log.Error("resolve: internal error", "aspect", aspectName, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Provider/judge/compact env resolve against the BASE aspect (the
	// persona/config identity), so a hand boots with the parent's
	// provider creds. ResolveByName already applied the base fallback
	// to provider/model.
	//
	// The MCP profile, however, is NOT served to derived identities
	// (NEX-609) — nor to pool workers `<personality>-<role>`, for the same
	// reason: the personality's profile entries authenticate with the
	// keyfile at /etc/nexus/keyfile.json, which hand/worker Jobs deliberately
	// do not mount (their credential is the CW_SESSION_JWT env). A
	// served profile would make every turn spawn MCP servers that
	// slow-fail their auth before bridle skips them — and the one tool
	// a hand must not have (spawn — no sub-of-sub) lives there too.
	base := aspects.PersonalityOf(aspectName)
	var mcpProfile string
	if !aspects.IsDerivedName(aspectName) && !aspects.IsWorkerName(aspectName) {
		var mcpErr error
		mcpProfile, mcpErr = resolveMCPProfile(r.Context(), v.Credentials, base)
		if mcpErr != nil {
			b.log.Error("resolve: mcp_profile resolve failed", "aspect", base, "err", mcpErr)
			writeError(w, http.StatusInternalServerError, "mcp profile resolve failed")
			return
		}
	}
	providerEnv := resolveProviderEnv(r.Context(), v.Credentials, base, resolved.Provider, b.log)
	judgeProvider, judgeModel, judgeEnv := resolveJudgeConfig(r.Context(), v.Credentials, base, b.log)
	compactModel, compactEnv := resolveCompactConfig(r.Context(), v.Credentials, base, b.log)

	resp := validateResponse{
		OK:               true,
		SessionJWT:       token,
		SessionExpiresAt: time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339),
		Provider:         resolved.Provider,
		Model:            resolved.Model,
		Personality:      personalityWireFrom(resolved.Personality),
		CentralNexusMD:   resolved.CentralNexusMD,
		CentralVersion:   resolved.CentralVersion,
		MCPProfile:       mcpProfile,
		ProviderEnv:      providerEnv,
		JudgeProvider:    judgeProvider,
		JudgeModel:       judgeModel,
		JudgeEnv:         judgeEnv,
		CompactModel:     compactModel,
		CompactEnv:       compactEnv,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		b.log.Warn("resolve: response encode failed", "err", err)
	}
}

// resolveProviderEnv resolves the aspect's default provider credential for
// the shape its provider needs and returns the {API_KEY, BASE_URL} env
// overlay (NEX-332 phase 4). Returns nil — caller falls back to process env
// — when: no store, the provider self-authenticates (claude-code), no
// default is configured for that shape, or the credential's mode forbids
// fetch (mode=proxy keeps the key inside nexus). Never fatal: a resolve
// error degrades to nil + a warning, so a misconfigured default can't block
// an otherwise-valid keyfile from connecting.
func resolveProviderEnv(ctx context.Context, store *credentials.Store, aspect, provider string, log *slog.Logger) map[string]string {
	if store == nil {
		return nil
	}
	var shape credentials.APIShape
	switch provider {
	case "claude-api", "claude":
		shape = credentials.ShapeAnthropic
	case "openai", "openai-api":
		shape = credentials.ShapeOpenAI
	default:
		// claude-code self-authenticates (subscription/keychain); other
		// providers have no env mapping. No delivery.
		return nil
	}
	cred, env, err := store.ResolveDefaultForAspect(ctx, aspect, shape)
	if err != nil {
		if !errors.Is(err, credentials.ErrNoDefault) && log != nil {
			log.Warn("validate: provider env resolve failed; aspect falls back to process env",
				"aspect", aspect, "shape", string(shape), "err", err)
		}
		return nil
	}
	// Mode gate: only hand the raw key off-box when the operator opted in
	// (fetch/both). mode=proxy means the key must never leave nexus, which
	// an out-of-process aspect can't honour — skip delivery, warn.
	if cred.Mode != credentials.ModeFetch && cred.Mode != credentials.ModeBoth {
		if log != nil {
			log.Warn("validate: aspect's default provider cred is mode=proxy — not delivered to an out-of-process aspect (set mode=fetch/both for native aspects)",
				"aspect", aspect, "credential", cred.Name, "mode", string(cred.Mode))
		}
		return nil
	}
	if log != nil {
		log.Info("validate: delivering provider-cred env from store (keyless aspect)",
			"aspect", aspect, "credential", cred.Name, "shape", string(shape))
	}
	return env
}

// resolveJudgeConfig resolves the EFFECTIVE cheap-judge provider + model
// (per-aspect override > network default) and the judge credential's env
// overlay, for delivery in the validate response (NEX-373). Best-effort:
// any miss returns zero values so the aspect's own judge defaults apply.
// The env honours the same fetch/both mode-gate as resolveProviderEnv.
func resolveJudgeConfig(ctx context.Context, store *credentials.Store, aspect string, log *slog.Logger) (provider, model string, env map[string]string) {
	if store == nil {
		return "", "", nil
	}
	provider, _ = store.EffectiveJudgeProvider(ctx, aspect)
	model, _ = store.EffectiveJudgeModel(ctx, aspect)
	if cred, _ := store.EffectiveJudgeCredential(ctx, aspect); cred != "" {
		env = resolveNamedCredEnv(ctx, store, aspect, cred, "judge", log)
	}
	return provider, model, env
}

// resolveCompactConfig mirrors resolveJudgeConfig for the compact/summarizer
// (rewriter) tier — model + the compact credential's env overlay.
func resolveCompactConfig(ctx context.Context, store *credentials.Store, aspect string, log *slog.Logger) (model string, env map[string]string) {
	if store == nil {
		return "", nil
	}
	model, _ = store.EffectiveCompactModel(ctx, aspect)
	if cred, _ := store.EffectiveCompactCredential(ctx, aspect); cred != "" {
		env = resolveNamedCredEnv(ctx, store, aspect, cred, "compact", log)
	}
	return model, env
}

// resolveNamedCredEnv materialises a NAMED provider credential's env overlay
// for an aspect, with the same allow-check + fetch/both mode-gate
// resolveProviderEnv applies. Returns nil (caller inherits) on any miss.
// tag is just for log lines ("judge"/"compact").
func resolveNamedCredEnv(ctx context.Context, store *credentials.Store, aspect, credName, tag string, log *slog.Logger) map[string]string {
	if store == nil || credName == "" {
		return nil
	}
	cred, err := store.Get(ctx, credName)
	if err != nil {
		if log != nil {
			log.Warn("validate: "+tag+" credential lookup failed; aspect inherits",
				"aspect", aspect, "credential", credName, "err", err)
		}
		return nil
	}
	if !cred.AllowedFor(aspect) {
		if log != nil {
			log.Warn("validate: "+tag+" credential not allowed for aspect; inheriting",
				"aspect", aspect, "credential", credName)
		}
		return nil
	}
	if cred.Mode != credentials.ModeFetch && cred.Mode != credentials.ModeBoth {
		if log != nil {
			log.Warn("validate: "+tag+" credential is mode=proxy — not delivered to an out-of-process aspect",
				"aspect", aspect, "credential", credName, "mode", string(cred.Mode))
		}
		return nil
	}
	env, err := store.EnvForCredential(cred)
	if err != nil {
		if log != nil {
			log.Warn("validate: "+tag+" credential env materialisation failed; inheriting",
				"aspect", aspect, "credential", credName, "err", err)
		}
		return nil
	}
	return env
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
