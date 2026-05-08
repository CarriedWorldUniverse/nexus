// Validation logic for the keyfile auth handshake.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §5.
//
// Flow (server side, mirroring spec §5 step order):
//
//   1. Decrypt the sealed payload using the server X25519 priv.
//      Failure → ErrDecryptionFailed.
//   2. Unmarshal Payload. Post-decrypt parse failure → ErrMalformedPayload
//      (distinct from #1 so the HTTP layer can render 400 vs. 401
//      accurately).
//   3. Look up the aspect row.
//      - missing → ErrUnknownAspect
//      - retired → ErrRetired
//      - keyfile_version < current → RevokedError (with current version)
//      - keyfile_version > current → also RevokedError (forgery; same
//        rejection shape, no separate sentinel — the wire only needs
//        one revocation message)
//   4. Reconstruct the full Ed25519 key from the seed and verify the
//      derived pubkey matches the stored aspect_pubkey. Defends against
//      a forged keyfile somehow surviving step 1 (paranoia layer; in
//      practice if step 1 succeeds the priv is the real one because
//      the operator minted it).
//   5. ── Part 5 INSERTION POINT ── uniqueness/supersede check (§7).
//      Lives between key-verify and JWT compose: by here we've proven
//      identity, but we haven't issued credentials yet, so an old live
//      session can be torn down before the new JWT goes out.
//   6. Compose Claims and sign with session_signing_secret. JWT TTL
//      drawn from caller-supplied JWTTTL (default 1h per spec §6).
//   7. Load personality bundle.
//   8. Return ValidatedSession with JWT, claims, personality.
//
// What this does NOT do (deferred to Parts 5-7):
//   - Uniqueness/supersede check (§7) — owned by the live WS roster
//     and only meaningful once agentfunnel can actually connect.
//   - personality.refresh push frame — Part 7.

package aspects

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/nacl/box"

	"github.com/nexus-cw/nexus/nexus/jwt"
)

// Sentinels surface specific failure shapes so the HTTP layer can
// render the correct status code per spec §5. Wrapped errors include
// underlying detail for logging; callers compare with errors.Is.
var (
	// ErrDecryptionFailed: sealed payload couldn't be opened. Either
	// the keyfile was sealed against a different Nexus's pubkey, or
	// the bytes were tampered with. → 401.
	ErrDecryptionFailed = errors.New("aspects.Validate: decryption failed")

	// ErrMalformedPayload: decryption succeeded but the inner JSON is
	// missing required fields, has the wrong shape, or fails to
	// unmarshal. Distinct from ErrDecryptionFailed because the failure
	// is on the client encoding side, not on the seal/key side. → 400.
	ErrMalformedPayload = errors.New("aspects.Validate: payload malformed")

	// ErrUnknownAspect: payload decrypted, aspect_name absent from
	// the aspects table. → 404.
	ErrUnknownAspect = errors.New("aspects.Validate: unknown aspect")

	// ErrRetired: aspect exists but its status is 'retired'. → 403.
	ErrRetired = errors.New("aspects.Validate: aspect retired")

	// ErrKeyMismatch: the privkey decoded from the payload does not
	// match the aspect_pubkey stored in DB. Should never happen
	// post-decryption (the operator minted both); if it does, treat
	// as a corruption / forgery attempt. → 403.
	ErrKeyMismatch = errors.New("aspects.Validate: key mismatch (privkey doesn't match stored pubkey)")
)

// RevokedError surfaces the version-mismatch case with the current
// version included so agentfunnel can log it. Not a sentinel because
// it carries data; use errors.As.
type RevokedError struct {
	PresentedVersion int64
	CurrentVersion   int64
}

func (e *RevokedError) Error() string {
	return fmt.Sprintf("aspects.Validate: keyfile version %d revoked (current=%d)",
		e.PresentedVersion, e.CurrentVersion)
}

// ValidatedSession is the successful outcome of Validate. The HTTP
// handler renders this into the spec §5 response shape.
type ValidatedSession struct {
	// AspectName from the validated payload.
	AspectName string

	// KeyfileVersion the aspect presented (== current at validation time).
	KeyfileVersion int64

	// SessionJWT is the HS256 token agentfunnel uses as a bearer for
	// subsequent requests. Same lifetime as Claims.Exp.
	SessionJWT string

	// Claims is the decoded claim set (returned for logging convenience).
	Claims jwt.Claims

	// ExpiresAt mirrors Claims.Exp as a time.Time for convenience.
	ExpiresAt time.Time

	// Aspect provider/model passed through to agentfunnel so it knows
	// which bridle backend to spin up.
	Provider string
	Model    string

	// Personality is the full bundle from aspect_personalities. Empty
	// strings + Version=0 if no personality row exists yet (this is
	// allowed; agentfunnel can run without one).
	Personality *Personality

	// CentralNexusMD is the network-wide central nexus_md from
	// nexus_settings (Part 9). Layered ABOVE Personality.NexusMD in
	// the composed prompt: central holds the operational scope shared
	// by every aspect; per-aspect nexus_md is a short delta. Empty
	// when Part 9 isn't wired (legacy callers without SettingsStore).
	CentralNexusMD string

	// CentralVersion lets agentfunnel detect when central content
	// changes between re-validations, independent of the per-aspect
	// Personality.Version.
	CentralVersion int64
}

// ValidateConfig packages the dependencies Validate needs. Pulled out
// so the handler can build it once at boot from identity.Identity +
// aspects.Store + a clock.
type ValidateConfig struct {
	// Store is the aspects backend.
	Store Store

	// Settings is the nexus_settings backend (Part 9). Optional —
	// when nil, ValidatedSession.CentralNexusMD is left empty and the
	// agent composes from per-aspect content alone (legacy behaviour).
	Settings SettingsStore

	// NexusID is the stable UUID. Goes into the JWT Iss claim.
	NexusID string

	// ServerEd25519Privkey is the Nexus's Ed25519 server private key
	// from nexus_identity. Validate derives the X25519 form internally
	// for the seal-open step.
	ServerEd25519Privkey ed25519.PrivateKey

	// ServerEd25519Pubkey is the matching Ed25519 server public key.
	// Required for box.OpenAnonymous (the API needs both).
	ServerEd25519Pubkey ed25519.PublicKey

	// SessionSigningSecret is the HMAC-SHA256 key for JWT signing.
	SessionSigningSecret []byte

	// JWTTTL is how long issued JWTs live. Default 1h per spec §6.
	JWTTTL time.Duration

	// Now is the clock; defaults to time.Now. Pinned in tests.
	Now func() time.Time

	// NewSessionID generates per-session UUIDs. Defaults to
	// uuid.NewString. Pinned in tests for golden-token comparisons.
	NewSessionID func() string
}

// Validate decrypts the sealed payload, validates the aspect, and
// issues a session JWT + personality bundle. Errors are sentinel-typed
// for the HTTP layer to map to status codes.
func Validate(ctx context.Context, cfg ValidateConfig, encryptedPayloadB64 string) (*ValidatedSession, error) {
	if cfg.Store == nil {
		return nil, errors.New("aspects.Validate: Store nil")
	}
	if cfg.JWTTTL <= 0 {
		cfg.JWTTTL = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewSessionID == nil {
		cfg.NewSessionID = uuid.NewString
	}

	// Step 1: base64 decode + open the sealed box.
	sealed, err := base64.StdEncoding.DecodeString(encryptedPayloadB64)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %v", ErrDecryptionFailed, err)
	}
	xPub, err := EdPubkeyToX25519(cfg.ServerEd25519Pubkey)
	if err != nil {
		return nil, fmt.Errorf("aspects.Validate: convert server pubkey: %w", err)
	}
	xPriv := EdPrivkeyToX25519(cfg.ServerEd25519Privkey)
	plaintext, ok := box.OpenAnonymous(nil, sealed, &xPub, &xPriv)
	if !ok {
		return nil, ErrDecryptionFailed
	}

	// Step 2: parse Payload. Post-decrypt parse failure is a malformed-
	// payload case (HTTP 400), distinct from decryption failure (401):
	// the seal worked, the bytes inside are just wrong.
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		return nil, fmt.Errorf("%w: unmarshal: %v", ErrMalformedPayload, err)
	}
	if p.AspectName == "" {
		return nil, fmt.Errorf("%w: missing aspect_name", ErrMalformedPayload)
	}

	// Step 3: aspect lookup.
	a, err := cfg.Store.Get(ctx, p.AspectName)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrUnknownAspect
	}
	if err != nil {
		return nil, fmt.Errorf("aspects.Validate: lookup %q: %w", p.AspectName, err)
	}
	if a.Status == StatusRetired {
		return nil, ErrRetired
	}
	if p.KeyfileVersion < a.CurrentKeyfileVersion {
		return nil, &RevokedError{
			PresentedVersion: p.KeyfileVersion,
			CurrentVersion:   a.CurrentKeyfileVersion,
		}
	}
	// p.KeyfileVersion > a.CurrentKeyfileVersion is impossible if mint
	// is the only path to creation; treat it as a forgery attempt and
	// reject with the same shape as version-too-low. Don't leak which
	// branch via separate error.
	if p.KeyfileVersion > a.CurrentKeyfileVersion {
		return nil, &RevokedError{
			PresentedVersion: p.KeyfileVersion,
			CurrentVersion:   a.CurrentKeyfileVersion,
		}
	}

	// Step 4: reconstruct full key from seed, verify pubkey matches DB.
	// Bad b64 / wrong-size seed are malformed-payload failures: the
	// seal opened cleanly, the inner privkey is just wrong-shaped.
	seed, err := base64.StdEncoding.DecodeString(p.AspectPrivkey)
	if err != nil {
		return nil, fmt.Errorf("%w: privkey b64: %v", ErrMalformedPayload, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: privkey wrong size %d (want %d)",
			ErrMalformedPayload, len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	derivedPub := priv.Public().(ed25519.PublicKey)
	// Constant-time compare. Mismatch shouldn't happen post-decryption
	// (operator minted both); if it does, treat as corruption.
	if subtle.ConstantTimeCompare(derivedPub, a.AspectPubkey) != 1 {
		return nil, ErrKeyMismatch
	}

	// Step 5 (Part 5 insertion point): uniqueness/supersede check goes
	// here. By this line identity is proven; by the next line the JWT
	// is on the wire. An old live session must be torn down between
	// these two points so credentials don't overlap.

	// Step 6: compose + sign JWT.
	now := cfg.Now()
	exp := now.Add(cfg.JWTTTL)
	claims := jwt.Claims{
		Iss: "nexus://" + cfg.NexusID,
		Sub: p.AspectName,
		Iat: now.Unix(),
		Exp: exp.Unix(),
		Kfv: p.KeyfileVersion,
		Ses: cfg.NewSessionID(),
	}
	tok, err := jwt.Sign(cfg.SessionSigningSecret, claims)
	if err != nil {
		return nil, fmt.Errorf("aspects.Validate: jwt sign: %w", err)
	}

	// Step 7: personality. After JWT compose so the response can be
	// assembled in one pass (the ValidatedSession ships both).
	var personality *Personality
	personality, err = cfg.Store.PersonalityGet(ctx, p.AspectName)
	if errors.Is(err, ErrNotFound) {
		// Allowed: aspect exists but no personality row yet. Caller
		// gets nil and renders an empty bundle.
		personality = nil
	} else if err != nil {
		return nil, fmt.Errorf("aspects.Validate: personality lookup: %w", err)
	}

	// Step 8 (Part 9): central nexus_md from nexus_settings. Layered
	// above the per-aspect bundle in the composed prompt. Optional —
	// nil SettingsStore means "no central content", which is the
	// legacy shape (Part 9 callers always wire it; only test paths
	// might leave it nil).
	//
	// Read failures degrade gracefully to the legacy shape. Identity
	// is already fully verified by this point; central is supplemental
	// context, not auth material, and a transient settings read
	// shouldn't reject a valid handshake. Mirrors frame.Embed's
	// warn-and-continue path.
	var centralContent string
	var centralVersion int64
	if cfg.Settings != nil {
		ns, sErr := cfg.Settings.Get(ctx)
		if sErr != nil {
			// Soft fail — caller still gets a working session, just
			// without the central section. Surface to the HTTP layer
			// log via the broker, not the wire.
			centralContent = ""
			centralVersion = 0
			_ = sErr // intentional: graceful degrade on transient read
		} else {
			centralContent = ns.NexusMD
			centralVersion = ns.Version
		}
	}

	return &ValidatedSession{
		AspectName:     p.AspectName,
		KeyfileVersion: p.KeyfileVersion,
		SessionJWT:     tok,
		Claims:         claims,
		ExpiresAt:      exp,
		Provider:       a.Provider,
		Model:          a.Model,
		Personality:    personality,
		CentralNexusMD: centralContent,
		CentralVersion: centralVersion,
	}, nil
}
