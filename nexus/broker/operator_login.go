// Operator login endpoints — dashboard-ws-port spec §2.2 / §5.3.
//
// Four routes land here, all bypass the auth() middleware (the
// passkey ceremony IS the credential):
//
//   POST /api/operator/register/begin   → start a registration ceremony
//   POST /api/operator/register/finish  → consume credential.create() result
//   POST /api/operator/login/begin      → start a login ceremony
//   POST /api/operator/login/finish     → consume credential.get() result, mint JWT
//
// Begin handlers return a short-lived `session_token` the browser
// must echo on the matching Finish call. Per-token state (the
// webauthn.SessionData the lib needs to verify the response) lives
// in an in-memory map gated by the session_token; entries TTL out
// after 5m. Process restart drops live ceremonies — operators just
// re-tap.
//
// Login success mints an HS256 JWT with sub:"operator" and a
// 1h TTL (matched to KeyfileValidator.JWTTTL so admin endpoints
// see the same lifetime). The dashboard SPA carries this JWT on
// the WS upgrade as the bearer token.
//
// Registration is gated. Two policies:
//
//   - First-passkey bootstrap: when no rows exist in operator_passkeys,
//     anyone with network reach can register the first device. This
//     is the cold-start path; operator runs `nexus operator
//     register-passkey` (5b2 CLI) on first install. Tighten in a
//     follow-up by requiring the bootstrap to come from localhost.
//   - Subsequent registrations: existing operator JWT required. A
//     logged-in operator can add more devices.

package broker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"

	"github.com/nexus-cw/nexus/nexus/jwt"
	"github.com/nexus-cw/nexus/nexus/operator"
)

// OperatorAuth abstracts the *operator.Auth surface this package
// uses. Lets tests substitute a fake without standing up a real
// WebAuthn ceremony (which requires a live authenticator). The
// production binding is *operator.Auth; tests inject a hand-rolled
// fake.
type OperatorAuth interface {
	BeginRegistration(ctx context.Context) (*protocol.CredentialCreation, *wa.SessionData, error)
	FinishRegistration(ctx context.Context, sd *wa.SessionData, label string, r *http.Request) (int64, error)
	BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, *wa.SessionData, error)
	FinishLogin(ctx context.Context, sd *wa.SessionData, r *http.Request) (int64, error)
	ListPasskeys(ctx context.Context) ([]operator.Passkey, error)
}

// Compile-time check that *operator.Auth satisfies OperatorAuth.
// Catches method-set drift the moment Auth grows or renames a
// method without the interface being updated to match.
var _ OperatorAuth = (*operator.Auth)(nil)

// OperatorLogin wires the operator endpoints. Carries the auth
// wrapper, a clock, and the in-memory ceremony-session store.
type OperatorLogin struct {
	Auth OperatorAuth

	// SessionSigningSecret + JWTTTL + NexusID share semantics with
	// KeyfileValidator — inject the same values at construction so
	// operator JWTs use the same signing secret as aspect JWTs.
	//
	// CAVEAT: today the WS upgrade path (resolveUpgradeAuth in ws.go)
	// resolves bearer tokens via TokenStore — a static map of opaque
	// per-aspect tokens. It does NOT call jwt.Verify. An operator JWT
	// minted here will be REJECTED by /connect until 5c lands JWT-
	// aware WS auth. The JWT works for HTTP endpoints that already
	// call jwt.Verify (e.g. aspect_self_edit), and for the gating
	// check in registrationGated below.
	SessionSigningSecret []byte
	JWTTTL               time.Duration
	NexusID              string

	// Now overrides time.Now for tests. Production callers leave nil.
	Now func() time.Time

	// NewSessionID generates session UUIDs for the JWT ses claim.
	// Defaults to a random hex string (16 bytes) when nil.
	NewSessionID func() string

	mu       sync.Mutex
	sessions map[string]*ceremonySession
}

// ceremonySession ties a Begin response to a Finish request. label
// is captured at register-begin time so register-finish doesn't need
// to re-pass it (and can't be lied about by the browser between
// begin and finish).
type ceremonySession struct {
	data    *wa.SessionData
	label   string
	expires time.Time
}

const (
	// ceremonyTTL bounds how long a Begin response remains valid.
	// 5 minutes covers a slow operator + browser passkey UI dialog;
	// abandoned ceremonies clear out so the in-memory map doesn't
	// leak.
	ceremonyTTL = 5 * time.Minute
	// ceremonyTokenBytes is the size of the random token the browser
	// echoes on Finish. 16 bytes = 128 bits, plenty for collision
	// resistance over a 5-minute window with single-digit operators.
	ceremonyTokenBytes = 16
)

// register attaches operator routes to mux. Called from
// ListenAndServe when broker.Config carries an OperatorLogin.
// Following the pattern of registerKeyfileEndpoints — bypass the
// auth() middleware because the passkey ceremony IS the credential.
func (l *OperatorLogin) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/operator/register/begin", l.handleRegisterBegin)
	mux.HandleFunc("POST /api/operator/register/finish", l.handleRegisterFinish)
	mux.HandleFunc("POST /api/operator/login/begin", l.handleLoginBegin)
	mux.HandleFunc("POST /api/operator/login/finish", l.handleLoginFinish)
}

func (l *OperatorLogin) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *OperatorLogin) newSessionID() string {
	if l.NewSessionID != nil {
		return l.NewSessionID()
	}
	b, err := operator.RandomChallengeBytes(16)
	if err != nil {
		// crypto/rand.Read failure is impossible in practice; if it
		// happens we want loud failure, not a degraded session id.
		panic("operator login: rand failure: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// stash records a ceremony session and returns the token. Sweeps
// expired sessions on every call — the map stays bounded without a
// background goroutine.
func (l *OperatorLogin) stash(data *wa.SessionData, label string) (string, error) {
	tokenBytes, err := operator.RandomChallengeBytes(ceremonyTokenBytes)
	if err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sessions == nil {
		l.sessions = make(map[string]*ceremonySession)
	}
	now := l.now()
	for k, v := range l.sessions {
		if now.After(v.expires) {
			delete(l.sessions, k)
		}
	}
	l.sessions[token] = &ceremonySession{
		data:    data,
		label:   label,
		expires: now.Add(ceremonyTTL),
	}
	return token, nil
}

// take pulls a ceremony session by token and removes it (one-shot —
// Finish must succeed on the first try; the browser doesn't get to
// retry with the same session_token). Returns nil if missing or
// expired.
func (l *OperatorLogin) take(token string) *ceremonySession {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[token]
	if !ok {
		return nil
	}
	delete(l.sessions, token)
	if l.now().After(s.expires) {
		return nil
	}
	return s
}

// registrationGated reports whether a registration request is
// allowed. The first registration is always permitted (bootstrap);
// subsequent registrations require a valid operator JWT in the
// Authorization header.
//
// BOOTSTRAP TOCTOU: two concurrent requests both observing
// len(rows)==0 will both pass. Each ceremony produces a distinct
// credential so both could land — the table ends up with two
// "first" devices instead of one. Acceptable in v1 given:
//
//   - tight race window (browser-driven ceremonies, single human),
//   - tailnet-only deployment surface (not exposed to attackers),
//   - operator can delete the extra row via the manage-devices view.
//
// The localhost-binding follow-up will close this structurally —
// only one local terminal session can race itself, and the
// follow-up makes that the only path.
func (l *OperatorLogin) registrationGated(ctx context.Context, r *http.Request) error {
	rows, err := l.Auth.ListPasskeys(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil // bootstrap path
	}
	token := ExtractBearer(r.Header.Get("Authorization"))
	if token == "" {
		return errRegistrationLocked
	}
	claims, err := jwt.Verify(l.SessionSigningSecret, token, l.now())
	if err != nil || claims.Sub != "operator" {
		return errRegistrationLocked
	}
	return nil
}

var errRegistrationLocked = errors.New("operator: registration locked — log in first")

// --- handlers ---

type registerBeginRequest struct {
	Label string `json:"label"`
}

type registerBeginResponse struct {
	SessionToken string          `json:"session_token"`
	Options      json.RawMessage `json:"options"`
}

func (l *OperatorLogin) handleRegisterBegin(w http.ResponseWriter, r *http.Request) {
	var req registerBeginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Label == "" {
		writeOpErr(w, http.StatusBadRequest, "label required")
		return
	}

	if err := l.registrationGated(r.Context(), r); err != nil {
		writeOpErr(w, http.StatusUnauthorized, err.Error())
		return
	}

	cc, sd, err := l.Auth.BeginRegistration(r.Context())
	if err != nil {
		writeOpErr(w, http.StatusInternalServerError, "begin: "+err.Error())
		return
	}
	token, err := l.stash(sd, req.Label)
	if err != nil {
		writeOpErr(w, http.StatusInternalServerError, "stash: "+err.Error())
		return
	}
	options, err := json.Marshal(cc)
	if err != nil {
		writeOpErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	writeOpJSON(w, http.StatusOK, registerBeginResponse{
		SessionToken: token,
		Options:      options,
	})
}

type registerFinishResponse struct {
	OK        bool  `json:"ok"`
	PasskeyID int64 `json:"passkey_id"`
}

func (l *OperatorLogin) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("session_token")
	if token == "" {
		writeOpErr(w, http.StatusBadRequest, "session_token required")
		return
	}
	sess := l.take(token)
	if sess == nil {
		writeOpErr(w, http.StatusUnauthorized, "session_token unknown or expired")
		return
	}
	id, err := l.Auth.FinishRegistration(r.Context(), sess.data, sess.label, r)
	if err != nil {
		writeOpErr(w, http.StatusBadRequest, "finish: "+err.Error())
		return
	}
	writeOpJSON(w, http.StatusOK, registerFinishResponse{OK: true, PasskeyID: id})
}

type loginBeginResponse struct {
	SessionToken string          `json:"session_token"`
	Options      json.RawMessage `json:"options"`
}

func (l *OperatorLogin) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	ca, sd, err := l.Auth.BeginLogin(r.Context())
	if err != nil {
		if errors.Is(err, operator.ErrNoPasskeysRegistered) {
			writeOpErr(w, http.StatusConflict, "no passkeys registered")
			return
		}
		writeOpErr(w, http.StatusInternalServerError, "begin: "+err.Error())
		return
	}
	token, err := l.stash(sd, "")
	if err != nil {
		writeOpErr(w, http.StatusInternalServerError, "stash: "+err.Error())
		return
	}
	options, err := json.Marshal(ca)
	if err != nil {
		writeOpErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	writeOpJSON(w, http.StatusOK, loginBeginResponse{
		SessionToken: token,
		Options:      options,
	})
}

type loginFinishResponse struct {
	OK              bool   `json:"ok"`
	SessionJWT      string `json:"session_jwt"`
	SessionExpires  string `json:"session_expires"`
	PasskeyID       int64  `json:"passkey_id"`
}

func (l *OperatorLogin) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("session_token")
	if token == "" {
		writeOpErr(w, http.StatusBadRequest, "session_token required")
		return
	}
	sess := l.take(token)
	if sess == nil {
		writeOpErr(w, http.StatusUnauthorized, "session_token unknown or expired")
		return
	}
	id, err := l.Auth.FinishLogin(r.Context(), sess.data, r)
	if err != nil {
		writeOpErr(w, http.StatusUnauthorized, "finish: "+err.Error())
		return
	}

	// Mint operator JWT — same shape as aspect JWTs, sub="operator".
	now := l.now()
	exp := now.Add(l.JWTTTL)
	if l.JWTTTL <= 0 {
		exp = now.Add(time.Hour)
	}
	claims := jwt.Claims{
		Iss: "nexus://" + l.NexusID,
		Sub: "operator",
		Iat: now.Unix(),
		Exp: exp.Unix(),
		// Kfv = 0 for operators — no keyfile rotation, the passkey
		// IS the long-term credential. No consumer reads Kfv today
		// for enforcement (validate.go uses it for aspect-specific
		// revocation; aspect_self_edit + ws auth ignore it).
		//
		// FORWARD-RISK INVARIANT: any future Kfv-based revocation
		// gate added to a shared JWT-verify path (e.g. 5c WS auth)
		// MUST explicitly exclude tokens where sub == "operator", or
		// route operator tokens through a separate verification
		// path. Otherwise Kfv:0 will trip a `claims.Kfv < current`
		// check the moment current_keyfile_version is non-zero.
		Kfv: 0,
		Ses: l.newSessionID(),
	}
	tok, err := jwt.Sign(l.SessionSigningSecret, claims)
	if err != nil {
		writeOpErr(w, http.StatusInternalServerError, "jwt: "+err.Error())
		return
	}
	writeOpJSON(w, http.StatusOK, loginFinishResponse{
		OK:             true,
		SessionJWT:     tok,
		SessionExpires: exp.UTC().Format(time.RFC3339),
		PasskeyID:      id,
	})
}

// writeOpJSON / writeOpErr are local helpers keeping the operator
// endpoints self-contained — they don't reach into the package's
// generic helpers (which assume aspect-flavored response shapes).
func writeOpJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpErr(w http.ResponseWriter, status int, msg string) {
	writeOpJSON(w, status, map[string]string{"error": msg})
}
