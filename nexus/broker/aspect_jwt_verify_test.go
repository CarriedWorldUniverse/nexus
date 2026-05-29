package broker

import (
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// NEX-367 follow-up: aspect /connect must work on a headless / aspect-only
// broker (no operator dashboard, OperatorLogin nil). The aspect session
// JWT is signed by the keyfile validator with the broker's session secret;
// /connect must verify it from Config.SessionSigningSecret, NOT depend on
// OperatorLogin being configured (which is gated behind NEXUS_OPERATOR_RPID).
func TestTryVerifyAspectJWT_NoOperatorLogin(t *testing.T) {
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	b := New(Config{
		Tokens:               NewTokenStore(),
		SessionSigningSecret: secret,
		// OperatorLogin deliberately nil — headless/aspect-only broker.
	}, roster.New())

	now := time.Now()
	tok, err := jwt.Sign(secret, jwt.Claims{
		Sub: "wren",
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign aspect jwt: %v", err)
	}

	info, ok := b.tryVerifyAspectJWT(tok)
	if !ok {
		t.Fatal("aspect JWT should verify against Config.SessionSigningSecret with no OperatorLogin")
	}
	if info.AgentID != "wren" {
		t.Errorf("AgentID = %q, want wren", info.AgentID)
	}
	if info.Admin || info.Operator {
		t.Errorf("aspect token must not carry admin/operator flags: %+v", info)
	}
}

// A JWT signed with the wrong secret must not verify (no silent accept).
func TestTryVerifyAspectJWT_WrongSecret(t *testing.T) {
	b := New(Config{
		Tokens:               NewTokenStore(),
		SessionSigningSecret: []byte("test-secret-32-bytes-padding-vvvv"),
	}, roster.New())

	now := time.Now()
	tok, _ := jwt.Sign([]byte("a-totally-different-secret-32byte"), jwt.Claims{
		Sub: "wren",
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
	})
	if _, ok := b.tryVerifyAspectJWT(tok); ok {
		t.Fatal("aspect JWT signed with the wrong secret must not verify")
	}
}
