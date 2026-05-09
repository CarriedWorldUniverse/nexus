package broker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

// These tests exercise the JWT fallback path in resolveUpgradeAuth.
// The TokenStore-only path is covered by existing ws_test.go cases;
// here we focus on the new operator-JWT branch (5b2).

func newBrokerWithOperatorLogin(t *testing.T, secret []byte, now func() time.Time) *Broker {
	t.Helper()
	b := New(Config{
		Tokens: NewTokenStore(),
		OperatorLogin: &OperatorLogin{
			SessionSigningSecret: secret,
			JWTTTL:               time.Hour,
			NexusID:              "test-nexus",
			Now:                  now,
		},
	}, nil)
	return b
}

func mintOperatorJWT(t *testing.T, secret []byte, sub string, exp time.Time) string {
	t.Helper()
	tok, err := jwt.Sign(secret, jwt.Claims{
		Iss: "nexus://test-nexus",
		Sub: sub,
		Iat: time.Unix(1700000000, 0).Unix(),
		Exp: exp.Unix(),
		Ses: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestResolveUpgradeAuth_OperatorJWT_Header(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := newBrokerWithOperatorLogin(t, secret, clock)

	tok := mintOperatorJWT(t, secret, "operator", now.Add(time.Hour))
	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	info, ok := b.resolveUpgradeAuth(req)
	if !ok {
		t.Fatal("operator JWT must resolve via fallback")
	}
	if info.AgentID != "operator" {
		t.Errorf("AgentID: got %q want operator", info.AgentID)
	}
	if !info.Operator {
		t.Error("Operator flag must be true")
	}
	if !info.Admin {
		t.Error("Admin must be true for operator")
	}
	if info.ViaLegacy {
		t.Error("ViaLegacy must be false for operator JWT")
	}
}

func TestResolveUpgradeAuth_OperatorJWT_QueryParam(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := newBrokerWithOperatorLogin(t, secret, clock)

	tok := mintOperatorJWT(t, secret, "operator", now.Add(time.Hour))
	req := httptest.NewRequest("GET", "/connect?token="+tok, nil)

	info, ok := b.resolveUpgradeAuth(req)
	if !ok {
		t.Fatal("operator JWT via query param must resolve")
	}
	if !info.Operator {
		t.Error("Operator flag must be true via query path too")
	}
}

func TestResolveUpgradeAuth_RejectsExpiredJWT(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := newBrokerWithOperatorLogin(t, secret, clock)

	// Issued in the past, already expired by `now`.
	tok := mintOperatorJWT(t, secret, "operator", now.Add(-time.Minute))
	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, ok := b.resolveUpgradeAuth(req); ok {
		t.Error("expired JWT must not resolve")
	}
}

func TestResolveUpgradeAuth_RejectsBadSignature(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	wrongSecret := []byte("different-secret-padding-vvvvvvvvvv")
	b := newBrokerWithOperatorLogin(t, secret, clock)

	tok := mintOperatorJWT(t, wrongSecret, "operator", now.Add(time.Hour))
	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, ok := b.resolveUpgradeAuth(req); ok {
		t.Error("JWT signed with wrong secret must not resolve")
	}
}

func TestResolveUpgradeAuth_RejectsNonOperatorSub(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := newBrokerWithOperatorLogin(t, secret, clock)

	// Aspect-issued JWT (sub:"keel") must NOT pass operator fallback —
	// aspects authenticate via TokenStore, not WS JWT.
	tok := mintOperatorJWT(t, secret, "keel", now.Add(time.Hour))
	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, ok := b.resolveUpgradeAuth(req); ok {
		t.Error("JWT with sub:keel must not resolve as operator")
	}
}

func TestResolveUpgradeAuth_TokenStoreWinsOverJWT(t *testing.T) {
	// If a token happens to match in TokenStore, that path is taken
	// even if it would also parse as a JWT (it can't, but pin the
	// ordering anyway: aspect tokens are not JWTs).
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := newBrokerWithOperatorLogin(t, secret, clock)
	b.cfg.Tokens.SetTokenForTest("anvil", "anvil-token", false)

	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer anvil-token")

	info, ok := b.resolveUpgradeAuth(req)
	if !ok {
		t.Fatal("expected resolve")
	}
	if info.AgentID != "anvil" {
		t.Errorf("expected anvil identity, got %q", info.AgentID)
	}
	if info.Operator {
		t.Error("aspect token must not be flagged as Operator")
	}
}

func TestResolveUpgradeAuth_NoOperatorLogin_FallsThroughToTokenStoreOnly(t *testing.T) {
	// Broker without OperatorLogin configured: any JWT bearer must
	// 401 since there's no signing secret to verify against.
	b := New(Config{Tokens: NewTokenStore()}, nil)

	now := time.Unix(1700000000, 0)
	tok := mintOperatorJWT(t, []byte("any-secret-padding-vvvvvvvvvvvv"), "operator", now.Add(time.Hour))
	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, ok := b.resolveUpgradeAuth(req); ok {
		t.Error("JWT must not resolve when OperatorLogin is unconfigured")
	}
}

func TestResolveUpgradeAuth_MissingToken(t *testing.T) {
	b := newBrokerWithOperatorLogin(t, []byte("secret-padding-vvvvvvvvvvvvvvvvv"), nil)
	req := httptest.NewRequest("GET", "/connect", nil)
	if _, ok := b.resolveUpgradeAuth(req); ok {
		t.Error("missing token must not resolve")
	}
}

// Compile-time guard against a runtime nil deref if Now is nil.
func TestResolveUpgradeAuth_NilNow(t *testing.T) {
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := New(Config{
		Tokens: NewTokenStore(),
		OperatorLogin: &OperatorLogin{
			SessionSigningSecret: secret,
			JWTTTL:               time.Hour,
			NexusID:              "test-nexus",
			// Now intentionally nil — code must fall back to time.Now.
		},
	}, nil)

	tok := mintOperatorJWT(t, secret, "operator", time.Now().Add(time.Hour))
	req := httptest.NewRequest("GET", "/connect", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, ok := b.resolveUpgradeAuth(req); !ok {
		t.Error("nil Now must fall back to time.Now and accept fresh JWT")
	}
}

// http.Request type-assert to ensure tests compile against the
// actual signature even if it changes.
var _ http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
