// Package heraldkeyfile loads the herald-rooted bootstrap keyfile an aspect
// reads at startup to sign its register-handshake assertion. The file is
// written by `cw agent enroll`; it carries the agent's DERIVED key (never the
// owner seed) plus the herald agent id, the nexus relay url, the slug, and
// the casket fingerprint.
package heraldkeyfile

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/CarriedWorldUniverse/cwb-client/identity"
)

// Keyfile is the parsed bootstrap keyfile.
type Keyfile struct {
	Key         string `json:"key"`         // base64 ed25519 private key (64-byte Go form)
	KeyID       string `json:"key_id"`      // herald agent UUID (assertion iss/sub)
	URL         string `json:"url"`         // nexus relay (discovery edge + connect)
	Slug        string `json:"slug"`        // agent name
	Fingerprint string `json:"fingerprint"` // base64url sha256(pub)[:16]
}

// PrivateKey base64-decodes Key into an ed25519 private key.
func (k *Keyfile) PrivateKey() (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(k.Key)
	if err != nil {
		return nil, fmt.Errorf("heraldkeyfile: decode key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("heraldkeyfile: key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// Load reads, parses, and validates a bootstrap keyfile. It checks every
// field is present, the key decodes to a valid ed25519 key, and the stored
// fingerprint matches the key's public half (a corruption guard).
func Load(path string) (*Keyfile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("heraldkeyfile: read %s: %w", path, err)
	}
	var k Keyfile
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("heraldkeyfile: parse %s: %w", path, err)
	}
	if k.Key == "" || k.KeyID == "" || k.URL == "" || k.Slug == "" || k.Fingerprint == "" {
		return nil, fmt.Errorf("heraldkeyfile: %s missing required field(s)", path)
	}
	priv, err := k.PrivateKey()
	if err != nil {
		return nil, err
	}
	if got := identity.Fingerprint(priv.Public().(ed25519.PublicKey)); got != k.Fingerprint {
		return nil, fmt.Errorf("heraldkeyfile: fingerprint mismatch (key=%s file=%s) — corrupt keyfile", got, k.Fingerprint)
	}
	return &k, nil
}
