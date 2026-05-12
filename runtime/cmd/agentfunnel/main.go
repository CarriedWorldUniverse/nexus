// Command agentfunnel is the keyfile-auth aspect runtime — single
// binary that any aspect host runs with `agentfunnel -k <keyfile>`.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §14
// part 5: replaces the per-home aspect.exe model. Identity, personality,
// provider, and model all come from the Nexus during the startup
// validation handshake — there is no on-disk aspect.json on the host.
//
// Boot flow:
//
//	read keyfile from -k path (runtime/keyfile.Load)
//	  → spec §4 envelope + encrypted_payload
//	dial GET /api/nexus_id, verify against envelope
//	  → spec §5: don't send the encrypted payload to the wrong Nexus
//	POST /api/aspect/validate
//	  → response carries: session_jwt, personality, provider, model
//	wire JWT as wsasp.Config.AuthToken (replaces NEXUS_TOKEN env)
//	wire personality.composed as funnel.Config.SystemPrompt
//	build provider via bridle, run the standard funnel deliberation loop
//
// Differences from runtime/cmd/aspect (the per-home aspect.json model):
//   - No -home flag; identity + personality + provider come from Nexus
//   - No NEXUS_TOKEN env; the JWT from validation is the bearer
//   - Personality SystemPrompt is the composed Nexus-side bundle, not
//     hand-assembled on the aspect host
//
// What's deferred to later parts:
//   - JWT refresh on expiry (spec §6) — Part 5 v0.1 exits ~5 minutes
//     before expiry via jwtExpiryMonitor and relies on the supervisor
//     restart loop to re-validate. Refresh-without-restart is Part 7.
//     (Without the monitor, wsclient.Run would treat 401s as transient
//     and reconnect forever, zombieing the process.)
//   - personality.refresh push protocol — Part 7.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/rewriter"
	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"github.com/CarriedWorldUniverse/nexus/runtime/obsforward"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/google/uuid"
)

func main() {
	keyfilePath := flag.String("k", "", "path to the aspect keyfile (required)")
	cursorDir := flag.String("cursor-dir", "", "directory for the Lock 6 message-cursor file (defaults to <cwd>/cursor)")
	contextMode := flag.String("context-mode", string(schemas.ContextThread), "context mode: global, thread, or stateless (Nexus does not yet ship context_mode in the validation response)")
	claudePath := flag.String("claude", "", "path to the claude-code CLI (optional; auto-detects /opt/homebrew/bin/claude, /usr/local/bin/claude, ~/.npm-global/bin/claude, then PATH; also honours CLAUDE_PATH env)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *keyfilePath == "" {
		fail(log, "missing -k flag (path to keyfile)", nil)
	}
	cm := schemas.ContextMode(*contextMode)
	switch cm {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		fail(log, fmt.Sprintf("invalid --context-mode %q (want global/thread/stateless)", *contextMode), nil)
	}

	// 1. Read keyfile.
	kf, err := keyfile.Load(*keyfilePath)
	if err != nil {
		fail(log, "load keyfile", err)
	}
	log.Info("agentfunnel: keyfile loaded",
		"path", *keyfilePath,
		"nexus_url", kf.Envelope.NexusURL,
		"nexus_id", kf.Envelope.NexusID)

	// 2. Validation handshake. The keyfile.Client has its own 10s
	// per-call HTTP timeout (covers the GET /api/nexus_id and POST
	// /api/aspect/validate calls separately). The 30s outer ctx
	// timeout is a backstop so a hung process between calls (e.g.
	// stuck in TLS handshake setup) eventually surfaces as a startup
	// error rather than dangling forever.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	client := keyfile.NewClient()
	res, err := client.Validate(bootCtx, kf)
	bootCancel()
	if err != nil {
		// Render the most actionable hint we can per the sentinel.
		switch {
		case errors.Is(err, keyfile.ErrNexusMismatch):
			log.Error("agentfunnel: keyfile envelope nexus_id does not match the server",
				"hint", "the keyfile may be stale (Nexus identity regenerated) or pointed at the wrong host",
				"err", err)
		case errors.Is(err, keyfile.ErrValidationRejected):
			log.Error("agentfunnel: server rejected validation",
				"hint", "check server response — likely revoked, retired, or unknown aspect",
				"err", err)
		case errors.Is(err, keyfile.ErrBadServerResponse):
			log.Error("agentfunnel: server returned malformed response — likely a Nexus bug",
				"err", err)
		case errors.Is(err, keyfile.ErrBadKeyfile):
			log.Error("agentfunnel: keyfile is malformed", "err", err)
		default:
			log.Error("agentfunnel: validation failed", "err", err)
		}
		os.Exit(1)
	}
	log.Info("agentfunnel: validated",
		"aspect", res.AspectName,
		"provider", res.Provider,
		"model", res.Model,
		"personality_version", res.Personality.Version,
		"jwt_expires", res.SessionExpiresAt.Format(time.RFC3339))

	// 3. Build provider.
	provider, err := buildProvider(res.Provider, *claudePath)
	if err != nil {
		fail(log, "build provider", err)
	}

	// 4. Compose funnel + wsasp client.
	sessionID := uuid.NewString()
	cursorFile := wsasp.CursorFileForAspect(resolveCursorDir(*cursorDir))

	// Defensive: the WS dial path must be /connect; older keyfiles
	// minted with the bare authority (no path) would silently hit the
	// broker's root handler instead. Surfaced on 2026-05-11 cutover
	// (plumb's first connect attempt). Append /connect if missing so
	// keyfiles without it still work.
	wsURL := res.NexusURL
	if !strings.HasSuffix(wsURL, "/connect") && !strings.HasSuffix(wsURL, "/connect/") {
		wsURL = strings.TrimRight(wsURL, "/") + "/connect"
	}

	var bridge *wsasp.Bridge
	wsCfg := wsasp.Config{
		URL:        wsURL,
		AuthToken:  res.SessionJWT, // <- the JWT replaces NEXUS_TOKEN
		AspectName: res.AspectName,
		CursorFile: cursorFile,
		OnDeliver: func(msg wsasp.DeliveredMessage) {
			if bridge != nil {
				bridge.OnDeliver(msg)
			}
		},
		Register: schemas.RegisterRequest{
			Name:        res.AspectName,
			ContextMode: cm,
			Provider:    res.Provider,
			PID:         os.Getpid(),
			StartedAt:   time.Now().UTC(),
			Model:       res.Model,
			SessionID:   sessionID,
		},
	}
	wsClient, err := wsasp.NewClient(wsCfg)
	if err != nil {
		fail(log, "wsasp.NewClient", err)
	}

	gateway := wsasp.NewGateway(wsClient)
	commsRunner := funnel.CommsRunner{Gateway: gateway}

	// Phase E remote forwarding: agentfunnel's funnel runs in a
	// different process from the broker's observability Hub, so the
	// hook here marshals each BeginTurn / OnBridleEvent / EndTurn call
	// into a wire frame and pushes it through the same WS connection
	// the aspect already uses. Best-effort path (no replay buffer) —
	// stale observability frames after a reconnect are worse than
	// missing ones.
	obsHook := obsforward.New(
		obsforward.SenderFunc(wsClient.SendBestEffort),
		res.AspectName,
		log,
	)

	// Rewriter wiring: default-on for claude-code-flavored providers,
	// no-op otherwise. The session jsonl path is resolved lazily
	// through funnelPtr so funnel session-id rotations (compaction,
	// rewriter-driven reset) are picked up automatically.
	var funnelPtr *funnel.Funnel
	postTurn := buildAgentFunnelRewriter(res.AspectName, res.Provider, provider, res.Model, func() string {
		if funnelPtr == nil {
			return ""
		}
		return funnelPtr.SessionID()
	}, log)

	systemPrompt := composeSystemPrompt(res)
	f, err := funnel.New(funnel.Config{
		AspectID:     res.AspectName,
		Harness:      bridle.NewHarness(provider),
		Provider:     bridle.ProviderID(res.Provider),
		Model:        res.Model,
		SystemPrompt: systemPrompt,
		// Tools field is for direct-API providers; claude-code subprocess
		// owns its own tool surface natively. Mirrors cmd/nexus/main.go's
		// toolsForProvider — see #181 for the MCP fix.
		Tools:        toolsForProviderAgent(bridle.ProviderID(res.Provider)),
		Runner:       funnel.ComposeRunner(commsRunner, &funnel.NullRunner{}),
		// ChatGateway routes the model's auto-post FinalText through the
		// same SendChat path CommsRunner uses for explicit send_chat tool
		// calls. Required for claude-code (subprocess mode): without it,
		// model output evaporates because the CLI has no MCP-loaded tools
		// to call. Mirrors cmd/nexus/main.go's Frame funnel wiring.
		ChatGateway:       gateway,
		PostTurn:          postTurn,
		ObservabilityHook: obsHook,
		Logger:            log,
	})
	if err != nil {
		fail(log, "funnel.New", err)
	}
	funnelPtr = f
	bridge = wsasp.NewBridge(f)

	log.Info("agentfunnel: starting deliberation loop",
		"aspect", res.AspectName,
		"session", sessionID,
		"system_prompt_bytes", len(systemPrompt),
		"central_version", res.CentralVersion,
		"personality_version", res.Personality.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// JWT pre-expiry monitor (v0.1 supervisor-restart model).
	//
	// Without this, the process zombies on JWT expiry: wsclient.Run
	// treats every dial error (including 401 after the bearer goes
	// stale) as transient and reconnects forever. The supervisor sees
	// a live process and never restarts. Until Part 7 lands proper
	// re-validate-without-restart, the only working strategy is to
	// exit cleanly before expiry so the supervisor cycles us.
	//
	// 5-minute lead time: tight enough to keep using the JWT for most
	// of its hour, generous enough that we don't hit "expired in
	// flight" mid-handshake on a slow network.
	go jwtExpiryMonitor(ctx, res.SessionExpiresAt, 5*time.Minute, stop, log)

	go deliberateLoop(ctx, f, log)

	if err := wsClient.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agentfunnel: wsClient.Run", "err", err)
		os.Exit(1)
	}
	log.Info("agentfunnel: stopped")
}

// composeSystemPrompt layers the validation result into the four-
// section concat per spec §3 (personality decomposition):
//
//	central.nexus_md ⊕ aspect.nexus_md ⊕ aspect.soul_md ⊕ aspect.primer_md
//
// When personality.composed is non-empty (Part 7 renderer populated it),
// uses central + composed instead — but the renderer must NOT bake
// central into composed (the no-double-bake invariant pinned in
// nexus/frame/embed_personality_test.go's
// TestEmbed_ComposedDoesNotDoubleBakeCentral).
//
// Empty sections are dropped from the join. Returns "" only when
// every section is empty (legacy / pre-Part-9 Nexus + unprovisioned
// aspect).
func composeSystemPrompt(res *keyfile.ValidationResult) string {
	if res == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if res.CentralNexusMD != "" {
		parts = append(parts, res.CentralNexusMD)
	}
	if res.Personality.Composed != "" {
		parts = append(parts, res.Personality.Composed)
	} else {
		if res.Personality.NexusMD != "" {
			parts = append(parts, res.Personality.NexusMD)
		}
		if res.Personality.SoulMD != "" {
			parts = append(parts, res.Personality.SoulMD)
		}
		if res.Personality.PrimerMD != "" {
			parts = append(parts, res.Personality.PrimerMD)
		}
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// jwtExpiryMonitor cancels ctx (via stop) shortly before the JWT
// expires so the supervisor's restart loop can re-validate. wsclient
// otherwise reconnects on every dial error including 401-after-stale-
// bearer, which would zombie the process indefinitely.
//
// `lead` is how far before expiry to fire. If we're already past
// (expiry - lead) at startup (e.g. supervisor handed us a near-
// expired JWT during a flap), cancel immediately so we restart fast.
func jwtExpiryMonitor(ctx context.Context, expiry time.Time, lead time.Duration, stop context.CancelFunc, log *slog.Logger) {
	wakeAt := expiry.Add(-lead)
	d := time.Until(wakeAt)
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}
	log.Info("agentfunnel: JWT nearing expiry — exiting for supervisor restart",
		"jwt_expires", expiry.Format(time.RFC3339),
		"lead", lead.String())
	stop()
}

// deliberateLoop drives funnel.Deliberate at a fixed cadence so any
// inbox items from chat.deliver get processed. Mirrors the rate from
// runtime/cmd/aspect (250ms — fast enough for mid-turn comms, slow
// enough not to busy-loop the LLM when idle).
func deliberateLoop(ctx context.Context, f *funnel.Funnel, log *slog.Logger) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := f.Deliberate(ctx, ""); err != nil &&
				!errors.Is(err, context.Canceled) &&
				!errors.Is(err, funnel.ErrEmptyInbox) {
				// ErrEmptyInbox is the normal idle case — the tick fires
				// 4x/sec regardless of inbox state. Logging it as WARN
				// floods the log and hides real errors. Surfaced on
				// 2026-05-11 when anvil came online and produced 4 WARN
				// lines per second.
				log.Warn("agentfunnel: deliberate", "err", err)
			}
		}
	}
}

// buildProvider maps the validation-response Provider string to a
// bridle backend. Unlike the per-home aspect.exe, agentfunnel does NOT
// fall back to a default on empty: the provider is Nexus-authoritative
// (set at `nexus aspect mint` time, NOT NULL on the aspects row), so
// an empty string here means the Nexus returned garbage or the row is
// corrupt — fail loudly rather than silently picking a default.
func buildProvider(provider, claudePath string) (bridle.Provider, error) {
	switch provider {
	case "":
		return nil, errors.New("buildProvider: validation response carried empty provider — Nexus DB row is corrupt; re-mint the aspect with --provider")
	case "claude-api", "claude":
		return claudeprovider.New(""), nil
	case "claude-code", "claudecode":
		p := claudecodeprovider.New()
		if resolved := resolveClaudePath(claudePath); resolved != "" {
			p.ClaudePath = resolved
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (claude-api and claude-code supported in v1)", provider)
	}
}

// resolveClaudePath picks the path to the claude-code CLI. Order:
//
//  1. -claude flag (explicit override).
//  2. CLAUDE_PATH env var (for systemd/launchctl units that can't pass
//     flags but can set env).
//  3. Common per-platform install locations checked in order. Catches
//     the case where agentfunnel is spawned with a stripped PATH
//     (launchctl on mac, sandboxed services on Windows) and can't see
//     /opt/homebrew/bin or %APPDATA%\npm even though claude is there.
//     Linux's package managers and npm installs already land in PATH
//     under typical service-account configs, but the fallbacks are
//     still listed so an unusual host doesn't break.
//  4. Empty result → caller leaves the provider's default ("claude")
//     and exec.LookPath handles it (Linux's normal path).
//
// Skips dangling symlinks / directories. Caller decides what empty
// means; on Linux PATH usually wins so this returns "" and exec
// does the right thing.
func resolveClaudePath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("CLAUDE_PATH"); env != "" {
		return env
	}
	for _, c := range claudeCandidates() {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// claudeCandidates returns the per-OS list of likely claude install
// locations. Each entry is checked in order; first hit wins. Windows
// candidates carry the .cmd / .exe shim extensions npm uses; macOS
// and Linux are plain "claude".
func claudeCandidates() []string {
	home, _ := os.UserHomeDir()
	switch goruntime.GOOS {
	case "darwin":
		paths := []string{
			"/opt/homebrew/bin/claude", // Apple Silicon homebrew
			"/usr/local/bin/claude",    // Intel homebrew + manual installs
		}
		if home != "" {
			paths = append(paths,
				filepath.Join(home, ".npm-global/bin/claude"),
				filepath.Join(home, ".bun/bin/claude"),
				filepath.Join(home, ".local/bin/claude"),
			)
		}
		return paths
	case "windows":
		// npm-global typically lives under %APPDATA%\npm; the shim is a
		// .cmd that node executes. claude.exe is the bundled standalone
		// build (rare but possible). Order: appdata first (most
		// installs), then localappdata variants.
		var paths []string
		if appData := os.Getenv("APPDATA"); appData != "" {
			paths = append(paths,
				filepath.Join(appData, "npm", "claude.cmd"),
				filepath.Join(appData, "npm", "claude.exe"),
			)
		}
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			paths = append(paths,
				filepath.Join(localAppData, "Programs", "claude", "claude.exe"),
				filepath.Join(localAppData, "Microsoft", "WindowsApps", "claude.exe"),
			)
		}
		if home != "" {
			paths = append(paths,
				// Native Windows install path used by the operator's
				// rebuild — the Windows `claude` standalone drops here.
				filepath.Join(home, ".local", "bin", "claude.exe"),
				filepath.Join(home, ".bun", "bin", "claude.exe"),
			)
		}
		return paths
	default: // linux, freebsd, etc
		paths := []string{
			"/usr/local/bin/claude",
			"/usr/bin/claude",
		}
		if home != "" {
			paths = append(paths,
				filepath.Join(home, ".npm-global/bin/claude"),
				filepath.Join(home, ".bun/bin/claude"),
				filepath.Join(home, ".local/bin/claude"),
			)
		}
		return paths
	}
}

// resolveCursorDir returns the dir for wsasp's Lock 6 cursor file.
// agentfunnel doesn't have an aspect home (deliberate — no on-disk
// state on the host); operator-supplied --cursor-dir or the working
// directory's "cursor" subdir.
func resolveCursorDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "cursor"
	}
	return cwd
}

// buildAgentFunnelRewriter wires the per-turn jsonl rewriter for
// aspects spawned via agentfunnel. Symmetric with the Frame-side
// wiring in cmd/nexus/main.go but simpler — no aspect.json on the
// host, so config defaults are inferred from the provider and
// thresholds use spec defaults.
//
// Rules:
//   - claude-code-flavored providers → enabled, claude-haiku-4-5 as
//     distiller (Frame's provider reused)
//   - direct-api providers (claude-api etc.) → no-op (no jsonl to
//     compress)
//   - any error during construction → no-op + WARN; never blocks
//     funnel startup
//
// The session path is resolved lazily so funnel session rotations
// land correctly without a config refresh.
func buildAgentFunnelRewriter(aspectName, providerName string, frameProvider bridle.Provider, frameModel string, sessionIDFn func() string, log *slog.Logger) funnel.PostTurnHook {
	if !isClaudeCodeProvider(providerName) {
		log.Info("agentfunnel: rewriter disabled (provider has no session jsonl)",
			"aspect", aspectName, "provider", providerName)
		return funnel.NoopPostTurn{}
	}

	// Distiller: reuse the frame provider with claude-haiku-4-5 as the
	// model. Operator override would land via a future rewriter
	// section in the validation response — out of scope for v1.
	haiku, err := rewriter.NewHaikuDistiller(bridle.NewHarness(frameProvider), bridle.ProviderID(providerName), "claude-haiku-4-5")
	if err != nil {
		log.Warn("agentfunnel: rewriter haiku construction failed; disabling rewriter",
			"aspect", aspectName, "err", err)
		return funnel.NoopPostTurn{}
	}
	haiku.AspectID = aspectName

	cwd, _ := os.Getwd()
	rw, err := rewriter.New(rewriter.Config{
		SessionPathFn: func() string {
			id := sessionIDFn()
			if id == "" {
				return ""
			}
			return rewriter.SessionPath(cwd, id)
		},
		Distiller: haiku,
		ModelName: "claude-haiku-4-5",
		Logger:    log,
	})
	if err != nil {
		log.Warn("agentfunnel: rewriter construction failed; disabling",
			"aspect", aspectName, "err", err)
		return funnel.NoopPostTurn{}
	}

	log.Info("agentfunnel: rewriter enabled",
		"aspect", aspectName, "provider", providerName,
		"distiller_model", "claude-haiku-4-5", "cwd", cwd)
	return rewriter.NewRunner(rw, log)
}

// isClaudeCodeProvider reports whether the provider name corresponds
// to claude-code (subprocess-stream — writes a session jsonl). Other
// providers don't, so the rewriter is moot for them.
func isClaudeCodeProvider(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-code", "claudecode":
		return true
	}
	return false
}

// toolsForProviderAgent mirrors cmd/nexus/main.go's toolsForProvider:
// claude-code subprocess owns its tool surface natively, so passing
// CommsToolDefs creates a phantom surface (model sees the SystemPrompt
// promise of send_chat etc. but cannot call them, AND can talk itself
// out of using legit native tools). Empty Tools for claude-code; full
// CommsToolDefs for direct-API providers. MCP is the proper fix (#181).
func toolsForProviderAgent(id bridle.ProviderID) []bridle.ToolDef {
	switch id {
	case "claude-code", "claudecode":
		return nil
	}
	return funnel.CommsToolDefs()
}

func fail(log *slog.Logger, msg string, err error) {
	if err != nil {
		log.Error(msg, "err", err)
	} else {
		log.Error(msg)
	}
	os.Exit(2)
}
