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
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
	antigravitycliprovider "github.com/CarriedWorldUniverse/bridle/provider/antigravitycli"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	codexcliprovider "github.com/CarriedWorldUniverse/bridle/provider/codexcli"
	ollamaprovider "github.com/CarriedWorldUniverse/bridle/provider/ollama"
	openaiprovider "github.com/CarriedWorldUniverse/bridle/provider/openai"
	toolrunner "github.com/CarriedWorldUniverse/bridle/toolrunner"
	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/judge"
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
	policyPath := flag.String("policy", "", "path to a per-aspect tool permission policy JSON file (optional; empty = permissive default_allow). See funnel.ToolPolicy for the JSON shape.")
	autoRecall := flag.Bool("auto-recall", false, "enable turn-time recall from the Commonplace (search the cross-session knowledge store with each incoming message and inject the strongest matches into the system prompt). Off by default.")
	autoRecallTopK := flag.Int("auto-recall-topk", 0, "auto-recall: max strongest matches to inject (0 = funnel default)")
	autoRecallMaxRank := flag.Float64("auto-recall-max-rank", 0, "auto-recall: BM25 relevance gate; only hits with score < this inject (ranks are negative, lower = stronger; 0 = no gate)")
	builderMode := flag.Bool("builder", false, "builder/one-shot mode: drain the dispatched brief, run to the task_done signal, then exit")
	builderTimeout := flag.Duration("builder-timeout", 30*time.Minute, "max wall-clock for a builder run before forced exit")
	builderMaxTurns := flag.Int("builder-max-turns", 20, "builder mode: max goal-pursuit turns before the ralph-loop gives up (NEX-477); -builder-timeout is the outer wall-clock backstop")
	briefFile := flag.String("brief-file", "", "builder mode: read the seed brief from this file instead of the broker inbox")
	replyTopic := flag.String("reply-topic", "", "builder mode: attach this topic to natural reply posts")
	repoFlag := flag.String("repo", "", "builder mode: dispatched repo (owner/name) for PR-existence verification")
	ticketFlag := flag.String("ticket", "", "builder mode: dispatched ticket, for the builder/<ticket> branch")
	branchFlag := flag.String("branch", "", "builder mode: dispatched branch (defaults to builder/<ticket>)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Boot credential: a normal aspect presents a keyfile (-k); a
	// spawned hand (NEX-571 Task D) presents a broker-minted session JWT
	// in CW_SESSION_JWT and NO keyfile — its Job ships no keyfile Secret,
	// so it boots through the JWT-resolve path instead of the keyfile
	// validate handshake. -k wins if both are somehow present (a keyfile
	// is the stronger credential).
	sessionJWTEnv := os.Getenv("CW_SESSION_JWT")
	handBoot := *keyfilePath == "" && sessionJWTEnv != ""
	if *keyfilePath == "" && !handBoot {
		fail(log, "missing credential: pass -k <keyfile> or set CW_SESSION_JWT (hand boot)", nil)
	}
	cm := schemas.ContextMode(*contextMode)
	switch cm {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		fail(log, fmt.Sprintf("invalid --context-mode %q (want global/thread/stateless)", *contextMode), nil)
	}

	var res *keyfile.ValidationResult
	var brokerTLS *tls.Config
	// kf is the loaded keyfile for the normal aspect path (used by the
	// TokenProvider re-validate loop). nil on the hand JWT-boot path —
	// hands re-resolve via the JWT instead.
	var kf *keyfile.Keyfile

	if handBoot {
		// JWT-boot (hand): resolve persona/provider/config from the
		// broker keyed on the JWT's verified sub. The broker is reached
		// via CW_SEAM_URL (the Job env); the WS dial URL is derived from
		// it. CW_SEAM_CA optionally pins a self-signed broker cert (nil =
		// system trust, the in-cluster CA-signed default).
		seamURL := os.Getenv("CW_SEAM_URL")
		if seamURL == "" {
			fail(log, "hand boot: CW_SESSION_JWT set but CW_SEAM_URL missing", nil)
		}
		wsURL := seamHTTPToWS(seamURL)
		var terr error
		brokerTLS, terr = keyfile.BrokerTLSConfigFromCAFile(os.Getenv("CW_SEAM_CA"))
		if terr != nil {
			fail(log, "build broker TLS config from CW_SEAM_CA", terr)
		}
		if brokerTLS != nil {
			log.Info("agentfunnel: trusting pinned broker cert from CW_SEAM_CA (hand boot)")
		}
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
		client := &keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(brokerTLS)}
		// NexusID is informational on this path (identity already proven
		// by the JWT signature); fetch it best-effort for logging/WS dial
		// parity, but a miss is non-fatal.
		nexusID := os.Getenv("CW_NEXUS_ID")
		res, terr = client.ResolveByJWT(bootCtx, wsURL, nexusID, sessionJWTEnv)
		bootCancel()
		if terr != nil {
			switch {
			case errors.Is(terr, keyfile.ErrValidationRejected):
				log.Error("agentfunnel: broker rejected hand JWT resolve",
					"hint", "JWT expired/invalid, or the parent aspect is unknown/retired", "err", terr)
			case errors.Is(terr, keyfile.ErrBadServerResponse):
				log.Error("agentfunnel: broker returned malformed resolve response", "err", terr)
			default:
				log.Error("agentfunnel: hand JWT boot failed", "err", terr)
			}
			os.Exit(1)
		}
		log.Info("agentfunnel: hand booted from JWT",
			"aspect", res.AspectName,
			"parent", os.Getenv("CW_SPAWN_PARENT"),
			"provider", res.Provider)
	} else {
		// 1. Read keyfile.
		loaded, err := keyfile.Load(*keyfilePath)
		if err != nil {
			fail(log, "load keyfile", err)
		}
		kf = loaded
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
		// NEX-367: if the keyfile pins a self-signed broker cert, build a
		// TLS config that trusts it (system roots + pinned cert) and use it
		// for BOTH the validate handshake here AND the WS dial below. Nil =
		// default system trust store (CA-signed certs just work).
		var terr error
		brokerTLS, terr = kf.BrokerTLSConfig()
		if terr != nil {
			fail(log, "build broker TLS config from keyfile", terr)
		}
		if brokerTLS != nil {
			log.Info("agentfunnel: trusting pinned broker cert from keyfile (self-signed-capable)")
		}
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
		client := &keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(brokerTLS)}
		res, terr = client.Validate(bootCtx, kf)
		bootCancel()
		if terr != nil {
			// Render the most actionable hint we can per the sentinel.
			switch {
			case errors.Is(terr, keyfile.ErrNexusMismatch):
				log.Error("agentfunnel: keyfile envelope nexus_id does not match the server",
					"hint", "the keyfile may be stale (Nexus identity regenerated) or pointed at the wrong host",
					"err", terr)
			case errors.Is(terr, keyfile.ErrValidationRejected):
				log.Error("agentfunnel: server rejected validation",
					"hint", "check server response — likely revoked, retired, or unknown aspect",
					"err", terr)
			case errors.Is(terr, keyfile.ErrBadServerResponse):
				log.Error("agentfunnel: server returned malformed response — likely a Nexus bug",
					"err", terr)
			case errors.Is(terr, keyfile.ErrBadKeyfile):
				log.Error("agentfunnel: keyfile is malformed", "err", terr)
			default:
				log.Error("agentfunnel: validation failed", "err", terr)
			}
			os.Exit(1)
		}
	}
	log.Info("agentfunnel: validated",
		"aspect", res.AspectName,
		"provider", res.Provider,
		"model", res.Model,
		"personality_version", res.Personality.Version,
		"jwt_expires", res.SessionExpiresAt.Format(time.RFC3339))

	if *builderMode {
		gitEmail := res.AspectName + "@agents.carriedworld.com"
		for k, v := range map[string]string{
			"GIT_AUTHOR_NAME": res.AspectName, "GIT_AUTHOR_EMAIL": gitEmail,
			"GIT_COMMITTER_NAME": res.AspectName, "GIT_COMMITTER_EMAIL": gitEmail,
		} {
			if err := os.Setenv(k, v); err != nil {
				log.Warn("agentfunnel: failed to export "+k+" for builder git author", "err", err)
			}
		}
	}

	var builderHome *agentHomeSession
	var builderRepo *builderRepoSession
	if *builderMode {
		var err error
		builderHome, err = setupBuilderHome(context.Background(), res.AspectName, os.Getenv("CW_DISPATCH_RUN_ID"))
		if err != nil {
			fail(log, "setup builder home", err)
		}
		log.Info("agentfunnel: builder home ready", "aspect", res.AspectName, "home", os.Getenv("HOME"))
		// Stage Antigravity OAuth creds into the (now-moved) $HOME/.gemini so
		// agy finds them. No-op when the antigravity-auth secret isn't mounted.
		stageAntigravityCreds(log)
		// Export the session JWT so cw — git's credential helper in the
		// worker image — can authenticate to the M1 custodian seam for
		// git clone/fetch/push during the build (NEX-437). cw's git-helper reads
		// CW_TOKEN; CW_SEAM_URL is supplied via the Job env. The codex/git
		// subprocess inherits this process's environment.
		if err := os.Setenv("CW_TOKEN", res.SessionJWT); err != nil {
			log.Warn("agentfunnel: failed to export CW_TOKEN for builder git auth", "err", err)
		}
		// Bridge the external git/gh tooling into our auth ecosystem before
		// touching the shared repo mirror. A failure remains non-fatal because
		// already-wired images can still have a working git credential helper,
		// but it is logged loudly because PR ops may fail later.
		if out, err := exec.CommandContext(context.Background(), "cw", "setup-git", "github").CombinedOutput(); err != nil {
			log.Error("agentfunnel: cw setup-git github failed — gh not bridged; PR ops may fail",
				"err", err, "out", strings.TrimSpace(string(out)))
		} else {
			log.Info("agentfunnel: cw setup-git github ok — gh/git bridged for builder")
		}
		builderRepo, err = setupBuilderRepo(context.Background(), res.AspectName, os.Getenv("CW_DISPATCH_RUN_ID"), *repoFlag, builderBranch(*branchFlag, *ticketFlag))
		if err != nil {
			fail(log, "setup builder repo", err)
		}
		if builderRepo != nil {
			log.Info("agentfunnel: builder repo ready", "aspect", res.AspectName, "repo", *repoFlag, "worktree", builderRepo.worktree)
		}
	}

	// 2.5 Materialise MCP profile (NEX-170). Must happen before
	// the claude-code subprocess spawns so .mcp.json is on disk
	// and auto-discovered from cwd. Atomic write — never leaves
	// a partial file. No-op when the profile is empty.
	cwd, _ := os.Getwd()
	if err := materialiseMCP(cwd, res.MCPProfile, log); err != nil {
		fail(log, "materialise .mcp.json", err)
	}

	// NEX-332 phase 4: apply the provider-credential env the broker
	// resolved from its store (keyless aspects). agentfunnel is a
	// single-aspect process, so setting it on the process env is safe and
	// makes the native-API providers (which read construction-time env)
	// pick up the broker-held key with no further plumbing. Falls back to
	// the existing process env when the broker delivered nothing (no
	// default cred / mode=proxy / claude-code self-auth).
	if len(res.ProviderEnv) > 0 {
		applied := make([]string, 0, len(res.ProviderEnv))
		for k, v := range res.ProviderEnv {
			if err := os.Setenv(k, v); err != nil {
				fail(log, "apply broker provider env", err)
			}
			applied = append(applied, k)
		}
		log.Info("agentfunnel: applied provider creds from broker store (keyless)",
			"aspect", res.AspectName, "vars", applied)
	}

	// 3. Build provider + initial binding cache.
	//
	// The binding (provider+model+harness triple) lives behind an
	// atomic.Pointer so the funnel's per-turn BindingFn can pick up
	// changes without rebuilding the funnel. v1: cache is seeded
	// here at startup and refreshed by the TokenProvider re-validate
	// path on JWT near-expiry — so an operator hitting PUT
	// /api/admin/aspects/{name}/provider-binding sees the new
	// binding take effect within one JWT cycle (≤ ~1 hour today,
	// no restart required). Future config.refresh push (NEX-332
	// phase 5) lands cache updates immediately.
	provider, err := buildProvider(res.Provider, *claudePath)
	if err != nil {
		fail(log, "build provider", err)
	}

	// Load the per-aspect tool permission policy once at startup (Tier A:
	// local JSON file). Empty -policy preserves the historical permissive
	// behaviour (DefaultAllow=true); a configured path that's missing or
	// malformed fails fast rather than silently falling back to permissive.
	// Threaded into every newBindingHarness call below so the P3b/P3c
	// permission hook enforces it on native-API providers.
	//
	// Tier B (follow-on): store the per-aspect policy centrally in the
	// Nexus and deliver it via keyfile.ValidationResult — mirroring the
	// existing res.MCPProfile field (keyfile.go ValidationResult.MCPProfile,
	// the NEX-169 resolved MCP-server blob). That removes the on-host file
	// and makes the policy Nexus-authoritative like provider/model.
	policy, err := loadToolPolicy(*policyPath)
	if err != nil {
		fail(log, "load tool policy", err)
	}
	log.Info("agentfunnel: tool policy loaded",
		"source", policySource(*policyPath),
		"default_allow", policy.DefaultAllow,
		"denied_tools", deniedToolCount(policy),
		"escalated_tools", len(policy.Escalate),
		"bash_deny_patterns", len(policy.BashDeny),
		"write_path_prefixes", len(policy.WritePathAllow))
	// escalator (P3c) is built from the wsasp client (below) once it
	// exists; declared here so the TokenProvider binding-refresh closure
	// captures the variable. Until assigned it is nil, so an escalate
	// verdict fail-safe-denies. The binding cache is re-stored with the
	// escalator-equipped harness right after the client is constructed,
	// before funnel.New consumes it.
	var escalator *funnel.Escalator
	bindingCache := &atomic.Pointer[funnel.Binding]{}
	bindingCache.Store(&funnel.Binding{
		Provider: bridle.ProviderID(res.Provider),
		Model:    res.Model,
		// Native-API providers get the P3b permission hook registered on
		// the Harness; claude-code is skipped (self-supplies tools).
		Harness: newBindingHarness(provider, res.Provider, escalator, policy),
	})

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
	registerMetadata := map[string]any(nil)
	if runID := os.Getenv("CW_DISPATCH_RUN_ID"); runID != "" {
		registerMetadata = map[string]any{"run_id": runID}
	}

	// sessionState holds the current JWT + expiry. Refreshed in-band
	// by sessionRefreshLoop via session.refresh frames; consulted by
	// the TokenProvider on every WS dial and by jwtExpiryMonitor.
	state := newSessionState(sessionSnapshot{
		JWT:     res.SessionJWT,
		Expires: res.SessionExpiresAt,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		// NEX-367: reuse the same pinned-cert trust on JWT refresh.
		client := &keyfile.Client{HTTP: keyfile.HTTPClientWithTLS(brokerTLS)}
		var fresh *keyfile.ValidationResult
		var ferr error
		if kf != nil {
			fresh, ferr = client.Validate(ctx, kf)
		} else {
			// Hand boot (NEX-571 Task D): no keyfile to re-validate with.
			// Re-resolve via the still-cached JWT against the broker. A
			// hand's run is normally far shorter than the JWT TTL, so this
			// is a cold-start/edge backstop, not the steady path.
			fresh, ferr = client.ResolveByJWT(ctx, res.NexusURL, res.NexusID, snap.JWT)
		}
		if ferr != nil {
			log.Warn("agentfunnel: TokenProvider re-validate failed, using cached token",
				"err", ferr)
			return "", ferr
		}
		state.Set(sessionSnapshot{JWT: fresh.SessionJWT, Expires: fresh.SessionExpiresAt})
		revalVia := "keyfile"
		if kf == nil {
			// Hand boot re-resolves via the cached JWT, not a keyfile.
			revalVia = "JWT"
		}
		log.Info("agentfunnel: TokenProvider re-validated",
			"via", revalVia,
			"expires", fresh.SessionExpiresAt.Format(time.RFC3339))

		// NEX-335: refresh the provider+model binding from the new
		// validate response. If the broker-side aspects.provider or
		// .model column changed (via the admin provider-binding
		// endpoint), the next turn picks up the new binding via
		// funnel.Config.BindingFn — no agentfunnel restart required.
		// Provider construction reads env (OPENAI_API_KEY/BASE_URL
		// for openai; same env-based path the initial buildProvider
		// uses) so a binding-type swap still respects the running
		// start-script's env. The current pre-fixed-credential path
		// (NEX-332 phase 4) — broker-resolved creds replace env
		// reads when wired.
		prev := bindingCache.Load()
		if prev.Model != fresh.Model || string(prev.Provider) != fresh.Provider {
			newProv, perr := buildProvider(fresh.Provider, *claudePath)
			if perr != nil {
				log.Warn("agentfunnel: binding refresh skipped — buildProvider failed",
					"err", perr, "provider", fresh.Provider)
			} else {
				bindingCache.Store(&funnel.Binding{
					Provider: bridle.ProviderID(fresh.Provider),
					Model:    fresh.Model,
					Harness:  newBindingHarness(newProv, fresh.Provider, escalator, policy),
				})
				log.Info("agentfunnel: binding refreshed via re-validate",
					"provider", fresh.Provider, "model", fresh.Model)
			}
		}
		return fresh.SessionJWT, nil
	}

	var bridge *wsasp.Bridge
	wsCfg := wsasp.Config{
		URL:           wsURL,
		AuthToken:     res.SessionJWT, // initial JWT; TokenProvider refreshes it
		TokenProvider: tokenProvider,
		TLSConfig:     brokerTLS, // NEX-367: trust the pinned broker cert on the WS dial too
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
			Metadata:       registerMetadata,
		},
	}
	wsClient, err := wsasp.NewClient(wsCfg)
	if err != nil {
		fail(log, "wsasp.NewClient", err)
	}

	// P3c: now that the wsasp client exists, build the operator-escalation
	// requester for native providers and re-store the binding cache with
	// an escalator-equipped harness. wsClient.Request blocks on the
	// correlated escalation.decision (no timeout) — the broker relays the
	// request to operators and routes the decision back. claude-code is
	// skipped inside newBindingHarness (it self-supplies tools).
	escalator = &funnel.Escalator{Requester: wsClient, AspectID: res.AspectName}
	bindingCache.Store(&funnel.Binding{
		Provider: bridle.ProviderID(res.Provider),
		Model:    res.Model,
		Harness:  newBindingHarness(provider, res.Provider, escalator, policy),
	})

	gateway := wsasp.NewGateway(wsClient)
	// NEX-knowledge-fix (operator 2026-05-27): wire knowledge gateway
	// over WS so remote aspects (harrow, anvil, plumb) can use the
	// search_knowledge / store_knowledge tools. Pre-fix the Knowledge
	// field was nil → CommsRunner returned "knowledge gateway not
	// configured" on every call.
	knowledgeGateway := wsasp.NewKnowledgeGateway(wsClient)
	var onTaskDone func(string)
	doneSentinel := ""
	if *builderMode {
		onTaskDone = builderOnTaskDone(stop, log, res.AspectName)
		doneSentinel = builderDoneSentinel
	}
	commsRunner := funnel.CommsRunner{
		Gateway:    gateway,
		Knowledge:  knowledgeGateway,
		AspectID:   res.AspectName,
		OnTaskDone: onTaskDone,
	}
	// Spawn (NEX-609): the native comms surface carries the spawn tool
	// for PARENT aspects only — a hand (derived identity) never gets a
	// working Spawner (no sub-of-sub; the broker enforces it too).
	if !aspects.IsDerivedName(res.AspectName) {
		commsRunner.Spawner = gateway
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
	// NEX-373: judge + compact config arrive in the VALIDATE response
	// (res.Judge*/Compact*), resolved broker-side. Previously the aspect
	// fetched them over WS here at startup — but that request raced
	// wsClient.Run (which connects ~170 lines below) and silently timed out,
	// dropping every judge/compact override (judges fell back to bare
	// claude-code and failed open). Delivering them via validate, like
	// provider_env, removes the race entirely.
	judgeProviderOverride := res.JudgeProvider
	judgeModelOverride := res.JudgeModel
	judgeEnv := res.JudgeEnv
	compactModelOverride := res.CompactModel
	compactEnv := res.CompactEnv
	if judgeProviderOverride != "" || judgeModelOverride != "" || compactModelOverride != "" {
		log.Info("agentfunnel: model overrides from validate",
			"aspect", res.AspectName, "judge_provider", judgeProviderOverride,
			"judge_model", judgeModelOverride, "compact_model", compactModelOverride,
			"judge_env_keys", envKeyNames(judgeEnv))
	}

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
	outputFilter := buildAgentFunnelFilter(provider, bridle.ProviderID(res.Provider), judgeProviderOverride, judgeModelOverride, judgeEnv, res.Model, log, obsHook)

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
	}, compactModelOverride, compactEnv, log)

	// Provider-aware ToolRunner. Native-API providers get the local
	// coding-tool lane (toolrunner.LocalToolRunner) composed behind comms;
	// claude-code keeps the NullRunner no-op (it self-supplies tools). The
	// aspect's cwd (autospawn home dir, where aspect.json lives) is the
	// WorkDir for local file/bash tools. Lynxai web access is opt-in via
	// env; empty base URL gracefully disables web_fetch/web_extract.
	toolRunner, err := runnerForProviderAgent(
		bridle.ProviderID(res.Provider),
		commsRunner,
		cwd,
		os.Getenv("LYNXAI_BASE_URL"),
		os.Getenv("LYNXAI_KEY"),
	)
	if err != nil {
		fail(log, "build tool runner", err)
	}

	systemPrompt := composeSystemPrompt(res)
	// Parse the validate response's mcp_profile into the funnel's MCP server
	// list so non-claude-code providers (openai, codex) receive the servers
	// in their TurnRequest. Empty/unparsed → keep MCP non-nil-but-empty,
	// which is what claude-code's .mcp.json discovery (NEX-170) wants.
	mcpCfg, perr := parseMCPProfile(res.MCPProfile)
	if perr != nil {
		log.Warn("agentfunnel: mcp_profile parse failed; non-claude-code providers get no MCP tools this run", "err", perr)
	}
	if mcpCfg == nil {
		mcpCfg = &bridle.MCPClientConfig{}
	}
	// NEX-609: the comms MCP server must NOT reach the bridle tool loop.
	// Non-claude-code providers already get the full native comms surface
	// (CommsToolDefs + the spawn tool above), so loading nexus-comms-mcp
	// as an MCP server duplicates send_chat/read_chat/… and bridle aborts
	// every turn with ErrToolNameCollision. claude-code is unaffected —
	// it ignores TurnRequest.MCP and discovers servers from the
	// materialised .mcp.json, where the comms server is exactly how it
	// gets comms + spawn.
	mcpCfg = dropCommsMCPServers(mcpCfg, log)
	cfg := funnel.Config{
		AspectID: res.AspectName,
		// Static binding fields kept populated for back-compat (some
		// non-aspect callers still construct without BindingFn) but
		// the agentfunnel flow always uses BindingFn — see below.
		// Hook registered here too so the back-compat path also enforces
		// the P3b policy for native providers (BindingFn overrides per turn).
		// escalator is set by now (built right after the wsasp client).
		Harness:  newBindingHarness(provider, res.Provider, escalator, policy),
		Provider: bridle.ProviderID(res.Provider),
		Model:    res.Model,
		// NEX-335: per-turn binding resolver reads from the binding
		// cache. TokenProvider refreshes the cache on JWT re-validate
		// when the broker-side binding has changed (operator hit the
		// admin endpoint). The funnel calls this every turn, so the
		// new binding takes effect on the next turn after the cache
		// updates — no funnel rebuild, no aspect restart.
		BindingFn:    func() funnel.Binding { return *bindingCache.Load() },
		OnTaskDone:   onTaskDone,
		DoneSentinel: doneSentinel,
		SystemPrompt: systemPrompt,
		// MCP carries the aspect's mcp_profile servers (parseMCPProfile
		// above) so non-claude-code providers get them in TurnRequest.MCP.
		// claude-code ignores this and discovers via cmd.Dir/.mcp.json,
		// materialised from the validate response by materialiseMCP (NEX-170).
		MCP: mcpCfg,
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
		Tools:  toolsForProviderAgent(bridle.ProviderID(res.Provider), res.AspectName),
		Runner: toolRunner,
		// AutoRecall (Commonplace): turn-time recall into the system prompt,
		// gated by the -auto-recall flag (no aspect.json on the host). Uses
		// the same knowledge gateway as the comms runner. Day-3 lever.
		AutoRecall: funnel.AutoRecallConfig{
			Gateway: knowledgeGateway,
			Enabled: *autoRecall,
			TopK:    *autoRecallTopK,
			MaxRank: *autoRecallMaxRank,
		},
		// ChatGateway routes the model's auto-post FinalText through the
		// same SendChat path CommsRunner uses for explicit send_chat tool
		// calls. Required for claude-code (subprocess mode): without it,
		// model output evaporates because the CLI has no MCP-loaded tools
		// to call. Mirrors cmd/nexus/main.go's Frame funnel wiring.
		ChatGateway:      gateway,
		StreamTextToChat: true, // NEX-240: stream text blocks to chat as they arrive
		ReplyTopic:       builderReplyTopic(*builderMode, *replyTopic),
		AspectHome:       cwd, // NEX-241: stderr log + session isolation anchor
		// NEX-302: per-aspect main-turn sampling overrides from
		// aspect.json on the aspect's home dir. Empty / unset block
		// leaves funnel's pass-through with zero-valued
		// MainTurnSampling -> bridle TurnRequest fields stay unset
		// -> provider defaults preserved (back-compat).
		MainTurnSampling:  mainTurnSampling,
		Filter:            outputFilter,
		PostTurn:          postTurn,
		ObservabilityHook: obsHook,
		// NEX-96: persist the seen-msg-id set alongside the wsasp cursor
		// so the idempotency guard survives agentfunnel restart. Same
		// dir resolution as the cursor file (--cursor-dir / cwd).
		IdempotencyFile: filepath.Join(resolveCursorDir(*cursorDir), "funnel-seen.json"),
		Logger:          log,
	}
	f, err := funnel.New(cfg)
	if err != nil {
		fail(log, "funnel.New", err)
	}
	funnelPtr = f
	bridge = wsasp.NewBridge(f)

	var seedBrief bridle.InboxItem
	if *builderMode && *briefFile != "" {
		b, err := readBriefFile(*briefFile)
		if err != nil {
			log.Error("agentfunnel: brief file unreadable", "err", err)
			os.Exit(1)
		}
		seedBrief = b
		f.Receive(seedBrief)
		log.Info("agentfunnel: seeded builder brief from file", "path", *briefFile, "bytes", len(seedBrief.Content))
	}

	log.Info("agentfunnel: starting deliberation loop",
		"aspect", res.AspectName,
		"session", sessionID,
		"system_prompt_bytes", len(systemPrompt),
		"central_version", res.CentralVersion,
		"personality_version", res.Personality.Version)

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

	// NEX-477: a builder with a captured Definition of Done (its seed brief)
	// runs the goal-pursuit loop — it posts progress and keeps working toward
	// the DoD instead of idling to the timeout when a turn lands goal_not_met.
	// Builders seeded via the broker inbox (no captured DoD) and always-on
	// aspects fall back to the plain cadence loop.
	if *builderMode && seedBrief.Content != "" {
		goalCfg := funnel.GoalConfig{
			TicketID:   *ticketFlag,
			DoD:        seedBrief.Content,
			MaxTurns:   *builderMaxTurns,
			ThreadRoot: seedBrief.ThreadRoot,
		}
		verifyPR := builderPRVerifier(log, res.AspectName, *repoFlag, *ticketFlag, *branchFlag)
		openPR := builderPROpener(log, *repoFlag, *ticketFlag, *branchFlag)
		go builderGoalLoop(ctx, f, log, goalCfg, verifyPR, openPR, stop)
	} else {
		var onComplete func() bool
		if *builderMode {
			onComplete = builderCompleteCheck(stop, log, res.AspectName, *repoFlag, *ticketFlag, *branchFlag)
		}
		go deliberateLoop(ctx, f, log, onComplete)
	}

	if *builderMode {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(*builderTimeout):
				log.Error("agentfunnel: builder timeout — forcing exit", "timeout", *builderTimeout)
				stop()
			}
		}()
	}

	if err := wsClient.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agentfunnel: wsClient.Run", "err", err)
		os.Exit(1)
	}
	if builderRepo != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := builderRepo.cleanDespawn(cleanupCtx); err != nil {
			cancel()
			log.Error("agentfunnel: builder repo worktree cleanup failed", "err", err)
			os.Exit(1)
		}
		cancel()
		log.Info("agentfunnel: builder repo worktree removed", "aspect", res.AspectName, "worktree", builderRepo.worktree)
	}
	if builderHome != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := builderHome.cleanDespawn(cleanupCtx); err != nil {
			cancel()
			log.Error("agentfunnel: builder home clean despawn failed", "err", err)
			os.Exit(1)
		}
		cancel()
		log.Info("agentfunnel: builder home committed and removed", "aspect", res.AspectName)
	}
	log.Info("agentfunnel: stopped")
}

func readBriefFile(path string) (bridle.InboxItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return bridle.InboxItem{}, fmt.Errorf("read brief file: %w", err)
	}
	return bridle.InboxItem{From: "dispatch", Content: string(b)}, nil
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

// builderDoneSentinel is the reply marker a builder emits to signal a finished
// task. Unlike the task_done ToolDef, a sentinel in the reply text is reachable
// by codex (which ignores bridle ToolDefs) and every other provider — NEX-440.
// The dispatch brief instructs the builder to end its final message with it.
const builderDoneSentinel = "<<TASK_COMPLETE>>"

func builderOnTaskDone(stop context.CancelFunc, log *slog.Logger, aspect string) func(string) {
	return func(summary string) {
		log.Info("agentfunnel: builder task_done — exiting", "aspect", aspect, "summary", summary)
		stop()
	}
}

// builderPRVerifier returns a pure check: does a PR exist for the builder branch?
// It logs the miss/error but has NO side effects (no stop), so a goal-loop can
// decide for itself what to do next. Fail-closed: a gh error reports false
// (NEX-468 — never declare completion we cannot verify).
func builderPRVerifier(log *slog.Logger, aspect, repo, ticket, branch string) func() bool {
	head := builderBranch(branch, ticket)
	return func() bool {
		ok, err := prExists(repo, head)
		if err != nil {
			log.Warn("agentfunnel: PR check errored — treating as not-yet-open", "aspect", aspect, "err", err)
			return false
		}
		if !ok {
			log.Info("agentfunnel: no PR found for builder branch", "aspect", aspect, "repo", repo, "branch", head)
		}
		return ok
	}
}

// builderCompleteCheck returns an onComplete callback for the plain builder
// loop (broker-inbox builders without a captured DoD). When the judge rules a
// turn complete (NEX-471) it verifies the PR exists (NEX-468) and only then
// stops the builder. Returns true when it stopped.
func builderCompleteCheck(stop context.CancelFunc, log *slog.Logger, aspect, repo, ticket, branch string) func() bool {
	verify := builderPRVerifier(log, aspect, repo, ticket, branch)
	return func() bool {
		if !verify() {
			return false
		}
		log.Info("agentfunnel: judge complete + PR verified — exiting", "aspect", aspect, "ticket", ticket)
		stop()
		return true
	}
}

// prExistsFn is the gh-backed PR lookup for a branch head, swappable in tests.
var prExistsFn = func(repo, branch string) (bool, error) {
	out, err := exec.Command("gh", "pr", "list", "--repo", repo, "--head", branch, "--json", "url", "-q", ".[0].url").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// prCreateFn opens a PR for branch via gh, filling title/body from the branch's
// commits (--fill). Swappable in tests. Returns the printed PR URL.
var prCreateFn = func(repo, branch string) (string, error) {
	out, err := exec.Command("gh", "pr", "create", "--repo", repo, "--head", branch, "--base", "main", "--fill").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// branchPushedFn reports whether branch exists on origin (i.e. the agent
// committed + pushed its work). Swappable in tests.
var branchPushedFn = func(branch string) (bool, error) {
	out, err := exec.Command("git", "ls-remote", "--heads", "origin", branch).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// harnessOpenPR opens the PR for a pushed builder branch when the agent
// committed + pushed but never ran `gh pr create` itself (NEX-528: codex
// reliably commits + pushes but is flaky at the final PR-open, looping the
// goal-loop to its timeout). Returns the PR URL and true on success; false when
// nothing is pushed yet (the agent still has work to do — re-prompt) or on error.
func harnessOpenPR(log *slog.Logger, repo, head string) (string, bool) {
	pushed, err := branchPushedFn(head)
	if err != nil {
		log.Warn("agentfunnel: harness PR open — branch check failed", "branch", head, "err", err)
		return "", false
	}
	if !pushed {
		return "", false
	}
	url, err := prCreateFn(repo, head)
	if err != nil {
		log.Warn("agentfunnel: harness PR open failed", "branch", head, "err", err)
		return "", false
	}
	return url, true
}

// builderPROpener returns a closure that opens the builder's PR from its pushed
// branch (NEX-528), passed to builderGoalLoop alongside verifyPR.
func builderPROpener(log *slog.Logger, repo, ticket, branch string) func() (string, bool) {
	head := builderBranch(branch, ticket)
	return func() (string, bool) { return harnessOpenPR(log, repo, head) }
}

// prExists reports whether a PR exists for branch in repo. Missing repo/branch
// returns an error so the builder does not exit on an unverifiable
// "complete" (fail-closed toward NEX-468).
func prExists(repo, branch string) (bool, error) {
	if repo == "" || branch == "" {
		return false, fmt.Errorf("prExists: repo/branch not set (repo=%q branch=%q)", repo, branch)
	}
	return prExistsFn(repo, branch)
}

func builderReplyTopic(builderMode bool, topic string) string {
	if !builderMode {
		return ""
	}
	return topic
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
func deliberateLoop(ctx context.Context, f *funnel.Funnel, log *slog.Logger, onComplete func() bool) {
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
				result, err := f.Deliberate(ctx, "")
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
				// NEX-471/NEX-468: in builder mode, when the judge rules the
				// turn complete (DoD met) and the PR is verified to exist, exit
				// promptly instead of idling to -builder-timeout.
				if onComplete != nil && result.Filter.Class == funnel.FilterClassComplete && onComplete() {
					return
				}
			}
		}
	}
}

// builderPRRepromptCap bounds how many times the builder goal-loop will
// re-prompt for a missing PR after the judge ruled the goal complete. The
// judge can false-positive "done" (NEX-468); rather than exit empty-handed we
// push back so the builder opens the PR — but only a few times, so a model
// that keeps claiming done without pushing cannot loop forever. GoalLoop
// MaxTurns and -builder-timeout are the outer backstops.
const builderPRRepromptCap = 3

// builderStep is builderDecide's verdict for the builder goal-loop.
type builderStep int

const (
	builderContinue   builderStep = iota // intermediate goal_not_met — keep pursuing
	builderExit                          // terminal: verified-complete, blocked, or exhausted
	builderRepromptPR                    // judge says complete but no PR — push back for it
)

// builderDecide maps a GoalLoop result plus a PR-verification outcome to the
// builder's next step. Pure (no funnel, no I/O) so it is unit-testable.
//
//   - blocked                                  -> exit (escalated)
//   - not done (intermediate goal_not_met)     -> continue (Pursue enqueued a continuation)
//   - done + PR verified                       -> exit (success)
//   - done, reason "complete", no PR, budget   -> reprompt for the PR
//   - any other done (scratch / loop_cap / no
//     PR with budget exhausted)                -> exit
func builderDecide(result funnel.GoalResult, prVerified bool, prRepromptsLeft int) builderStep {
	if result.Blocked {
		return builderExit
	}
	if !result.Done {
		return builderContinue
	}
	if prVerified {
		return builderExit
	}
	if result.Reason == "complete" && prRepromptsLeft > 0 {
		return builderRepromptPR
	}
	return builderExit
}

// builderGoalLoop drives a dispatch builder to its Definition of Done using the
// goal-pursuit loop (NEX-477). Unlike the bare deliberateLoop — which drains
// the inbox and then idles when the judge rules goal_not_met — GoalLoop posts
// the progress reply (ShouldPost) AND enqueues a continuation so the builder
// keeps working toward the DoD until it is met, blocked, or capped. Completion
// is gated on the PR actually existing (NEX-468/471): if the judge rules
// complete but no PR is found, the builder is re-prompted to open it rather
// than exiting empty-handed. The -builder-timeout goroutine remains the
// wall-clock backstop.
func builderGoalLoop(ctx context.Context, f *funnel.Funnel, log *slog.Logger, cfg funnel.GoalConfig, verifyPR func() bool, openPR func() (string, bool), stop context.CancelFunc) {
	gl := funnel.NewGoalLoop(f, cfg)
	prRepromptsLeft := builderPRRepromptCap
	for {
		if ctx.Err() != nil {
			return
		}
		result, err := gl.Pursue(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Warn("agentfunnel: builder goal-loop pursue", "err", err)
			return
		}
		prVerified := false
		if result.Done && !result.Blocked {
			// Only spend a gh call when the loop thinks it is finished.
			prVerified = verifyPR()
		}
		switch builderDecide(result, prVerified, prRepromptsLeft) {
		case builderContinue:
			continue
		case builderRepromptPR:
			// The agent ruled itself done but no PR exists. If the work is
			// already pushed, the harness opens the PR itself (NEX-528) rather
			// than burning reprompt turns to the timeout. Only re-prompt when
			// nothing is pushed yet (the agent still has work to do).
			if url, opened := openPR(); opened {
				log.Info("agentfunnel: builder PR opened by harness (agent pushed but did not open it)", "ticket", cfg.TicketID, "pr", url)
				stop()
				return
			}
			prRepromptsLeft--
			log.Info("agentfunnel: judge complete but no PR — re-prompting", "ticket", cfg.TicketID, "reprompts_left", prRepromptsLeft)
			f.ReceiveSynthetic(bridle.InboxItem{
				From:       "system",
				Source:     "builder_pr_check",
				ThreadRoot: cfg.ThreadRoot,
				Content: fmt.Sprintf(
					"[CONTINUATION] The Definition of Done requires an open pull request from branch builder/%s, "+
						"but none was found. Push your branch and open the PR with the gh CLI (gh pr create). "+
						"If you cannot, say so explicitly and name the blocker. End your final message with %s once the PR is open.",
					cfg.TicketID, builderDoneSentinel),
			})
			continue
		case builderExit:
			log.Info("agentfunnel: builder goal-loop done", "ticket", cfg.TicketID, "reason", result.Reason, "blocked", result.Blocked, "pr_verified", prVerified, "turns", result.TurnsRun)
			stop()
			return
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
	case "openai":
		// OPENAI_API_KEY + OPENAI_BASE_URL come from the start script
		// (or the credential bundle once NEX-332 phase 4 lands). Empty
		// baseURL falls back to api.openai.com (matching the bridle
		// constructor's behaviour); set baseURL to point at any
		// OpenAI-compatible endpoint (DeepSeek's /v1, Together,
		// local Ollama). Per-aspect API-key handoff is the same shape
		// as claude-api today — env is the v1 surface; broker-pushed
		// creds replace it in the dynamic-config arc.
		return openaiprovider.NewWithBaseURL(
			os.Getenv("OPENAI_API_KEY"),
			os.Getenv("OPENAI_BASE_URL"),
		), nil
	case "ollama", "ollama-local":
		// bridle chat provider lane — distinct from the embeddings-only
		// runtime/providers/ollama-local package despite sharing the name.
		// Native Ollama API (NEX-563). The openai case pointed at
		// ollama's /v1 compat endpoint also works, but the compat
		// surface cannot express keep_alive (model stays loaded across
		// quiet periods) or options.num_ctx (context window) — the two
		// knobs that matter for an always-on local-model aspect. Env is
		// the v1 config surface, same shape as the openai case.
		return ollamaFromEnv(os.Getenv)
	case "codex-cli", "codex", "codexcli":
		// Headless Codex CLI (subprocess-stream): owns its own session +
		// resume, so a codex aspect gets cross-turn memory without the
		// funnel SessionTail path. Uses the operator's codex login; the
		// model comes from the validate binding (or the codex config default).
		return codexcliprovider.New(), nil
	case "antigravity-cli", "antigravity", "agy":
		// Headless Antigravity CLI (agy, plain-text subprocess): runs its own
		// agentic loop internally and prints the final response — no tool-call
		// streaming. OAuth creds are staged into $HOME/.gemini by the builder
		// (stageAntigravityCreds), since agy reads $HOME/.gemini with no
		// CODEX_HOME-style override and the builder moves HOME to the worktree.
		return antigravitycliprovider.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q (claude-api, claude-code, openai, ollama, codex-cli, antigravity-cli supported)", provider)
	}
}

// ollamaFromEnv constructs the native ollama provider from env. getenv
// is injected (rather than calling os.Getenv directly) so tests can
// exercise the parse paths without touching process-global env.
//
//	OLLAMA_BASE_URL   server URL; empty → http://localhost:11434
//	OLLAMA_KEEP_ALIVE Go duration ("30m", "1h"; negative e.g. "-1s"
//	                  = keep the model loaded forever); empty → 0,
//	                  bridle applies its own 30m default
//	OLLAMA_NUM_CTX    integer context window (options.num_ctx);
//	                  empty → 0, model default in effect
//
// Malformed values fail provider construction loudly — a silently
// dropped keep_alive would look like it worked while the model
// reloads on every post-lull turn.
func ollamaFromEnv(getenv func(string) string) (*ollamaprovider.Provider, error) {
	baseURL := getenv("OLLAMA_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	p := ollamaprovider.NewWithURL(baseURL)
	if v := getenv("OLLAMA_KEEP_ALIVE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("buildProvider: OLLAMA_KEEP_ALIVE %q is not a Go duration (want e.g. \"30m\", \"-1s\"): %w", v, err)
		}
		p.KeepAlive = d
	}
	if v := getenv("OLLAMA_NUM_CTX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("buildProvider: OLLAMA_NUM_CTX %q is not an integer: %w", v, err)
		}
		p.NumCtx = n
	}
	return p, nil
}

// stageAntigravityCreds copies the mounted Antigravity OAuth creds into the
// builder's $HOME/.gemini so agy (which reads $HOME/.gemini, HOME-dependent
// with no CODEX_HOME-style override) finds them after setupBuilderHome moves
// HOME to the per-agent worktree. The dest is writable so agy can refresh the
// access token via the refresh token. No-op when the secret isn't mounted
// (i.e. non-antigravity builders).
func stageAntigravityCreds(log *slog.Logger) {
	const src = "/antigravity-secret/oauth_creds.json"
	data, err := os.ReadFile(src)
	if err != nil {
		return // secret not mounted — not an antigravity builder
	}
	dir := filepath.Join(os.Getenv("HOME"), ".gemini")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warn("agentfunnel: mkdir .gemini for antigravity creds", "err", err)
		return
	}
	dst := filepath.Join(dir, "oauth_creds.json")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		log.Warn("agentfunnel: stage antigravity creds", "err", err)
		return
	}
	log.Info("agentfunnel: staged antigravity OAuth creds", "dst", dst)
}

// newBindingHarness builds the *bridle.Harness the funnel runs turns
// against for a given binding, and — for NATIVE-API providers only —
// registers the autonomous permission hook (P3b).
//
// claude-code is skipped: it self-supplies its tool surface inside the
// spawned subprocess, so a BeforeToolCall hook on this Harness never sees
// those calls (claude-code's own --disallowedTools is its guardrail). For
// direct-API providers (claude-api, openai) the funnel executes every tool
// via the Harness, so the hook is the enforcement point.
//
// policy is the per-aspect ToolPolicy enforced by the P3b/P3c permission
// hook. It is loaded once at startup by loadToolPolicy (Tier A: local
// -policy JSON file) and threaded through unchanged so every binding
// refresh re-registers the same policy. An empty -policy yields the
// permissive DefaultAllow=true policy, preserving pre-config behaviour.
//
// esc (P3c) handles VerdictEscalate tool calls by asking the operator
// over the aspect's WS. It is built from the wsasp client (the
// Requester) + aspect id once that client exists; the earliest harness
// store (before the client is constructed) passes nil, which makes any
// escalate verdict fail-safe to deny until the escalator-equipped
// harness is re-stored.
func newBindingHarness(provider bridle.Provider, providerName string, esc *funnel.Escalator, policy funnel.ToolPolicy) *bridle.Harness {
	h := bridle.NewHarness(provider)
	switch providerName {
	case "claude-code", "claudecode":
		// claude-code owns its tools in-subprocess — hook can't see them.
	default:
		h.RegisterBeforeToolCall(funnel.PermissionHook(policy, esc))
	}
	return h
}

// loadToolPolicy resolves the per-aspect tool permission policy.
//
// Tier A (this function): an empty path returns the permissive default
// (DefaultAllow=true) so unconfigured aspects behave exactly as they did
// before the -policy flag existed. A non-empty path is read and
// JSON-decoded into a funnel.ToolPolicy; a missing or malformed file
// returns a wrapped error so startup fails fast rather than silently
// running permissive — a misconfigured policy must be loud.
//
// Tier B (follow-on): the Nexus stores the per-aspect policy and delivers
// it through keyfile.ValidationResult, mirroring the existing MCPProfile
// field, removing the need for an on-host file.
func loadToolPolicy(path string) (funnel.ToolPolicy, error) {
	if path == "" {
		return funnel.ToolPolicy{DefaultAllow: true}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return funnel.ToolPolicy{}, fmt.Errorf("read tool policy %q: %w", path, err)
	}
	var p funnel.ToolPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return funnel.ToolPolicy{}, fmt.Errorf("parse tool policy %q: %w", path, err)
	}
	return p, nil
}

// policySource renders a human label for the policy origin in log lines.
func policySource(path string) string {
	if path == "" {
		return "default (permissive)"
	}
	return path
}

// deniedToolCount counts tools the policy denies outright via the Tools
// map (entries set to false). Bash-denylist and write-path rules are
// command/path conditional, so they're logged separately as pattern
// counts rather than folded in here.
func deniedToolCount(p funnel.ToolPolicy) int {
	n := 0
	for _, allowed := range p.Tools {
		if !allowed {
			n++
		}
	}
	return n
}

// buildAgentFunnelFilter constructs the cheap-judge output filter for an
// agentfunnel aspect by delegating to the shared judge package — the same
// builder the in-process Frame uses (NEX-365 #2). The agentfunnel resolves
// its judge inputs from the broker (NEX-293/#3): judgeProviderOverride +
// judgeModelOverride from the admin model_config row, providerEnv from the
// resolved judge credential. Empty override = the judge runs on the
// aspect's own provider. There's no aspect.json on the host, so the filter
// is always cheap-or-(downgrade-to-)hard; mainModel (res.Model) is the
// non-Claude judge-model fallback.
func buildAgentFunnelFilter(provider bridle.Provider, providerID bridle.ProviderID, judgeProviderOverride, judgeModelOverride string, providerEnv map[string]string, mainModel string, log *slog.Logger, obsHook funnel.ObservabilityHook) funnel.OutputFilter {
	return judge.BuildFilter(judge.Spec{
		Label:             "agentfunnel",
		MainProvider:      provider,
		MainProviderID:    providerID,
		MainModel:         mainModel,
		JudgeProviderName: judgeProviderOverride,
		JudgeModel:        judgeModelOverride,
		JudgeEnv:          providerEnv,
		ObsHook:           obsHook,
		Logger:            log,
	})
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
func toolsForProviderAgent(id bridle.ProviderID, aspectName string) []bridle.ToolDef {
	switch id {
	case "claude-code", "claudecode":
		return nil
	}
	// Native-API providers: the funnel supplies the full tool surface —
	// host comms (lane 2: send_chat etc.) + local coding tools (lane 1:
	// bash/read/write/edit/glob/grep/web_fetch/web_extract). The two name
	// sets are disjoint so ComposeRunner can route purely by tool name.
	defs := append(funnel.CommsToolDefs(), toolrunner.Defs()...)
	// Spawn (NEX-609): parents only — a derived (hand) identity never
	// sees the tool, mirroring nexus-comms-mcp's spawnToolAvailable gate.
	if !aspects.IsDerivedName(aspectName) {
		defs = append(defs, funnel.SpawnToolDef())
	}
	return defs
}

// dropCommsMCPServers filters nexus-comms servers out of the funnel's
// bridle MCP config (NEX-609). The native tool loop already carries
// the full comms surface (send_chat, read_chat, spawn, …), so loading
// the comms MCP server beside it makes bridle's merge fail with
// ErrToolNameCollision and aborts the turn. Matches by the canonical
// server names ("nexus-comms-mcp" / "nexus-comms"); other servers
// (nexus-jira, …) pass through untouched.
func dropCommsMCPServers(cfg *bridle.MCPClientConfig, log *slog.Logger) *bridle.MCPClientConfig {
	if cfg == nil || len(cfg.Servers) == 0 {
		return cfg
	}
	kept := cfg.Servers[:0]
	for _, s := range cfg.Servers {
		if s.Name == "nexus-comms-mcp" || s.Name == "nexus-comms" {
			if log != nil {
				log.Info("agentfunnel: comms MCP server excluded from native tool loop (comms are native; spawn rides CommsRunner)", "server", s.Name)
			}
			continue
		}
		kept = append(kept, s)
	}
	cfg.Servers = kept
	return cfg
}

// runnerForProviderAgent builds the funnel's ToolRunner. claude-code owns
// its own tools natively, so the non-comms lane is a no-op (NullRunner);
// native-API providers get the real LocalToolRunner as the non-comms lane,
// composed behind the comms runner (ComposeRunner routes comms names to
// comms, everything else to the local runner).
func runnerForProviderAgent(id bridle.ProviderID, comms funnel.CommsRunner, workDir, lynxaiBase, lynxaiKey string) (bridle.ToolRunner, error) {
	switch id {
	case "claude-code", "claudecode":
		return funnel.ComposeRunner(comms, &funnel.NullRunner{}), nil
	}
	local, err := toolrunner.New(toolrunner.Config{
		WorkDir:       workDir,
		LynxaiBaseURL: lynxaiBase, // empty → web_fetch/web_extract return a disabled-error result (graceful)
		LynxaiKey:     lynxaiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("build local tool runner: %w", err)
	}
	return funnel.ComposeRunner(comms, local), nil
}

func fail(log *slog.Logger, msg string, err error) {
	if err != nil {
		log.Error(msg, "err", err)
	} else {
		log.Error(msg)
	}
	os.Exit(2)
}

// seamHTTPToWS turns the CW_SEAM_URL the broker injects into a hand's
// Job env (`https://host:7888`) into the WS dial URL agentfunnel needs
// (`wss://host:7888/connect`). The /connect suffix matches what the
// keyfile path appends below. http:// → ws:// for non-TLS test seams.
func seamHTTPToWS(seam string) string {
	ws := seam
	switch {
	case strings.HasPrefix(ws, "https://"):
		ws = "wss://" + strings.TrimPrefix(ws, "https://")
	case strings.HasPrefix(ws, "http://"):
		ws = "ws://" + strings.TrimPrefix(ws, "http://")
	}
	ws = strings.TrimRight(ws, "/")
	if !strings.HasSuffix(ws, "/connect") {
		ws += "/connect"
	}
	return ws
}
