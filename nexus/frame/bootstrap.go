package frame

// Bootstrap-mode HTTP shell — runs when Detect finds no Frame. Serves a
// minimal landing page (P4 will replace with the real wizard SPA) and
// accepts a single POST that writes the new Frame's home folder. Returns
// when setup completes successfully so the caller can exit cleanly and
// be restarted with the new Frame attached.
//
// P2 scope: HTTP shell only. Templates (P3) and the rich wizard UI (P4)
// land in subsequent parts. P2 writes a minimal aspect.json with
// role:frame and the operator-supplied name; that's enough for a
// normal-mode startup to succeed and detect the new Frame.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nexus-cw/nexus/shared/schemas"
)

// Reserved aspect names — operators can't use these for the Frame because
// they'd collide with built-in identities or cause confusion in chat.
// Keep aligned with broker auth.go and orchestrator conventions.
var reservedNames = map[string]struct{}{
	"operator":     {}, // human user — never an aspect
	"system":       {}, // broker bot identity
	"orchestrator": {}, // existing orchestrator identity
	"nexus":        {}, // would conflate with the network itself
	"all":          {}, // @all is a broadcast keyword in some contexts
	"":             {}, // empty
}

// nameValid matches alphanumeric + underscore, 1–32 chars. Spec §5.3:
// "Sanity-check: alphanumeric + underscore, no collision with reserved
// names." 32 char cap is arbitrary but enough for any sensible handle
// and short enough that broker logs stay readable.
var nameValid = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// SetupRequest is the wire payload for POST /bootstrap/setup. Minimal at
// P2 — just the fields needed to write a valid aspect.json. P3+ extends
// this with voice/values/provider/api-key as templates land.
type SetupRequest struct {
	Name string `json:"name"`
}

// SetupResponse is what the operator's browser sees on success. The
// HTTP server has begun shutting down by the time this is sent; the
// browser shows it briefly before the supervisor restarts the process.
type SetupResponse struct {
	Status      string `json:"status"`
	FramePath   string `json:"frame_path"`
	Message     string `json:"message"`
	RestartHint string `json:"restart_hint"`
}

// BootstrapConfig tunes the Bootstrap server. Caller fills in.
type BootstrapConfig struct {
	Addr      string        // listen address (typically same as normal-mode addr)
	AgentsDir string        // where to write the new Frame's home folder
	Timeout   time.Duration // shutdown grace after successful setup
	Logger    *slog.Logger
}

// ErrBootstrapAlreadyComplete is returned by Run when a Frame already
// exists in AgentsDir at startup. Caller should treat this as "we're
// not in bootstrap mode after all" and not run bootstrap.
var ErrBootstrapAlreadyComplete = errors.New("frame: bootstrap not needed — frame already exists")

// Run serves the bootstrap shell on cfg.Addr until either:
//   - a successful POST /bootstrap/setup completes (returns nil — caller
//     should exit with restart-requested code).
//   - ctx is canceled (returns ctx.Err()).
//   - the HTTP server itself errors.
//
// Caller responsibilities:
//   - Verify Detect returned no Frame before calling.
//   - Handle the restart after Run returns nil. Bootstrap does NOT
//     restart the process itself — it exits its server cleanly and
//     leaves restart to the supervisor.
func Run(ctx context.Context, cfg BootstrapConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Addr == "" {
		return errors.New("frame: BootstrapConfig.Addr required")
	}
	if cfg.AgentsDir == "" {
		return errors.New("frame: BootstrapConfig.AgentsDir required")
	}

	// Defensive recheck: if the operator created a frame folder by hand
	// between the Detect call and now, don't overwrite it.
	pre, perr := Detect(cfg.AgentsDir)
	if perr == nil && pre.Frame != nil {
		return ErrBootstrapAlreadyComplete
	}

	server := newBootstrapServer(cfg)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.mux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		cfg.Logger.Info("frame: bootstrap mode listening", "addr", cfg.Addr, "agents_dir", cfg.AgentsDir)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		// shutdown best-effort
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return ctx.Err()

	case <-server.done:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil

	case err := <-errCh:
		return fmt.Errorf("frame: bootstrap server: %w", err)
	}
}

// bootstrapServer holds the per-run state. Setup is single-shot — once
// a successful POST /bootstrap/setup happens, the done channel closes
// and Run shuts the server down.
type bootstrapServer struct {
	cfg  BootstrapConfig
	mu   sync.Mutex
	done chan struct{}
	used bool // setup already completed; reject further POSTs
}

func newBootstrapServer(cfg BootstrapConfig) *bootstrapServer {
	return &bootstrapServer{cfg: cfg, done: make(chan struct{})}
}

func (s *bootstrapServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /bootstrap/setup", s.handleSetup)
	return mux
}

// handleIndex returns a stub page so an operator hitting the server in a
// browser sees something. P4 replaces this with the real wizard SPA.
func (s *bootstrapServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(stubPage))
}

func (s *bootstrapServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"bootstrap","ready":true}`))
}

// handleSetup writes the new Frame's home folder. Single-shot: once a
// setup succeeds the server is on its way down; further POSTs return 409.
func (s *bootstrapServer) handleSetup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.used {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "bootstrap_already_complete", "setup has already run; restart Nexus to continue")
		return
	}
	s.mu.Unlock()

	if r.ContentLength > 64*1024 {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "setup payload exceeds 64KB")
		return
	}

	var req SetupRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if err := validateName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error())
		return
	}

	homePath, err := writeFrameHome(s.cfg.AgentsDir, req.Name)
	if err != nil {
		s.cfg.Logger.Error("frame: bootstrap write failed", "name", req.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}

	s.mu.Lock()
	if s.used {
		// Concurrent POST raced us; rollback our write to avoid drift.
		s.mu.Unlock()
		_ = os.RemoveAll(homePath)
		writeError(w, http.StatusConflict, "bootstrap_already_complete", "another setup completed first")
		return
	}
	s.used = true
	close(s.done)
	s.mu.Unlock()

	s.cfg.Logger.Info("frame: bootstrap complete", "name", req.Name, "path", homePath)

	resp := SetupResponse{
		Status:      "ok",
		FramePath:   homePath,
		Message:     fmt.Sprintf("Frame %q created. Restart Nexus to bring it online.", req.Name),
		RestartHint: "Nexus is shutting down its bootstrap server. Your supervisor (systemd, docker, or manual relaunch) should restart the process.",
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// validateName enforces the spec §5.3 sanity check + reserved-name
// blocklist. Path traversal is impossible because the regex disallows
// `/`, `\`, and `.` — but we still join with filepath.Join + Clean
// before the write to be defensive.
func validateName(name string) error {
	if !nameValid.MatchString(name) {
		return errors.New(`name must match [A-Za-z0-9_]{1,32}`)
	}
	if _, ok := reservedNames[name]; ok {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

// writeFrameHome creates <agentsDir>/<name>/aspect.json with role:frame
// and the supplied name. Returns the absolute home path on success.
//
// Atomicity: writes a temp file in the home dir then renames into place.
// If the rename fails, the partial home dir is left for the operator to
// inspect — better than masking an unclear error with a silent cleanup.
func writeFrameHome(agentsDir, name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}

	homePath := filepath.Clean(filepath.Join(agentsDir, name))

	// Confirm the joined path is still inside agentsDir — defensive
	// even though validateName already excludes path-traversal chars.
	absAgents, err := filepath.Abs(agentsDir)
	if err != nil {
		return "", fmt.Errorf("resolve agents dir: %w", err)
	}
	absHome, err := filepath.Abs(homePath)
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	// Defense-in-depth: the resolved home must sit *under* the resolved
	// agents dir. Use a separator-suffixed prefix check rather than
	// filepath.Rel — Rel-based checks have mixed-separator edge cases
	// on Windows where forward and back slashes intermix.
	prefix := absAgents + string(filepath.Separator)
	if !strings.HasPrefix(absHome+string(filepath.Separator), prefix) {
		return "", fmt.Errorf("home path escapes agents dir")
	}

	if _, statErr := os.Stat(absHome); statErr == nil {
		return "", fmt.Errorf("home %s already exists", absHome)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("stat home: %w", statErr)
	}

	if err := os.MkdirAll(absAgents, 0o755); err != nil {
		return "", fmt.Errorf("ensure agents dir: %w", err)
	}
	if err := os.Mkdir(absHome, 0o755); err != nil {
		return "", fmt.Errorf("create home: %w", err)
	}

	cfg := schemas.AspectConfig{
		Name:        name,
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		// Provider intentionally left empty at P2 — P3 templates fill
		// this in with the operator's chosen LLM. A nexus startup with
		// a frame missing Provider will surface a clear error at P5
		// time; that's fine for v1 ops.
	}
	raw, mErr := json.MarshalIndent(cfg, "", "  ")
	if mErr != nil {
		return "", fmt.Errorf("marshal aspect.json: %w", mErr)
	}

	tmpPath := filepath.Join(absHome, "aspect.json.tmp")
	if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
		return "", fmt.Errorf("write tmp aspect.json: %w", err)
	}
	finalPath := filepath.Join(absHome, "aspect.json")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename aspect.json: %w", err)
	}

	return absHome, nil
}

// writeError writes a JSON error response. Single shape across all
// bootstrap endpoints so the wizard SPA in P4 has one parser path.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

// stubPage is the placeholder index served at GET /. P4 replaces this
// with the real wizard SPA. Kept inline so P2 has no static-asset
// dependencies — easier to reason about, harder to confuse with the
// real wizard during P4 review.
const stubPage = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Nexus — first-boot</title>
  <style>
    body{font-family:system-ui,sans-serif;max-width:42rem;margin:3rem auto;padding:0 1rem;color:#222}
    code{background:#f0f0f0;padding:0.2rem 0.4rem;border-radius:3px}
  </style>
</head>
<body>
  <h1>Nexus is in bootstrap mode.</h1>
  <p>No Frame personality found. The first-boot wizard will land here in a future build (§6.5 P4).</p>
  <p>For now, set up the Frame by POSTing JSON to <code>/bootstrap/setup</code>:</p>
  <pre>curl -X POST http://localhost:7888/bootstrap/setup \
  -H 'Content-Type: application/json' \
  -d '{"name":"frame"}'</pre>
  <p>After the setup succeeds, restart Nexus.</p>
</body>
</html>
`
