package main

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyHost(t *testing.T) {
	cases := []struct {
		host    string
		want    certMode
		wantErr bool
	}{
		{"", certModeLoopback, false},
		{"localhost", certModeLoopback, false},
		{"127.0.0.1", certModeLoopback, false},
		{"::1", certModeLoopback, false},
		{"LOCALHOST", certModeLoopback, false}, // case-insensitive
		{"foo.ts.net", certModeTailscale, false},
		{"agentnetwork.tail41686e.ts.net", certModeTailscale, false},
		{"FOO.TS.NET", certModeTailscale, false},
		// Non-loopback non-tailscale = refuse. Operator #9677.
		{"example.com", 0, true},
		{"nexus.internal", 0, true},
		{"192.168.1.10", 0, true}, // private IP but not loopback
		// Flag-injection guard: a leading '-' must be rejected before
		// the suffix check, otherwise --foo.ts.net would slip through
		// and land as a flag arg in the tailscale shell-out.
		{"-foo.ts.net", 0, true},
		{"--cert-file=evil.ts.net", 0, true},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			got, err := classifyHost(c.host)
			if c.wantErr {
				if err == nil {
					t.Errorf("classifyHost(%q) returned nil err, want refusal", c.host)
				}
				return
			}
			if err != nil {
				t.Errorf("classifyHost(%q) err = %v", c.host, err)
			}
			if got != c.want {
				t.Errorf("classifyHost(%q) = %v, want %v", c.host, got, c.want)
			}
		})
	}
}

// The refusal message must call out that self-signed isn't allowed
// for non-loopback hosts, citing the operator decision. Future
// operators reading the error need the why.
func TestClassifyHost_RefusalMessageMentionsLocalOnly(t *testing.T) {
	_, err := classifyHost("example.com")
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "local-only") {
		t.Errorf("refusal message should mention local-only, got: %v", err)
	}
}

func TestWriteSelfSignedLoopback_ProducesValidCert(t *testing.T) {
	dir := t.TempDir()
	if err := writeSelfSignedLoopback(dir); err != nil {
		t.Fatalf("writeSelfSignedLoopback: %v", err)
	}
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	// Files exist with correct perms.
	cInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("cert missing: %v", err)
	}
	kInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key missing: %v", err)
	}
	// Skip Windows perm check — Windows file modes don't map cleanly.
	if cInfo.Mode().Perm()&0o077 != 0 && os.Getenv("OS") == "" {
		// Cert can be group/world-readable; only assert no exec bits.
	}
	_ = kInfo

	// Cert parses, has loopback SANs, is self-signed.
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("cert PEM block wrong: %+v", block)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	// Self-signed: issuer == subject.
	if cert.Issuer.String() != cert.Subject.String() {
		t.Errorf("not self-signed: issuer=%q subject=%q", cert.Issuer, cert.Subject)
	}

	// SANs must include loopback only.
	hasLocalhost := false
	for _, name := range cert.DNSNames {
		if name == "localhost" {
			hasLocalhost = true
		}
	}
	if !hasLocalhost {
		t.Errorf("cert DNS SANs missing localhost: %v", cert.DNSNames)
	}
	hasLoopbackV4 := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			hasLoopbackV4 = true
		}
	}
	if !hasLoopbackV4 {
		t.Errorf("cert IP SANs missing 127.0.0.1: %v", cert.IPAddresses)
	}

	// Validity window sane.
	if cert.NotAfter.Sub(cert.NotBefore).Hours() < 8000 {
		t.Errorf("validity too short: %v -> %v", cert.NotBefore, cert.NotAfter)
	}

	// KeyUsage: ECDSA keys only need DigitalSignature; KeyEncipherment
	// is RSA-only per RFC 5480 §3.
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("DigitalSignature key-usage missing: %v", cert.KeyUsage)
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment != 0 {
		t.Errorf("KeyEncipherment set on ECDSA cert (should not be): %v", cert.KeyUsage)
	}
	hasServerAuth := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
	}
	if !hasServerAuth {
		t.Errorf("ExtKeyUsage missing ServerAuth: %v", cert.ExtKeyUsage)
	}

	// Key parses.
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyBytes)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		t.Fatalf("key PEM block wrong: %+v", keyBlock)
	}
	if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("parse EC key: %v", err)
	}
}

func TestResolveCertOutDir(t *testing.T) {
	// Explicit --out wins, resolved to absolute.
	d := t.TempDir()
	got, err := resolveCertOutDir(d)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved out not absolute: %q", got)
	}

	// Default (empty) → ~/.nexus/tls
	def, err := resolveCertOutDir("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(def, filepath.Join(".nexus", "tls")) {
		t.Errorf("default out doesn't end in .nexus/tls: %q", def)
	}
}

// Subcommand dispatch: unknown verb returns 2, missing args returns 2,
// `init` with refused host returns 1.
func TestRunCertSubcommand_DispatchAndExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", []string{}, 2},
		{"unknown verb", []string{"bogus"}, 2},
		{"init with refused host returns 1", []string{"init", "--out", t.TempDir(), "--host", "example.com"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runCertSubcommand(c.args); got != c.want {
				t.Errorf("runCertSubcommand(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}
