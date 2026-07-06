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
	"sync"
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

// defaultDrainPrompt is the self-contained orchestrate-drain instruction handed
// to `claude -p` in -drain mode. It is deliberately standalone (does not rely on
// claude-side skill discovery in the pod): if the `orchestrate` skill is present
// claude follows it, otherwise this prompt carries the procedure. Mirrors
// .agents/skills/orchestrate/SKILL.md — keep them in step.
const defaultDrainPrompt = `You are shadow, woken for ONE autonomous orchestrate drain of your NEX work queue, then exit. State lives in Jira + git; you hold no memory of prior drains — re-read truth, never assume. If the orchestrate skill is available, follow it. Otherwise follow this procedure:

1. Snapshot: jira_list_ready for Ready/To-Do issues labelled "shadow-queue". Treat the snapshot as fixed for this drain; new/changed issues wait for the next wake.
2. For each unit, one at a time:
   - Epic/goal with no dispatchable leaf children yet -> DECOMPOSE into leaf Tasks via jira_create (parent=<epic>, labels include "shadow-queue", a clear definition-of-done, skills tags for routing); set the goal In Progress. Do NOT dispatch the children this drain.
   - Ready leaf task -> DISPATCH to a builder ("!dispatch <builder>%<provider> repo=<r> ticket=<NEX-key>"), VERIFY acceptance in the broker log (builder job created + Submit err=nil + pod Running, NOT the send_chat ok), then IMMEDIATELY set the unit In Progress (the double-dispatch guard). If acceptance fails -> escalate.
3. Reconcile dispatched (In Progress) units: if their PR has landed, REVIEW it.
4. Gates: AUTO-MERGE only when ALL hold — CI green, single-ticket scope, your review found no blocking issue, NOT cross-cutting (no deploy / proto / auth / multi-repo / scope change) — then squash-merge, delete branch, set Done. OTHERWISE ESCALATE+PARK: leave the PR open, set the unit Blocked, log a line "orchestrate: ESCALATION <key> <reason>", and ping the operator via comms. Deploys ALWAYS escalate. Builder failed/stalled -> redispatch-with-feedback ONCE, then escalate.
5. Exit; report a one-line summary of what you did this drain.

Hard rules: one ticket per builder; never bundle tickets in a dispatch; transition-on-dispatch is mandatory; when in doubt about a merge, ESCALATE; if it isn't in Jira/git/the run log, it didn't happen.`

func main() {
	// processStartedAt anchors the M1 Unit 5 worker.status heartbeat's
	// `started_at` field — stable across every heartbeat this process
	// emits (the worker_status store treats a zero StartedAt on later
	// upserts as "don't overwrite," so this is really only read once,
	// at the first emit).
	processStartedAt := time.Now().UTC()
	keyfilePath := flag.String("k", "", "path to the aspect keyfile (required)")
	cursorDir := flag.String("cursor-dir", "", "directory for the Lock 6 message-cursor file (defaults to <cwd>/cursor)")
	contextMode := flag.String("context-mode", string(schemas.ContextThread), "context mode: global, thread, or stateless (Nexus does not yet ship context_mode in the validation response)")
	claudePath := flag.String("claude", "", "path to the claude-code CLI (optional; auto-detects /opt/homebrew/bin/claude, /usr/local/bin/claude, ~/.npm-global/bin/claude, then PATH; also honours CLAUDE_PATH env)")
	policyPath := flag.String("policy", "", "path to a per-aspect tool permission policy JSON file (optional; empty = permissive default_allow). See funnel.ToolPolicy for the JSON shape.")
	autoRecall := flag.Bool("auto-recall", false, "enable turn-time recall from the Commonplace (search the cross-session knowledge store with each incoming message and inject the strongest matches into the system prompt). Off by default.")
	autoRecallTopK := flag.Int("auto-recall-topk", 0, "auto-recall: max strongest matches to inject (0 = funnel default)")
	autoRecallMaxRank := flag.Float64("auto-recall-max-rank", 0, "auto-recall: BM25 relevance gate; only hits with score < this inject (ranks are negative, lower = stronger; 0 = no gate)")
	builderMode := flag.Bool("builder", false, "builder/one-shot mode: drain the dispatched brief, run to the task_done signal, then exit")
	builderTimeout := flag.Duration("builder-timeout", 30*time.Minute, "builder hard ceiling value supplied by the dispatch Job; Kubernetes activeDeadlineSeconds enforces it")
	builderIdleTimeout := flag.Duration("builder-idle-timeout", builderIdleTimeoutDefaultFromEnv(os.Getenv), "builder mode: max time without progress before stalled failure (env CW_IDLE_TIMEOUT, default 2m)")
	builderMaxTurns := flag.Int("builder-max-turns", 20, "builder mode: max goal-pursuit turns before the ralph-loop gives up (NEX-477); -builder-timeout is the outer wall-clock backstop")
	briefFile := flag.String("brief-file", "", "builder mode: read the seed brief from this file instead of the broker inbox")
	replyTopic := flag.String("reply-topic", "", "builder mode: attach this topic to natural reply posts")
	repoFlag := flag.String("repo", "", "builder mode: dispatched repo (owner/name) for PR-existence verification")
	ticketFlag := flag.String("ticket", "", "builder mode: dispatched ticket, for the builder/<ticket> branch")
	branchFlag := flag.String("branch", "", "builder mode: dispatched branch (defaults to builder/<ticket>)")
	drainMode := flag.Bool("drain", false, "drain/one-shot mode: run ONE autonomous orchestrate drain over shadow's queue (claude -p with the materialised MCPs + gh bridged), then exit. No builder worktree/PR-verify coupling — used by the heartbeat CronJob.")
	drainPrompt := flag.String("drain-prompt", defaultDrainPrompt, "drain mode: the orchestrate drain instruction handed to claude -p")
	roleFile := flag.String("role-file", "", "builder mode: path to the resolved role system-prompt text for this spawn (role-at-spawn overlay; optional — dispatch.Brief.RolePrompt written by BuildJob when a RolePrompt is set). composeSystemPrompt prepends its contents above the (thin) personality.")
	policyFragmentFile := flag.String("policy-fragment-file", "", "builder mode: path to a spawn-supplied funnel.ToolPolicy JSON overlay applied over -policy for this spawn (role-at-spawn overlay; optional — dispatch.Brief.PolicyFragment written by BuildJob when set).")
	acceptanceFile := flag.String("acceptance-file", "", "builder mode: path to the ledger work item's acceptance criteria text (Unit B verified task_done, NET-22/23/24; optional — dispatch.Brief.AcceptanceCriteria written by BuildJob when the work item has a DoD). When set, task_done is verified against this text before the builder is allowed to exit.")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Role-at-spawn overlay (M1 Unit 3): read here, before both the tool
	// policy load and composeSystemPrompt need them. A missing/malformed
	// file at a NON-empty path fails fast (same posture as -policy) —
	// BuildJob only ever passes these flags when the ConfigMap key it
	// names actually exists, so a read failure here means a real bug.
	// Empty flags (the default) leave both zero values → no overlay,
	// exactly matching pre-role-at-spawn behavior.
	var spawnRolePrompt string
	if *roleFile != "" {
		raw, err := os.ReadFile(*roleFile)
		if err != nil {
			fail(log, "read role-file", err)
		}
		spawnRolePrompt = strings.TrimSpace(string(raw))
	}
	var spawnPolicyFragment *funnel.ToolPolicy
	if *policyFragmentFile != "" {
		raw, err := os.ReadFile(*policyFragmentFile)
		if err != nil {
			fail(log, "read policy-fragment-file", err)
		}
		var frag funnel.ToolPolicy
		if err := json.Unmarshal(raw, &frag); err != nil {
			fail(log, "parse policy-fragment-file", err)
		}
		spawnPolicyFragment = &frag
	}
	acceptanceCriteria := readAcceptanceCriteriaFile(*acceptanceFile, log)

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

	// -drain: one-shot autonomous orchestrate drain. Everything claude needs is
	// now in place (.mcp.json materialised in cwd, provider creds applied); run a
	// single `claude -p` over shadow's queue and exit. No funnel serve-loop, no
	// builder worktree/PR-verify. runDrain never returns (it os.Exits).
	if *drainMode {
		runDrain(log, res, *claudePath, *drainPrompt)
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
	// Tier B (role-at-spawn, M1 Unit 3): a spawn-supplied PolicyFragment
	// (dispatch.Brief.PolicyFragment, delivered via -policy-fragment-file)
	// overlays the static -policy file per-spawn rather than per-aspect —
	// see applyPolicyFragment. Computed ONCE here, same as the static file
	// always was; every newBindingHarness call below (incl. the binding
	// refresh loop at re-validate) re-registers this SAME policy value
	// unchanged, so the refresh loop cannot clobber the spawn overlay.
	policy, err := loadToolPolicy(*policyPath, spawnPolicyFragment)
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
	// CW_PROMPT_MODE=replace makes agentfunnel pass the composed system
	// prompt to claude-code via --system-prompt (replacing the CLI's base
	// prompt) instead of --append-system-prompt. Used by the local lane,
	// whose model has no use for claude-code's Anthropic base framing.
	// Any other value (incl. unset) = append (the unchanged default).
	promptMode := bridle.SystemPromptAppend
	if os.Getenv("CW_PROMPT_MODE") == "replace" {
		promptMode = bridle.SystemPromptReplace
	}
	bindingCache := &atomic.Pointer[funnel.Binding]{}
	bindingCache.Store(&funnel.Binding{
		Provider:   bridle.ProviderID(res.Provider),
		Model:      res.Model,
		PromptMode: promptMode,
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
	progressCh := make(chan string, 16)
	recordBuilderProgress := newBuilderProgressReporter(progressCh)

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
					Provider:   bridle.ProviderID(fresh.Provider),
					Model:      fresh.Model,
					PromptMode: promptMode,
					Harness:    newBindingHarness(newProv, fresh.Provider, escalator, policy),
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

	// M1 Unit 5 fix (root cause): wsClient.Run(ctx) — the call that actually
	// dials + registers the WS connection — used to be the LAST statement in
	// this function, invoked only after the entire funnel/filter/tool-runner
	// setup below (hundreds of lines) and after the boot heartbeat had
	// already been Emit()'d. Since Emit()/SendWorkerStatus route through
	// wsasp.Client.SendBestEffort, which requires the connection to already
	// be up, that meant the boot heartbeat ("running" or "spawning") was
	// GUARANTEED to be dropped — Connected() is trivially false before Run()
	// has ever been called — and every subsequent heartbeat raced the
	// dial+register handshake for however long that took. Starting the dial
	// loop here, as early as wsClient exists, gives it the whole span of the
	// remaining setup (escalator, gateways, funnel construction, tool runner,
	// etc.) to reach Ready() before the first heartbeat fires. wsClientRunErr
	// carries Run's terminal error to the wait point that replaces the old
	// blocking call (search for "wsClientRunErr" below).
	wsClientRunErr := make(chan error, 1)
	go func() { wsClientRunErr <- wsClient.Run(ctx) }()

	// P3c: now that the wsasp client exists, build the operator-escalation
	// requester for native providers and re-store the binding cache with
	// an escalator-equipped harness. wsClient.Request blocks on the
	// correlated escalation.decision (no timeout) — the broker relays the
	// request to operators and routes the decision back. claude-code is
	// skipped inside newBindingHarness (it self-supplies tools).
	escalator = &funnel.Escalator{Requester: wsClient, AspectID: res.AspectName}
	bindingCache.Store(&funnel.Binding{
		Provider:   bridle.ProviderID(res.Provider),
		Model:      res.Model,
		PromptMode: promptMode,
		Harness:    newBindingHarness(provider, res.Provider, escalator, policy),
	})

	gateway := wsasp.NewGateway(wsClient)
	chatGateway := funnel.ChatGateway(gateway)
	if *builderMode {
		chatGateway = progressChatGateway{ChatGateway: gateway, progress: recordBuilderProgress}
	}
	// NEX-knowledge-fix (operator 2026-05-27): wire knowledge gateway
	// over WS so remote aspects (harrow, anvil, plumb) can use the
	// search_knowledge / store_knowledge tools. Pre-fix the Knowledge
	// field was nil → CommsRunner returned "knowledge gateway not
	// configured" on every call.
	knowledgeGateway := wsasp.NewKnowledgeGateway(wsClient)
	// onTaskDone/doneSentinel are assigned below, once the acceptance
	// verifier (Unit B — verified task_done, NET-22/23/24) is built
	// alongside the output filter; commsRunner.OnTaskDone is patched in
	// place afterward (same variable, value-copy semantics — see below).
	// seedBrief is populated later (the -brief-file read, further down)
	// but declared here so the OnTaskDone closure can read its
	// ThreadRoot lazily at call time rather than needing it threaded
	// through as an extra parameter.
	var onTaskDone func(string)
	doneSentinel := ""
	var seedBrief bridle.InboxItem
	commsRunner := funnel.CommsRunner{
		Gateway:    chatGateway,
		Knowledge:  knowledgeGateway,
		AspectID:   res.AspectName,
		OnTaskDone: onTaskDone,
	}
	// Spawn + convene_close (NEX-609 / roundtable P3): the native comms
	// surface carries the parent-only tools for non-derived identities —
	// a hand never gets a working Spawner (no sub-of-sub) nor a close
	// seam (hands never facilitate; the broker enforces both).
	if !aspects.IsDerivedName(res.AspectName) {
		commsRunner.Spawner = gateway
		commsRunner.ConveneCloser = gateway
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
	var funnelObsHook funnel.ObservabilityHook = obsHook
	// realOutputTracker captures the CURRENT turn's streamed model text so
	// the verified-task_done gate can judge what the model actually
	// produced, not just its task_done self-report (review finding on
	// Unit B pass 1 — see builderOnTaskDone/verificationInput). Declared
	// here (rather than where it's read, near onTaskDone below) because it
	// must be wrapped into funnelObsHook BEFORE funnel.New so every turn's
	// events reach it.
	var realOutputTracker *builderRealOutputTracker
	// builderInFlight is shared between the progress hook (which feeds it
	// ToolCallStart/ToolCallResult) and startBuilderIdleMonitor below
	// (which reads it on every tick to suspend the stall timer while a
	// tool call is still executing).
	builderInFlight := &builderInFlightTracker{}
	if *builderMode {
		funnelObsHook = progressObservabilityHook{next: obsHook, progress: recordBuilderProgress, inFlight: builderInFlight}
		realOutputTracker = &builderRealOutputTracker{next: funnelObsHook}
		funnelObsHook = realOutputTracker
	}

	// M1 Unit 5 — worker-status heartbeat (PHASE2-DESIGN §5). The
	// turnMetricsTracker wraps whichever observability hook is already
	// wired above (transparent decorator — every call still reaches
	// obsforward/progress reporting unchanged) and additionally counts
	// main-deliberation turns + cumulative tokens for the heartbeat
	// shape. statusEmitter is constructed just below; the tracker's
	// onMainTurnEnd callback closes over the pointer so a turn boundary
	// fires an Emit ("each turn boundary" per the build spec) without a
	// circular-construction ordering problem.
	var statusEmitter *workerStatusEmitter
	metricsTracker := newTurnMetricsTracker(funnelObsHook, func() {
		if statusEmitter != nil {
			statusEmitter.Emit(ctx, "running")
		}
	})
	funnelObsHook = metricsTracker

	statusEmitter = newWorkerStatusEmitter(
		wsClient,
		res.AspectName,
		os.Getenv("CW_ROLE"),
		os.Getenv("CW_PERSONALITY"),
		os.Getenv("CW_WORK_ITEM_ID"),
		detectCLIVersion(*claudePath, log),
		os.Getenv("CW_IMAGE_TAG"),
		processStartedAt,
		func() funnel.Binding { return *bindingCache.Load() },
		func() (bool, time.Time) {
			snap := state.Snapshot()
			return snap.JWT != "" && time.Until(snap.Expires) > 0, snap.Expires
		},
		metricsTracker,
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

	// funnelPtr is declared here (rather than at its original spot below,
	// next to postTurn) so the builder task_done verification gate — wired
	// immediately below — can read funnelPtr.ReceiveSynthetic for the
	// bounded re-prompt path. Both users tolerate the nil window before
	// funnel.New assigns it further down (postTurn's closure already did;
	// the OnTaskDone closure only fires from inside Deliberate, long after
	// funnelPtr is set).
	var funnelPtr *funnel.Funnel

	// Verified task_done + goal-loop completion (Unit B — NET-22/23/24/27):
	// builds the same judge this aspect's output filter uses
	// (buildAcceptanceVerifier mirrors buildAgentFunnelFilter's Spec
	// exactly — one judge credential drives both), then wraps it in ONE
	// builderAcceptanceGate shared by BOTH of a builder's completion paths:
	//
	//  1. the model calls task_done (builderOnTaskDone below), and
	//  2. the goal-loop's own judge classifies a turn FilterClassComplete
	//     with no task_done call at all (builderGoalLoop, wired further
	//     down where the goalCfg/GoalLoop exist).
	//
	// Unit B's first pass only gated path 1. Live evidence 2026-07-05
	// (NET-27) showed path 2 exiting completely ungated: a repo-less brief
	// with deliberately unsatisfiable criteria ("contains the SHA-512 of a
	// password you cannot know") "completed successfully" because the
	// cheap judge that classifies FilterClassComplete only ever sees the
	// task text, never the work item's acceptance criteria — it rated a
	// one-line greeting substantive and the model never even called
	// task_done. One shared gate (rather than two independent ones) means
	// a builder racing both paths in the same turn is judged once, against
	// one reprompt budget — see builderAcceptanceGate's doc comment.
	var acceptanceGate *builderAcceptanceGate
	if *builderMode {
		acceptanceVerifier := buildAcceptanceVerifier(provider, bridle.ProviderID(res.Provider), judgeProviderOverride, judgeModelOverride, judgeEnv, res.Model, log, obsHook)
		var verify taskDoneVerifyFn
		if acceptanceVerifier != nil {
			verify = acceptanceVerifier.Verify
		}
		acceptanceGate = newBuilderAcceptanceGate(acceptanceCriteria, verify)
		onTaskDone = builderOnTaskDone(stop, log, res.AspectName, acceptanceGate,
			func() string {
				if realOutputTracker == nil {
					return ""
				}
				return realOutputTracker.snapshot()
			},
			func() *funnel.Funnel { return funnelPtr },
			func() int64 { return seedBrief.ThreadRoot },
			wsClient, os.Getenv("CW_DISPATCH_RUN_ID"))
		doneSentinel = builderDoneSentinel
		commsRunner.OnTaskDone = onTaskDone
	}

	// Rewriter wiring: default-on for claude-code-flavored providers,
	// no-op otherwise. The session jsonl path is resolved lazily
	// through funnelPtr so funnel session-id rotations (compaction,
	// rewriter-driven reset) are picked up automatically.
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

	systemPrompt := composeSystemPrompt(res, spawnRolePrompt)
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
		ChatGateway:      chatGateway,
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
		ObservabilityHook: funnelObsHook,
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

	if *builderMode && *briefFile != "" {
		b, err := readBriefFile(*briefFile)
		if err != nil {
			log.Error("agentfunnel: brief file unreadable", "err", err)
			os.Exit(1)
		}
		b.Content = withBranchInstruction(b.Content, *repoFlag, builderBranch(*branchFlag, *ticketFlag))
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

	// M1 Unit 5: boot heartbeat (spawning->running — by the time we reach
	// here validation/funnel/wsasp setup is done and the loop is about to
	// start, so "running" is the accurate boot state) plus the ~60s
	// wall-clock ticker for the remainder of this process's life.
	statusEmitter.Emit(ctx, "running")
	statusEmitter.StartHeartbeat(ctx, heartbeatInterval)

	if *builderMode {
		log.Info("agentfunnel: builder liveness configured",
			"idle_timeout", *builderIdleTimeout,
			"job_hard_timeout", *builderTimeout)
		emitBuilderAccepted(ctx, wsClient, log, os.Getenv("CW_DISPATCH_RUN_ID"))
		go startBuilderIdleMonitor(ctx, *builderIdleTimeout, progressCh, builderInFlight, func() {
			runID := os.Getenv("CW_DISPATCH_RUN_ID")
			if err := wsClient.SendDispatchStatus(context.Background(), runID, "failed", builderStalledReason, time.Now().UTC()); err != nil {
				log.Warn("agentfunnel: builder stalled status enqueue failed", "run_id", runID, "err", err)
			}
			stop()
		}, log)
		if builderRepo != nil {
			go watchBuilderGitProgress(ctx, builderRepo.worktree, gitProgressPollInterval, recordBuilderProgress, log)
		}
	}

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
		go builderGoalLoop(ctx, f, log, goalCfg, verifyPR, openPR, stop,
			acceptanceGate, wsClient, os.Getenv("CW_DISPATCH_RUN_ID"))
	} else {
		var onComplete func() bool
		if *builderMode {
			onComplete = builderCompleteCheck(stop, log, res.AspectName, *repoFlag, *ticketFlag, *branchFlag)
		}
		go deliberateLoop(ctx, f, log, onComplete)
	}

	// wsClient.Run(ctx) was started early (see wsClientRunErr above) so the
	// boot heartbeat had a live, registered connection to land on; this wait
	// preserves the old blocking call's terminal-error semantics exactly.
	if err := <-wsClientRunErr; err != nil && !errors.Is(err, context.Canceled) {
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

// withBranchInstruction appends a one-line directive telling the model
// exactly which branch it is expected to work on. NET-46 live evidence:
// anvil-builder committed to its own branch name (anvil/workers-json-flag)
// instead of the conventional builder/<ticket> one; the harness's PR check
// now tolerates that (prExists ticket-search fallback), but instructing the
// model up front is the cheaper fix — most workers will just follow the
// stated branch when told. A repo-less brief (respond-only completion,
// NET-22) or an unresolved branch has nothing to instruct — returns content
// unchanged.
func withBranchInstruction(content, repo, branch string) string {
	if repo == "" || branch == "" {
		return content
	}
	line := fmt.Sprintf("\n\n[HARNESS] Work on branch `%s` — commit and push your changes to this exact branch name so the harness can find your pull request.", branch)
	return content + line
}

func builderIdleTimeoutDefaultFromEnv(getenv func(string) string) time.Duration {
	raw := strings.TrimSpace(getenv("CW_IDLE_TIMEOUT"))
	if raw == "" {
		return defaultBuilderIdleTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultBuilderIdleTimeout
	}
	return d
}

func emitBuilderAccepted(ctx context.Context, wsClient *wsasp.Client, log *slog.Logger, runID string) {
	if runID == "" {
		log.Warn("agentfunnel: builder accepted not emitted; CW_DISPATCH_RUN_ID missing")
		return
	}
	if err := wsClient.SendDispatchStatus(ctx, runID, "accepted", "", time.Now().UTC()); err != nil {
		log.Warn("agentfunnel: builder accepted status enqueue failed", "run_id", runID, "err", err)
		return
	}
	log.Info("agentfunnel: builder accepted status queued", "run_id", runID)
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
// rolePrompt is the role-at-spawn overlay (M1 Unit 3, dispatch.Brief.RolePrompt,
// read from -role-file): the resolved system-prompt text for this
// work-item's assigned role. It is inserted ABOVE the (thin) personality —
// after central (org-wide base knowledge always applies first) but before
// aspect/personality (decoration) — per PHASE2-DESIGN §3 / ROLE-MODEL.md
// §3 ("capability = role + task spec + base knowledge; personality is
// decoration"). Empty rolePrompt (the default — no Role on the brief) is
// dropped from the join exactly like any other empty section, reproducing
// today's prompt exactly.
//
// Empty sections are dropped from the join. Returns "" only when
// every section is empty (legacy / pre-Part-9 Nexus + unprovisioned
// aspect).
func composeSystemPrompt(res *keyfile.ValidationResult, rolePrompt string) string {
	if res == nil {
		return ""
	}
	parts := make([]string, 0, 5)
	if res.CentralNexusMD != "" {
		parts = append(parts, res.CentralNexusMD)
	}
	if rolePrompt != "" {
		parts = append(parts, rolePrompt)
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

// builderAcceptanceRepromptCap bounds how many times a rejected task_done
// gets re-prompted before the builder gives up and exits BLOCKED. Mirrors
// builderPRRepromptCap's rationale: a model that keeps calling task_done
// without actually meeting the criteria must not loop forever — GoalLoop
// MaxTurns and -builder-timeout remain the outer backstops.
const builderAcceptanceRepromptCap = 3

// taskDoneStep is builderAcceptanceGate.Decide's verdict.
type taskDoneStep int

const (
	taskDoneHonor    taskDoneStep = iota // exit success: no criteria, judge unavailable/errored (fail open), or met
	taskDoneReprompt                     // not met, reprompt budget remains — keep the builder running
	taskDoneBlocked                      // not met, budget exhausted — exit BLOCKED, never a silent success
)

// taskDoneDecide is the pure decision core of verified task_done/goal-loop
// completion (Unit B — NET-22/23/24/27). Pulled out of builderAcceptanceGate
// (which does the judge call + the shared-budget bookkeeping) so the
// decision matrix is unit-testable without mocking any of that:
//
//   - no criteria captured on this dispatch, or the verifier is unavailable
//     (judge unbuildable), or the verify call itself errored -> honor.
//     Fail OPEN is the deliberate posture (mirrors CheapModelFilter): the
//     completion path must not hard-depend on the judge, and a dispatch
//     with no captured DoD reproduces today's unconditional-honor behavior
//     exactly (back-compat for non-ledger !dispatch).
//   - met -> honor.
//   - not met, reprompts remain -> reprompt (bounded).
//   - not met, reprompts exhausted -> blocked (honest failure).
func taskDoneDecide(hasCriteria bool, verifierAvailable bool, verifyErr error, met bool, repromptsLeft int) taskDoneStep {
	if !hasCriteria || !verifierAvailable || verifyErr != nil {
		return taskDoneHonor
	}
	if met {
		return taskDoneHonor
	}
	if repromptsLeft > 0 {
		return taskDoneReprompt
	}
	return taskDoneBlocked
}

// taskDoneVerifyFn is the injectable shape of AcceptanceVerifier.Verify —
// builderAcceptanceGate takes this instead of a concrete
// *funnel.AcceptanceVerifier so tests can supply a fake judge without a real
// bridle harness. nil means "no verifier available" (judge unbuildable at
// startup).
type taskDoneVerifyFn func(ctx context.Context, criteria, output string) (funnel.AcceptanceVerdict, error)

// builderAcceptanceGate is the SINGLE arbiter for verified completion,
// shared between the two independent exit paths a builder-mode process can
// take:
//
//  1. the model calls the task_done tool (CommsRunner.runTaskDone ->
//     builderOnTaskDone below), and
//  2. the goal-loop's own cheap judge classifies a turn FilterClassComplete
//     with no task_done call at all (builderGoalLoop/builderDecide).
//
// A prior pass gated ONLY path 1 — live evidence 2026-07-05 (NET-27) showed
// path 2 exiting ungated (a judge that never sees acceptance criteria
// declaring victory on a one-line greeting against unsatisfiable criteria).
// Wiring two INDEPENDENT gates back in would let a builder that races both
// paths (e.g. calls task_done mid-turn while the SAME turn's judge is also
// about to rule complete) get judged twice against two separate reprompt
// budgets — burning 2x the intended budget, or worse, disagreeing (one path
// honors while the other blocks). This type holds ONE decision + ONE
// mutex-guarded reprompt budget so whichever path fires first — or both, in
// a race — consumes the same shared budget and reaches the same verdict for
// the same output.
type builderAcceptanceGate struct {
	mu            sync.Mutex
	criteria      string
	hasCriteria   bool
	verify        taskDoneVerifyFn
	repromptsLeft int
}

// newBuilderAcceptanceGate constructs the shared gate. verify may be nil
// (judge unbuildable at startup) — Decide then always fails open via
// taskDoneDecide's verifierAvailable=false branch, exactly like an empty
// criteria dispatch.
func newBuilderAcceptanceGate(criteria string, verify taskDoneVerifyFn) *builderAcceptanceGate {
	return &builderAcceptanceGate{
		criteria:      criteria,
		hasCriteria:   strings.TrimSpace(criteria) != "",
		verify:        verify,
		repromptsLeft: builderAcceptanceRepromptCap,
	}
}

// Decide runs ONE verification of output — the REAL posted turn output,
// never a bare self-report (NET-24: a task_done summary can claim success
// while the model produced nothing matching the required criteria) —
// against the gate's criteria, and returns the shared decision, consuming
// one unit of the SHARED reprompt budget on taskDoneReprompt. The mutex
// makes this safe even if both completion paths fire close together (task_done
// mid-turn racing the same turn's judge-complete classification).
func (g *builderAcceptanceGate) Decide(ctx context.Context, output string, log *slog.Logger) (taskDoneStep, funnel.AcceptanceVerdict) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var verdict funnel.AcceptanceVerdict
	var verr error
	if g.hasCriteria && g.verify != nil {
		verdict, verr = g.verify(ctx, g.criteria, output)
		if verr != nil {
			log.Warn("agentfunnel: acceptance verification errored — failing open", "err", verr)
		}
	}
	step := taskDoneDecide(g.hasCriteria, g.verify != nil, verr, verdict.Met, g.repromptsLeft)
	if step == taskDoneReprompt {
		g.repromptsLeft--
	}
	return step, verdict
}

// builderOnTaskDone returns the CommsRunner/funnel completion callback for
// builder mode. Live evidence 2026-07-05 (NET-24): keel-builder called
// task_done with a confabulated summary ("0 conflicts, 100% memory match")
// and never produced the required token — the Job "completed successfully"
// because task_done trusted the model's self-report unconditionally. gate
// runs ONE cheap-judge verification (shared with the goal-loop's own exit
// path — see builderAcceptanceGate) before honoring completion.
//
// realOutputFor returns the actual text the model streamed THIS turn (see
// builderRealOutputTracker) — the review finding behind this pass: verifying
// only args.Summary lets a model write "posted CONVERGED-BETA-OK" in the
// tool-call summary without ever having produced it. The real output, when
// available, is folded in as the authoritative signal alongside the
// self-report; an empty real-output snapshot (tracker not wired, or nothing
// streamed yet at the moment task_done fired) degrades to summary-only,
// unchanged from before this pass.
//
// funnelFor/threadRootFor are late-bound accessors (rather than direct
// values) because builderOnTaskDone is constructed before funnel.New and
// before the seed brief is read — both are populated by the time task_done
// can actually fire (Deliberate only runs after the funnel and inbox seed
// are both up), so the indirection just avoids a construction-order
// dependency, not a real race.
func builderOnTaskDone(stop context.CancelFunc, log *slog.Logger, aspect string, gate *builderAcceptanceGate,
	realOutputFor func() string, funnelFor func() *funnel.Funnel, threadRootFor func() int64,
	wsClient *wsasp.Client, runID string) func(string) {
	return func(summary string) {
		output := verificationInput(summary, realOutputFor)
		step, verdict := gate.Decide(context.Background(), output, log)
		switch step {
		case taskDoneHonor:
			log.Info("agentfunnel: builder task_done — exiting", "aspect", aspect, "summary", summary, "verified", gate.hasCriteria && gate.verify != nil)
			stop()
		case taskDoneReprompt:
			log.Info("agentfunnel: task_done rejected — acceptance criteria not met, re-prompting",
				"aspect", aspect, "reason", verdict.Reason)
			if f := funnelFor(); f != nil {
				f.ReceiveSynthetic(bridle.InboxItem{
					From:       "system",
					Source:     "builder_acceptance_check",
					ThreadRoot: threadRootFor(),
					Content: fmt.Sprintf(
						"[CONTINUATION] Your task_done was rejected: the acceptance criteria are not met (%s). "+
							"ACCEPTANCE CRITERIA:\n%s\n\nKeep working and only call task_done again once you have "+
							"genuinely satisfied these criteria — do not re-claim completion without new work to back it up.",
						verdict.Reason, gate.criteria),
				})
			}
		case taskDoneBlocked:
			log.Error("agentfunnel: ESCALATION task_done rejected — acceptance criteria not met and reprompt budget exhausted",
				"aspect", aspect, "reason", verdict.Reason)
			if wsClient != nil {
				if serr := wsClient.SendDispatchStatus(context.Background(), runID, "failed", "acceptance_criteria_not_met", time.Now().UTC()); serr != nil {
					log.Warn("agentfunnel: blocked status enqueue failed", "run_id", runID, "err", serr)
				}
			}
			stop()
		}
	}
}

// verificationInput combines a model's self-report (summary — the
// task_done tool's free-text argument) with the real output streamed this
// turn (realOutputFor, when non-empty), labeling the real output as
// authoritative. Isolated as its own function so the "which signal wins"
// policy is stated once and unit-testable without a gate/harness.
func verificationInput(summary string, realOutputFor func() string) string {
	var real string
	if realOutputFor != nil {
		real = strings.TrimSpace(realOutputFor())
	}
	if real == "" {
		return summary
	}
	return "AGENT SELF-REPORT (task_done summary — may be inaccurate or confabulated):\n" + summary +
		"\n\nACTUAL TURN OUTPUT (authoritative — judge against THIS, not the self-report above):\n" + real
}

// builderPRVerifier returns a pure check: does a PR exist for the builder branch?
// It logs the miss/error but has NO side effects (no stop), so a goal-loop can
// decide for itself what to do next. Fail-closed: a gh error reports false
// (NEX-468 — never declare completion we cannot verify).
func builderPRVerifier(log *slog.Logger, aspect, repo, ticket, branch string) func() bool {
	if repo == "" {
		// Unit B item 3 (respond-only completion, NET-22): a repo-less
		// brief has no PR to gate on — the completion contract for a
		// repo-less dispatch is verified-task_done only (builderOnTaskDone
		// above). Bypass the PR gate unconditionally rather than looping
		// builderPRRepromptCap re-prompts asking the model to open a PR
		// that can never exist; that was exactly the anvil-builder bug
		// (NET-22: judge ruled complete, no repo, blocked after the work
		// was already done and correct).
		return func() bool {
			log.Info("agentfunnel: no repo on brief — PR gate skipped (respond-only completion)", "aspect", aspect, "ticket", ticket)
			return true
		}
	}
	head := builderBranch(branch, ticket)
	return func() bool {
		ok, err := prExists(repo, head, ticket)
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
	out, err := exec.Command("gh", "pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", "url", "-q", ".[0].url").CombinedOutput()
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

// prExistsByTicketFn searches open PRs in repo for one whose head branch
// name or title contains ticket — swappable in tests. NET-46 live evidence:
// anvil-builder committed to its own branch (anvil/workers-json-flag)
// instead of the conventional builder/<ticket> one and opened a real PR
// (#413) that the head-branch-only check above missed entirely.
var prExistsByTicketFn = func(repo, ticket string) (bool, error) {
	out, err := exec.Command("gh", "pr", "list", "--repo", repo, "--state", "open",
		"--json", "number,headRefName,title").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gh pr list (ticket fallback): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return matchPRByTicket(out, ticket)
}

// prTicketSearchEntry is one row of
// `gh pr list --json number,headRefName,title`.
type prTicketSearchEntry struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	Title       string `json:"title"`
}

// matchPRByTicket is the pure decision core of the ticket-search fallback:
// given the raw JSON of `gh pr list`, reports whether any PR's head branch
// name or title contains ticket. Pulled out of prExistsByTicketFn so the
// matching rule is unit-testable without shelling out to gh.
func matchPRByTicket(out []byte, ticket string) (bool, error) {
	var prs []prTicketSearchEntry
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("gh pr list (ticket fallback): parse: %w", err)
	}
	for _, pr := range prs {
		if strings.Contains(pr.HeadRefName, ticket) || strings.Contains(pr.Title, ticket) {
			return true, nil
		}
	}
	return false, nil
}

// prExists reports whether a PR exists for branch in repo. Missing repo/branch
// returns an error so the builder does not exit on an unverifiable
// "complete" (fail-closed toward NEX-468).
//
// Checks the conventional branch head first; when that misses (or errors)
// and ticket is non-empty, falls back to searching open PRs by ticket ID in
// the head branch name or title (NET-46: a worker may have committed to its
// own branch name instead of the conventional one, opening a real PR the
// head-only check can't find).
func prExists(repo, branch, ticket string) (bool, error) {
	if repo == "" || branch == "" {
		return false, fmt.Errorf("prExists: repo/branch not set (repo=%q branch=%q)", repo, branch)
	}
	ok, err := prExistsFn(repo, branch)
	if err == nil && ok {
		return true, nil
	}
	if strings.TrimSpace(ticket) == "" {
		return ok, err
	}
	found, ferr := prExistsByTicketFn(repo, ticket)
	if ferr != nil {
		if err != nil {
			return false, err
		}
		return false, ferr
	}
	if found {
		return true, nil
	}
	return ok, err
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
	builderContinue           builderStep = iota // intermediate goal_not_met — keep pursuing
	builderExit                                  // terminal: verified-complete, blocked, or exhausted
	builderRepromptPR                            // judge says complete but no PR — push back for it
	builderRepromptAcceptance                    // judge says complete but acceptance criteria not met — push back with the criteria
	builderBlockedAcceptance                     // acceptance criteria never met, reprompt budget exhausted — honest failure exit
)

// builderDecide maps a GoalLoop result plus the acceptance/PR-verification
// outcomes to the builder's next step. Pure (no funnel, no I/O) so it is
// unit-testable.
//
// Live evidence 2026-07-05 (NET-27): a repo-less brief with deliberately
// unsatisfiable acceptance criteria ("contains the SHA-512 of a password
// you cannot know") exited via THIS function's judge-complete branch with a
// one-line greeting — the cheap judge that classifies FilterClassComplete
// only ever sees the task text, never the work item's acceptance criteria,
// so it rated the greeting substantive and complete. builderOnTaskDone's
// verified-task_done gate (Unit B pass 1) never fired because the model
// never called task_done at all — it just replied, and the judge alone
// declared victory. acceptance (below) closes that second exit path with
// the SAME taskDoneDecide matrix builderOnTaskDone uses, run against the
// goal-loop's LastFinalText (the actual posted turn output, not a
// self-report) instead of a task_done summary.
//
// Live-reproduced 2026-07-05 08:19 (bounded residual, NET-27 follow-up): a
// model can call task_done mid-turn (REJECTED — not met, reprompt budget
// remains, ctx stays LIVE since only an HONORED task_done cancels ctx) and
// have that SAME turn's judge classify anything OTHER than
// FilterClassComplete (scratch/loop_cap/unknown_class) once the tool round
// finishes. The REJECTED task_done is still a live completion CLAIM this
// gate exists to police — gating acceptance ONLY on reason=="complete" let
// that claim's rejection get silently overridden by an unconditional exit
// the moment the SAME turn's overall reply also happened to read as
// "scratch". acceptance is now decided (in builderGoalLoop) for EVERY
// Done && !Blocked result, not just "complete" — gate on shape
// (Done && !Blocked), not on the reason string, because the gate itself
// fails open when no criteria were ever captured, so over-gating a run
// with no acceptance criteria is a no-op (identical behavior to before this
// pass). Only the PR check remains reason=="complete"-scoped below: a
// scratch/loop_cap/unknown_class turn never claimed a PR-worthy completion,
// so there's still nothing to verify a PR against for those reasons.
//
//   - blocked                                       -> exit (escalated)
//   - not done (intermediate goal_not_met)          -> continue (Pursue enqueued a continuation)
//   - done, acceptance says reprompt                 -> reprompt acceptance (criteria not met, budget remains —
//     regardless of reason: a rejected task_done this turn is a live claim
//     even when the judge separately called the turn scratch/loop_cap/unknown_class)
//   - done, acceptance says blocked                  -> exit BLOCKED (criteria never met, budget exhausted)
//   - done, reason != "complete", acceptance honors  -> exit (no PR gate — no completion claim to verify a PR against)
//   - done, reason "complete", acceptance honors
//     (met / no criteria / verifier unavailable /
//     verify errored — fail open), PR verified        -> exit (success)
//   - done, reason "complete", acceptance honors,
//     no PR, budget                                   -> reprompt for the PR
//   - done, reason "complete", acceptance honors,
//     no PR, budget exhausted                         -> exit
func builderDecide(result funnel.GoalResult, acceptance taskDoneStep, prVerified bool, prRepromptsLeft int) builderStep {
	if result.Blocked {
		return builderExit
	}
	if !result.Done {
		return builderContinue
	}
	switch acceptance {
	case taskDoneReprompt:
		return builderRepromptAcceptance
	case taskDoneBlocked:
		return builderBlockedAcceptance
	}
	// acceptance == taskDoneHonor: met, no criteria captured, verifier
	// unavailable, or verify call errored (fail open) — including the case
	// where this Done reason never carried any completion claim to verify
	// in the first place (no task_done this turn, no criteria captured).
	if result.Reason != "complete" {
		// No PR gate for non-complete reasons — there's no completion
		// claim to verify a PR against (NEX-468/471 unaffected).
		return builderExit
	}
	if prVerified {
		return builderExit
	}
	if prRepromptsLeft > 0 {
		return builderRepromptPR
	}
	return builderExit
}

// builderGoalLoop drives a dispatch builder to its Definition of Done using the
// goal-pursuit loop (NEX-477). Unlike the bare deliberateLoop — which drains
// the inbox and then idles when the judge rules goal_not_met — GoalLoop posts
// the progress reply (ShouldPost) AND enqueues a continuation so the builder
// keeps working toward the DoD until it is met, blocked, or capped. Completion
// is gated TWICE (NET-22/23/24/27): acceptance criteria (when the work item
// carried any — same judge/gate builderOnTaskDone uses, run here against the
// judge-classified-complete turn's actual output) AND the PR actually
// existing (NEX-468/471, when the brief names a repo). Either gate can push
// back a bounded re-prompt before the builder is allowed to exit; the
// -builder-timeout goroutine remains the wall-clock backstop.
func builderGoalLoop(ctx context.Context, f *funnel.Funnel, log *slog.Logger, cfg funnel.GoalConfig, verifyPR func() bool, openPR func() (string, bool), stop context.CancelFunc,
	gate *builderAcceptanceGate, wsClient *wsasp.Client, runID string) {
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

		// Review finding #3 (Unit B fix pass 2) — dual-arbiter race: a
		// builder can call task_done mid-turn (canceling ctx via stop())
		// AND have that SAME turn's judge classify FilterClassComplete, so
		// Pursue can return a normal (non-error) Done result even though
		// completion was ALREADY decided by the task_done path a moment
		// ago. Without this check the goal-loop below would call
		// gate.Decide a second time for output the task_done path already
		// judged — burning an extra unit of the SHARED reprompt budget, or
		// worse, sending its own terminal SendDispatchStatus after
		// task_done's callback already did. Once ctx is canceled, someone
		// else (task_done, the idle monitor, JWT expiry, …) has already
		// made the terminal call — this loop has nothing left to decide.
		if ctx.Err() != nil {
			return
		}

		// Acceptance gate (NET-27, broadened by the 2026-07-05 08:19 bounded
		// residual pass): gated on the RESULT SHAPE (Done && !Blocked), not
		// on result.Reason == "complete". A rejected task_done mid-turn is a
		// live completion CLAIM regardless of what the SAME turn's judge
		// separately classified it as (complete/scratch/loop_cap/
		// unknown_class) — restricting this to reason=="complete" let that
		// claim's rejection get silently overridden the moment the judge
		// happened to call the turn something else. gate.Decide fails open
		// on its own (no criteria captured -> honor unconditionally), so
		// gating every Done && !Blocked exit — rather than trying to guess
		// which Reason values might carry a completion claim — is safe and
		// behaviorally identical to before this pass for any run that never
		// captured acceptance criteria. gl.LastFinalText() is the turn's
		// actual posted reply (what the judge itself just judged), not a
		// task_done self-report — the live NET-27 failure was a judge
		// rating a one-line greeting "complete" against unsatisfiable
		// criteria it never saw; feeding it the real output closes that gap.
		// gate is the SAME builderAcceptanceGate builderOnTaskDone uses —
		// one shared decision + one shared reprompt budget across both
		// completion paths (see builderAcceptanceGate's doc comment).
		acceptance := taskDoneHonor
		var verdict funnel.AcceptanceVerdict
		if result.Done && !result.Blocked {
			acceptance, verdict = gate.Decide(ctx, gl.LastFinalText(), log)
		}

		prVerified := false
		if result.Done && !result.Blocked && acceptance == taskDoneHonor {
			// Only spend a gh call once acceptance (if applicable) already
			// passed — no point checking for a PR on output that doesn't
			// even meet the DoD yet.
			prVerified = verifyPR()
		}

		switch builderDecide(result, acceptance, prVerified, prRepromptsLeft) {
		case builderContinue:
			continue
		case builderRepromptAcceptance:
			log.Info("agentfunnel: completion claimed but acceptance criteria not met — re-prompting",
				"ticket", cfg.TicketID, "reason", verdict.Reason, "goal_loop_reason", result.Reason)
			f.ReceiveSynthetic(bridle.InboxItem{
				From:       "system",
				Source:     "builder_acceptance_check",
				ThreadRoot: cfg.ThreadRoot,
				Content: fmt.Sprintf(
					"[CONTINUATION] Your last reply claimed or implied completion, but it does not satisfy the work item's "+
						"acceptance criteria (%s):\n%s\n\nRevise your work so the criteria above are genuinely met, "+
						"then reply again. If you cannot meet them, say so explicitly and name the blocker.",
					verdict.Reason, gate.criteria),
			})
			continue
		case builderBlockedAcceptance:
			log.Error("agentfunnel: ESCALATION acceptance criteria never met — reprompt budget exhausted",
				"ticket", cfg.TicketID, "reason", verdict.Reason)
			if wsClient != nil {
				if serr := wsClient.SendDispatchStatus(context.Background(), runID, "failed", "acceptance_criteria_not_met", time.Now().UTC()); serr != nil {
					log.Warn("agentfunnel: blocked status enqueue failed", "run_id", runID, "err", serr)
				}
			}
			stop()
			return
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
// Tier A (base): an empty path returns the permissive default
// (DefaultAllow=true) so unconfigured aspects behave exactly as they did
// before the -policy flag existed. A non-empty path is read and
// JSON-decoded into a funnel.ToolPolicy; a missing or malformed file
// returns a wrapped error so startup fails fast rather than silently
// running permissive — a misconfigured policy must be loud.
//
// Tier B (role-at-spawn, M1 Unit 3): fragment, when non-nil, is a
// spawn-supplied ToolPolicy overlay (dispatch.Brief.PolicyFragment,
// delivered via -policy-fragment-file) applied over the Tier-A base by
// applyPolicyFragment. This is the "Tier B" the original comment
// recorded as a follow-on — delivered per-spawn (via the brief) rather
// than centrally in the Nexus/ValidationResult, which was the other
// option considered (see README.md). A nil fragment is a total no-op:
// loadToolPolicy("", nil) and loadToolPolicy(path, nil) behave exactly
// as before this change.
func loadToolPolicy(path string, fragment *funnel.ToolPolicy) (funnel.ToolPolicy, error) {
	base := funnel.ToolPolicy{DefaultAllow: true}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return funnel.ToolPolicy{}, fmt.Errorf("read tool policy %q: %w", path, err)
		}
		var p funnel.ToolPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return funnel.ToolPolicy{}, fmt.Errorf("parse tool policy %q: %w", path, err)
		}
		base = p
	}
	return applyPolicyFragment(base, fragment), nil
}

// applyPolicyFragment overlays a spawn-supplied PolicyFragment onto the
// Tier-A base policy. Precedence, field by field:
//
//   - DefaultAllow always takes the fragment's value when a fragment is
//     present — presence of any fragment means the role made an explicit
//     decision about it (it isn't an optional sub-field like the others).
//   - Tools/Escalate/BashDeny/WritePathAllow: a field the fragment sets
//     (a non-nil map/slice — including an explicit empty one, e.g. an
//     empty write_path_allow for a read-only role) REPLACES the base
//     field outright. A field the fragment OMITS (nil, the Go zero value
//     for an absent JSON key) leaves the base field untouched.
//
// A nil fragment returns base unchanged — the total no-op that preserves
// today's static-file-only behavior.
func applyPolicyFragment(base funnel.ToolPolicy, fragment *funnel.ToolPolicy) funnel.ToolPolicy {
	if fragment == nil {
		return base
	}
	out := base
	out.DefaultAllow = fragment.DefaultAllow
	if fragment.Tools != nil {
		out.Tools = fragment.Tools
	}
	if fragment.Escalate != nil {
		out.Escalate = fragment.Escalate
	}
	if fragment.BashDeny != nil {
		out.BashDeny = fragment.BashDeny
	}
	if fragment.WritePathAllow != nil {
		out.WritePathAllow = fragment.WritePathAllow
	}
	return out
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

// readAcceptanceCriteriaFile reads path (the -acceptance-file flag) and
// returns its trimmed contents, or "" for an empty path OR an unreadable
// file. Unlike -role-file/-policy-fragment-file (which fail() the process
// on a read error), an unreadable acceptance file WARNS and falls open
// (empty criteria) rather than crashing. Review finding: a ConfigMap-mount
// race (the volume not yet materialized when agentfunnel starts) is a real,
// recoverable class of failure for k8s-mounted files, and this file's
// entire purpose is a fail-OPEN verification gate — crashing the builder
// over a missing acceptance.md would be exactly backwards (turning an
// optional stricter check into a hard startup dependency).
func readAcceptanceCriteriaFile(path string, log *slog.Logger) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Warn("agentfunnel: acceptance-file unreadable — proceeding with no acceptance criteria (fail open)", "path", path, "err", err)
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// buildAcceptanceVerifier constructs the verified-task_done judge (Unit B —
// NET-22/23/24) with the IDENTICAL judge.Spec buildAgentFunnelFilter uses,
// so a single configured judge credential drives both the output filter and
// the task_done verification gate — no separate "acceptance judge" knob for
// operators to configure. May return nil (judge unbuildable); callers must
// treat nil as "unavailable" and fail open (see judge.BuildAcceptanceVerifier).
func buildAcceptanceVerifier(provider bridle.Provider, providerID bridle.ProviderID, judgeProviderOverride, judgeModelOverride string, providerEnv map[string]string, mainModel string, log *slog.Logger, obsHook funnel.ObservabilityHook) *funnel.AcceptanceVerifier {
	return judge.BuildAcceptanceVerifier(judge.Spec{
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
	// Spawn + convene_close (NEX-609 / roundtable P3): parents only — a
	// derived (hand) identity never sees them, mirroring nexus-comms-mcp's
	// spawnToolAvailable gate.
	if !aspects.IsDerivedName(aspectName) {
		defs = append(defs, funnel.SpawnToolDef(), funnel.ConveneCloseToolDef())
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
