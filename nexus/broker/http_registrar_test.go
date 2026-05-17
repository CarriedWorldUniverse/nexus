package broker

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/internal/testcerts"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// TestListenAndServe_HTTPRegistrar verifies the broker invokes the
// embedder-supplied HTTPRegistrar callback on its internal mux before
// the server starts, so external services (e.g. ledger.HealthzHandler)
// can mount their routes on the broker's HTTPS listener without the
// broker package taking a hard dependency on them.
func TestListenAndServe_HTTPRegistrar(t *testing.T) {
	certPath, keyPath := testcerts.Mint(t)
	r := roster.New()
	addr := freeAddr(t)

	const probePath = "/healthz/probe"
	const probeBody = `{"status":"ok","probe":1}`

	registrarCalled := false
	b := New(Config{
		Addr:               addr,
		HeartbeatIntervalS: 15,
		TLSCertFile:        certPath,
		TLSKeyFile:         keyPath,
		HTTPRegistrar: func(mux *http.ServeMux) {
			registrarCalled = true
			mux.HandleFunc(probePath, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, probeBody)
			})
		},
	}, r)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- b.ListenAndServe(ctx) }()

	insecure := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   500 * time.Millisecond,
	}
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var dialErr error
	for time.Now().Before(deadline) {
		resp, dialErr = insecure.Get("https://" + addr + probePath)
		if dialErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("HTTPS %s never came up: %v", probePath, dialErr)
	}
	defer resp.Body.Close()

	if !registrarCalled {
		t.Error("HTTPRegistrar callback was not invoked")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != probeBody {
		t.Errorf("body = %q, want %q", string(body), probeBody)
	}

	cancel()
	if err := <-serveErrCh; err != nil && !errors.Is(err, context.Canceled) {
		if !strings.Contains(err.Error(), "Server closed") {
			t.Logf("ListenAndServe returned: %v", err)
		}
	}
}
