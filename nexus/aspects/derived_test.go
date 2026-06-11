package aspects

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

func derivedMintConfig(now time.Time) RefreshConfig {
	return RefreshConfig{
		NexusID:              "test-nexus-id",
		SessionSigningSecret: []byte("test-secret-32-bytes-padding-xyz"),
		NewSessionID:         func() string { return "hand-session-uuid" },
		Now:                  func() time.Time { return now },
		JWTTTL:               time.Hour,
	}
}

// The derived mint round-trip: the hand's JWT signs with the broker
// secret, sub = the derived name, kfv = the PARENT's keyfile version
// (mirrored so kfv-based revocation enforcement, once wired, fences
// hands with their parent) — the exact claim shape validate/refresh
// issue, so the broker's WS upgrade (tryVerifyAspectJWT) accepts it
// with no new crypto.
func TestMintDerivedSessionRoundTrip(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	cfg := derivedMintConfig(now)
	parent := &Aspect{Name: "plumb", CurrentKeyfileVersion: 3}

	sess, err := MintDerivedSession(cfg, parent, "plumb.sub-2")
	if err != nil {
		t.Fatalf("MintDerivedSession: %v", err)
	}
	if sess.AspectName != "plumb.sub-2" {
		t.Errorf("AspectName = %q", sess.AspectName)
	}
	claims, err := jwt.Verify(cfg.SessionSigningSecret, sess.SessionJWT, now)
	if err != nil {
		t.Fatalf("derived JWT failed self-verify: %v", err)
	}
	if claims.Sub != "plumb.sub-2" {
		t.Errorf("sub = %q, want plumb.sub-2", claims.Sub)
	}
	if claims.Kfv != 3 {
		t.Errorf("kfv = %d, want parent's 3 (mirrored so future kfv-based revocation enforcement will fence hands)", claims.Kfv)
	}
	if claims.Iss != "nexus://test-nexus-id" {
		t.Errorf("iss = %q", claims.Iss)
	}
	if claims.Exp != now.Add(time.Hour).Unix() {
		t.Errorf("exp = %d", claims.Exp)
	}
}

func TestMintDerivedSessionRejections(t *testing.T) {
	cfg := derivedMintConfig(time.Now())
	cases := []struct {
		name    string
		parent  *Aspect
		derived string
	}{
		{"nil parent", nil, "plumb.sub-1"},
		{"not a derived name", &Aspect{Name: "plumb"}, "plumb"},
		{"wrong lineage", &Aspect{Name: "plumb"}, "anvil.sub-1"},
		{"sub-of-sub", &Aspect{Name: "plumb.sub-1"}, "plumb.sub-1.sub-1"},
	}
	for _, c := range cases {
		if _, err := MintDerivedSession(cfg, c.parent, c.derived); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
	if _, err := MintDerivedSession(cfg, &Aspect{Name: "plumb", Status: StatusRetired}, "plumb.sub-1"); !errors.Is(err, ErrRetired) {
		t.Errorf("retired parent: err = %v, want ErrRetired", err)
	}
}

// Persona lookup for a derived name serves the BASE aspect's bundle.
// Exercised through Validate with an aspects row for the derived name
// (the herald-rooted future shape) and a personality row only on the
// base: the hand must come up with its parent's soul.
func TestValidateDerivedNameServesBasePersona(t *testing.T) {
	store := freshStore(t)
	ctx := context.Background()

	serverPub, serverPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subPub, subPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// Base aspect owns the personality; the derived row has its own key.
	if err := store.Insert(ctx, Aspect{Name: "plumb", AspectPubkey: subPub}); err != nil {
		t.Fatal(err)
	}
	if err := store.PersonalitySet(ctx, Personality{
		AspectName: "plumb",
		NexusMD:    "plumb nexus.md",
		SoulMD:     "plumb soul",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, Aspect{Name: "plumb.sub-1", AspectPubkey: subPub}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	kf, _, err := Mint(MintInput{
		AspectName:     "plumb.sub-1",
		KeyfileVersion: 1,
		AspectPrivkey:  subPriv,
		ServerPubkey:   serverPub,
		NexusID:        "test-nexus-id",
		NexusURL:       "wss://test/connect",
		MintedAt:       now,
	})
	if err != nil {
		t.Fatal(err)
	}

	sess, err := Validate(ctx, ValidateConfig{
		Store:                store,
		NexusID:              "test-nexus-id",
		ServerEd25519Privkey: serverPriv,
		ServerEd25519Pubkey:  serverPub,
		SessionSigningSecret: []byte("test-secret-32-bytes-padding-xyz"),
		JWTTTL:               time.Hour,
		Now:                  func() time.Time { return now },
		NewSessionID:         func() string { return "fixed" },
	}, kf.EncryptedPayload)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.AspectName != "plumb.sub-1" {
		t.Fatalf("AspectName = %q", sess.AspectName)
	}
	if sess.Personality == nil {
		t.Fatal("Personality nil — derived name must serve the base aspect's bundle")
	}
	if sess.Personality.SoulMD != "plumb soul" || sess.Personality.NexusMD != "plumb nexus.md" {
		t.Errorf("persona = %+v, want plumb's bundle", sess.Personality)
	}
}
