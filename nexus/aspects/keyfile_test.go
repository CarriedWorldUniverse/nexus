package aspects

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// TestMint_RoundTrip — primary load-bearing test. Mint a keyfile against
// a fresh server keypair, then decrypt the payload using the matching
// X25519 priv. The decrypted JSON must round-trip to the input Payload
// fields. Failure here means the Ed25519→X25519 conversion is wrong on
// at least one side, the seal/open shape is wrong, or the JSON encoding
// is incompatible — all of which would silently break Part 4 validation.
func TestMint_RoundTrip(t *testing.T) {
	serverPub, serverPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	aspectPub, aspectPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("aspect key: %v", err)
	}
	_ = aspectPub

	in := MintInput{
		AspectName:     "plumb",
		KeyfileVersion: 7,
		AspectPrivkey:  aspectPriv,
		ServerPubkey:   serverPub,
		NexusID:        "550e8400-e29b-41d4-a716-446655440000",
		NexusURL:       "wss://example.tail.ts.net:8001/connect",
		MintedAt:       time.Date(2026, 5, 8, 10, 30, 0, 0, time.UTC),
	}

	kf, fingerprint, err := Mint(in)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Envelope sanity.
	if kf.Format != KeyfileFormat {
		t.Errorf("Format = %q; want %q", kf.Format, KeyfileFormat)
	}
	if kf.Version != KeyfileVersion {
		t.Errorf("Version = %d; want %d", kf.Version, KeyfileVersion)
	}
	if kf.Envelope.NexusURL != in.NexusURL {
		t.Errorf("envelope NexusURL mismatch")
	}
	if kf.Envelope.NexusID != in.NexusID {
		t.Errorf("envelope NexusID mismatch")
	}
	if kf.Envelope.IssuedAt != "2026-05-08T10:30:00Z" {
		t.Errorf("envelope IssuedAt = %q; want 2026-05-08T10:30:00Z", kf.Envelope.IssuedAt)
	}

	if len(fingerprint) != 64 {
		t.Errorf("fingerprint len = %d; want 64 (hex sha256)", len(fingerprint))
	}

	// Decrypt the sealed payload using the matching server priv.
	xPriv := EdPrivkeyToX25519(serverPriv)
	xPub, err := EdPubkeyToX25519(serverPub)
	if err != nil {
		t.Fatalf("edPubkeyToX25519: %v", err)
	}

	sealed, err := base64.StdEncoding.DecodeString(kf.EncryptedPayload)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	plain, ok := box.OpenAnonymous(nil, sealed, &xPub, &xPriv)
	if !ok {
		t.Fatal("box.OpenAnonymous failed — ed→x25519 conversion or seal pairing is broken")
	}

	var p Payload
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}

	if p.AspectName != in.AspectName {
		t.Errorf("payload AspectName = %q; want %q", p.AspectName, in.AspectName)
	}
	if p.KeyfileVersion != in.KeyfileVersion {
		t.Errorf("payload KeyfileVersion = %d; want %d", p.KeyfileVersion, in.KeyfileVersion)
	}
	if p.NexusID != in.NexusID {
		t.Errorf("payload NexusID = %q; want %q", p.NexusID, in.NexusID)
	}
	if p.MintedAt != "2026-05-08T10:30:00Z" {
		t.Errorf("payload MintedAt = %q; want 2026-05-08T10:30:00Z", p.MintedAt)
	}
	gotSeed, err := base64.StdEncoding.DecodeString(p.AspectPrivkey)
	if err != nil {
		t.Fatalf("aspect priv b64 decode: %v", err)
	}
	if len(gotSeed) != ed25519.SeedSize {
		t.Errorf("aspect_privkey wire size = %d; want %d (spec §4: 32-byte seed)", len(gotSeed), ed25519.SeedSize)
	}
	if !bytes.Equal(gotSeed, aspectPriv.Seed()) {
		t.Error("decoded aspect seed != input seed — privkey did not round-trip through seal")
	}
	// Reconstruction: ed25519.NewKeyFromSeed(gotSeed) must equal aspectPriv.
	reconstructed := ed25519.NewKeyFromSeed(gotSeed)
	if !bytes.Equal(reconstructed, aspectPriv) {
		t.Error("reconstructed key from decoded seed != original — agentfunnel side of the contract is broken")
	}
}

// TestMint_KeyfileMarshalsAsJSON catches future struct-tag drift. A
// minted Keyfile must serialise to the exact wire shape spec'd in §4.
func TestMint_KeyfileMarshalsAsJSON(t *testing.T) {
	serverPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, aspectPriv, _ := ed25519.GenerateKey(rand.Reader)

	kf, _, err := Mint(MintInput{
		AspectName: "plumb", KeyfileVersion: 1,
		AspectPrivkey: aspectPriv, ServerPubkey: serverPub,
		NexusID: "id", NexusURL: "wss://x/y",
		MintedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	raw, err := json.Marshal(kf)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(raw)

	for _, want := range []string{
		`"version":1`,
		`"format":"nexus-keyfile-v1"`,
		`"envelope":{`,
		`"nexus_url":"wss://x/y"`,
		`"nexus_id":"id"`,
		`"issued_at":`,
		`"encrypted_payload":`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshalled JSON missing %q\nfull: %s", want, s)
		}
	}
}

// TestMint_RejectsInvalidInputs covers the input-validation surface.
// Each missing/wrong field is its own error path; a single happy-path
// test would silently mask any regression here.
func TestMint_RejectsInvalidInputs(t *testing.T) {
	serverPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, aspectPriv, _ := ed25519.GenerateKey(rand.Reader)
	good := MintInput{
		AspectName: "plumb", KeyfileVersion: 1,
		AspectPrivkey: aspectPriv, ServerPubkey: serverPub,
		NexusID: "id", NexusURL: "wss://x", MintedAt: time.Now(),
	}

	cases := []struct {
		name    string
		mutate  func(*MintInput)
		wantSub string
	}{
		{"empty name", func(m *MintInput) { m.AspectName = "" }, "AspectName empty"},
		{"empty nexus id", func(m *MintInput) { m.NexusID = "" }, "NexusID empty"},
		{"empty url", func(m *MintInput) { m.NexusURL = "" }, "NexusURL empty"},
		{"zero version", func(m *MintInput) { m.KeyfileVersion = 0 }, "KeyfileVersion"},
		{"negative version", func(m *MintInput) { m.KeyfileVersion = -1 }, "KeyfileVersion"},
		{"short aspect priv", func(m *MintInput) { m.AspectPrivkey = []byte{1, 2, 3} }, "AspectPrivkey wrong size"},
		{"short server pub", func(m *MintInput) { m.ServerPubkey = []byte{1, 2, 3} }, "ServerPubkey wrong size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := good
			tc.mutate(&in)
			_, _, err := Mint(in)
			if err == nil {
				t.Fatalf("expected error for %q; got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestEdPubkeyToX25519_DerivesGenuineX25519 — sanity: the conversion
// produces 32 bytes that an X25519 scalar can multiply against (rather
// than e.g. all-zero output from a misnamed function).
func TestEdPubkeyToX25519_RoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	xpub, err := EdPubkeyToX25519(pub)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var zero [32]byte
	if xpub == zero {
		t.Error("converted pubkey is all zeros")
	}
}
