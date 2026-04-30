package frame

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/shared/schemas"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func startBootstrap(t *testing.T, agentsDir string) (addr string, stop func(), waitErr <-chan error) {
	t.Helper()
	addr = freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, BootstrapConfig{
			Addr:      addr,
			AgentsDir: agentsDir,
			Timeout:   2 * time.Second,
		})
	}()
	// Poll for /healthz so the test doesn't race the listener bind.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return addr, cancel, errCh
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatalf("bootstrap server did not become ready on %s", addr)
	return
}

func postSetup(t *testing.T, addr string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+addr+"/bootstrap/setup", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func TestBootstrap_HappyPath(t *testing.T) {
	dir := t.TempDir()
	addr, _, errCh := startBootstrap(t, dir)

	resp := postSetup(t, addr, SetupRequest{Name: "frame"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr SetupResponse
	if err := json.Unmarshal(readBody(t, resp), &sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.Status != "ok" || sr.FramePath == "" {
		t.Fatalf("unexpected response: %+v", sr)
	}

	// Run should return nil (clean shutdown after setup) within Timeout
	// + a little slack.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned err: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after successful setup")
	}

	// Detect should now find the new Frame.
	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("post-setup Detect: %v", err)
	}
	if d.Frame == nil || d.Frame.Name != "frame" {
		t.Fatalf("expected frame=frame, got %+v", d.Frame)
	}
	if d.Frame.Config.EffectiveRole() != schemas.RoleFrame {
		t.Fatalf("expected role=frame, got %q", d.Frame.Config.EffectiveRole())
	}
}

func TestBootstrap_RejectsSecondSetup(t *testing.T) {
	// After a successful setup, Run begins shutting down. We can't
	// reliably hit a second POST after that, so this test exercises the
	// in-memory used-flag check by submitting two near-simultaneous
	// requests from goroutines and asserting one wins, one loses.
	dir := t.TempDir()
	addr, _, errCh := startBootstrap(t, dir)

	type result struct{ status int }
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			body := SetupRequest{Name: fmt.Sprintf("frame%d", i)}
			resp := postSetup(t, addr, body)
			results <- result{resp.StatusCode}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(results)

	statuses := []int{}
	for r := range results {
		statuses = append(statuses, r.status)
	}
	// One should be 200, the other should NOT be 200 — could be 409
	// (already-complete) OR 500 (filesystem rejected the second
	// folder), depending on timing. Not asserting on which non-200.
	gotOK := 0
	for _, s := range statuses {
		if s == 200 {
			gotOK++
		}
	}
	if gotOK != 1 {
		t.Fatalf("expected exactly one 200 across concurrent setups, got statuses=%v", statuses)
	}

	<-errCh
}

func TestBootstrap_RejectsBadName(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	cases := []struct {
		name string
		want int
	}{
		{"", http.StatusBadRequest},
		{"has space", http.StatusBadRequest},
		{"has-dash", http.StatusBadRequest},
		{"has/slash", http.StatusBadRequest},
		{"has\\backslash", http.StatusBadRequest},
		{"..", http.StatusBadRequest},
		{"../escape", http.StatusBadRequest},
		{"operator", http.StatusBadRequest}, // reserved
		{"system", http.StatusBadRequest},   // reserved
		{strings.Repeat("a", 33), http.StatusBadRequest}, // too long
	}
	for _, tc := range cases {
		resp := postSetup(t, addr, SetupRequest{Name: tc.name})
		if resp.StatusCode != tc.want {
			t.Errorf("name=%q: status=%d want=%d body=%s", tc.name, resp.StatusCode, tc.want, readBody(t, resp))
		} else {
			resp.Body.Close()
		}
	}

	// Server should still be alive — bad names don't burn the bootstrap.
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz after bad names: %v", err)
	}
	resp.Body.Close()
}

func TestBootstrap_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	resp, err := http.Post("http://"+addr+"/bootstrap/setup", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed json: status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBootstrap_RejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	resp, err := http.Post("http://"+addr+"/bootstrap/setup", "application/json",
		strings.NewReader(`{"name":"frame","wat":"surprise"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d body=%s", resp.StatusCode, readBody(t, resp))
	} else {
		resp.Body.Close()
	}
}

func TestBootstrap_PreExistingFrameRefusesRun(t *testing.T) {
	dir := t.TempDir()
	// pre-create a frame
	if err := os.MkdirAll(filepath.Join(dir, "keel"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := schemas.AspectConfig{Name: "keel", Role: schemas.RoleFrame, ContextMode: schemas.ContextGlobal}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "keel", "aspect.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	err := Run(context.Background(), BootstrapConfig{
		Addr:      freePort(t),
		AgentsDir: dir,
		Timeout:   time.Second,
	})
	if !errors.Is(err, ErrBootstrapAlreadyComplete) {
		t.Fatalf("expected ErrBootstrapAlreadyComplete, got %v", err)
	}
}

func TestBootstrap_ContextCancel(t *testing.T) {
	dir := t.TempDir()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, BootstrapConfig{
			Addr:      addr,
			AgentsDir: dir,
			Timeout:   500 * time.Millisecond,
		})
	}()
	// wait for ready
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, herr := http.Get("http://" + addr + "/healthz")
		if herr == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatal("server never became ready")
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestBootstrap_PayloadSizeLimit(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	huge := strings.Repeat("a", 200*1024)
	resp, err := http.Post("http://"+addr+"/bootstrap/setup", "application/json",
		strings.NewReader(`{"name":"`+huge+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		// Either Content-Length triggers 413 or MaxBytesReader triggers 400 — both fine.
		t.Errorf("oversized payload: status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBootstrap_StubIndexServed(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("index status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "bootstrap mode") {
		t.Errorf("index body missing bootstrap text: %s", string(body))
	}
}

func TestWriteFrameHome_RefusesPathEscape(t *testing.T) {
	dir := t.TempDir()
	// validateName already blocks ../ etc., so we hit writeFrameHome
	// directly with a name that *happened* to pass validation but
	// resolved outside agentsDir. Since the name regex disallows the
	// chars needed for escape, this is mostly defense-in-depth — assert
	// the validateName gate fires first.
	_, err := writeFrameHome(dir, "../escape")
	if err == nil {
		t.Fatal("expected error on path-escape name")
	}
}

func TestWriteFrameHome_RefusesExistingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frame"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := writeFrameHome(dir, "frame")
	if err == nil {
		t.Fatal("expected error when home already exists")
	}
}

func TestValidateName(t *testing.T) {
	good := []string{"frame", "keel", "a", "x_y_z", "Frame123"}
	bad := []string{"", "has space", "has-dash", "../up", "operator", "system", strings.Repeat("a", 33)}

	for _, n := range good {
		if err := validateName(n); err != nil {
			t.Errorf("validateName(%q) unexpected err: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := validateName(n); err == nil {
			t.Errorf("validateName(%q) expected err, got nil", n)
		}
	}
}
