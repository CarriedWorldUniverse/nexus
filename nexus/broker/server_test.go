package broker

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/internal/testcerts"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// freeAddr returns a 127.0.0.1:PORT string with a known-free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// Regression for PR-A2.2: ListenAndServe must require TLS cert+key.
// A broker constructed without them must fail-fast at ListenAndServe
// time with a clear error.
func TestListenAndServe_RequiresTLS(t *testing.T) {
	r := roster.New()
	b := New(Config{
		Addr:               freeAddr(t),
		HeartbeatIntervalS: 15,
	}, r)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := b.ListenAndServe(ctx)
	if err == nil {
		t.Fatal("ListenAndServe with no TLS cert should fail, got nil")
	}
	if !strings.Contains(err.Error(), "TLSCertFile") {
		t.Errorf("err = %v, want mention of TLSCertFile", err)
	}
}

// Regression for PR-A2.2: when TLS is configured the broker actually
// serves over TLS. A plain HTTP request to /health must fail with a
// TLS-handshake-shape error; an HTTPS request with InsecureSkipVerify
// must succeed.
func TestListenAndServe_TLSAcceptsHTTPSRejectsHTTP(t *testing.T) {
	certPath, keyPath := testcerts.Mint(t)
	r := roster.New()
	addr := freeAddr(t)
	b := New(Config{
		Addr:               addr,
		HeartbeatIntervalS: 15,
		TLSCertFile:        certPath,
		TLSKeyFile:         keyPath,
	}, r)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- b.ListenAndServe(ctx) }()

	// Wait for server to bind. Poll a TLS dial up to 2s.
	insecure := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   500 * time.Millisecond,
	}
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var dialErr error
	for time.Now().Before(deadline) {
		resp, dialErr = insecure.Get("https://" + addr + "/health")
		if dialErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("HTTPS /health never came up: %v", dialErr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTPS /health status = %d, want 200", resp.StatusCode)
	}

	// Plain HTTP request must NOT succeed. Go's net/http server
	// returns 400 Bad Request with body "Client sent an HTTP request
	// to an HTTPS server" when it sees plain HTTP on a TLS listener.
	// Pin to 400 specifically: a 200 means TLS isn't enforced; a 301
	// would mean someone added an HTTP→HTTPS redirect (also a
	// regression — the broker should expose only the TLS port, no
	// plain listener at all). A transport error (err != nil) is also
	// acceptable depending on client behavior; we only fail on a
	// confirmed-success or unexpected-status response.
	plainClient := &http.Client{Timeout: 500 * time.Millisecond}
	resp2, err := plainClient.Get("http://" + addr + "/health")
	if err == nil {
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusBadRequest {
			t.Errorf("plain HTTP /health returned %d, want 400 (TLS-required) or transport error",
				resp2.StatusCode)
		}
	}

	cancel()
	if err := <-serveErrCh; err != nil && !errors.Is(err, context.Canceled) {
		// Shutdown returns nil normally; ctx.Canceled is also fine.
		// Anything else is a real error.
		if !strings.Contains(err.Error(), "Server closed") {
			t.Logf("ListenAndServe returned: %v", err)
		}
	}
}
