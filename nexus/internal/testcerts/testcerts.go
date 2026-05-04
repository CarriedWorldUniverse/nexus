// Package testcerts mints a self-signed loopback cert + key on disk
// for tests that need a real TLS server. Mirrors the production
// cert generator at nexus/cmd/nexus/cert.go but lives in a test-only
// dependency so production code doesn't import it.
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Mint generates a self-signed cert + key and writes them to a fresh
// temp dir tied to t. Returns the absolute paths to cert and key.
// Loopback SANs only — fits any test server on 127.0.0.1.
func Mint(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testcerts: generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("testcerts: serial: %v", err)
	}
	now := time.Now().UTC()
	tpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "nexus-test"},
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("testcerts: create certificate: %v", err)
	}
	certPath = filepath.Join(dir, "test.crt")
	keyPath = filepath.Join(dir, "test.key")

	cf, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("testcerts: open cert: %v", err)
	}
	defer cf.Close()
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("testcerts: encode cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("testcerts: marshal key: %v", err)
	}
	kf, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("testcerts: open key: %v", err)
	}
	defer kf.Close()
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("testcerts: encode key: %v", err)
	}
	return certPath, keyPath
}
