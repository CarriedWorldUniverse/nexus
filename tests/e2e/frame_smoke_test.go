// Package e2e is the end-to-end smoke for §6.5 P9.
//
// Scope: bootstrap → setup → verify Frame folder rendered correctly →
// confirm Detect picks up the new frame on the next "boot." Stops short
// of actually re-launching the Nexus process (which would need a live
// LLM for the funnel turn) — the embed/funnel/admin/routing pieces
// each have their own unit tests; this exercises the cross-package
// integration through P1-P4.
//
// The bigger e2e (live model, real chat round-trip, admin shutdown) is
// in tests/e2e/MANUAL.md as an operator checklist.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// TestFrameSmoke_BootstrapToDetected runs the §6.5 happy path:
//  1. Empty agents dir.
//  2. Run frame.Detect → no frame.
//  3. Run frame.Run (bootstrap mode) on a free port.
//  4. POST /bootstrap/setup → 200, frame home written.
//  5. Run frame.Detect again → frame found.
//  6. Verify all four template files rendered with no unresolved placeholders.
func TestFrameSmoke_BootstrapToDetected(t *testing.T) {
	agentsDir := t.TempDir()

	// Step 1+2: empty agents dir, no frame.
	pre, err := frame.Detect(agentsDir)
	if err != nil {
		t.Fatalf("pre-Detect: %v", err)
	}
	if pre.Frame != nil {
		t.Fatalf("expected no frame in empty dir, got %+v", pre.Frame)
	}

	// Step 3: spin up bootstrap server on a free port.
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- frame.Run(ctx, frame.BootstrapConfig{
			Addr:      addr,
			AgentsDir: agentsDir,
			Timeout:   2 * time.Second,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	})

	// Wait for healthz.
	waitForReady(t, addr)

	// Step 4: POST setup with full operator-supplied fields.
	setupReq := map[string]any{
		"name":     "anchor",
		"voice":    "Direct, terse, plain.",
		"values":   "the network running well, honest reporting.",
		"provider": "claude-api",
		"model":    "claude-opus-4-7",
	}
	resp := postJSON(t, "http://"+addr+"/bootstrap/setup", setupReq)
	if resp.StatusCode != 200 {
		t.Fatalf("setup status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var setupResp struct {
		Status    string `json:"status"`
		FramePath string `json:"frame_path"`
	}
	if err := json.Unmarshal(readBody(t, resp), &setupResp); err != nil {
		t.Fatalf("decode setup response: %v", err)
	}
	if setupResp.Status != "ok" {
		t.Errorf("status=%q want ok", setupResp.Status)
	}

	// Run should return nil (clean shutdown) since setup succeeded.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("frame.Run returned err: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frame.Run did not return after successful setup")
	}

	// Step 5: post-setup Detect finds the frame.
	post, err := frame.Detect(agentsDir)
	if err != nil {
		t.Fatalf("post-Detect: %v", err)
	}
	if post.Frame == nil {
		t.Fatal("expected frame after setup, got nil")
	}
	if post.Frame.Name != "anchor" {
		t.Errorf("frame name=%q want anchor", post.Frame.Name)
	}
	if post.Frame.Config.EffectiveRole() != schemas.RoleFrame {
		t.Errorf("role=%q want frame", post.Frame.Config.EffectiveRole())
	}

	// Step 6: each template file rendered, no unresolved placeholders.
	homeDir := filepath.Join(agentsDir, "anchor")
	for _, fname := range []string{"aspect.json", "SOUL.md", "CLAUDE.md", "PRIMER.md"} {
		fpath := filepath.Join(homeDir, fname)
		raw, err := os.ReadFile(fpath)
		if err != nil {
			t.Errorf("read %s: %v", fname, err)
			continue
		}
		if strings.Contains(string(raw), "{{") {
			t.Errorf("%s contains unresolved placeholder: %s", fname, raw)
		}
		if !strings.Contains(string(raw), "anchor") && fname != "aspect.json" {
			// aspect.json doesn't include the literal name in its template
			// body for SOUL/CLAUDE/PRIMER — they all reference {{name}}.
			t.Errorf("%s should reference frame name 'anchor': %s", fname, raw)
		}
	}

	// SOUL.md should carry voice + values from the wizard.
	soul, _ := os.ReadFile(filepath.Join(homeDir, "SOUL.md"))
	if !strings.Contains(string(soul), "Direct, terse, plain.") {
		t.Error("SOUL.md missing operator-supplied voice")
	}
	if !strings.Contains(string(soul), "the network running well") {
		t.Error("SOUL.md missing operator-supplied values")
	}
}

// TestFrameSmoke_BootstrapAlreadyComplete: if a frame exists, frame.Run
// errors with ErrBootstrapAlreadyComplete and doesn't bind a port.
func TestFrameSmoke_BootstrapAlreadyComplete(t *testing.T) {
	agentsDir := t.TempDir()

	// Pre-create a frame folder.
	homeDir := filepath.Join(agentsDir, "existing")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := schemas.AspectConfig{
		Name:        "existing",
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(homeDir, "aspect.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	err := frame.Run(context.Background(), frame.BootstrapConfig{
		Addr:      freePort(t),
		AgentsDir: agentsDir,
		Timeout:   time.Second,
	})
	if err == nil {
		t.Fatal("expected ErrBootstrapAlreadyComplete, got nil")
	}
	// (Don't import errors.Is here; a substring match is enough for the
	// smoke test.)
	if !strings.Contains(err.Error(), "frame already exists") {
		t.Errorf("error should mention frame already exists, got: %v", err)
	}
}

// helpers ---------------------------------------------------------------

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

func waitForReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server not ready on %s", addr)
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
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
