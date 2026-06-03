package heraldkeyfile

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
)

func writeKF(t *testing.T, kf map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kf.json")
	data, _ := json.Marshal(kf)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func goodKF(t *testing.T) map[string]string {
	t.Helper()
	priv, pub, _ := casket.DeriveAgentKey([]byte("0123456789abcdef0123456789abcdef"), "plumb")
	return map[string]string{
		"key":         base64.StdEncoding.EncodeToString(priv),
		"key_id":      "agent-uuid-9",
		"url":         "ws://nexus.local/connect",
		"slug":        "plumb",
		"fingerprint": identity.Fingerprint(pub),
	}
}

func TestLoadGood(t *testing.T) {
	kf, err := Load(writeKF(t, goodKF(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if kf.KeyID != "agent-uuid-9" || kf.Slug != "plumb" || kf.URL != "ws://nexus.local/connect" {
		t.Fatalf("kf = %+v", kf)
	}
	priv, err := kf.PrivateKey()
	if err != nil {
		t.Fatalf("PrivateKey: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("priv len = %d", len(priv))
	}
}

func TestLoadMissingField(t *testing.T) {
	m := goodKF(t)
	delete(m, "key_id")
	if _, err := Load(writeKF(t, m)); err == nil {
		t.Fatal("expected error for missing key_id")
	}
}

func TestLoadBadKey(t *testing.T) {
	m := goodKF(t)
	m["key"] = "!!!not-base64!!!"
	if _, err := Load(writeKF(t, m)); err == nil {
		t.Fatal("expected error for bad base64 key")
	}
}

func TestLoadFingerprintMismatch(t *testing.T) {
	m := goodKF(t)
	m["fingerprint"] = "deadbeefdeadbeef"
	if _, err := Load(writeKF(t, m)); err == nil {
		t.Fatal("expected error for fingerprint mismatch")
	}
}
