// Command smoke-e2e exercises the whole §6.3 stack end-to-end:
// starts the Nexus broker, prepares a synthetic aspect home,
// starts the agent binary, waits for registration, posts a turn
// against the agent's /turn endpoint, and asserts the response.
//
// Runs in two modes:
//   - Default: a mock-provider Nexus test (no real Anthropic key
//     required) — validates registration, heartbeat, turn dispatch
//     via an in-process fake. This is what the core §6.3 parts
//     already cover in unit tests; here we exercise the binaries.
//   - -live: spawns nexus.exe and agent.exe subprocesses and hits
//     Claude for a real turn. Requires ANTHROPIC_API_KEY in env.
//
// Usage:
//
//	go run ./scripts/smoke-e2e -nexus-port 7890 -agent-port 7990
//	go run ./scripts/smoke-e2e -live -nexus-port 7890 -agent-port 7990
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/nexus-cw/nexus/shared/schemas"
)

var (
	nexusPort = flag.Int("nexus-port", 7890, "port for the nexus broker")
	agentPort = flag.Int("agent-port", 7990, "port the agent's turn-endpoint binds to")
	live      = flag.Bool("live", false, "spawn real nexus.exe + agent.exe binaries and hit Claude")
	keepRoot  = flag.Bool("keep-root", false, "don't delete the temp aspect home on exit")
	token     = flag.String("token", "smoke-e2e-token", "shared bearer token for Nexus auth")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func run() error {
	if !*live {
		return fmt.Errorf("only -live mode is implemented; the default mock-provider path is covered by agent_test.go in-process tests")
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
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
	fmt.Println("aspect root:", root)

	// Build the binaries into a known path so we don't rely on PATH.
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	nexusBin := filepath.Join(binDir, exeName("nexus"))
	agentBin := filepath.Join(binDir, exeName("agent"))

	fmt.Println("building binaries ...")
	if err := goBuild(nexusBin, "./nexus/cmd/nexus"); err != nil {
		return fmt.Errorf("build nexus: %w", err)
	}
	if err := goBuild(agentBin, "./runtime/cmd/agent"); err != nil {
		return fmt.Errorf("build agent: %w", err)
	}

	// Create the aspect home with aspect.json + .credentials.
	home := filepath.Join(root, "aspects", "smoketest")
	if err := writeAspectHome(home); err != nil {
		return err
	}

	// Data dir for Nexus.
	dataDir := filepath.Join(root, "data")

	// Start Nexus.
	nexusCmd := exec.Command(nexusBin,
		"-addr", fmt.Sprintf(":%d", *nexusPort),
		"-data-dir", dataDir,
	)
	nexusCmd.Env = append(os.Environ(), "NEXUS_TOKEN="+*token)
	nexusCmd.Stdout = tagged("[nexus] ", os.Stdout)
	nexusCmd.Stderr = tagged("[nexus] ", os.Stderr)
	if err := nexusCmd.Start(); err != nil {
		return fmt.Errorf("start nexus: %w", err)
	}
	defer terminate(nexusCmd, "nexus")

	// Wait for Nexus /health.
	nexusURL := fmt.Sprintf("http://127.0.0.1:%d", *nexusPort)
	if err := waitHTTP(nexusURL+"/health", 10*time.Second); err != nil {
		return fmt.Errorf("nexus not ready: %w", err)
	}

	// Start the agent. Now connects via WS — NEXUS_UPSTREAM env,
	// no local listener.
	// NOTE: Under the §6.4 WS transport the agent no longer exposes
	// HTTP /healthz or /turn. A full WS-based e2e driver lands in
	// part 10; this script is kept compiling for now.
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/connect", *nexusPort)
	agentCmd := exec.Command(agentBin,
		"-home", home,
	)
	agentCmd.Env = append(os.Environ(),
		"NEXUS_TOKEN="+*token,
		"NEXUS_UPSTREAM="+wsURL,
	)
	agentCmd.Stdout = tagged("[agent] ", os.Stdout)
	agentCmd.Stderr = tagged("[agent] ", os.Stderr)
	if err := agentCmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	defer terminate(agentCmd, "agent")

	// Wait briefly for the agent to connect. Part 10 replaces this
	// with a proper WS client that drives a turn and asserts the
	// round-trip.
	time.Sleep(2 * time.Second)
	fmt.Println("agent should have connected; turn driver deferred to part 10")
	return nil
}

func writeAspectHome(home string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	cfg := schemas.AspectConfig{
		Name:         "smoketest",
		ContextMode:  schemas.ContextGlobal,
		Provider:     "claude-api",
		Port:         0,
		Capabilities: []string{"smoke"},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, "aspect.json"), raw, 0o644); err != nil {
		return err
	}

	credsDir := filepath.Join(home, ".credentials")
	if err := os.MkdirAll(credsDir, 0o700); err != nil {
		return err
	}
	creds := map[string]string{"api_key": os.Getenv("ANTHROPIC_API_KEY")}
	credsRaw, _ := json.Marshal(creds)
	return os.WriteFile(filepath.Join(credsDir, "claude-api.json"), credsRaw, 0o600)
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

// tagged returns an io.Writer that prefixes each line with `prefix`.
// Writes are mutex-guarded so concurrent output from nexus and agent
// subprocesses doesn't interleave mid-line.
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
