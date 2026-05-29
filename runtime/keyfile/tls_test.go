package keyfile

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// genSelfSignedPEM produces a valid self-signed cert PEM for tests.
func genSelfSignedPEM(t *testing.T) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// NEX-367: a keyfile that pins a broker cert yields a TLS config whose
// RootCAs trust it — without touching the system trust store.
func TestBrokerTLSConfig_PinnedCert(t *testing.T) {
	kf := &Keyfile{Envelope: Envelope{BrokerTLSCert: genSelfSignedPEM(t)}}
	cfg, err := kf.BrokerTLSConfig()
	if err != nil {
		t.Fatalf("BrokerTLSConfig: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("expected a non-nil *tls.Config with RootCAs set")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want >= TLS 1.2", cfg.MinVersion)
	}
}

// No pinned cert → nil config → callers fall back to the system trust
// store (CA-signed certs just work). Backward-compatible default.
func TestBrokerTLSConfig_NoCert(t *testing.T) {
	kf := &Keyfile{Envelope: Envelope{}}
	cfg, err := kf.BrokerTLSConfig()
	if err != nil {
		t.Fatalf("BrokerTLSConfig: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil *tls.Config when no cert is pinned")
	}
}

// A malformed cert is a loud error, not a silent fall-through to the
// system store (which could mask a tampered keyfile).
func TestBrokerTLSConfig_BadPEM(t *testing.T) {
	kf := &Keyfile{Envelope: Envelope{BrokerTLSCert: "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----"}}
	if _, err := kf.BrokerTLSConfig(); err == nil {
		t.Fatal("expected an error for malformed broker_tls_cert")
	}
}

func TestHTTPClientWithTLS(t *testing.T) {
	if c := HTTPClientWithTLS(nil); c == nil || c.Timeout == 0 {
		t.Fatal("nil tls should yield a default timeout client")
	}
	c := HTTPClientWithTLS(&tls.Config{})
	if c == nil || c.Transport == nil {
		t.Fatal("non-nil tls should set a transport")
	}
}
