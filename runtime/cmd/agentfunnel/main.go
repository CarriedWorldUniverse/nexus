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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/rewriter"
	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/runtime/brokercreds"
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

	// 2.5 Materialise MCP profile (NEX-170). Must happen before
	// the claude-code subprocess spawns so .mcp.json is on disk
	// and auto-discovered from cwd. Atomic write — never leaves
	// a partial file. No-op when the profile is empty.
	cwd, _ := os.Getwd()
	if err := materialiseMCP(cwd, res.MCPProfile, log); err != nil {
		fail(log, "materialise .mcp.json", err)
	}

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

	// sessionState holds the current JWT + expiry. Refreshed in-band
	// by sessionRefreshLoop via session.refresh frames; consulted by
	// the TokenProvider on every WS dial and by jwtExpiryMonitor.
	state := newSessionState(sessionSnapshot{
		JWT:     res.SessionJWT,
		Expires: res.SessionExpiresAt,
	})

	// TokenProvider returns the cached JWT when it's still healthy.
	// Falls back to a keyfile re-validate when sessionState is empty
	// or the JWT is within 1 minute of expiry (post-restart cold start,
	// or the refresh loop has been failing long enough that the JWT
	// is about to die).
	tokenProvider := func(ctx context.Context) (string, error) {
		snap := state.Snapshot()
		if snap.JWT != "" && time.Until(snap.Expires) > 1*time.Minute {
			return snap.JWT, nil
		}
		client := keyfile.NewClient()
		fresh, ferr := client.Validate(ctx, kf)
		if ferr != nil {
			log.Warn("agentfunnel: TokenProvider re-validate failed, using cached token",
				"err", ferr)
			return "", ferr
		}
		state.Set(sessionSnapshot{JWT: fresh.SessionJWT, Expires: fresh.SessionExpiresAt})
		log.Info("agentfunnel: TokenProvider re-validated via keyfile",
			"expires", fresh.SessionExpiresAt.Format(time.RFC3339))
		return fresh.SessionJWT, nil
	}

	var bridge *wsasp.Bridge
	wsCfg := wsasp.Config{
		URL:           wsURL,
		AuthToken:     res.SessionJWT, // initial JWT; TokenProvider refreshes it
		TokenProvider: tokenProvider,
		AspectName:    res.AspectName,
		CursorFile:    cursorFile,
		OnDeliver: func(msg wsasp.DeliveredMessage) {
			if bridge != nil {
				bridge.OnDeliver(msg)
			}
		},
		Register: schemas.RegisterRequest{
			Name:           res.AspectName,
			ContextMode:    cm,
			Provider:       res.Provider,
			PID:            os.Getpid(),
			StartedAt:      time.Now().UTC(),
			Model:          res.Model,
			SessionID:      sessionID,
			PrimarySurface: schemas.SurfaceFunnel,
		},
	}
	wsClient, err := wsasp.NewClient(wsCfg)
	if err != nil {
		fail(log, "wsasp.NewClient", err)
	}

	gateway := wsasp.NewGateway(wsClient)
	// NEX-knowledge-fix (operator 2026-05-27): wire knowledge gateway
	// over WS so remote aspects (harrow, anvil, plumb) can use the
	// search_knowledge / store_knowledge tools. Pre-fix the Knowledge
	// field was nil → CommsRunner returned "knowledge gateway not
	// configured" on every call.
	commsRunner := funnel.CommsRunner{
		Gateway:   gateway,
		Knowledge: wsasp.NewKnowledgeGateway(wsClient),
		AspectID:  res.AspectName,
	}

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

	// NEX-293: fetch the per-aspect admin model_config overrides
	// before constructing the filter. agentfunnel runs out-of-process
	// so it doesn't have direct access to the broker's credentials
	// store — we read it over the same WS the rest of the funnel uses.
	//
	// Best-effort: if the fetch fails (broker DB hiccup, frame
	// rejected, etc.) we log and fall through with empty overrides.
	// Filter still works via shell-env passthrough (PR #146) or
	// subscription auth; we don't want a transient broker hiccup to
	// take the aspect down entirely.
	// NEX-293 + NEX-301: fetch judge AND compact admin overrides in one
	// WS round-trip. judge values flow into buildAgentFunnelFilter;
	// compact values flow into buildAgentFunnelRewriter below.
	overrides := fetchAspectModelOverrides(wsClient, res.AspectName, log)
	judgeModelOverride, judgeEnv := overrides.JudgeModel, overrides.JudgeEnv

	// NEX-302: read MainTurnSampling from the aspect's own aspect.json
	// on disk (autospawn convention: aspect.json lives at the aspect's
	// home dir, which is our cwd). Symmetric source-of-truth with the
	// in-process Frame's NEX-300 wiring (which also reads aspect.json).
	// Best-effort — missing file / malformed JSON / absent block all
	// return a zero MainTurnSampling, preserving provider defaults.
	mainTurnSampling := readMainTurnSamplingFromAspectJSON(cwd, log)

	// Build the output filter (cheap-judge by default). Mirrors the
	// Frame's buildOutputFilter but simpler: identity comes from Nexus
	// validation, not aspect.json on the host. The admin model_config
	// row (judge model + judge credential) IS available though, via
	// fetchAspectJudgeOverrides above — that's the NEX-293 parity work
	// that brings agentfunnels in line with the in-process Frame.
	//
	// Constructed AFTER obsHook so the CheapModelFilter can publish its
	// judge turn through the same observability stream as the main
	// turn — otherwise the judge runs invisibly and operators can't see
	// why a reply was suppressed.
	outputFilter := buildAgentFunnelFilter(provider, bridle.ProviderID(res.Provider), judgeModelOverride, judgeEnv, log, obsHook)

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
	}, overrides.CompactModel, overrides.CompactEnv, log)

	systemPrompt := composeSystemPrompt(res)
	f, err := funnel.New(funnel.Config{
		AspectID:     res.AspectName,
		Harness:      bridle.NewHarness(provider),
		Provider:     bridle.ProviderID(res.Provider),
		Model:        res.Model,
		SystemPrompt: systemPrompt,
		// MCP: non-nil enables MCP tool discovery via cmd.Dir/.mcp.json
		// for claude-code subprocess. .mcp.json is materialised from the
		// validate response by materialiseMCP (NEX-170) before funnel init.
		MCP: &bridle.MCPClientConfig{},
		// ContextMode (#226.5): funnel-driven aspects key per-thread
		// sessions on the chat thread root, so each chat thread keeps
		// its own claude-code jsonl. schemas.ContextMode and
		// funnel.ContextMode share their string values ("global" /
		// "thread" / "stateless"), so a direct cast carries the
		// --context-mode flag (default "thread") through without
		// translation. Today the validation response doesn't yet ship
		// ContextMode, so the flag is the source of truth; when it
		// does, prefer res.ContextMode over the flag (see flag help).
		ContextMode: funnel.ContextMode(cm),
		// Tools field is for direct-API providers; claude-code subprocess
		// owns its own tool surface natively. Mirrors cmd/nexus/main.go's
		// toolsForProvider — see #181 for the MCP fix.
		Tools:  toolsForProviderAgent(bridle.ProviderID(res.Provider)),
		Runner: funnel.ComposeRunner(commsRunner, &funnel.NullRunner{}),
		// ChatGateway routes the model's auto-post FinalText through the
		// same SendChat path CommsRunner uses for explicit send_chat tool
		// calls. Required for claude-code (subprocess mode): without it,
		// model output evaporates because the CLI has no MCP-loaded tools
		// to call. Mirrors cmd/nexus/main.go's Frame funnel wiring.
		ChatGateway:       gateway,
		StreamTextToChat:  true, // NEX-240: stream text blocks to chat as they arrive
		AspectHome:        cwd,  // NEX-241: stderr log + session isolation anchor
		// NEX-302: per-aspect main-turn sampling overrides from
		// aspect.json on the aspect's home dir. Empty / unset block
		// leaves funnel's pass-through with zero-valued
		// MainTurnSampling -> bridle TurnRequest fields stay unset
		// -> provider defaults preserved (back-compat).
		MainTurnSampling: mainTurnSampling,
		Filter:            outputFilter,
		PostTurn:          postTurn,
		ObservabilityHook: obsHook,
		// NEX-96: persist the seen-msg-id set alongside the wsasp cursor
		// so the idempotency guard survives agentfunnel restart. Same
		// dir resolution as the cursor file (--cursor-dir / cwd).
		IdempotencyFile: filepath.Join(resolveCursorDir(*cursorDir), "funnel-seen.json"),
		Logger:          log,
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

	// JWT pre-expiry monitor — safety net only.
	//
	// The TokenProvider handles JWT refresh on every reconnect, so
	// a stale bearer no longer causes permanent reconnect failure.
	// This monitor catches the edge case where the connection stays
	// up past JWT expiry (e.g. idle aspect with no disconnects):
	// the next reconnect would fail if the TokenProvider path is
	// unavailable (network down, broker unreachable). Exiting here
	// lets the supervisor restart us with a fresh handshake.
	//
	// 1-minute lead: generous enough not to hit "expired in flight"
	// and short enough that the primary TokenProvider path carries
	// all normal-ops reconnects.
	go jwtExpiryMonitor(ctx, func() time.Time { return state.Snapshot().Expires },
		1*time.Minute, stop, log)

	// In-protocol JWT refresh: 1h before expiry (±10% jitter), the
	// loop sends a session.refresh frame and on success rolls the new
	// JWT into sessionState. The safety-net jwtExpiryMonitor above
	// re-reads expiry after each sleep so a successful refresh pushes
	// its wakeup forward — only repeated refresh failure 1 minute
	// before expiry triggers a supervisor restart.
	go sessionRefreshLoop(ctx, state, wsClient, 1*time.Hour, log)

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
func jwtExpiryMonitor(ctx context.Context, expiryFn func() time.Time, lead time.Duration,
	stop context.CancelFunc, log *slog.Logger) {
	for {
		wakeAt := expiryFn().Add(-lead)
		d := time.Until(wakeAt)
		if d <= 0 {
			break
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
		// Re-check: did a successful refresh push expiry out? If yes,
		// sleep again against the new wake-at. If not (we genuinely
		// crossed the lead boundary), fall through to stop().
		if time.Until(expiryFn().Add(-lead)) > 0 {
			continue
		}
		break
	}
	log.Info("agentfunnel: JWT nearing expiry — exiting for supervisor restart",
		"jwt_expires", expiryFn().Format(time.RFC3339),
		"lead", lead.String())
	stop()
}

// deliberateLoop drives funnel.Deliberate at a fixed cadence so any
// inbox items from chat.deliver get processed. Mirrors the rate from
// runtime/cmd/aspect (250ms — fast enough for mid-turn comms, slow
// enough not to busy-loop the LLM when idle).
//
// Per #224, each Deliberate call handles ONE inbox message. When a
// burst arrives, drain the queue within a single tick — looping until
// ErrEmptyInbox — rather than waiting one tick per item (which would
// stretch a 5-msg burst to ~1.25s). The inner loop respects ctx
// cancellation between iterations.
func deliberateLoop(ctx context.Context, f *funnel.Funnel, log *slog.Logger) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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
					log.Warn("agentfunnel: deliberate", "err", err)
					break
				}
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
		p.DisallowedTools = funnel.DisallowedNativeTools
		return p, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (claude-api and claude-code supported in v1)", provider)
	}
}

// buildAgentFunnelFilter constructs the output filter for an agentfunnel
// aspect. Mirrors the Frame's buildOutputFilter but simpler — no
// aspect.json on the host means there's no operator-level filter
// override yet, so we hard-code the cheap-judge default:
//
//   - claude-flavoured provider → HardRulesFilter wrapping
//     CheapModelFilter using the same provider + bare "haiku" model.
//     Bare "haiku" matches the Frame's choice (cmd/nexus/main.go):
//     under claude-code (subprocess CLI), the versioned api-style id
//     "claude-haiku-4-5" makes the CLI run as a full agent instead of
//     a classifier, so we use the CLI's own default haiku tier.
//   - non-claude provider → HardRulesFilter only (no usable cheap-tier
//     judge yet; ollama/openai support comes when the operator gains a
//     filter override path).
//
// judgeModelOverride / providerEnv come from NEX-293 — the per-aspect
// admin model_config row + the resolved FilterCredential's env
// overlay. Empty model = use "haiku" default; nil env = inherit
// ambient process env (legacy / no override configured). Both are
// resolved upstream from the broker over WS.
//
// Without this every reply through the funnel hits chat unfiltered —
// observed 2026-05-12 as noisy multi-aspect threads bypassing the
// suppression the keel Frame applies.
func buildAgentFunnelFilter(provider bridle.Provider, providerID bridle.ProviderID, judgeModelOverride string, providerEnv map[string]string, log *slog.Logger, obsHook funnel.ObservabilityHook) funnel.OutputFilter {
	if !isClaudeFlavor(providerID) {
		log.Info("agentfunnel: filter=hard (no cheap-judge for non-claude provider)", "provider", providerID)
		return funnel.HardRulesFilter{}
	}
	model := judgeModelOverride
	if model == "" {
		model = "haiku"
	}
	log.Info("agentfunnel: filter=cheap (hard rules + cheap-model judge)",
		"judge_provider", providerID,
		"judge_model", model,
		"judge_env_keys", envKeyNames(providerEnv))
	return funnel.HardRulesFilter{
		Inner: &funnel.CheapModelFilter{
			Harness:           bridle.NewHarness(bareJudgeProvider(provider, providerID)),
			Provider:          providerID,
			Model:             model,
			Logger:            log,
			ObservabilityHook: obsHook,
			ProviderEnv:       providerEnv,
		},
	}
}

// aspectOverrides bundles the resolved admin-override knobs for an
// agentfunnel-spawned aspect. Each pair (model, env) is independent
// — judge and compact have separate credential lookups so they can
// route to different providers (e.g. judge → DeepSeek bare, compact
// → claude-haiku). Empty values mean "no override; caller's legacy
// fallback applies".
type aspectOverrides struct {
	JudgeModel   string
	JudgeEnv     map[string]string
	CompactModel string
	CompactEnv   map[string]string
}

// fetchAspectModelOverrides retrieves the judge + compact model +
// credential overrides (NEX-263 + NEX-294 effective values) for this
// aspect from the broker over WS, and resolves each credential into
// an env overlay. NEX-301 extends NEX-293's judge-only path to also
// cover compact, so out-of-process aspects pick up the same admin
// + network-default plane the in-process Frame uses via
// applyAspectModelOverrides.
//
// Best-effort: every failure path logs and returns the partial
// result. A failed credential fetch leaves that side's env nil
// (subprocess inherits ambient process env); a failed model_config
// fetch returns zero values across the board so the funnel falls
// through to legacy hardcoded defaults rather than refusing to start.
func fetchAspectModelOverrides(ws brokercreds.Requester, aspectName string, log *slog.Logger) aspectOverrides {
	out := aspectOverrides{}
	if ws == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := brokercreds.FetchAspectModelConfig(ctx, ws)
	if err != nil {
		log.Warn("agentfunnel: fetch aspect model config failed; falling back to defaults",
			"aspect", aspectName, "err", err)
		return out
	}

	// Judge side (NEX-293).
	if cfg.JudgeModel != "" {
		log.Info("agentfunnel: admin override applied",
			"aspect", aspectName, "kind", "judge_model", "to", cfg.JudgeModel)
		out.JudgeModel = cfg.JudgeModel
	}
	out.JudgeEnv = resolveCredentialEnv(ws, aspectName, "judge", cfg.JudgeCredential, log)

	// Compact side (NEX-301). Symmetric with judge.
	if cfg.CompactModel != "" {
		log.Info("agentfunnel: admin override applied",
			"aspect", aspectName, "kind", "compact_model", "to", cfg.CompactModel)
		out.CompactModel = cfg.CompactModel
	}
	out.CompactEnv = resolveCredentialEnv(ws, aspectName, "compact", cfg.CompactCredential, log)

	return out
}

// resolveCredentialEnv fetches a named credential's provider bundle
// over WS and translates it to an env overlay (ANTHROPIC_API_KEY +
// optional ANTHROPIC_BASE_URL for Anthropic-shape; OPENAI_* for
// OpenAI-shape). Returns nil on any failure path or when the
// credential name is empty — caller falls back to ambient env.
//
// kindTag is just for log lines ("judge" / "compact") so operators
// can grep for the specific side when debugging.
func resolveCredentialEnv(ws brokercreds.Requester, aspectName, kindTag, credentialName string, log *slog.Logger) map[string]string {
	if credentialName == "" {
		return nil
	}
	credCtx, credCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer credCancel()
	resolvedName, bundle, err := brokercreds.FetchProvider(credCtx, ws, credentialName)
	if err != nil {
		log.Warn("agentfunnel: fetch credential failed; inherits ambient env",
			"aspect", aspectName, "kind", kindTag,
			"credential", credentialName, "err", err)
		return nil
	}
	env := providerBundleToEnv(bundle)
	if env == nil {
		log.Warn("agentfunnel: credential has no env mapping for api_shape; inherits ambient env",
			"aspect", aspectName, "kind", kindTag,
			"credential", resolvedName, "api_shape", bundle.APIShape)
		return nil
	}
	log.Info("agentfunnel: credential resolved",
		"aspect", aspectName, "kind", kindTag,
		"credential", resolvedName, "api_shape", bundle.APIShape,
		"env_keys", envKeyNames(env))
	return env
}


// readMainTurnSamplingFromAspectJSON loads the aspect's aspect.json
// from `aspectHome/aspect.json`, extracts the main_turn_sampling
// block, and translates it to funnel.MainTurnSampling. Best-effort:
// missing file, malformed JSON, or absent block all return a zero
// MainTurnSampling so the funnel falls through to provider defaults
// — same back-compat semantics as the in-process Frame's NEX-300
// wiring when no main_turn_sampling block is present.
//
// NEX-302: provides parity with the Frame's main-turn sampling
// surface for autospawn'd aspects. Operator edits aspect.json on
// the aspect's home dir + restarts the aspect → sampling applies.
// Identical workflow to the Frame's case (cmd/nexus/main.go); the
// aspect home is just a different directory.
//
// Future admin-managed sampling (broker-side overrides analogous to
// NEX-263's model + credential) would extend the WS frame
// AspectModelConfigGetResultPayload + this helper merges/layers
// admin > aspect.json. Not in scope today — the broker has no
// source for sampling values.
func readMainTurnSamplingFromAspectJSON(aspectHome string, log *slog.Logger) funnel.MainTurnSampling {
	if aspectHome == "" {
		return funnel.MainTurnSampling{}
	}
	jsonPath := filepath.Join(aspectHome, "aspect.json")
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		// Aspect.json absent is the normal case for many autospawn
		// setups — only operators wanting sampling overrides put one
		// there. Debug-level so it's quiet in steady state.
		log.Debug("agentfunnel: no aspect.json found for main-turn sampling; using provider defaults",
			"path", jsonPath)
		return funnel.MainTurnSampling{}
	}
	var cfg schemas.AspectConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Warn("agentfunnel: aspect.json malformed; main-turn sampling not applied",
			"path", jsonPath, "err", err)
		return funnel.MainTurnSampling{}
	}
	if cfg.MainTurnSampling == nil {
		return funnel.MainTurnSampling{}
	}
	out := funnel.MainTurnSampling{
		Temperature:     cfg.MainTurnSampling.Temperature,
		TopP:            cfg.MainTurnSampling.TopP,
		TopK:            cfg.MainTurnSampling.TopK,
		Seed:            cfg.MainTurnSampling.Seed,
		MaxOutputTokens: cfg.MainTurnSampling.MaxOutputTokens,
		StopSequences:   cfg.MainTurnSampling.StopSequences,
	}
	log.Info("agentfunnel: main-turn sampling loaded from aspect.json",
		"path", jsonPath,
		"temperature_set", out.Temperature != nil,
		"top_p_set", out.TopP != nil,
		"top_k_set", out.TopK != nil,
		"seed_set", out.Seed != nil,
		"max_output_tokens", out.MaxOutputTokens,
		"stop_sequences_count", len(out.StopSequences))
	return out
}

// providerBundleToEnv mirrors credentials.Store.EnvForCredential's
// shape mapping. Anthropic-shape emits ANTHROPIC_API_KEY +
// optional ANTHROPIC_BASE_URL; OpenAI-shape emits the OPENAI_*
// equivalents. Returns nil for unknown shapes so the caller can fall
// back to ambient env.
func providerBundleToEnv(b brokercreds.ProviderBundle) map[string]string {
	switch b.APIShape {
	case "anthropic":
		env := map[string]string{"ANTHROPIC_API_KEY": b.Key}
		if b.BaseURL != "" {
			env["ANTHROPIC_BASE_URL"] = b.BaseURL
		}
		return env
	case "openai":
		env := map[string]string{"OPENAI_API_KEY": b.Key}
		if b.BaseURL != "" {
			env["OPENAI_BASE_URL"] = b.BaseURL
		}
		return env
	default:
		return nil
	}
}

// envKeyNames returns the keys of env (sorted) for log output. Just
// names, never values — values are credential material.
func envKeyNames(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bareJudgeProvider mirrors cmd/nexus/main.go: when the judge runs
// under claude-code, the original intent (#196) was a fresh Provider
// with Bare=true for a lean CLI surface. Disabled 2026-05-13 same as
// the in-process Frame: --bare is API-key-only mode (disables
// subscription auth entirely), and aspects running this binary run on
// subscription, so the bare subprocess had no auth path and returned
// "Not logged in" as every verdict. See main.go bareJudgeProvider for
// the full incident write-up. Re-enable post-#222 once the credentials
// store can hand an API key to the bare subprocess.
//
// Until then: judge inherits the deliberation provider's surface.
// Contamination risk #196 was meant to fix is mitigated by #195's
// prompt hardening + #212's verdict format.
func bareJudgeProvider(p bridle.Provider, id bridle.ProviderID) bridle.Provider {
	// NEX-103 Phase 1a parity with cmd/nexus/main.go: bare branch
	// re-enabled. Caller (Frame buildOutputFilter) supplies the
	// ANTHROPIC_API_KEY via filter credential lookup; this side
	// (agentfunnel) doesn't yet have brokercreds wired in — still
	// returns p unchanged for non-claude-code providers and skips
	// bare unless the credential plumbing lands first.
	switch id {
	case "claude-code", "claudecode":
		jp := claudecodeprovider.New()
		jp.Bare = true
		return jp
	}
	return p
}

// isClaudeFlavor reports whether providerID is one of the Claude
// providers. Mirrors the Frame's helper in cmd/nexus/main.go — accepts
// the canonical IDs ("claude-api", "claude-code") and the validation-
// response aliases ("claude", "claudecode") so callers don't have to
// normalise.
func isClaudeFlavor(id bridle.ProviderID) bool {
	switch id {
	case "claude-api", "claude-code", "claude", "claudecode":
		return true
	}
	return false
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
// wiring in cmd/nexus/main.go.
//
// NEX-301: now consumes admin-managed compact model + credential
// overrides (NEX-263) layered under network defaults (NEX-294),
// fetched over WS at startup via fetchAspectModelOverrides. Without
// this wiring the distiller hardcoded "claude-haiku-4-5" on the
// aspect's main provider regardless of operator config — parity gap
// with the in-process Frame.
//
// Rules:
//   - claude-code-flavored providers → enabled, distiller model =
//     compactModelOverride or "claude-haiku-4-5" fallback
//   - direct-api providers (claude-api etc.) → no-op (no jsonl to
//     compress)
//   - any error during construction → no-op + WARN; never blocks
//     funnel startup
//
// compactEnv (when non-nil) is the resolved ProviderEnv overlay for
// the compact credential. The distiller's claudecode subprocess
// inherits these env vars, routing the compact-tier call to whatever
// auth domain the credential points at (DeepSeek, separate Anthropic
// account, etc.) — same pattern as the judge's ProviderEnv.
//
// The session path is resolved lazily so funnel session rotations
// land correctly without a config refresh.
func buildAgentFunnelRewriter(aspectName, providerName string, frameProvider bridle.Provider, frameModel string, sessionIDFn func() string, compactModelOverride string, compactEnv map[string]string, log *slog.Logger) funnel.PostTurnHook {
	if !isClaudeCodeProvider(providerName) {
		log.Info("agentfunnel: rewriter disabled (provider has no session jsonl)",
			"aspect", aspectName, "provider", providerName)
		return funnel.NoopPostTurn{}
	}

	// Pick the effective distiller model: operator override > legacy
	// hardcoded haiku fallback.
	const defaultDistillerModel = "claude-haiku-4-5"
	model := compactModelOverride
	if model == "" {
		model = defaultDistillerModel
	}

	// Distiller harness reuses the aspect's main provider — claudecode
	// subprocess inherits env via per-TurnRequest ProviderEnv, so a
	// separate provider instance per credential isn't needed (the
	// distinct env routes the subprocess to the operator-chosen auth
	// domain). Same architecture as CheapModelFilter's ProviderEnv
	// pattern.
	haiku, err := rewriter.NewHaikuDistiller(bridle.NewHarness(frameProvider), bridle.ProviderID(providerName), model)
	if err != nil {
		log.Warn("agentfunnel: rewriter haiku construction failed; disabling rewriter",
			"aspect", aspectName, "err", err)
		return funnel.NoopPostTurn{}
	}
	haiku.AspectID = aspectName
	haiku.ProviderEnv = compactEnv

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
		ModelName: model,
		Logger:    log,
	})
	if err != nil {
		log.Warn("agentfunnel: rewriter construction failed; disabling",
			"aspect", aspectName, "err", err)
		return funnel.NoopPostTurn{}
	}

	log.Info("agentfunnel: rewriter enabled",
		"aspect", aspectName, "provider", providerName,
		"distiller_model", model,
		"distiller_env_keys", envKeyNames(compactEnv),
		"cwd", cwd)
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
