package aspects

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"

	"github.com/nexus-cw/nexus/nexus/jwt"
)

// validateFixture sets up a fresh DB, server identity, and a minted
// keyfile for "plumb" at version 1. Returns the bits the test needs to
// drive Validate. Pinned clock + session ID for deterministic JWTs.
type validateFixture struct {
	cfg    ValidateConfig
	sealed string // base64 encrypted_payload — the input to Validate
	store  *SQLStore
	now    time.Time
}

func newValidateFixture(t *testing.T) *validateFixture {
	t.Helper()
	store := freshStore(t)

	serverPub, serverPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	aspectPub, aspectPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("aspect key: %v", err)
	}

	if err := store.Insert(context.Background(), Aspect{
		Name:         "plumb",
		AspectPubkey: aspectPub,
		Provider:     "claude-api",
		Model:        "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("store.Insert: %v", err)
	}

	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	kf, _, err := Mint(MintInput{
		AspectName:     "plumb",
		KeyfileVersion: 1,
		AspectPrivkey:  aspectPriv,
		ServerPubkey:   serverPub,
		NexusID:        "test-nexus-id",
		NexusURL:       "wss://test/connect",
		MintedAt:       now,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	cfg := ValidateConfig{
		Store:                store,
		NexusID:              "test-nexus-id",
		ServerEd25519Privkey: serverPriv,
		ServerEd25519Pubkey:  serverPub,
		SessionSigningSecret: []byte("test-secret-32-bytes-padding-xyz"),
		JWTTTL:               time.Hour,
		Now:                  func() time.Time { return now },
		NewSessionID:         func() string { return "fixed-session-uuid" },
	}
	return &validateFixture{cfg: cfg, sealed: kf.EncryptedPayload, store: store, now: now}
}

// TestValidate_HappyPath — the contract. Decrypt → lookup → key-match
// → sign → return. Every successful keyfile validation runs this path.
func TestValidate_HappyPath(t *testing.T) {
	f := newValidateFixture(t)
	sess, err := Validate(context.Background(), f.cfg, f.sealed)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.AspectName != "plumb" {
		t.Errorf("AspectName = %q; want plumb", sess.AspectName)
	}
	if sess.KeyfileVersion != 1 {
		t.Errorf("KeyfileVersion = %d; want 1", sess.KeyfileVersion)
	}
	if sess.Provider != "claude-api" || sess.Model != "claude-opus-4-7" {
		t.Errorf("provider/model passthrough wrong: %q / %q", sess.Provider, sess.Model)
	}
	if sess.SessionJWT == "" {
		t.Error("SessionJWT empty")
	}
	// JWT must verify with the same secret + pinned clock.
	got, err := jwt.Verify(f.cfg.SessionSigningSecret, sess.SessionJWT, f.now)
	if err != nil {
		t.Fatalf("issued JWT failed self-verify: %v", err)
	}
	if got.Sub != "plumb" || got.Kfv != 1 || got.Ses != "fixed-session-uuid" {
		t.Errorf("claims wrong: %+v", got)
	}
	if got.Iss != "nexus://test-nexus-id" {
		t.Errorf("iss = %q; want nexus://test-nexus-id", got.Iss)
	}
	if got.Exp != f.now.Add(time.Hour).Unix() {
		t.Errorf("exp = %d; want %d", got.Exp, f.now.Add(time.Hour).Unix())
	}
	if sess.Personality != nil {
		t.Errorf("Personality should be nil (no personality row inserted); got %+v", sess.Personality)
	}
}

// TestValidate_DeliversCentralNexusMD — Part 9b: when a SettingsStore
// is wired and nexus_settings.nexus_md is populated, Validate
// surfaces the central content + version on the response so
// agentfunnel can layer it above the per-aspect bundle.
func TestValidate_DeliversCentralNexusMD(t *testing.T) {
	f := newValidateFixture(t)
	settings := NewSQLSettingsStore(f.store.DBForTest())
	if _, err := settings.SetNexusMD(context.Background(), "## central"); err != nil {
		t.Fatalf("SetNexusMD: %v", err)
	}
	f.cfg.Settings = settings

	sess, err := Validate(context.Background(), f.cfg, f.sealed)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.CentralNexusMD != "## central" {
		t.Errorf("CentralNexusMD = %q; want ## central", sess.CentralNexusMD)
	}
	if sess.CentralVersion != 1 {
		t.Errorf("CentralVersion = %d; want 1", sess.CentralVersion)
	}
}

// TestValidate_NoSettingsStore_LegacyShape — when SettingsStore is
// absent (legacy callers, tests), Central fields are zero-valued and
// the response shape stays Part 5 / spec §5 compatible.
func TestValidate_NoSettingsStore_LegacyShape(t *testing.T) {
	f := newValidateFixture(t)
	// f.cfg.Settings is nil by default.

	sess, err := Validate(context.Background(), f.cfg, f.sealed)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.CentralNexusMD != "" || sess.CentralVersion != 0 {
		t.Errorf("legacy shape: central populated unexpectedly: %q / %d",
			sess.CentralNexusMD, sess.CentralVersion)
	}
}

// TestValidate_DeliversPersonalityWhenSet — the bundle-fetch path. If
// a personality row exists, Validate must pass it through.
func TestValidate_DeliversPersonalityWhenSet(t *testing.T) {
	f := newValidateFixture(t)
	if err := f.store.PersonalitySet(context.Background(), Personality{
		AspectName: "plumb", NexusMD: "## plumb operational",
		SoulMD: "soul", PrimerMD: "primer",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}
	sess, err := Validate(context.Background(), f.cfg, f.sealed)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sess.Personality == nil {
		t.Fatal("Personality nil; want bundle")
	}
	if sess.Personality.NexusMD != "## plumb operational" {
		t.Errorf("NexusMD = %q; want '## plumb operational'", sess.Personality.NexusMD)
	}
	if sess.Personality.Version != 1 {
		t.Errorf("Personality.Version = %d; want 1", sess.Personality.Version)
	}
}

// TestValidate_DecryptionFailed — the seal was made for a different
// Nexus's pubkey. Most common real-world failure mode: keyfile from a
// different Nexus instance presented to this one.
func TestValidate_DecryptionFailed(t *testing.T) {
	f := newValidateFixture(t)
	// Mint a keyfile against a *different* server pubkey.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	kf, _, err := Mint(MintInput{
		AspectName: "plumb", KeyfileVersion: 1,
		AspectPrivkey: otherPriv, ServerPubkey: otherPub,
		NexusID: "x", NexusURL: "wss://x", MintedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = Validate(context.Background(), f.cfg, kf.EncryptedPayload)
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Errorf("Validate cross-Nexus = %v; want ErrDecryptionFailed", err)
	}
}

// TestValidate_DecryptionFailed_BadBase64 — input doesn't even
// base64-decode. → still ErrDecryptionFailed, never panics.
func TestValidate_DecryptionFailed_BadBase64(t *testing.T) {
	f := newValidateFixture(t)
	_, err := Validate(context.Background(), f.cfg, "this is not base64!!!")
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Errorf("Validate bad b64 = %v; want ErrDecryptionFailed", err)
	}
}

// TestValidate_DecryptionFailed_EmptyInput — defensive shape.
func TestValidate_DecryptionFailed_EmptyInput(t *testing.T) {
	f := newValidateFixture(t)
	_, err := Validate(context.Background(), f.cfg, "")
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Errorf("Validate empty = %v; want ErrDecryptionFailed", err)
	}
}

// TestValidate_UnknownAspect — payload decrypts cleanly but the
// aspect_name isn't in the DB. Could happen if the row was deleted
// between mint and use, or if a forged payload (decrypts only because
// we used the right seal but referenced a non-existent aspect name)
// got crafted.
func TestValidate_UnknownAspect(t *testing.T) {
	f := newValidateFixture(t)
	// Drop the row we inserted in the fixture so the otherwise-valid
	// keyfile presents a name that no longer exists.
	if _, err := f.store.db.ExecContext(context.Background(),
		`DELETE FROM aspects WHERE name = ?`, "plumb"); err != nil {
		t.Fatalf("delete aspect: %v", err)
	}
	_, err := Validate(context.Background(), f.cfg, f.sealed)
	if !errors.Is(err, ErrUnknownAspect) {
		t.Errorf("Validate unknown = %v; want ErrUnknownAspect", err)
	}
}

// TestValidate_Retired — aspect exists but is retired. Hard rejection.
func TestValidate_Retired(t *testing.T) {
	f := newValidateFixture(t)
	if err := f.store.SetStatus(context.Background(), "plumb", StatusRetired); err != nil {
		t.Fatalf("SetStatus retired: %v", err)
	}
	_, err := Validate(context.Background(), f.cfg, f.sealed)
	if !errors.Is(err, ErrRetired) {
		t.Errorf("Validate retired = %v; want ErrRetired", err)
	}
}

// TestValidate_Revoked_VersionTooLow — the load-bearing auto-revoke
// case. Operator re-minted (current version is now 2); the v1 keyfile
// must be rejected with the current version surfaced.
func TestValidate_Revoked_VersionTooLow(t *testing.T) {
	f := newValidateFixture(t)
	// Bump the DB to version 2 + new pubkey, but present the v1 keyfile.
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := f.store.BumpKeyfileVersion(context.Background(), "plumb", newPub); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	_, err := Validate(context.Background(), f.cfg, f.sealed)
	var rev *RevokedError
	if !errors.As(err, &rev) {
		t.Fatalf("Validate revoked = %v; want *RevokedError", err)
	}
	if rev.PresentedVersion != 1 || rev.CurrentVersion != 2 {
		t.Errorf("RevokedError = %+v; want presented=1 current=2", rev)
	}
}

// TestValidate_Revoked_VersionTooHigh — paranoia branch. A keyfile
// presenting a version higher than DB is impossible from a well-behaved
// mint, so treat it as forgery and reject the same way.
func TestValidate_Revoked_VersionTooHigh(t *testing.T) {
	f := newValidateFixture(t)
	// Hand-mint a keyfile claiming version 999 — has to bypass the
	// production Mint which derives version from the DB. Easiest: use
	// the same crypto helpers Mint uses internally, but we need a way
	// to set a specific version. Re-call Mint with a fake version
	// number — Mint accepts any positive int.
	serverPub := f.cfg.ServerEd25519Pubkey
	_, aspectPriv, _ := ed25519.GenerateKey(rand.Reader)
	kf, _, err := Mint(MintInput{
		AspectName: "plumb", KeyfileVersion: 999,
		AspectPrivkey: aspectPriv, ServerPubkey: serverPub,
		NexusID: "test-nexus-id", NexusURL: "wss://x", MintedAt: f.now,
	})
	if err != nil {
		t.Fatalf("Mint forged: %v", err)
	}
	_, err = Validate(context.Background(), f.cfg, kf.EncryptedPayload)
	var rev *RevokedError
	if !errors.As(err, &rev) {
		t.Fatalf("Validate too-high = %v; want *RevokedError", err)
	}
	if rev.PresentedVersion != 999 || rev.CurrentVersion != 1 {
		t.Errorf("RevokedError = %+v; want presented=999 current=1", rev)
	}
}

// TestValidate_KeyMismatch — the privkey in the payload doesn't match
// the stored aspect_pubkey. Real-world cause: an operator hand-edited
// the DB's aspect_pubkey, or the DB row got out of sync. Worth
// catching even though it shouldn't happen — silent acceptance would
// let a stolen-then-edited keyfile pass.
func TestValidate_KeyMismatch(t *testing.T) {
	f := newValidateFixture(t)
	// Replace the stored aspect_pubkey with a different key WITHOUT
	// going through Bump (which would force a version mismatch first).
	differentPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := f.store.db.ExecContext(context.Background(),
		`UPDATE aspects SET aspect_pubkey = ? WHERE name = ?`,
		[]byte(differentPub), "plumb"); err != nil {
		t.Fatalf("update pubkey: %v", err)
	}
	_, err := Validate(context.Background(), f.cfg, f.sealed)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("Validate key mismatch = %v; want ErrKeyMismatch", err)
	}
}

// TestValidate_TamperedPayload_FailsDecryption — flip a byte in the
// sealed bytes. Box authentication catches it; we never reach
// payload parsing.
func TestValidate_TamperedPayload_FailsDecryption(t *testing.T) {
	f := newValidateFixture(t)
	sealed, _ := base64.StdEncoding.DecodeString(f.sealed)
	sealed[10] ^= 0xff
	tampered := base64.StdEncoding.EncodeToString(sealed)
	_, err := Validate(context.Background(), f.cfg, tampered)
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Errorf("Validate tampered = %v; want ErrDecryptionFailed", err)
	}
}

// sealPlaintext seals arbitrary bytes against the fixture's server
// pubkey. Lets tests construct payloads that decrypt cleanly but carry
// non-Payload-shaped bytes.
func sealPlaintext(t *testing.T, f *validateFixture, plaintext []byte) string {
	t.Helper()
	xPub, err := EdPubkeyToX25519(f.cfg.ServerEd25519Pubkey)
	if err != nil {
		t.Fatalf("EdPubkeyToX25519: %v", err)
	}
	sealed, err := box.SealAnonymous(nil, plaintext, &xPub, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sealed)
}

// TestValidate_MalformedPayload_NotJSON — decryption succeeds but the
// inner bytes aren't JSON. → ErrMalformedPayload, not ErrDecryptionFailed.
// This distinction matters for the HTTP layer's status-code mapping
// (400 client encoding error vs 401 auth failure).
func TestValidate_MalformedPayload_NotJSON(t *testing.T) {
	f := newValidateFixture(t)
	sealed := sealPlaintext(t, f, []byte("this is not json"))
	_, err := Validate(context.Background(), f.cfg, sealed)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("Validate non-JSON payload = %v; want ErrMalformedPayload", err)
	}
}

// TestValidate_MalformedPayload_MissingAspectName — decrypts cleanly,
// JSON parses, but aspect_name is empty.
func TestValidate_MalformedPayload_MissingAspectName(t *testing.T) {
	f := newValidateFixture(t)
	sealed := sealPlaintext(t, f, []byte(`{"aspect_name":"","keyfile_version":1}`))
	_, err := Validate(context.Background(), f.cfg, sealed)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("Validate empty aspect_name = %v; want ErrMalformedPayload", err)
	}
}

// TestValidate_MalformedPayload_BadPrivkey — decrypts cleanly, JSON
// parses with a real aspect_name, but aspect_privkey is wrong size.
// Hits the post-version-check size guard.
func TestValidate_MalformedPayload_BadPrivkey(t *testing.T) {
	f := newValidateFixture(t)
	plaintext := []byte(`{"aspect_name":"plumb","aspect_privkey":"AQID","keyfile_version":1,"minted_at":"2026-05-08T10:00:00Z","nexus_id":"test-nexus-id"}`)
	sealed := sealPlaintext(t, f, plaintext)
	_, err := Validate(context.Background(), f.cfg, sealed)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("Validate short privkey = %v; want ErrMalformedPayload", err)
	}
}

// TestValidate_NilStore — defensive guard.
func TestValidate_NilStore(t *testing.T) {
	cfg := ValidateConfig{}
	_, err := Validate(context.Background(), cfg, "ignored")
	if err == nil {
		t.Error("Validate with nil Store should error; got nil")
	}
}
