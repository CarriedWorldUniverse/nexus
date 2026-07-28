// Command aspect is the out-of-process aspect runtime binary (F2.5b).
// One process per aspect; dials Nexus over WS, registers, and runs
// the funnel-driven main loop until shutdown.
//
// Usage:
//
//	aspect -home <aspect-home-folder>
//
// Environment:
//   - NEXUS_TOKEN  — bearer token presented at WS handshake (or the
//     env var named in aspect.json's auth_token_env)
//   - NEXUS_URL    — Nexus WS endpoint, e.g.
//     wss://agentnetwork.<tailnet>.ts.net:7888/connect
//
// Composition:
//
//	cfg := aspect.json
//	provider := claude-api adapter (per cfg.Provider)
//	bridle.Harness(provider) → drives single turns
//	wsasp.Client → owns the WS, cursor persistence, OnDeliver fan-out
//	wsasp.Bridge → translates DeliveredMessage → bridle.InboxItem
//	wsasp.Gateway → ChatGateway impl backed by wsasp.Client
//	funnel.New(...) → deliberation loop using Bridge as inbox source
//	                  and Gateway via funnel.CommsRunner as ToolRunner
//
// The aspect host (wsasp.Client) hides connection state from the
// model — the funnel sees a steady inbox and a working ChatGateway
// even across reconnects (Lock 6).
//
// Differences from runtime/cmd/agent (the pre-funnel scaffold):
//   - Uses wsasp instead of runtime/agent (which was pre-Lock-1)
//   - Wires the funnel deliberation loop instead of provider-direct
//     turn handling
//   - Lock 6 cursor persistence under <home>/cursor (auto-managed by
//     wsasp) — no aspect-side knowledge of replay required

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	codexcliprovider "github.com/CarriedWorldUniverse/bridle/provider/codexcli"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/runtime/ctxmapwire"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/google/uuid"
)

func main() {
	home := flag.String("home", "", "aspect home folder (must contain aspect.json)")
	tokenEnv := flag.String("token-env", "NEXUS_TOKEN", "env var holding the bearer token")
	urlFlag := flag.String("url", "", "Nexus WS endpoint (overrides NEXUS_URL env var)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *home == "" {
		fail(log, "missing -home flag", nil)
	}
	absHome, err := filepath.Abs(*home)
	if err != nil {
		fail(log, "resolve home", err)
	}

	cfg, err := loadAspectConfig(absHome)
	if err != nil {
		fail(log, "load aspect.json", err)
	}

	if cfg.EffectiveRole() == schemas.RoleFrame {
		fail(log, "this binary runs aspects, not frames; Frame is embedded in nexus", nil)
	}

	// aspect.json's auth_token_env / nexus_url_env are honored as
	// fallbacks when the operator didn't override via flag/env. Flag >
	// env > aspect.json. The defaults documented in the package docblock
	// (NEXUS_TOKEN / NEXUS_URL) only apply when aspect.json doesn't
	// specify its own env names.
	tokenEnvName := *tokenEnv
	if tokenEnvName == "NEXUS_TOKEN" && cfg.AuthTokenEnv != "" {
		tokenEnvName = cfg.AuthTokenEnv
	}
	token := os.Getenv(tokenEnvName)
	if token == "" {
		fail(log, fmt.Sprintf("missing auth token (env %s)", tokenEnvName), nil)
	}

	url := *urlFlag
	if url == "" {
		url = os.Getenv("NEXUS_URL")
	}
	if url == "" && cfg.NexusURLEnv != "" {
		url = os.Getenv(cfg.NexusURLEnv)
	}
	if url == "" {
		fail(log, "missing Nexus URL — set NEXUS_URL, aspect.json's nexus_url_env, or pass -url", nil)
	}

	provider, err := buildProvider(cfg)
	if err != nil {
		fail(log, "build provider", err)
	}

	model := pickModel(cfg)
	if model == "" {
		fail(log, "aspect.json: provider_config must specify a model", nil)
	}

	sessionID := uuid.NewString()

	// wsasp client: the WS host. Bridge is wired below once the funnel
	// exists (chicken-and-egg: the bridge needs the funnel; the funnel
	// uses the gateway; the gateway uses the client).
	var bridge *wsasp.Bridge
	wsCfg := wsasp.Config{
		URL:        url,
		AuthToken:  token,
		AspectName: cfg.Name,
		CursorFile: wsasp.CursorFileForAspect(absHome),
		OnDeliver: func(msg wsasp.DeliveredMessage) {
			// bridge is set after we construct the funnel; calls
			// before that are dropped (no inbox to land in yet).
			// In practice OnDeliver fires only after Run is called,
			// which we do AFTER the funnel + bridge are wired.
			if bridge != nil {
				bridge.OnDeliver(msg)
			}
		},
		Register: schemas.RegisterRequest{
			Name:         cfg.Name,
			ContextMode:  cfg.ContextMode,
			Provider:     cfg.Provider,
			Port:         cfg.Port,
			PID:          os.Getpid(),
			StartedAt:    time.Now().UTC(),
			Model:        model,
			Capabilities: cfg.Capabilities,
			Home:         absHome,
			SessionID:    sessionID,
			Metadata:     cfg.Metadata,
		},
	}
	wsClient, err := wsasp.NewClient(wsCfg)
	if err != nil {
		fail(log, "wsasp.NewClient", err)
	}

	// Compose funnel. Recipe mirrors the embedded Frame's funnel
	// construction in nexus/cmd/nexus/main.go (frameFunnelCallbacks),
	// substituting wsasp.Gateway for framecomms.Gateway and
	// wsasp.Bridge for the in-process Receive call.
	gateway := wsasp.NewGateway(wsClient)
	// Wire WS knowledge gateway so search_knowledge / store_knowledge
	// tool calls reach the broker. Same fix as agentfunnel — without
	// this, runStoreKnowledge / runSearchKnowledge return
	// "knowledge gateway not configured".
	commsRunner := funnel.CommsRunner{
		Gateway:   gateway,
		Knowledge: wsasp.NewKnowledgeGateway(wsClient),
		AspectID:  cfg.Name,
	}

	// The aspect runs the whole process against one long-lived harness.
	// Optionally attach ctxmap working memory (off unless CTXMAP_ENABLED and the
	// ctxmap_llama build tag; fail-open). Closed at process shutdown.
	harness := bridle.NewHarness(provider)
	ctxmapHandle := ctxmapwire.Build(ctxmapwire.Resolve(absHome, cfg.Name), log)
	ctxmapHandle.AttachTo(harness)
	defer ctxmapHandle.Close()

	f, err := funnel.New(funnel.Config{
		AspectID:   cfg.Name,
		AspectHome: absHome,
		Harness:    harness,
		Provider:   bridle.ProviderID(cfg.Provider),
		Model:      model,
		// ContextMode (#226.5): sourced from aspect.json. Values match
		// the funnel package's ContextMode constants 1:1, so a direct
		// cast carries it through.
		ContextMode: funnel.ContextMode(cfg.ContextMode),
		// MCP: non-nil enables MCP tool discovery via cmd.Dir/.mcp.json
		// for claude-code subprocess. Marker-only -- actual MCP loading
		// is via the subprocess's own .mcp.json discovery from AspectHome.
		MCP: &bridle.MCPClientConfig{},
		// Tools: bridle-side tool defs are for direct-API providers
		// where bridle routes tool_use through ToolRunner. For
		// claude-code (subprocess-stream), the CLI owns its tool
		// surface natively — passing CommsToolDefs() creates phantom
		// tool names in --allowedTools that block MCP-discovered tools.
		Tools:  toolsForProvider(bridle.ProviderID(cfg.Provider)),
		Runner: funnel.ComposeRunner(commsRunner, &funnel.NullRunner{}),
		// NEX-96: persist the seen-msg-id set under aspect home so the
		// idempotency guard survives aspect restart. Sits beside the
		// wsasp cursor file under <home>/.
		IdempotencyFile: filepath.Join(absHome, "funnel-seen.json"),
		Logger:          log,
	})
	if err != nil {
		fail(log, "funnel.New", err)
	}
	// bridge MUST be set before wsClient.Run starts. The OnDeliver
	// closure captures `bridge` by reference and nil-checks each call;
	// the WS goroutines that fire OnDeliver only spawn inside Run.
	bridge = wsasp.NewBridge(f)

	log.Info("aspect runtime starting",
		"aspect", cfg.Name,
		"role", cfg.EffectiveRole(),
		"provider", cfg.Provider,
		"model", model,
		"home", absHome,
		"nexus_url", url,
		"session", sessionID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Deliberation loop: every chat.deliver arrives via the bridge,
	// which calls funnel.ReceiveWithMsgID. We drive Deliberate on a
	// short tick so accumulated inbox items get processed; the funnel
	// itself is a no-op when the inbox is empty. The wsasp.Client.Run
	// blocks the main goroutine for the WS lifecycle.
	go deliberateLoop(ctx, f, log)

	if err := wsClient.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("wsasp.Run", "err", err)
		os.Exit(1)
	}
	log.Info("aspect stopped")
}

// deliberateLoop drives funnel.Deliberate on a periodic tick. The
// funnel is no-op when the inbox is empty, so the tick rate is the
// "aspect-thinks-about-things" interval — fast enough to feel
// responsive, slow enough not to busy-loop the LLM.
//
// 250ms keeps mid-turn comms responsive (the funnel checks the inbox
// at deliberation start; mid-turn additions land on the next tick).
// Per-turn cost is dominated by provider latency, not the tick rate.
func deliberateLoop(ctx context.Context, f *funnel.Funnel, log *slog.Logger) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Per #224, each Deliberate handles ONE message. Drain a
			// burst within a single tick by looping until ErrEmptyInbox.
			for {
				if ctx.Err() != nil {
					return
				}
				_, err := f.Deliberate(ctx, "")
				if errors.Is(err, funnel.ErrEmptyInbox) {
					break
				}
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					log.Warn("deliberate", "err", err)
					break
				}
			}
		}
	}
}

// loadAspectConfig parses aspect.json in the home folder.
func loadAspectConfig(home string) (schemas.AspectConfig, error) {
	path := filepath.Join(home, "aspect.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return schemas.AspectConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg schemas.AspectConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return schemas.AspectConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		return schemas.AspectConfig{}, errors.New("aspect.json: missing name")
	}
	if cfg.EffectiveRole() != schemas.RoleAspect && cfg.EffectiveRole() != schemas.RoleFrame {
		// RoleOperator is a recognized runtime identity (login flow)
		// but is intentionally not loadable from disk — the operator
		// boundary is mint-at-login-only. Distinguish that case so the
		// operator doesn't go hunting for a typo.
		if cfg.EffectiveRole() == schemas.RoleOperator {
			return schemas.AspectConfig{}, fmt.Errorf("aspect.json: role %q is runtime-only (operators are minted at login, not loaded from disk)", cfg.Role)
		}
		return schemas.AspectConfig{}, fmt.Errorf("aspect.json: unknown role %q", cfg.Role)
	}
	// Pre-flight context_mode check: the broker rejects empty / unknown
	// values with a register error that the aspect doesn't surface in
	// v1 (sendRegister fire-and-forgets). Catch the misconfiguration
	// here so startup fails loudly with a clear message instead of the
	// aspect connecting and sitting silent.
	switch cfg.ContextMode {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		return schemas.AspectConfig{}, fmt.Errorf("aspect.json: context_mode must be global/thread/stateless, got %q", cfg.ContextMode)
	}
	return cfg, nil
}

// buildProvider constructs the bridle.Provider named in aspect.json.
// Mirrors the embedded Frame's path in nexus/cmd/nexus/main.go so the
// in-process Frame and out-of-process aspects share the same provider
// surface.
//
// `claude-api`/`claude` use the bridle Anthropic SDK adapter (needs
// ANTHROPIC_API_KEY env). `claude-code`/`claudecode` shells out to the
// `claude` CLI (uses the operator's local Claude Code installation +
// subscription auth, no API key required).
func buildProvider(cfg schemas.AspectConfig) (bridle.Provider, error) {
	switch cfg.Provider {
	case "claude-api", "claude":
		return claudeprovider.New(""), nil
	case "claude-code", "claudecode", "":
		// Default to claudecode when unset — operator's running on
		// subscription, not API key, so this is the safe default for
		// the rebuild deploy.
		return claudecodeprovider.New(), nil
	case "codex-cli", "codex", "codexcli":
		return codexcliprovider.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (claude-api, claude-code, codex-cli supported in v1)", cfg.Provider)
	}
}

// pickModel returns the model string for the funnel. Looks at
// aspect.json's provider_config.model first; falls back to a
// per-provider default if absent.
func pickModel(cfg schemas.AspectConfig) string {
	if cfg.ProviderConfig != nil {
		if m, ok := cfg.ProviderConfig["model"].(string); ok && m != "" {
			return m
		}
	}
	// Defaults by provider — the funnel needs a model name regardless,
	// and the providers package wraps "" with its own default.
	switch cfg.Provider {
	case "claude-api", "claude", "claude-code", "claudecode", "":
		return "claude-sonnet-4-6"
	default:
		return ""
	}
}

// toolsForProvider returns the bridle-side tool defs for the funnel.
// Mirrors the broker's toolsForProvider in nexus/cmd/nexus/main.go:
// nil for claude-code (subprocess owns its tool surface), CommsToolDefs
// for direct-API providers (bridle routes tool_use through ToolRunner).
func toolsForProvider(id bridle.ProviderID) []bridle.ToolDef {
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
