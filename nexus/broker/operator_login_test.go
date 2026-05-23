package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"

	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/operator"
)

// fakeAuth implements OperatorAuth without standing up a real
// WebAuthn ceremony. Each method's behavior is controlled per-test
// via the configured fields.
type fakeAuth struct {
	beginRegResp    *protocol.CredentialCreation
	beginRegSession *wa.SessionData
	beginRegErr     error

	finishRegID  int64
	finishRegErr error

	beginLoginResp    *protocol.CredentialAssertion
	beginLoginSession *wa.SessionData
	beginLoginErr     error

	finishLoginID  int64
	finishLoginErr error

	passkeys    []operator.Passkey
	passkeysErr error
}

func (f *fakeAuth) BeginRegistration(ctx context.Context) (*protocol.CredentialCreation, *wa.SessionData, error) {
	return f.beginRegResp, f.beginRegSession, f.beginRegErr
}
func (f *fakeAuth) FinishRegistration(ctx context.Context, sd *wa.SessionData, label string, r *http.Request) (int64, error) {
	return f.finishRegID, f.finishRegErr
}
func (f *fakeAuth) BeginLogin(ctx context.Context) (*protocol.CredentialAssertion, *wa.SessionData, error) {
	return f.beginLoginResp, f.beginLoginSession, f.beginLoginErr
}
func (f *fakeAuth) FinishLogin(ctx context.Context, sd *wa.SessionData, r *http.Request) (int64, error) {
	return f.finishLoginID, f.finishLoginErr
}
func (f *fakeAuth) ListPasskeys(ctx context.Context) ([]operator.Passkey, error) {
	return f.passkeys, f.passkeysErr
}

// stubCreation / stubAssertion are skeletal protocol structs the
// handler marshals to JSON for the SPA. We don't validate their
// contents in tests — go-webauthn's tests cover that.
func stubCreation() *protocol.CredentialCreation {
	return &protocol.CredentialCreation{
		Response: protocol.PublicKeyCredentialCreationOptions{
			Challenge: []byte("test-challenge-32-bytes-fixed-vlu"),
		},
	}
}
func stubAssertion() *protocol.CredentialAssertion {
	return &protocol.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge: []byte("test-challenge-32-bytes-fixed-vlu"),
		},
	}
}
func stubSession() *wa.SessionData {
	return &wa.SessionData{Challenge: "test-challenge-32-bytes-fixed-vlu"}
}

func newTestLogin(t *testing.T, auth OperatorAuth) *OperatorLogin {
	t.Helper()
	return &OperatorLogin{
		Auth:                 auth,
		SessionSigningSecret: []byte("test-secret-32-bytes-padding-vvvv"),
		JWTTTL:               time.Hour,
		NexusID:              "test-nexus-id",
		Now:                  func() time.Time { return time.Unix(1700000000, 0) },
		NewSessionID:         func() string { return "test-session-id" },
	}
}

func postJSON(t *testing.T, mux *http.ServeMux, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("POST", path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func newOperatorMux(l *OperatorLogin) *http.ServeMux {
	mux := http.NewServeMux()
	l.register(mux)
	return mux
}

func TestRegisterBegin_Bootstrap_Allowed(t *testing.T) {
	auth := &fakeAuth{
		beginRegResp:    stubCreation(),
		beginRegSession: stubSession(),
		passkeys:        nil, // empty → bootstrap allowed
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	rec := postJSON(t, mux, "/api/operator/register/begin", map[string]string{"label": "<operator-host>"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp registerBeginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SessionToken == "" {
		t.Error("session_token empty")
	}
	if len(resp.Options) == 0 {
		t.Error("options empty")
	}
	// Stashed session must be retrievable.
	sess := l.take(resp.SessionToken)
	if sess == nil || sess.label != "<operator-host>" {
		t.Errorf("session not stashed correctly: %+v", sess)
	}
}

func TestRegisterBegin_Subsequent_RequiresJWT(t *testing.T) {
	auth := &fakeAuth{
		beginRegResp:    stubCreation(),
		beginRegSession: stubSession(),
		passkeys:        []operator.Passkey{{ID: 1, Label: "first-device", CredentialJSON: "{}"}},
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	// No JWT → 401
	rec := postJSON(t, mux, "/api/operator/register/begin", map[string]string{"label": "second"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestRegisterBegin_Subsequent_AcceptsValidJWT(t *testing.T) {
	auth := &fakeAuth{
		beginRegResp:    stubCreation(),
		beginRegSession: stubSession(),
		passkeys:        []operator.Passkey{{ID: 1}},
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	// Mint a fresh operator JWT for the call.
	now := l.now()
	tok, err := jwt.Sign(l.SessionSigningSecret, jwt.Claims{
		Iss: "nexus://" + l.NexusID, Sub: "operator",
		Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(), Ses: "x",
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]string{"label": "second"})
	req := httptest.NewRequest("POST", "/api/operator/register/begin", &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterBegin_Subsequent_RejectsNonOperatorSub(t *testing.T) {
	auth := &fakeAuth{
		passkeys: []operator.Passkey{{ID: 1}},
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	// JWT with sub:"keel" must NOT pass the gate.
	now := l.now()
	tok, _ := jwt.Sign(l.SessionSigningSecret, jwt.Claims{
		Iss: "nexus://" + l.NexusID, Sub: "keel",
		Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(), Ses: "x",
	})

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]string{"label": "x"})
	req := httptest.NewRequest("POST", "/api/operator/register/begin", &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-operator sub, got %d", rec.Code)
	}
}

func TestRegisterBegin_RejectsEmptyLabel(t *testing.T) {
	auth := &fakeAuth{passkeys: nil}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	rec := postJSON(t, mux, "/api/operator/register/begin", map[string]string{"label": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRegisterFinish_HappyPath(t *testing.T) {
	auth := &fakeAuth{
		beginRegResp:    stubCreation(),
		beginRegSession: stubSession(),
		passkeys:        nil,
		finishRegID:     42,
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	// Begin to get a session_token.
	beginRec := postJSON(t, mux, "/api/operator/register/begin", map[string]string{"label": "device"})
	var beginResp registerBeginResponse
	_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)

	// Finish.
	req := httptest.NewRequest("POST", "/api/operator/register/finish", strings.NewReader("{}"))
	req.Header.Set("X-Session-Token", beginResp.SessionToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp registerFinishResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || resp.PasskeyID != 42 {
		t.Errorf("unexpected: %+v", resp)
	}

	// Replay must fail — session is one-shot.
	req2 := httptest.NewRequest("POST", "/api/operator/register/finish", strings.NewReader("{}"))
	req2.Header.Set("X-Session-Token", beginResp.SessionToken)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("session replay must 401, got %d", rec2.Code)
	}
}

func TestRegisterFinish_UnknownToken(t *testing.T) {
	auth := &fakeAuth{}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	req := httptest.NewRequest("POST", "/api/operator/register/finish", strings.NewReader("{}"))
	req.Header.Set("X-Session-Token", "ghost")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown token must 401, got %d", rec.Code)
	}
}

func TestSessionExpires(t *testing.T) {
	auth := &fakeAuth{
		beginRegResp:    stubCreation(),
		beginRegSession: stubSession(),
		passkeys:        nil,
	}
	l := newTestLogin(t, auth)
	// Override Now to advance time after begin.
	clock := time.Unix(1700000000, 0)
	l.Now = func() time.Time { return clock }
	mux := newOperatorMux(l)

	rec := postJSON(t, mux, "/api/operator/register/begin", map[string]string{"label": "device"})
	var beginResp registerBeginResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &beginResp)

	// Advance past TTL.
	clock = clock.Add(ceremonyTTL + time.Second)

	req := httptest.NewRequest("POST", "/api/operator/register/finish", strings.NewReader("{}"))
	req.Header.Set("X-Session-Token", beginResp.SessionToken)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expired session must 401, got %d", rec2.Code)
	}
}

func TestLoginBegin_NoPasskeys(t *testing.T) {
	auth := &fakeAuth{
		beginLoginErr: operator.ErrNoPasskeysRegistered,
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	rec := postJSON(t, mux, "/api/operator/login/begin", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("no-passkeys must 409, got %d", rec.Code)
	}
}

func TestLoginFinish_MintsValidJWT(t *testing.T) {
	auth := &fakeAuth{
		beginLoginResp:    stubAssertion(),
		beginLoginSession: stubSession(),
		finishLoginID:     7,
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	// Begin → token.
	beginRec := postJSON(t, mux, "/api/operator/login/begin", nil)
	if beginRec.Code != http.StatusOK {
		t.Fatalf("begin: %d, body: %s", beginRec.Code, beginRec.Body.String())
	}
	var beginResp loginBeginResponse
	_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)

	// Finish → JWT.
	req := httptest.NewRequest("POST", "/api/operator/login/finish", strings.NewReader("{}"))
	req.Header.Set("X-Session-Token", beginResp.SessionToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish: %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp loginFinishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.SessionJWT == "" || resp.PasskeyID != 7 {
		t.Errorf("unexpected response: %+v", resp)
	}

	// Verify the JWT round-trips with our secret + clock.
	claims, err := jwt.Verify(l.SessionSigningSecret, resp.SessionJWT, l.now())
	if err != nil {
		t.Fatalf("JWT must verify: %v", err)
	}
	if claims.Sub != "operator" {
		t.Errorf("sub: got %q, want operator", claims.Sub)
	}
	if claims.Iss != "nexus://"+l.NexusID {
		t.Errorf("iss: got %q", claims.Iss)
	}
	if claims.Exp != l.now().Add(time.Hour).Unix() {
		t.Errorf("exp: got %d, want %d", claims.Exp, l.now().Add(time.Hour).Unix())
	}
}

func TestLoginFinish_FailureSurfacesAs401(t *testing.T) {
	auth := &fakeAuth{
		beginLoginResp:    stubAssertion(),
		beginLoginSession: stubSession(),
		finishLoginErr:    errors.New("verification failed"),
	}
	l := newTestLogin(t, auth)
	mux := newOperatorMux(l)

	beginRec := postJSON(t, mux, "/api/operator/login/begin", nil)
	var beginResp loginBeginResponse
	_ = json.Unmarshal(beginRec.Body.Bytes(), &beginResp)

	req := httptest.NewRequest("POST", "/api/operator/login/finish", strings.NewReader("{}"))
	req.Header.Set("X-Session-Token", beginResp.SessionToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("verification failure must 401, got %d", rec.Code)
	}
}
