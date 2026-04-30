// Command smoke-e2e exercises the §6.4 cross-aspect stack end-to-end:
// spawns a Nexus, auto-starts a wren aspect from a test aspect dir,
// drives a `dispatch` frame over WS, asserts the `dispatch.result`
// round-trip — everything talks WS.
//
// Two modes:
//
//   - Default: uses a fake harness (this binary itself, via
//     HANDQUEUE_FAKE_HARNESS env) so no real Claude calls happen.
//     Proves the wire: Nexus binds, aspect registers via WS,
//     dispatch enqueues, subprocess spawns, dispatch.result flows
//     back correlated.
//
//   - -live: uses the real harness binary (built from this repo)
//     and hits Claude for a real verify-canon invocation. Requires
//     ANTHROPIC_API_KEY in env.
//
// Usage:
//
//	go run ./scripts/smoke-e2e                  # default fake
//	go run ./scripts/smoke-e2e -live            # real Claude
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/handexec"
	"github.com/nexus-cw/nexus/shared/schemas"
)

var (
	live     = flag.Bool("live", false, "use real harness binary + real Claude (needs ANTHROPIC_API_KEY)")
	keepRoot = flag.Bool("keep-root", false, "don't delete the temp aspect home on exit")
	token    = flag.String("token", "smoke-e2e-token", "shared bearer token")
)

// TestMain hook: the smoke binary itself acts as a fake harness
// when HANDQUEUE_FAKE_HARNESS is set. Reads handexec.Request from
// stdin, writes a canned DispatchResultPayload to stdout. Keeps the
// default mode dependency-free (no Claude).
func init() {
	if os.Getenv("HANDQUEUE_FAKE_HARNESS") != "" {
		runFakeHarness()
		os.Exit(0)
	}
}

func runFakeHarness() {
	var req handexec.Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "fake harness: decode stdin:", err)
		os.Exit(2)
	}
	resp := frames.DispatchResultPayload{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: req.DispatchID,
		Output: map[string]any{
			"consistent": true,
			"issues":     []string{},
			"fake":       true,
			"echoed":     req.Payload,
		},
	}
	raw, _ := json.Marshal(resp)
	fmt.Println(string(raw))
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func run() error {
	if *live && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("-live requires ANTHROPIC_API_KEY env var")
	}

	root, err := os.MkdirTemp("", "nexus-smoke-*")
	if err != nil {
		return err
	}
	if !*keepRoot {
		defer func() {
			if err := os.RemoveAll(root); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: RemoveAll(%s): %v\n", root, err)
			}
		}()
	}
	fmt.Println("smoke root:", root)

	// Build what we need.
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	nexusBin := filepath.Join(binDir, exeName("nexus"))
	if err := goBuild(nexusBin, "./nexus/cmd/nexus"); err != nil {
		return fmt.Errorf("build nexus: %w", err)
	}

	// The harness path depends on mode.
	var harnessPath string
	if *live {
		harnessPath = filepath.Join(binDir, exeName("agent"))
		if err := goBuild(harnessPath, "./runtime/cmd/agent"); err != nil {
			return fmt.Errorf("build agent: %w", err)
		}
	} else {
		// Reuse this smoke binary as a fake harness.
		self, err := os.Executable()
		if err != nil {
			return err
		}
		harnessPath = self
	}

	// Pick an ephemeral port so parallel smoke runs don't collide.
	port, err := pickFreePort()
	if err != nil {
		return err
	}

	// Aspect home for wren.
	aspectDir := filepath.Join(root, "aspects")
	if err := writeWrenHome(aspectDir); err != nil {
		return err
	}

	dataDir := filepath.Join(root, "data")

	// Start Nexus with auto-spawn pointing at the aspect dir.
	nexusCmd := exec.Command(nexusBin,
		"-addr", fmt.Sprintf(":%d", port),
		"-data-dir", dataDir,
		"-aspect-dir", aspectDir,
		"-harness-path", harnessPath,
	)
	nexusCmd.Env = append(os.Environ(),
		"NEXUS_TOKEN="+*token,
		"HANDQUEUE_FAKE_HARNESS=1", // no-op for -live since real harness isn't this binary
	)
	nexusCmd.Stdout = tagged("[nexus] ", os.Stdout)
	nexusCmd.Stderr = tagged("[nexus] ", os.Stderr)
	if err := nexusCmd.Start(); err != nil {
		return fmt.Errorf("start nexus: %w", err)
	}
	defer terminate(nexusCmd, "nexus")

	// Wait for /health.
	nexusHTTPURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitHTTP(nexusHTTPURL+"/health", 10*time.Second); err != nil {
		return fmt.Errorf("nexus not ready: %w", err)
	}

	// The aspect is a stub harness in default mode — it will get
	// spawned by Nexus BUT because we don't want it competing with
	// our hand.dispatch test (and because auto_spawn: false is in
	// wren/aspect.json), we skip spawning the long-running aspect
	// here. Instead we drive hand.dispatch directly from this
	// script against the Nexus's hand queue. The fake harness will
	// be spawned by the queue itself when a hand.dispatch arrives.
	// However wren's aspect.json has auto_spawn:false, which means
	// Nexus won't auto-start wren; the SpawnExecutor (when it runs
	// a hand) resolves the home via the roster — wren isn't
	// registered. We fix that for the smoke by force-registering a
	// stub aspect via a throwaway WS client.

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/connect", port)

	// Open a WS client as "wren" to register it in the roster so
	// the HandExecutor's HomeResolver finds wren's home.
	stubHome := filepath.Join(aspectDir, "wren")
	regClient, err := dialWS(wsURL, *token)
	if err != nil {
		return fmt.Errorf("register client dial: %w", err)
	}
	defer regClient.Close(websocket.StatusNormalClosure, "done")

	regEnv, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:         "wren",
			ContextMode:  schemas.ContextThread,
			Provider:     "claude-api",
			SessionID:    "smoke-wren-1",
			Home:         stubHome,
			StartedAt:    time.Now().UTC(),
			Capabilities: []string{"canon-verify"},
		},
	})
	if err := writeFrame(regClient, regEnv); err != nil {
		return fmt.Errorf("send register: %w", err)
	}
	ack, err := readFrame(regClient)
	if err != nil {
		return fmt.Errorf("read register ack: %w", err)
	}
	if ack.Kind != frames.KindRegisterAck {
		return fmt.Errorf("register ack kind = %q, want register.ack", ack.Kind)
	}
	fmt.Println("wren registered")

	// Now drive a hand.dispatch from a second client — simulates
	// keel (or operator) asking wren to verify a passage.
	caller, err := dialWS(wsURL, *token)
	if err != nil {
		return fmt.Errorf("caller dial: %w", err)
	}
	defer caller.Close(websocket.StatusNormalClosure, "done")

	// Register the caller minimally so it can participate (broker
	// accepts frames on any connection once it's registered, but
	// the hand.dispatch doesn't actually require registration — it
	// just needs a valid upstream connection).
	callerReg, _ := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:        "smoke-caller",
			ContextMode: schemas.ContextStateless,
			Provider:    "claude-api",
			SessionID:   "smoke-caller-1",
			Home:        stubHome, // home doesn't matter for this client
			StartedAt:   time.Now().UTC(),
		},
	})
	if err := writeFrame(caller, callerReg); err != nil {
		return fmt.Errorf("send caller register: %w", err)
	}
	if ack, err := readFrame(caller); err != nil || ack.Kind != frames.KindRegisterAck {
		return fmt.Errorf("caller register ack: kind=%v err=%v", ack.Kind, err)
	}

	// Submit a dispatch.
	dispatchEnv, _ := frames.NewRequest(frames.KindDispatch, frames.DispatchPayload{
		Aspect:     "wren",
		Thread:     "smoke-thread-1",
		DispatchID: "smoke-dispatch-1",
		Payload:    map[string]any{"text": "A passage for canon check."},
	})
	if err := writeFrame(caller, dispatchEnv); err != nil {
		return fmt.Errorf("send dispatch: %w", err)
	}

	resp, err := readFrame(caller)
	if err != nil {
		return fmt.Errorf("read dispatch.result: %w", err)
	}
	if resp.InReplyTo != dispatchEnv.ID {
		return fmt.Errorf("response InReplyTo = %q, want %q", resp.InReplyTo, dispatchEnv.ID)
	}
	if resp.Kind == frames.KindDispatchError {
		var errPayload frames.DispatchErrorPayload
		_ = frames.PayloadAs(resp, &errPayload)
		return fmt.Errorf("dispatch.error: code=%s reason=%s", errPayload.Code, errPayload.Reason)
	}
	if resp.Kind != frames.KindDispatchResult {
		return fmt.Errorf("response kind = %q, want dispatch.result", resp.Kind)
	}
	var result frames.DispatchResultPayload
	if err := frames.PayloadAs(resp, &result); err != nil {
		return fmt.Errorf("parse result: %w", err)
	}
	fmt.Printf("dispatch.result: aspect=%s dispatch_id=%s output=%v\n", result.Aspect, result.DispatchID, result.Output)

	if *live {
		// Real mode: result["consistent"] should exist as bool.
		if _, ok := result.Output["consistent"]; !ok {
			fmt.Println("WARNING: live mode output missing 'consistent' key — check prompt compliance")
		}
	} else {
		// Fake mode: we know the canned shape.
		if result.Output["fake"] != true {
			return fmt.Errorf("fake mode expected output.fake=true, got %v", result.Output)
		}
		fmt.Println("fake-mode round-trip verified ✓")
	}

	return nil
}

// -------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------

func writeWrenHome(aspectDir string) error {
	wrenDir := filepath.Join(aspectDir, "wren")
	if err := os.MkdirAll(wrenDir, 0o755); err != nil {
		return err
	}
	cfg := schemas.AspectConfig{
		Name:         "wren",
		ContextMode:  schemas.ContextThread,
		Provider:     "claude-api",
		Capabilities: []string{"canon-verify"},
		Metadata:     map[string]any{"auto_spawn": false},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(wrenDir, "aspect.json"), raw, 0o644); err != nil {
		return err
	}

	// In -live mode the real harness will read .credentials/claude-api.json.
	if *live {
		credsDir := filepath.Join(wrenDir, ".credentials")
		if err := os.MkdirAll(credsDir, 0o700); err != nil {
			return err
		}
		creds := map[string]string{"api_key": os.Getenv("ANTHROPIC_API_KEY")}
		raw, _ := json.Marshal(creds)
		if err := os.WriteFile(filepath.Join(credsDir, "claude-api.json"), raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func dialWS(url, token string) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	return c, err
}

func writeFrame(c *websocket.Conn, env frames.Envelope) error {
	raw, err := frames.Encode(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, raw)
}

func readFrame(c *websocket.Conn) (frames.Envelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		return frames.Envelope{}, err
	}
	return frames.Decode(data)
}

func pickFreePort() (int, error) {
	ln, err := listen("127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	_, portStr, _ := strings.Cut(ln.Addr().String(), ":")
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, err
	}
	return port, nil
}

func listen(addr string) (nl net.Listener, err error) {
	return net.Listen("tcp", addr)
}

func goBuild(out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func terminate(cmd *exec.Cmd, name string) {
	if cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		_ = cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Fprintf(os.Stderr, "[%s] shutdown timeout — killing\n", name)
		_ = cmd.Process.Kill()
		<-done
	}
}

func waitHTTP(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s: last err=%v", timeout, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func tagged(prefix string, w io.Writer) io.Writer {
	return &taggedWriter{prefix: []byte(prefix), w: w}
}

type taggedWriter struct {
	prefix []byte
	w      io.Writer
	mu     sync.Mutex
	inLine bool
}

func (t *taggedWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		if !t.inLine {
			_, _ = t.w.Write(t.prefix)
			t.inLine = true
		}
		nl := bytes.IndexByte(p, '\n')
		if nl < 0 {
			t.w.Write(p)
			break
		}
		t.w.Write(p[:nl+1])
		t.inLine = false
		p = p[nl+1:]
	}
	return n, nil
}
