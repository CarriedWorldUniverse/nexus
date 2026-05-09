package frame

// Bootstrap-mode HTTP shell — runs when Detect finds no Frame. Serves the
// embedded wizard SPA at / and a single POST /bootstrap/setup that writes
// the new Frame's home folder via the templates package. Returns when
// setup completes so the caller can exit cleanly and be restarted with
// the new Frame attached.
//
// P2 introduced the HTTP shell + atomic write of a minimal aspect.json.
// P3 added Markdown templates with placeholder substitution.
// P4 (this file's current state) wires the templates into setup, ships
//     the wizard SPA via go:embed, and exposes /bootstrap/config so the
//     SPA can stay in lockstep with server-side validation.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/templates"
)

//go:embed bootstrap_static
var bootstrapStaticFS embed.FS

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

// SetupRequest is the wire payload for POST /bootstrap/setup. The wizard
// (P4) gathers these answers; the bootstrap server hands them to the
// templates package to render the new Frame's home folder.
//
// All fields except Name have defaults applied if the wizard left them
// empty — see applyDefaults. The strict-vars contract in templates
// requires every key to be present; defaults turn "operator skipped"
// into "operator accepted the default."
type SetupRequest struct {
	Name     string `json:"name"`
	Voice    string `json:"voice,omitempty"`
	Values   string `json:"values,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
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
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /bootstrap/config", s.handleConfig)
	mux.HandleFunc("POST /bootstrap/setup", s.handleSetup)
	// Catch-all for static SPA assets (index.html + any sibling files).
	// Must be last so the /bootstrap/* and /healthz routes win.
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

// handleIndex serves the embedded wizard SPA at /. Static files (HTML,
// CSS, JS) live under bootstrap_static/ and ship in the binary via
// go:embed. Strip the bootstrap_static prefix so URLs like /styles.css
// resolve to bootstrap_static/styles.css inside the embed.FS.
func (s *bootstrapServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimPrefix(r.URL.Path, "/")
	if requested == "" {
		requested = "index.html"
	}
	fpath := "bootstrap_static/" + requested
	data, err := fs.ReadFile(bootstrapStaticFS, fpath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(fpath, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(fpath, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(fpath, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}
	// Security headers — bootstrap port is unauthenticated by design and
	// runs only until first-boot completes, but the page writes filesystem
	// state. Same-origin only; no framing; minimal hygiene.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'self'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleConfig returns the wizard's static configuration — provider list,
// default model per provider, and any other choices the SPA needs to
// render. Surfaced via /bootstrap/config so the SPA stays in sync with
// server-side validation rather than duplicating the allowlist.
//
// Single source of truth: providerDefaults. Adding a provider needs one
// edit there and nothing here.
func (s *bootstrapServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	providers := make([]string, 0, len(providerDefaults))
	for k := range providerDefaults {
		providers = append(providers, k)
	}
	sort.Strings(providers)
	defaults := make(map[string]string, len(providerDefaults))
	for k, v := range providerDefaults {
		defaults[k] = v
	}
	cfg := struct {
		Providers     []string          `json:"providers"`
		DefaultModels map[string]string `json:"default_models"`
	}{
		Providers:     providers,
		DefaultModels: defaults,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(cfg)
}

func (s *bootstrapServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"bootstrap","ready":true}`))
}

// handleSetup writes the new Frame's home folder. Single-shot: once a
// setup begins, further POSTs return 409.
//
// Single-shot semantics use check-and-set-before-write: we claim the
// `used` slot under the lock BEFORE calling writeFrameHome, so concurrent
// POSTs are rejected at the door. This avoids the unlock-between-check-
// and-write window where two requests with different names could both
// succeed in writing distinct frame dirs (leaving Detect to find two
// frame-role aspects on restart, which is ErrMultipleFrames).
//
// On write failure after claiming the slot, we restore `used = false` so
// the operator can retry. The done channel only closes on success.
func (s *bootstrapServer) handleSetup(w http.ResponseWriter, r *http.Request) {
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

	applyDefaults(&req)
	if err := validateProvider(req.Provider); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider", err.Error())
		return
	}

	// Claim the single-shot slot atomically before doing any filesystem
	// work. Concurrent POSTs are rejected with 409 here, not after their
	// own write succeeds.
	s.mu.Lock()
	if s.used {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "bootstrap_already_complete", "setup has already run; restart Nexus to continue")
		return
	}
	s.used = true
	s.mu.Unlock()

	homePath, err := writeFrameHome(s.cfg.AgentsDir, req)
	if err != nil {
		// Release the slot so the operator can retry after fixing the
		// underlying issue (disk full, permissions, etc.).
		s.mu.Lock()
		s.used = false
		s.mu.Unlock()
		s.cfg.Logger.Error("frame: bootstrap write failed", "name", req.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "write_failed", err.Error())
		return
	}

	close(s.done)

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

// providerDefaults is the single source of truth for both the wizard's
// allowed provider list AND the default model per provider. applyDefaults
// and handleConfig both read from this map so adding a provider needs
// only one edit.
var providerDefaults = map[string]string{
	"claude-api":   "claude-opus-4-7",
	"openai-api":   "gpt-4o",
	"ollama-local": "llama3.2:3b",
}

// applyDefaults fills in optional SetupRequest fields with sensible
// defaults so the strict-vars contract in templates.Render always sees
// a complete map. Operator-empty fields become operator-accepted defaults.
func applyDefaults(req *SetupRequest) {
	if req.Voice == "" {
		req.Voice = "Direct, low-affect, plain. Reports what's happening, asks for clarification when needed, doesn't perform."
	}
	if req.Values == "" {
		req.Values = "the network running well, the operator's time, honest reporting, the aspects' work landing cleanly."
	}
	if req.Provider == "" {
		req.Provider = "claude-api"
	}
	if req.Model == "" {
		req.Model = providerDefaults[req.Provider]
	}
}

// validateProvider checks the provider against the allowed set.
func validateProvider(p string) error {
	if _, ok := providerDefaults[p]; !ok {
		names := make([]string, 0, len(providerDefaults))
		for k := range providerDefaults {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("provider %q not allowed; one of: %s", p, strings.Join(names, ", "))
	}
	return nil
}

// writeFrameHome creates <agentsDir>/<name>/{aspect.json,SOUL.md,CLAUDE.md,
// PRIMER.md} from the "default" template, substituting the wizard's
// answers. Returns the absolute home path on success.
//
// Atomicity: writes the home dir, then each file via temp+rename. If any
// step fails the partial home is left for inspection — masking errors
// with silent cleanup costs more in debuggability than it saves.
func writeFrameHome(agentsDir string, req SetupRequest) (string, error) {
	if err := validateName(req.Name); err != nil {
		return "", err
	}

	homePath := filepath.Clean(filepath.Join(agentsDir, req.Name))

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

	bundle, rerr := templates.Render("default", map[string]string{
		"name":     req.Name,
		"voice":    req.Voice,
		"values":   req.Values,
		"provider": req.Provider,
		"model":    req.Model,
	})
	if rerr != nil {
		return "", fmt.Errorf("render template: %w", rerr)
	}

	if err := os.MkdirAll(absAgents, 0o755); err != nil {
		return "", fmt.Errorf("ensure agents dir: %w", err)
	}
	if err := os.Mkdir(absHome, 0o755); err != nil {
		return "", fmt.Errorf("create home: %w", err)
	}

	for fname, content := range bundle {
		tmpPath := filepath.Join(absHome, fname+".tmp")
		if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
			return "", fmt.Errorf("write tmp %s: %w", fname, err)
		}
		finalPath := filepath.Join(absHome, fname)
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("rename %s: %w", fname, err)
		}
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
