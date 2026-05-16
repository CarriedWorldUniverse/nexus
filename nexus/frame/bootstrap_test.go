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

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
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

// dialAndQueueSetup opens a TCP connection to the bootstrap server,
// writes the HTTP POST headers + body, then waits on `gate` before
// flushing. Returns the connection so the caller can read the
// response after closing the gate.
//
// Used by the concurrent-setup tests to guarantee both requests reach
// the server's handleSetup at the same time on slow Windows TCP. The
// stdlib http.Post + raw goroutine pattern raced the listener's
// post-Shutdown close on Windows-latest CI — the second goroutine
// could fail TCP connect after the first succeeded and triggered
// Shutdown, producing a single 200 but also a test-aborting t.Fatalf
// from the goroutine. Dialing both connections up front, *before*
// either has flushed bytes that the server will act on, removes the
// race: by the time the listener closes, both sockets are already
// established and in-flight.
func dialAndQueueSetup(t *testing.T, addr string, body any, gate <-chan struct{}) net.Conn {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	go func() {
		<-gate
		req := fmt.Sprintf("POST /bootstrap/setup HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			addr, len(raw), raw)
		_, _ = c.Write([]byte(req))
	}()
	return c
}

// readResponseStatus parses just the HTTP status code from a raw
// connection opened by dialAndQueueSetup, then closes the conn.
func readResponseStatus(t *testing.T, c net.Conn) int {
	t.Helper()
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := make([]byte, 12) // "HTTP/1.1 NNN"
	if _, err := io.ReadFull(c, br); err != nil {
		t.Logf("read status: %v", err)
		return 0
	}
	// br looks like "HTTP/1.1 200" — extract bytes 9..12.
	var code int
	if _, err := fmt.Sscanf(string(br[9:]), "%d", &code); err != nil {
		t.Logf("parse status %q: %v", br, err)
		return 0
	}
	// Drain the rest so the server doesn't see a half-read peer.
	_, _ = io.Copy(io.Discard, c)
	return code
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

func TestBootstrap_HappyPath_FullBundle(t *testing.T) {
	dir := t.TempDir()
	addr, _, errCh := startBootstrap(t, dir)

	resp := postSetup(t, addr, SetupRequest{
		Name:     "frame",
		Voice:    "Terse and direct.",
		Values:   "the operator's time, accuracy.",
		Provider: "claude-api",
		Model:    "claude-opus-4-7",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	<-errCh

	// All four template files present in the home folder.
	homeDir := filepath.Join(dir, "frame")
	for _, fname := range []string{"aspect.json", "SOUL.md", "CLAUDE.md", "PRIMER.md"} {
		fp := filepath.Join(homeDir, fname)
		if _, err := os.Stat(fp); err != nil {
			t.Errorf("missing %s: %v", fname, err)
		}
	}

	// SOUL.md should contain the operator's voice + values.
	soul, err := os.ReadFile(filepath.Join(homeDir, "SOUL.md"))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	if !strings.Contains(string(soul), "Terse and direct.") {
		t.Error("SOUL.md missing operator's voice")
	}
	if !strings.Contains(string(soul), "the operator's time, accuracy.") {
		t.Error("SOUL.md missing operator's values")
	}

	// aspect.json should round-trip with role:frame.
	ajRaw, err := os.ReadFile(filepath.Join(homeDir, "aspect.json"))
	if err != nil {
		t.Fatalf("read aspect.json: %v", err)
	}
	var aj map[string]any
	if err := json.Unmarshal(ajRaw, &aj); err != nil {
		t.Fatalf("aspect.json invalid: %v", err)
	}
	if aj["role"] != "frame" {
		t.Errorf("aspect.json role=%v", aj["role"])
	}
	if aj["name"] != "frame" {
		t.Errorf("aspect.json name=%v", aj["name"])
	}
	pc, _ := aj["provider_config"].(map[string]any)
	if pc["model"] != "claude-opus-4-7" {
		t.Errorf("aspect.json model=%v", pc["model"])
	}
}

func TestBootstrap_DefaultsAppliedWhenOptionalFieldsAbsent(t *testing.T) {
	// Operator submits only the required name. Defaults should fill in
	// voice/values/provider/model so the templates render.
	dir := t.TempDir()
	addr, _, errCh := startBootstrap(t, dir)

	resp := postSetup(t, addr, SetupRequest{Name: "frame"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	<-errCh

	soul, err := os.ReadFile(filepath.Join(dir, "frame", "SOUL.md"))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	if strings.Contains(string(soul), "{{") {
		t.Errorf("SOUL.md contains unresolved placeholder: %s", string(soul))
	}
}

func TestBootstrap_HandleIndexRefusesPathTraversal(t *testing.T) {
	// embed.FS rejects ".." path elements via fs.ValidPath; a request
	// like /../etc/passwd should produce a 404 from handleIndex without
	// any filesystem access. This pins the property explicitly so a
	// future refactor that switches off embed.FS doesn't regress.
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	// Request the encoded form so net/http doesn't normalize the dotdot
	// before our handler sees it. Both should 404.
	for _, path := range []string{"/../etc/passwd", "/..%2Fescape", "/styles.css/../../../etc/passwd"} {
		req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("traversal %s: status=%d (want 404 or 400)", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestBootstrap_WizardIndexHasSecurityHeaders(t *testing.T) {
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
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}
}

func TestBootstrap_ConcurrentDifferentNamesProduceOneFrame(t *testing.T) {
	// Regression for the TOCTOU bug: concurrent setups with different
	// names must not produce two frame dirs. Check-and-set-before-write
	// ensures the second submission is rejected before its filesystem
	// work begins.
	//
	// Windows-flake fix: dial both connections BEFORE either sends
	// bytes the server will act on, then release a barrier. http.Post
	// raced the listener's post-success Shutdown on Windows-latest CI
	// and the loser goroutine t.Fatalf'd on connection refused.
	dir := t.TempDir()
	addr, _, errCh := startBootstrap(t, dir)

	names := []string{"frame_a", "frame_b"}
	gate := make(chan struct{})
	conns := make([]net.Conn, len(names))
	for i, name := range names {
		conns[i] = dialAndQueueSetup(t, addr, SetupRequest{Name: name}, gate)
	}
	close(gate)

	statuses := make([]int, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			statuses[i] = readResponseStatus(t, c)
		}(i, c)
	}
	wg.Wait()
	<-errCh

	gotOK := 0
	for _, s := range statuses {
		if s == 200 {
			gotOK++
		}
	}
	if gotOK != 1 {
		t.Errorf("expected exactly one 200 across concurrent different-name setups, got %d (statuses=%v)", gotOK, statuses)
	}

	// And exactly one frame dir exists in agentsDir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	frameCount := 0
	for _, e := range entries {
		if e.IsDir() {
			frameCount++
		}
	}
	if frameCount != 1 {
		t.Errorf("expected exactly one frame dir, got %d", frameCount)
	}

	// And Detect should find that single frame cleanly.
	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("post-concurrent Detect: %v", err)
	}
	if d.Frame == nil {
		t.Fatal("expected one frame, got none")
	}
}

func TestBootstrap_RejectsUnknownProvider(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	resp := postSetup(t, addr, SetupRequest{Name: "frame", Provider: "made-up-llm"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(string(body), "made-up-llm") {
		t.Errorf("error should name bad provider: %s", body)
	}
}

func TestBootstrap_RejectsSecondSetup(t *testing.T) {
	// After a successful setup, Run begins shutting down. We can't
	// reliably hit a second POST after that, so this test exercises the
	// in-memory used-flag check by submitting two near-simultaneous
	// requests and asserting one wins, one loses.
	//
	// Windows-flake fix: we dial both TCP connections BEFORE either has
	// sent its request bytes, then release a barrier so the server sees
	// both arrive on already-established sockets. http.Post's transport
	// would race the listener close on slow Windows TCP, producing a
	// single 200 but also a connection-refused error from the loser
	// goroutine that t.Fatalf'd inside the goroutine — making the test
	// fail despite the server having behaved correctly.
	dir := t.TempDir()
	addr, _, errCh := startBootstrap(t, dir)

	gate := make(chan struct{})
	conns := make([]net.Conn, 2)
	for i := 0; i < 2; i++ {
		body := SetupRequest{Name: fmt.Sprintf("frame%d", i)}
		conns[i] = dialAndQueueSetup(t, addr, body, gate)
	}
	close(gate) // release both writers simultaneously

	statuses := make([]int, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			statuses[i] = readResponseStatus(t, c)
		}(i, c)
	}
	wg.Wait()

	gotOK := 0
	for _, s := range statuses {
		if s == 200 {
			gotOK++
		}
	}
	if gotOK != 1 {
		t.Errorf("expected exactly one 200 across concurrent setups, got statuses=%v", statuses)
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
		{"operator", http.StatusBadRequest},              // reserved
		{"system", http.StatusBadRequest},                // reserved
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

func TestBootstrap_WizardIndexServed(t *testing.T) {
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
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	// Wizard markers — not chasing wording, just confirming the SPA loaded
	// rather than the old stub.
	for _, want := range []string{"<form id=\"setup-form\"", "/wizard.js", "/styles.css"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("wizard index missing %q in body", want)
		}
	}
}

func TestBootstrap_WizardStaticAssetsServed(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	for path, wantCT := range map[string]string{
		"/styles.css": "text/css",
		"/wizard.js":  "application/javascript",
	} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("%s: status=%d", path, resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, wantCT) {
			t.Errorf("%s: content-type=%q want prefix %q", path, ct, wantCT)
		}
		resp.Body.Close()
	}
}

func TestBootstrap_ConfigEndpoint(t *testing.T) {
	dir := t.TempDir()
	addr, stop, errCh := startBootstrap(t, dir)
	defer func() {
		stop()
		<-errCh
	}()

	resp, err := http.Get("http://" + addr + "/bootstrap/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("config status: %d", resp.StatusCode)
	}
	var cfg struct {
		Providers     []string          `json:"providers"`
		DefaultModels map[string]string `json:"default_models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if len(cfg.Providers) == 0 {
		t.Error("config returned no providers")
	}
	want := map[string]bool{"claude-api": false, "openai-api": false, "ollama-local": false}
	for _, p := range cfg.Providers {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, present := range want {
		if !present {
			t.Errorf("config missing provider %q", p)
		}
	}
}

func TestWriteFrameHome_RefusesPathEscape(t *testing.T) {
	dir := t.TempDir()
	// validateName already blocks ../ etc., so we hit writeFrameHome
	// directly with a name that *happened* to pass validation but
	// resolved outside agentsDir. Since the name regex disallows the
	// chars needed for escape, this is mostly defense-in-depth — assert
	// the validateName gate fires first.
	req := SetupRequest{Name: "../escape"}
	applyDefaults(&req)
	_, err := writeFrameHome(dir, req)
	if err == nil {
		t.Fatal("expected error on path-escape name")
	}
}

func TestWriteFrameHome_RefusesExistingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frame"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := SetupRequest{Name: "frame"}
	applyDefaults(&req)
	_, err := writeFrameHome(dir, req)
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
