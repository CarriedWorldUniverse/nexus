// TLS cert bootstrap subcommand for nexus.
//
// `nexus cert init [--host HOST] [--out DIR]` provisions a server cert
// + key pair into the chosen directory. Intended workflow:
//
//  1. Operator runs `nexus cert init` once per host.
//  2. Operator points the broker at the resulting files via the
//     --tls-cert / --tls-key flags (or NEXUS_TLS_CERT / NEXUS_TLS_KEY).
//  3. Broker boots with TLS-always.
//
// Behaviour by --host:
//
//   - unset / 127.0.0.1 / ::1 / localhost  → self-signed cert with
//     loopback SANs only. Local development only; aspects on the same
//     host need the cert added to their system trust store.
//
//   - *.ts.net (or any --host whose suffix matches a tailscale-cert
//     hostname pattern) → shells out to `tailscale cert <host>`. The
//     tailscale daemon issues a real CA-rooted cert. Aspects connecting
//     over the tailnet trust it via the system trust store with no
//     extra config.
//
//   - any other non-loopback hostname → REFUSED. We don't auto-issue
//     self-signed certs for non-loopback names because operator
//     #9677: "self-signed cert are fine for local testing - but not
//     deploy/live". Operators wanting a CA-issued cert for a custom
//     hostname provide their own cert+key (skip this subcommand,
//     point the flags at the BYO files).
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// runCertSubcommand parses `cert <verb> [...]` from os.Args[2:] and
// dispatches. Returns process exit code; main() calls os.Exit on it.
func runCertSubcommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nexus cert <init>")
		return 2
	}
	switch args[0] {
	case "init":
		return runCertInit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown cert subcommand %q (expected: init)\n", args[0])
		return 2
	}
}

// runCertInit implements `cert init`. Returns exit code.
func runCertInit(args []string) int {
	fs := flag.NewFlagSet("cert init", flag.ContinueOnError)
	host := fs.String("host", "", "hostname / SAN for the cert (default: loopback only)")
	outDir := fs.String("out", "", "output directory (default: ~/.nexus/tls)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolvedOut, err := resolveCertOutDir(*outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cert init: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(resolvedOut, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "cert init: mkdir %q: %v\n", resolvedOut, err)
		return 1
	}

	mode, err := classifyHost(*host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cert init: %v\n", err)
		return 1
	}

	switch mode {
	case certModeLoopback:
		if err := writeSelfSignedLoopback(resolvedOut); err != nil {
			fmt.Fprintf(os.Stderr, "cert init: %v\n", err)
			return 1
		}
		fmt.Printf("self-signed loopback cert written to %s\n", resolvedOut)
		fmt.Println(trustHintForOS(filepath.Join(resolvedOut, "server.crt")))
	case certModeTailscale:
		if err := writeTailscaleCert(resolvedOut, *host); err != nil {
			fmt.Fprintf(os.Stderr, "cert init: %v\n", err)
			return 1
		}
		fmt.Printf("tailscale cert for %s written to %s\n", *host, resolvedOut)
	}
	fmt.Printf("\npoint the broker at:\n  --tls-cert %s\n  --tls-key  %s\n",
		filepath.Join(resolvedOut, "server.crt"),
		filepath.Join(resolvedOut, "server.key"))
	return 0
}

// certMode is what classifyHost decides for a given --host.
type certMode int

const (
	certModeLoopback certMode = iota
	certModeTailscale
)

// classifyHost decides whether the requested host can be served by a
// loopback self-signed cert, by a tailscale-issued cert, or whether
// we refuse and require BYO. Refusal is a hard error per #9677.
func classifyHost(host string) (certMode, error) {
	host = strings.TrimSpace(host)
	if host == "" || isLoopbackHost(host) {
		return certModeLoopback, nil
	}
	// Reject anything that could be misread as a flag by tailscale's
	// own argv parser. Without this, --foo.ts.net would pass the
	// suffix check below and then land as a flag argument when we
	// shell out.
	if strings.HasPrefix(host, "-") {
		return 0, fmt.Errorf("host %q starts with '-'; pass a valid hostname", host)
	}
	if isTailscaleHost(host) {
		return certModeTailscale, nil
	}
	return 0, fmt.Errorf("host %q is not loopback or tailscale; "+
		"self-signed certs are local-only (operator #9677). "+
		"For a custom hostname, obtain a CA-issued cert and pass it "+
		"directly via --tls-cert / --tls-key", host)
}

func isLoopbackHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// isTailscaleHost matches the conventional tailscale MagicDNS suffix
// `*.ts.net`. Tailscale also issues certs for `*.tailXXXX.ts.net`
// (the tailnet shard); both match this suffix check.
func isTailscaleHost(h string) bool {
	return strings.HasSuffix(strings.ToLower(h), ".ts.net")
}

// resolveCertOutDir picks the output directory: --out flag if set,
// else ~/.nexus/tls.
func resolveCertOutDir(flagVal string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", fmt.Errorf("resolve --out: %w", err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".nexus", "tls"), nil
}

// writeSelfSignedLoopback generates an ECDSA P-256 cert + key valid
// for 365 days, with loopback SANs only, and writes both PEM files.
func writeSelfSignedLoopback(outDir string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("serial: %w", err)
	}
	now := time.Now().UTC()
	tpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "nexus-local",
			Organization: []string{"nexus self-signed (local-only)"},
		},
		NotBefore: now.Add(-1 * time.Minute),
		NotAfter:  now.Add(365 * 24 * time.Hour),
		// ECDSA keys: only DigitalSignature is meaningful (TLS handshake
		// uses ECDHE for key exchange, not the cert key). RFC 5480 §3.
		// KeyEncipherment is RSA-only and confuses strict validators.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	certPath := filepath.Join(outDir, "server.crt")
	keyPath := filepath.Join(outDir, "server.key")

	certPEM, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open cert: %w", err)
	}
	defer certPEM.Close()
	if err := pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return fmt.Errorf("encode cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	// 0o600 — private key, owner-only. mkdir was 0o700.
	keyPEM, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open key: %w", err)
	}
	defer keyPEM.Close()
	if err := pem.Encode(keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("encode key: %w", err)
	}
	return nil
}

// writeTailscaleCert shells out to the tailscale CLI to issue a real
// CA-rooted server cert for `host`, writing the cert and key directly
// to outDir/server.crt + outDir/server.key.
//
// We pass --cert-file and --key-file explicitly rather than relying on
// tailscale's default output location: that location has changed
// across tailscale versions (some platforms write to a daemon-managed
// dir, not CWD), so the explicit form is version-stable and avoids a
// silent-failure window where the rename step would have hit a missing
// file. classifyHost guarantees `host` is non-empty, doesn't start
// with '-', and ends in `.ts.net`, so the argv we build can't be
// misread by tailscale's own flag parser.
func writeTailscaleCert(outDir, host string) error {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return fmt.Errorf("`tailscale` CLI not on PATH (install tailscale, "+
			"or pass --tls-cert/--tls-key with a BYO cert): %w", err)
	}
	certPath := filepath.Join(outDir, "server.crt")
	keyPath := filepath.Join(outDir, "server.key")
	cmd := exec.Command("tailscale", "cert",
		"--cert-file", certPath,
		"--key-file", keyPath,
		host)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tailscale cert %s: %w", host, err)
	}
	return nil
}

// trustHintForOS returns a one-line OS-specific instruction for adding
// the given cert path to the system trust store. Printed (not
// executed) so operators can review before running.
func trustHintForOS(certPath string) string {
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf(
			"to trust this cert system-wide:\n"+
				"  sudo security add-trusted-cert -d -r trustRoot "+
				"-k /Library/Keychains/System.keychain %s",
			certPath)
	case "linux":
		return fmt.Sprintf(
			"to trust this cert system-wide:\n"+
				"  sudo cp %s /usr/local/share/ca-certificates/nexus-local.crt && "+
				"sudo update-ca-certificates",
			certPath)
	case "windows":
		return fmt.Sprintf(
			"to trust this cert system-wide (PowerShell as admin):\n"+
				"  Import-Certificate -FilePath '%s' -CertStoreLocation 'Cert:\\LocalMachine\\Root'",
			certPath)
	default:
		return fmt.Sprintf("to trust this cert: add %s to your OS root store",
			certPath)
	}
}
