// WebAuthn authentication wrapper.
//
// Bridges the operator passkey storage (this package) and the
// go-webauthn library. Provides Begin/Finish helpers for
// registration and login, plus a User adaptor backed by
// PasskeyStore. The webauthn.User identity is constant — there is
// exactly one operator principal in v1, with multiple registered
// devices (passkeys) attached to it.
//
// Session data (the per-ceremony challenge + state the lib needs
// passed through Begin → Finish) is intentionally NOT persisted here;
// the broker holds it in-memory keyed by a short-lived token returned
// to the browser. See nexus/broker/operator_login.go.

package operator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
)

// OperatorWebAuthnID is the constant 32-byte user handle for the
// single operator principal. Stable across all registered devices —
// WebAuthn requires every credential under the same user to share
// the same handle so the authenticator UI can group them. Random-
// looking but deterministic; the value itself isn't a secret.
//
// ASCII "nexus-operator-v1\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" — 32 bytes.
var OperatorWebAuthnID = []byte{
	'n', 'e', 'x', 'u', 's', '-', 'o', 'p', 'e', 'r', 'a', 't', 'o', 'r', '-', 'v',
	'1', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
}

// Auth wraps a go-webauthn instance + the passkey store. Construct
// once at broker startup and share across handlers.
type Auth struct {
	wa    *wa.WebAuthn
	store *PasskeyStore
}

// NewAuth constructs the wrapper. rpID is the effective domain
// ("agentnetwork.<tailnet>.ts.net"); rpDisplayName is operator-
// visible ("The Nexus"); origins is the list of acceptable Origin
// header values (e.g. "https://agentnetwork.<tailnet>.ts.net:7888").
//
// All three are required; a misconfigured RP fails registration AND
// login the same way (challenge mismatch), so failing loud at boot
// is the only useful behavior.
func NewAuth(rpID, rpDisplayName string, origins []string, store *PasskeyStore) (*Auth, error) {
	if rpID == "" {
		return nil, errors.New("operator.NewAuth: empty rpID")
	}
	if rpDisplayName == "" {
		return nil, errors.New("operator.NewAuth: empty rpDisplayName")
	}
	if len(origins) == 0 {
		return nil, errors.New("operator.NewAuth: empty origins")
	}
	if store == nil {
		return nil, errors.New("operator.NewAuth: nil store")
	}
	cfg := &wa.Config{
		RPID:          rpID,
		RPDisplayName: rpDisplayName,
		RPOrigins:     origins,
	}
	w, err := wa.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("operator.NewAuth: webauthn.New: %w", err)
	}
	return &Auth{wa: w, store: store}, nil
}

// operatorUser is the webauthn.User implementation. There's exactly
// one of these per Nexus — the operator principal. WebAuthnCredentials
// is loaded from the store at construction time; the lib reads it
// during Begin/Finish to enforce excludeCredentials (registration)
// and credential lookup (login).
type operatorUser struct {
	credentials []wa.Credential
}

func (u *operatorUser) WebAuthnID() []byte                         { return OperatorWebAuthnID }
func (u *operatorUser) WebAuthnName() string                       { return "operator" }
func (u *operatorUser) WebAuthnDisplayName() string                { return "Operator" }
func (u *operatorUser) WebAuthnCredentials() []wa.Credential       { return u.credentials }

// loadUser materializes the operator's User snapshot from the store.
// Each registered passkey (with non-empty credential_json) becomes a
// webauthn.Credential the lib can match against. Rows lacking
// credential_json (registered before the json column was added, or
// raw test rows) are skipped — they can't satisfy FinishLogin
// anyway, and re-registering refreshes them.
func (a *Auth) loadUser(ctx context.Context) (*operatorUser, error) {
	rows, err := a.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("operator.Auth.loadUser: %w", err)
	}
	u := &operatorUser{}
	for _, p := range rows {
		if p.CredentialJSON == "" {
			continue
		}
		var cred wa.Credential
		if err := json.Unmarshal([]byte(p.CredentialJSON), &cred); err != nil {
			// A row whose credential_json fails to decode is corrupt
			// or schema-skew. Skip rather than aborting the whole
			// ceremony — operator can still log in via a different
			// device, and the bad row will be flagged in registration.
			continue
		}
		u.credentials = append(u.credentials, cred)
	}
	return u, nil
}

// BeginRegistration starts a registration ceremony. Returns the
// CredentialCreation payload the SPA hands to navigator.credentials
// .create(), plus the SessionData the broker must persist between
// Begin and Finish. label is the operator-supplied device name —
// not consumed by the lib but threaded through so FinishRegistration
// can attach it to the resulting passkey.
func (a *Auth) BeginRegistration(ctx context.Context) (*protocol.CredentialCreation, *wa.SessionData, error) {
	u, err := a.loadUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Default options are fine for v1: no attestation requirement
	// (we don't run an MDS3 metadata service), discoverable
	// credentials preferred, user verification preferred.
	cc, sd, err := a.wa.BeginRegistration(u)
	if err != nil {
		return nil, nil, fmt.Errorf("operator.Auth.BeginRegistration: %w", err)
	}
	return cc, sd, nil
}

// FinishRegistration consumes the SPA's PublicKeyCredential response
// (carried in *http.Request as the lib expects), verifies it against
// the prior SessionData, and persists the new passkey. Returns the
// row id on success.
//
// Caller is responsible for matching the SessionData to the request
// (typically via a short-lived cookie or temp token from
// BeginRegistration).
func (a *Auth) FinishRegistration(ctx context.Context, sd *wa.SessionData, label string, r *http.Request) (int64, error) {
	if sd == nil {
		return 0, errors.New("operator.Auth.FinishRegistration: nil session data")
	}
	if label == "" {
		return 0, errors.New("operator.Auth.FinishRegistration: empty label")
	}
	u, err := a.loadUser(ctx)
	if err != nil {
		return 0, err
	}
	cred, err := a.wa.FinishRegistration(u, *sd, r)
	if err != nil {
		return 0, fmt.Errorf("operator.Auth.FinishRegistration: %w", err)
	}
	credJSON, err := json.Marshal(cred)
	if err != nil {
		return 0, fmt.Errorf("operator.Auth.FinishRegistration: marshal credential: %w", err)
	}
	id, err := a.store.Register(ctx, cred.ID, cred.PublicKey, label, string(credJSON))
	if err != nil {
		return 0, err
	}
	return id, nil
}

// BeginLogin starts a login ceremony. Returns the CredentialAssertion
// payload the SPA hands to navigator.credentials.get(), plus
// SessionData the broker must persist for FinishLogin.
//
// If the operator has no registered passkeys, returns an error
// rather than producing a useless assertion the browser would
// reject — surface the missing-registration cleanly.
func (a *Auth) BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, *wa.SessionData, error) {
	u, err := a.loadUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(u.credentials) == 0 {
		return nil, nil, ErrNoPasskeysRegistered
	}
	ca, sd, err := a.wa.BeginLogin(u)
	if err != nil {
		return nil, nil, fmt.Errorf("operator.Auth.BeginLogin: %w", err)
	}
	return ca, sd, nil
}

// FinishLogin consumes the SPA's PublicKeyCredential response,
// verifies it against the stored credential, and updates the row's
// sign_count + last_used_at. Returns the matched passkey row id on
// success.
//
// On replay (sign_count not strictly increasing for a counter-
// supporting authenticator, or downgrade for a previously-counter-
// supporting one), returns ErrSignCountReplay — the SaveSignCount
// rules apply identically here.
func (a *Auth) FinishLogin(ctx context.Context, sd *wa.SessionData, r *http.Request) (int64, error) {
	if sd == nil {
		return 0, errors.New("operator.Auth.FinishLogin: nil session data")
	}
	u, err := a.loadUser(ctx)
	if err != nil {
		return 0, err
	}
	matched, err := a.wa.FinishLogin(u, *sd, r)
	if err != nil {
		return 0, fmt.Errorf("operator.Auth.FinishLogin: %w", err)
	}
	// Re-marshal the (possibly updated) credential and persist along
	// with the new sign_count. The lib mutates the Credential's
	// Authenticator.SignCount during FinishLogin; persisting the
	// whole record means the next ceremony observes the latest state.
	credJSON, err := json.Marshal(matched)
	if err != nil {
		return 0, fmt.Errorf("operator.Auth.FinishLogin: marshal: %w", err)
	}
	// Look up the row id by credential_id — the lib doesn't carry
	// our row id through the ceremony.
	row, err := a.store.GetByCredentialID(ctx, matched.ID)
	if err != nil {
		return 0, fmt.Errorf("operator.Auth.FinishLogin: lookup: %w", err)
	}
	if err := a.store.UpdateAfterLogin(ctx, row.ID, int64(matched.Authenticator.SignCount), string(credJSON)); err != nil {
		return 0, err
	}
	return row.ID, nil
}

// ListPasskeys exposes the underlying store's List for callers (e.g.
// the broker login handler) that need to gate registration on
// "are there any registered passkeys yet" without poking the store
// directly.
func (a *Auth) ListPasskeys(ctx context.Context) ([]Passkey, error) {
	return a.store.List(ctx)
}

// ErrNoPasskeysRegistered surfaces from BeginLogin when no operator
// passkey exists. Distinct from generic "lib failed" so the handler
// can return a clean 409 / "register a device first" message instead
// of a 500.
var ErrNoPasskeysRegistered = errors.New("operator: no passkeys registered")

// RandomChallengeBytes returns N cryptographically-random bytes. Used
// by the broker to mint short-lived session tokens that key
// SessionData between Begin and Finish.
func RandomChallengeBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("operator.RandomChallengeBytes: %w", err)
	}
	return b, nil
}
