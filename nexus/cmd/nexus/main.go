// Command nexus is the central Nexus process: broker, orchestrator, and
// (future) embedded frame-agent. v1 covers broker + in-memory roster +
// the stale-reap sweep; the orchestrator and frame-agent slots in as
// later spec migration steps.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	bridle "github.com/CarriedWorldUniverse/bridle"
	claudeprovider "github.com/CarriedWorldUniverse/bridle/provider/claude"
	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
	"github.com/CarriedWorldUniverse/ledger"
	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/autospawn"
	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/framecomms"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/judge"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel/rewriter"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/route"
	"github.com/CarriedWorldUniverse/nexus/nexus/handqueue"
	"github.com/CarriedWorldUniverse/nexus/nexus/identity"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability/jsonlsink"
	"github.com/CarriedWorldUniverse/nexus/nexus/operator"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/sessions"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/nexus/usage"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// exitCodeBootstrapDone signals a successful first-boot setup. Supervisor
// scripts (or operator) restart the process; on the next boot, the new
// Frame is detected and Nexus comes up in normal mode.
const exitCodeBootstrapDone = 64

func main() {
	// Subcommand dispatch — `nexus cert <verb>` and `nexus identity <verb>`
	// peel off here before the broker flagset is parsed. Other subcommands
	// land beside them.
	if len(os.Args) >= 2 && os.Args[1] == "cert" {
		os.Exit(runCertSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "identity" {
		os.Exit(runIdentitySubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "aspect" {
		os.Exit(runAspectSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "personality" {
		os.Exit(runPersonalitySubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "migrate" {
		os.Exit(runMigrateSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "admin" {
		os.Exit(runAdminSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "operator" {
		os.Exit(runOperatorSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "credential" {
		os.Exit(runCredentialSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "init" {
		os.Exit(runInitSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "test-provider" {
		os.Exit(runTestProviderSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "triage-tickets" {
		os.Exit(runTriageTicketsSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "close-merged-tickets" {
		os.Exit(runCloseMergedTicketsSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "triage-prs" {
		os.Exit(runTriagePRsSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "comms-digest" {
		os.Exit(runCommsDigestSubcommand(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "activity-summary" {
		os.Exit(runActivitySummarySubcommand(os.Args[2:]))
	}
	addr := flag.String("addr", ":7888", "broker listen address")
	tokenEnv := flag.String("token-env", "NEXUS_TOKEN", "env var holding the shared bearer token")
	staleAfter := flag.Duration("stale-after", 30*time.Second, "aspect becomes stale after this gap without heartbeat")
	reapEvery := flag.Duration("reap-every", 10*time.Second, "how often to sweep for stale aspects")
	dataDir := flag.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	aspectDir := flag.String("aspect-dir", "", "comma-separated directories to scan for aspect homes (falls back to NEXUS_ASPECT_DIR env; disabled if neither set). The broker uses this as the source of truth for aspect homes (#21).")
	harnessPath := flag.String("harness-path", "", "path to the agentfunnel binary used for auto-spawn (falls back to NEXUS_HARNESS env)")
	agoraPath := flag.String("agora-path", "", "path to the agora binary used for auto-spawn when aspect primary_surface=agora (falls back to NEXUS_AGORA env)")
	// Defaults from env so explicit `--tls-cert=` (empty) is honored
	// as the operator's intent (fail-fast at broker startup) rather
	// than silently falling back to env.
	tlsCert := flag.String("tls-cert", os.Getenv("NEXUS_TLS_CERT"), "path to TLS server cert PEM (default: NEXUS_TLS_CERT env). Required.")
	tlsKey := flag.String("tls-key", os.Getenv("NEXUS_TLS_KEY"), "path to TLS server key PEM (default: NEXUS_TLS_KEY env). Required.")
	// Dev override: serve the dashboard SPA from this on-disk directory
	// instead of the embedded copy baked into nexus.exe. Point at the
	// static/dashboard tree (the one containing index.html). When unset,
	// the embedded copy is used. Production deployments leave this empty.
	dashboardDir := flag.String("dashboard-dir", os.Getenv("NEXUS_DASHBOARD_DIR"), "serve dashboard SPA from this on-disk directory instead of the embedded copy (dev override; default: NEXUS_DASHBOARD_DIR env)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// First-boot detection. If no Frame personality exists, Nexus comes
	// up in bootstrap mode (HTTP shell only — no broker, no aspects, no
	// database) until the operator runs the setup wizard. After a
	// successful setup, exit with exitCodeBootstrapDone so the supervisor
	// restarts the process and Nexus boots in normal mode with the new
	// Frame attached. See docs/2026-04-28-frame-role-spec.md §5.
	resolvedAspectDir := resolveAspectDir(*aspectDir)
	var detectedFrame *frame.FrameAspect
	if resolvedAspectDir != "" {
		detected, derr := frame.Detect(resolvedAspectDir)
		if derr != nil {
			logger.Error("frame detect failed", "err", derr, "agents_dir", resolvedAspectDir)
			os.Exit(1)
		}
		if detected.Frame == nil {
			logger.Info("frame: bootstrap mode — no Frame personality found", "agents_dir", resolvedAspectDir)
			berr := frame.Run(ctx, frame.BootstrapConfig{
				Addr:      *addr,
				AgentsDir: resolvedAspectDir,
				Logger:    logger,
			})
			if berr == nil {
				logger.Info("frame: bootstrap complete — exiting for restart")
				os.Exit(exitCodeBootstrapDone)
			}
			if errors.Is(berr, context.Canceled) {
				logger.Info("frame: bootstrap interrupted")
				os.Exit(0)
			}
			logger.Error("frame: bootstrap failed", "err", berr)
			os.Exit(1)
		}
		detectedFrame = detected.Frame
		logger.Info("frame: detected", "name", detectedFrame.Name, "path", detectedFrame.Path)
	}

	// Normal mode from here on.
	token := os.Getenv(*tokenEnv)
	if token == "" {
		logger.Error("missing auth token", "env_var", *tokenEnv)
		os.Exit(2)
	}

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		logger.Error("storage open failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// Application-layer identity — required for keyfile decryption +
	// JWT signing. Operator runs `nexus identity init` once per Nexus;
	// boot fails loud if missing (don't auto-init: nexus_id must be
	// stable across restarts so existing keyfiles continue to validate).
	// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md.
	nexusIdentity, err := identity.Load(ctx, db)
	if err != nil {
		if errors.Is(err, identity.ErrNotInitialized) {
			logger.Error("nexus identity not initialised — run `nexus identity init` to populate the application-layer identity row before starting the broker")
			os.Exit(2)
		}
		logger.Error("identity load failed", "err", err)
		os.Exit(1)
	}
	logger.Info("nexus identity loaded", "nexus_id", nexusIdentity.NexusID)

	// Personality decomposition Part 9a: seed the central
	// nexus_settings.nexus_md from existing aspect content if it
	// hasn't been initialised yet. Idempotent — skips when populated.
	// Per spec §6 (revised) this is a soft migration: per-aspect rows
	// are left untouched; operator manually prunes duplicates.
	preferredFrame := ""
	if detectedFrame != nil {
		preferredFrame = detectedFrame.Name
	}
	if mres, merr := aspects.MigrateCentralFromAspect(ctx, db, preferredFrame); merr != nil {
		logger.Warn("nexus_settings migration: failed (continuing)", "err", merr)
	} else if mres.Skipped {
		logger.Debug("nexus_settings migration: skipped", "reason", mres.Reason)
	} else {
		logger.Info("nexus_settings migration: seeded central nexus_md",
			"from", mres.SeededFrom,
			"bytes", mres.ContentBytes,
			"divergent_aspects", mres.DivergentAspects)
		if len(mres.DivergentAspects) > 0 {
			logger.Warn("nexus_settings migration: aspects with divergent nexus_md content",
				"aspects", mres.DivergentAspects,
				"hint", "manually prune via `nexus personality edit <name>` to keep only aspect-specific deltas")
		}
	}

	// Build the keyfile validator (Part 4b). When wired into
	// broker.Config below, this enables GET /api/nexus_id and POST
	// /api/aspect/validate per spec §5.
	keyfileValidator := &broker.KeyfileValidator{
		NexusID:              nexusIdentity.NexusID,
		ServerEd25519Pubkey:  nexusIdentity.ServerPublicKey,
		ServerEd25519Privkey: nexusIdentity.ServerPrivateKey,
		SessionSigningSecret: nexusIdentity.SessionSigningSecret,
		Store:                aspects.NewSQLStore(db),
		Settings:             aspects.NewSQLSettingsStore(db), // Part 9
		// 24h: passkey is the strong credential; this JWT is a session
		// bridge between WebAuthn ceremonies. 24h matches the operator
		// expectation of "reauth once a day" without re-prompting on
		// every refresh. Tightening to <24h breaks the workday session;
		// loosening to 7d+ stretches blast radius if a token leaks.
		JWTTTL: 24 * time.Hour,
	}

	// Broker-mediated credentials store (task #218). Keys are encrypted
	// at rest with a data key derived from the session signing secret
	// via HKDF — so the same key material that signs JWTs also gates
	// access to API credentials. If derivation fails (empty secret,
	// nil db) we log and continue with a nil store; admin endpoints
	// gracefully report "credentials store not configured" rather than
	// taking down the broker.
	credentialStore, err := credentials.NewStore(db, nexusIdentity.SessionSigningSecret)
	if err != nil {
		logger.Warn("credentials store unavailable", "err", err)
		credentialStore = nil
	}
	// Migrate the legacy `provider_credentials` table into the post-NEX-75
	// `credentials` table on first boot after the schema rename. No-op on
	// fresh DBs and on already-migrated DBs. Failure here is fatal: we'd
	// rather not boot than silently lose stored credentials. See
	// credentials/migrate.go for the idempotency guards.
	if credentialStore != nil {
		if err := credentialStore.MigrateLegacyTable(ctx); err != nil {
			logger.Error("credentials legacy-table migration failed", "err", err)
			os.Exit(1)
		}
	}

	// NEX-169: wire the credentials store into the keyfile validator
	// so /api/aspect/validate resolves the aspect's mcp_profile and
	// includes it in the response. Set post-construction because
	// credentialStore is built downstream of keyfileValidator above
	// (HKDF needs the same SessionSigningSecret). Nil-tolerant: when
	// the store init failed and credentialStore is nil, the validator
	// emits an empty mcp_profile field (legacy shape).
	keyfileValidator.Credentials = credentialStore

	r := roster.New()
	proj := sessions.New(db)

	// Per-aspect token store. Resolved aspect IDs come from the
	// autospawn discovery pass — those are the aspects this Nexus
	// is responsible for bringing up, and therefore the ones whose
	// tokens we mint/load at boot. Aspects that register later via
	// the WS surface but weren't on the autospawn list resolve via
	// the legacy master token until reconciled (deliberate graceful
	// degrade; cleanup tracked separately).
	tokenStore := broker.NewTokenStore()
	// Frame-role aspects are excluded by autospawn.Discover (which
	// discoverAspectIDs delegates to), so the Frame name will not appear
	// in aspectIDs. The Frame's admin token is reconciled separately via
	// frame.Embed below; if the filter is ever relaxed, this would
	// silently double-reconcile the Frame as a non-admin first.
	aspectIDs := discoverAspectIDs(*aspectDir, logger)
	if len(aspectIDs) > 0 {
		if err := tokenStore.ReconcileAgentTokens(ctx, db, aspectIDs); err != nil {
			logger.Error("token reconcile (aspects)", "err", err)
			os.Exit(1)
		}
		logger.Info("token reconcile (aspects)", "count", len(aspectIDs))
	}
	// Frame embedding (P5). When Detect found a Frame, instantiate it
	// as an in-process aspect — registers in roster with admin=true,
	// reconciles its admin token. Used by P6 (deliberation loop) and
	// P7 (admin REST endpoints) downstream. When Detect found no Frame
	// (resolvedAspectDir was unset), fall back to the legacy default
	// "frame" identity so legacy callers using NEXUS_TOKEN continue
	// resolving to an admin identity.
	var embeddedFrame *frame.EmbeddedFrame
	if detectedFrame != nil {
		ef, err := frame.Embed(ctx, frame.EmbedConfig{
			Detected:         detectedFrame,
			Roster:           r,
			TokenStore:       tokenStore,
			DB:               db,
			Logger:           logger,
			PersonalityStore: aspects.NewSQLStore(db),         // spec §11
			SettingsStore:    aspects.NewSQLSettingsStore(db), // Part 9
		})
		if err != nil {
			logger.Error("frame embed failed", "err", err)
			os.Exit(1)
		}
		embeddedFrame = ef
		// NEX-263: apply per-aspect model + credential overrides from
		// the credentials store on top of the keyfile-loaded cfg.
		// Mutates ef.Aspect.Config in place so downstream funnel
		// builders (buildChatRouter, buildFrameCheapFilter,
		// buildRewriterRunner) read the override-resolved values.
		applyAspectModelOverrides(ctx, &embeddedFrame.Aspect.Config, credentialStore, logger)
	} else {
		// Pre-§6.5 fallback: no aspect dir configured, so no Frame to
		// embed. Reconcile the legacy "frame" identity so existing
		// callers using NEXUS_TOKEN keep resolving to an admin identity.
		// Operators should set --aspect-dir / NEXUS_ASPECT_DIR to get a
		// real Frame embedded.
		logger.Warn("frame: no aspect dir configured; using legacy frame token — set --aspect-dir for §6.5 Frame embedding")
		if _, err := tokenStore.ReconcileFrameToken(ctx, db); err != nil {
			logger.Error("token reconcile (frame, legacy)", "err", err)
			os.Exit(1)
		}
	}
	// P6: build the deliberation funnel from the embedded Frame. The
	// funnel is the bridge between incoming chat frames and the Frame's
	// AI personality. When no Frame is embedded (legacy mode), chatRouter
	// stays nil and the broker logs + drops chat.send frames.
	chatStore := chat.NewSQLStore(db)
	triageStore := chat.NewSQLTriageStore(db)
	knowledgeStore := knowledge.New(db, logger)
	// Phase E: pre-construct the observability Hub so the embedded
	// Frame's funnel can hold its Grouper at construction time.
	// onFrame stays nil here; broker.New rewires it once the broker
	// exists. Until then any emit is a silent drop — safe because no
	// turns can run before the broker is up.
	obsHub := observability.NewHub(500, nil)

	// NEX-144: bring up the ledger issue-tracker service alongside the
	// broker. ledger.db lives parallel to broker's nexus.db in the
	// resolved data directory; the broker mounts /healthz/ledger on its
	// HTTPS listener via the HTTPRegistrar hook below.
	resolvedDataDir := filepath.Dir(storage.ResolvePath(*dataDir))
	ledgerSvc, err := ledger.New(ctx, ledger.Config{
		DBPath: filepath.Join(resolvedDataDir, "ledger.db"),
	})
	if err != nil {
		logger.Error("ledger service init failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := ledgerSvc.Close(); err != nil {
			logger.Warn("ledger close", "err", err)
		}
	}()
	logger.Info("ledger service initialised", "db", filepath.Join(resolvedDataDir, "ledger.db"))

	chatRouter, frameGateway, frameFunnel := buildChatRouter(ctx, embeddedFrame, r, chatStore, triageStore, usage.NewSQLStore(db), knowledgeStore, obsHub, credentialStore, logger, ledgerSvc)

	// Adapter: handqueue.AspectTokenResolver / autospawn.AspectTokenResolver
	// over the broker's TokenStore. TokenForAgent returns "" on miss; we
	// surface that as (_, false) so SpawnExecutor / autospawn can fall
	// back to the legacy NEXUS_TOKEN in their respective ExtraEnv/BaseEnv.
	// Deliberate transition pattern: an aspect that registers without a
	// reconciled token still spawns under the master token until the
	// next reconcile pass picks it up. Drop the fallback once all
	// aspects are reconciled (separate cleanup task).
	tokenResolverFunc := func(aspect string) (string, bool) {
		t := tokenStore.TokenForAgent(aspect)
		if t == "" {
			return "", false
		}
		return t, true
	}

	// Hand dispatch queue. Executor spawns harness subprocesses in
	// hand mode. Resolves aspect home paths from the roster — v1
	// only dispatches to aspects whose home is on this Nexus host;
	// cross-host hand dispatch lands when Outposts gain their own
	// queues.
	// HardCeiling defaults to roster_size + 1 per spec §2.1, computed
	// once at startup. Roster grows via registration; restart picks up
	// any size change. v0.1 defaults to soft+1 if no roster is yet
	// populated (early boot before aspects connect) — handqueue's
	// constructor will further bump to MaxConcurrent+1 if needed.
	hardCeiling := len(r.List()) + 1
	queue, err := handqueue.New(handqueue.Config{
		MaxConcurrent: 3,
		HardCeiling:   hardCeiling,
		Executor: &handqueue.SpawnExecutor{
			HomeResolver: handqueue.AspectHomeResolverFunc(func(aspect string) (string, bool) {
				for _, a := range r.List() {
					if a.Name == aspect {
						return a.Home, true
					}
				}
				return "", false
			}),
			TokenResolver: handqueue.AspectTokenResolverFunc(tokenResolverFunc),
			ExtraEnv: []string{
				// Legacy fallback: when TokenResolver returns false for
				// an unknown aspect, this NEXUS_TOKEN is still in the
				// child env (spec'd as the master back-compat path).
				"NEXUS_TOKEN=" + token,
			},
		},
		Logger: logger,
	})
	if err != nil {
		logger.Error("handqueue.New", "err", err)
		os.Exit(1)
	}

	// Admin callbacks (#79 lock — REST-only admin surface). Wired only
	// when an EmbeddedFrame is present, since admin operations belong
	// to the Frame per spec §3.3. Shutdown is the only fully-implemented
	// op at P7; compact/rewind ship as 501 not_implemented (REST shape
	// locked, implementations land when the underlying machinery does).
	var adminCallbacks *broker.AdminCallbacks
	if embeddedFrame != nil {
		adminCallbacks = &broker.AdminCallbacks{
			Shutdown: func(_ context.Context) error {
				logger.Info("frame: admin shutdown requested")
				stop() // cancels the signal-notify ctx → broker's ListenAndServe returns
				return nil
			},
			DispatchStatus: func(_ context.Context) (broker.DispatchStatusReport, error) {
				stats := queue.Stats()
				busy := make([]string, 0, len(stats.ActiveByAspect))
				for a := range stats.ActiveByAspect {
					busy = append(busy, a)
				}
				return broker.DispatchStatusReport{
					ActiveWorkers: stats.ActiveTotal,
					SoftCap:       stats.SoftCap,
					HardCeiling:   stats.HardCeiling,
					QueueDepth:    stats.QueueDepth,
					BusyAspects:   busy,
				}, nil
			},
			// Compact / Rewind: nil → 501 not_implemented. Lands in P9
			// or follow-up parts when the underlying session-storage
			// surfaces those operations.
		}
	}

	// RecipientPolicy: who receives chat.deliver for each chat.send.
	// Used both by live fan-out (broker.HandleChatSend) and Lock 6
	// replay (broker.Replayer). One policy, two consumers.
	frameName := ""
	if embeddedFrame != nil {
		frameName = embeddedFrame.Aspect.Name
	}
	recipientPolicy := &broker.RecipientPolicy{
		Parent: func(parentID int64) (string, error) {
			msg, err := chatStore.GetByID(ctx, parentID)
			if err != nil {
				return "", err
			}
			return msg.From, nil
		},
		Aspects: func() []string {
			return r.AspectNames()
		},
		ThreadParticipants: func(msgID int64) ([]string, error) {
			return chatStore.ThreadParticipants(ctx, msgID)
		},
		FrameName: frameName,
	}

	// Lock 6 replay engine. Aspects reconnecting with since_msg_id > 0
	// trigger a query against chatStore for messages addressed to them
	// since the cursor; broker emits each as a chat.deliver with
	// Replay=true. Same RecipientPolicy as the live path so replay
	// shape matches what the aspect would have seen if it had been
	// online (modulo @-mention semantics that depend on live state).
	replayer := broker.NewReplayer(chatStore, *recipientPolicy)

	// Legacy-master opt-in (#31): operators upgrading from the
	// pre-per-aspect-token world set NEXUS_ALLOW_LEGACY_MASTER=1 to
	// keep their existing NEXUS_TOKEN-based deployments working
	// during migration. Default off.
	allowLegacy := os.Getenv("NEXUS_ALLOW_LEGACY_MASTER") == "1"

	// Operator auth bypass (dev-only): when NEXUS_AUTH_BYPASS=1, the
	// broker accepts /connect upgrades without an operator token and
	// the HTTP login endpoints return a stub success. Lets remote
	// testing run without WebAuthn while the SPA is still in flux.
	// SECURITY: never enable in production — there is no replacement
	// authn for operator-role connections when this is on.
	authBypass := os.Getenv("NEXUS_AUTH_BYPASS") == "1"
	if authBypass {
		logger.Warn("operator auth bypass ENABLED — DO NOT use in production",
			"reason", "NEXUS_AUTH_BYPASS=1 set in environment")
	}

	// #21: derive canonical aspect homes from the discovery scan so
	// the register handler can override payload.Home (closes the
	// cmd.Dir control vector for stolen aspect tokens).
	aspectHomes := discoverAspectHomes(*aspectDir, logger)

	b := broker.New(broker.Config{
		Addr:               *addr,
		AuthToken:          token,
		AllowLegacyMaster:  allowLegacy,
		OperatorAuthBypass: authBypass,
		Tokens:             tokenStore,
		StaleAfter:         *staleAfter,
		Logger:             logger,
		Projection:         proj,
		HandQueue:          queue,
		Admin:              adminCallbacks,
		ChatRouter:         chatRouter,
		Replayer:           replayer,
		ChatStore:          chatStore,
		RecipientPolicy:    recipientPolicy,
		AspectHomes:        aspectHomes,
		TLSCertFile:        *tlsCert,
		TLSKeyFile:         *tlsKey,
		DashboardDir:       *dashboardDir,
		KeyfileValidator:   keyfileValidator,
		// NEX-367 follow-up: the session secret the keyfile validator
		// signs aspect JWTs with, surfaced at broker level so aspect
		// /connect can verify them even on a headless/aspect-only broker
		// (OperatorLogin nil because NEXUS_OPERATOR_RPID is unset).
		SessionSigningSecret: nexusIdentity.SessionSigningSecret,
		// Knowledge store powers operator-facing knowledge frames
		// (knowledge.list / knowledge.search / knowledge.store) on the
		// dashboard's WS surface. Same store the bridle tool runner
		// uses for aspects (Crossing Part 4); operator reads the same
		// rows via a different transport.
		KnowledgeStore: knowledgeStore,
		// Task #218: broker-mediated credentials. Nil-safe — admin
		// routes register only when non-nil, otherwise return 503.
		Credentials: credentialStore,
		// Spec §11: REST/CLI personality edits trigger an in-process
		// refresh on the embedded Frame so the new prompt takes effect
		// on the next deliberation turn. Non-Frame aspects pick up at
		// next JWT re-validation; broadcast frame is a future Part.
		OnPersonalityChange: buildOnPersonalityChange(ctx, embeddedFrame, logger),
		// Spec §5: nexus_md changes are network-wide (every aspect's
		// composed prompt includes central content). The in-process
		// Frame's cached central is refreshed via the callback below;
		// remote aspects pick up at next JWT re-validation. Future
		// broadcast frame (`personality.refresh`) will hook in here too.
		OnNexusMDChange: buildOnNexusMDChange(ctx, embeddedFrame, logger),
		// Operator login (dashboard-ws-port spec §2.2 / 5b1).
		// Constructed only when the Nexus has identity (signing
		// secret available) AND the operator endpoints are wanted.
		// We build it unconditionally when KeyfileValidator is
		// present — same prerequisite — so the dashboard SPA can
		// reach /api/operator/* once the broker is up.
		OperatorLogin: buildOperatorLogin(db, nexusIdentity.NexusID, nexusIdentity.SessionSigningSecret, *addr, logger),
		// Phase E: adopt the Hub already wired into the funnel so
		// emitted frames reach the broker's broadcast path.
		Observability: obsHub,
		// NEX-144 Phase 0: mount ledger's healthz on the broker's TLS
		// listener. The registrar runs inside ListenAndServe with the
		// broker's internal mux, so /healthz/ledger lives alongside
		// /health and /api/* on the same port.
		//
		// NEX-208 / NEX-270: mount the ledger's /api/issues/* endpoints
		// on the same listener so nexus-jira-mcp's dual-write shim
		// (and future ledger UI clients) can reach the issue tracker
		// over HTTPS rather than via a separate process. We forward to
		// ledgerSvc.Handler() — its internal mux dispatches GET/POST/
		// PATCH/DELETE on the issue subpaths. Ledger's /api/admin/*
		// routes are deliberately NOT forwarded; nexus owns that prefix
		// for credential + aspect admin (NEX-76, NEX-263 et al.) and the
		// two surfaces are kept distinct.
		HTTPRegistrar: func(mux *http.ServeMux) {
			mux.Handle("/healthz/ledger", ledgerSvc.HealthzHandler())
			ledgerHandler := ledgerSvc.Handler()
			mux.Handle("/api/issues", ledgerHandler)
			mux.Handle("/api/issues/", ledgerHandler)
			mux.Handle("/healthz/issues", ledgerHandler)
		},
	}, r)

	// Activity log persistence: chain a jsonlsink writer onto the Hub's
	// fan-out alongside the live broker broadcast. Co-exists with
	// in-memory tail (Hub.Buffer) — adds a durable file-per-aspect-
	// per-day record so post-incident debugging has evidence to read.
	// Writes happen on background goroutines per aspect; channel-full
	// drops with logged warning rather than blocking emit. Closed by
	// the cleanup goroutine on shutdown.
	activityLogDir := filepath.Join(*dataDir, "activity")
	activitySink, err := jsonlsink.New(activityLogDir, logger)
	if err != nil {
		logger.Warn("activity-log sink disabled", "err", err)
	} else {
		brokerBroadcast := b.BroadcastObserveFrame
		obsHub.SetOnFrame(func(aspect string, f observability.Frame) {
			brokerBroadcast(aspect, f)
			activitySink.OnFrame(aspect, f)
		})
		logger.Info("activity log persistence enabled", "dir", activityLogDir)
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := activitySink.Close(closeCtx); err != nil {
				logger.Warn("activity-log sink: close failed", "err", err)
			}
		}()
	}

	// Wire the embedded Frame's chat gateway to broker.HandleChatSend so
	// in-process Frame posts persist + fan-out via the same canonical
	// path as out-of-process aspect WS frames (per
	// docs/2026-05-04-unify-frame-aspect-chat-path.md). When no Frame
	// is embedded, frameGateway is nil and this is a no-op.
	if frameGateway != nil {
		frameGateway.Sender = b
		// ReactBroadcaster: same reason as Sender — in-process Frame
		// reactions (the funnel's 👀/👍/🙊 work-signal path) must push
		// chat.reaction.update to operators or the dashboard never
		// sees them. Without this, Frame keel looks silent even when
		// the funnel is firing reactions on every turn. See #193.
		frameGateway.ReactBroadcaster = b
	}

	// Stale-reap sweep. Runs until ctx cancels. Reaper queries the
	// broker's dispatcher to refresh heartbeats for live WS-connected
	// aspects before the sweep — under the WS transport an open
	// connection IS the heartbeat per Lock 2.
	go reaper(ctx, r, b, *staleAfter, *reapEvery, logger)

	// File-replacement watcher (Crossing pre-cutover hardening).
	//
	// On Windows in particular, if the on-disk nexus.db is replaced by
	// another process while the broker holds it open, the broker keeps
	// writing to an orphaned inode (a "phantom"). Same-process read-back
	// stays consistent with the phantom; external readers see the
	// pre-replacement state, frozen. This bit agent-network for ~5 days
	// in 2026-05; ~400 chat messages were lost on broker restart.
	//
	// The watcher captures a FileInfo baseline at startup and re-stats
	// every DefaultWatchInterval. On divergence (replacement OR deletion),
	// it cancels the broker context — the broker exits, main returns,
	// the supervisor restarts cleanly with a fresh handle to whatever's
	// at the path. Cheap (stat-only, no SQL, no fresh DB open). Pairs
	// with the §6.2 fresh-handle write verifier (Part 2) for
	// defence-in-depth on subtler write-loss modes.
	go func() {
		dbPath := storage.ResolvePath(*dataDir)
		err := storage.WatchFileReplacement(ctx, dbPath, 0 /*default interval*/, logger, stop)
		// Three exit paths:
		//   ErrFileReplaced — phantom detected. stop() was already
		//     called via the onReplaced callback; broker is winding
		//     down. Log CRIT-level so the supervisor's log scrape
		//     surfaces this distinctly from a normal shutdown.
		//   context.Canceled / DeadlineExceeded — clean shutdown,
		//     watcher exiting alongside everything else. No-op.
		//   Other (e.g. stat error at startup) — log loud but don't
		//     panic; the broker can still run, the watcher just isn't
		//     guarding it. Rare.
		if errors.Is(err, storage.ErrFileReplaced) {
			logger.Error("storage watcher: phantom-handle mode detected — broker shutting down for supervisor restart", "path", dbPath)
		} else if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("storage watcher: stopped with non-fatal error", "err", err, "path", dbPath)
		}
	}()

	// Fresh-handle write verifier (Crossing Part 2) — defence-in-depth
	// against write-loss modes that don't manifest as file replacement.
	// Every DefaultVerifyInterval (60s) opens a fresh sql.DB, queries
	// MAX(id) FROM chat_messages, and compares against the live broker
	// handle. Live > fresh = phantom. Less frequent than the file-
	// replacement watcher (which is the cheap fast path) and more
	// expensive (fresh DB connection per tick), but catches WAL desync,
	// partial-write rollback, and long-handle-with-FS-mismatch that
	// stat-only detection misses.
	go func() {
		dbPath := storage.ResolvePath(*dataDir)
		err := storage.WatchWriteDurability(ctx, dbPath, db, 0 /*default interval*/, logger, stop)
		if errors.Is(err, storage.ErrWriteDurabilityFailed) {
			logger.Error("storage verifier: write-durability failure detected — broker shutting down for supervisor restart", "path", dbPath)
		} else if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("storage verifier: stopped with non-fatal error", "err", err, "path", dbPath)
		}
	}()

	// Embedded-frame heartbeat (#133). The in-process Frame has no WS
	// connection of its own, so the reaper's WS-connected refresh
	// doesn't see it. Tick its Heartbeat directly; the funnel is alive
	// for as long as cmd/nexus is alive, so the heartbeat is just a
	// liveness marker for the roster. Half the staleAfter window so
	// reaper sweeps never catch a transient gap.
	if embeddedFrame != nil {
		go func() {
			interval := *staleAfter / 2
			if interval <= 0 {
				interval = 15 * time.Second
			}
			t := time.NewTicker(interval)
			defer t.Stop()
			// Stamp once immediately so registration → first-tick gap
			// can't trip stale.
			_ = embeddedFrame.Heartbeat(r, time.Now().UTC())
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					if err := embeddedFrame.Heartbeat(r, now.UTC()); err != nil {
						logger.Warn("embedded frame heartbeat failed", "err", err)
					}
				}
			}
		}()
	}

	// NEX-176: queue-manager goroutine polls the ledger for ready
	// tickets and dispatches work to assignee aspects. Runs until ctx
	// cancels. When no Frame funnel is available (legacy / no-aspect-dir
	// mode), the queue manager is a no-op.
	if frameFunnel != nil && ledgerSvc != nil {
		go runQueueManager(ctx, frameFunnel, ledgerSvc, b, embeddedFrame.Aspect.Name, logger)
	}

	// Auto-spawn: after the broker has bound its listener (brief
	// delay), scan the aspect dir and fire off harness children.
	// Non-blocking; failures are logged per-aspect. The supervisor
	// pointer is populated once Spawn returns so the parent can kill
	// the children on shutdown (otherwise Windows leaks one funnel
	// per aspect per nexus run).
	var supervisor atomic.Pointer[autospawn.Supervisor]
	go runAutoSpawn(ctx, logger, *aspectDir, *harnessPath, *agoraPath, *dataDir, *addr, token,
		autospawn.AspectTokenResolverFunc(tokenResolverFunc), &supervisor)

	serveErr := b.ListenAndServe(ctx)

	// Kill any spawned harnesses before exit so Task Manager doesn't
	// accumulate orphans across nexus restarts. 5s grace gives the
	// reaper goroutines time to drain cmd.Wait + log pipes.
	if sup := supervisor.Load(); sup != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		sup.Shutdown(shutCtx)
		cancel()
	}

	if serveErr != nil {
		logger.Error("broker exited with error", "err", serveErr)
		os.Exit(1)
	}
	logger.Info("nexus stopped")
}

// runAutoSpawn discovers aspect homes under aspectDir (or
// NEXUS_ASPECT_DIR env) and spawns a harness for each. Skipped if
// no dir is configured. Runs after a short delay so the broker's
// listener has bound before children try to dial in.
func runAutoSpawn(ctx context.Context, log *slog.Logger, aspectDirFlag, harnessPathFlag, agoraPathFlag, dataDirFlag, brokerAddr, token string, tokens autospawn.AspectTokenResolver, supOut *atomic.Pointer[autospawn.Supervisor]) {
	dir := aspectDirFlag
	if dir == "" {
		dir = os.Getenv("NEXUS_ASPECT_DIR")
	}
	if dir == "" {
		return // auto-spawn disabled
	}
	harnessPath := harnessPathFlag
	if harnessPath == "" {
		harnessPath = os.Getenv("NEXUS_HARNESS")
	}
	if harnessPath == "" {
		log.Warn("auto-spawn dir set but no harness path; skipping", "dir", dir)
		return
	}
	absHarness, err := filepath.Abs(harnessPath)
	if err == nil {
		harnessPath = absHarness
	}

	agoraBinPath := agoraPathFlag
	if agoraBinPath == "" {
		agoraBinPath = os.Getenv("NEXUS_AGORA")
	}
	if agoraBinPath != "" {
		if absAgora, err := filepath.Abs(agoraBinPath); err == nil {
			agoraBinPath = absAgora
		}
	}

	// Give the broker a moment to bind before children dial in.
	select {
	case <-ctx.Done():
		return
	case <-time.After(250 * time.Millisecond):
	}

	upstream := brokerAddr
	if upstream[0] == ':' {
		upstream = "127.0.0.1" + upstream
	}
	// Broker is TLS-only post PR-A2.2; auto-spawned aspects must dial
	// wss://. If the spawned harness's wsclient hits a self-signed dev
	// cert, the operator must have added it to the host's system trust
	// store (see `nexus cert init` + the printed trust hint).
	wsURL := "wss://" + upstream + "/connect"

	// Keyfiles live at <data-dir>/keyfiles/<name>.keyfile.json (Part 8
	// migration). storage.ResolvePath returns the *db file* path —
	// strip the filename to get the data dir, then append keyfiles.
	keyfileDir := filepath.Join(filepath.Dir(storage.ResolvePath(dataDirFlag)), "keyfiles")

	cfg := autospawn.Config{
		ScanDir:     dir,
		HarnessPath: harnessPath,
		AgoraPath:   agoraBinPath,
		// Resolve per-aspect keyfiles from <data-dir>/keyfiles when
		// autospawning. agentfunnel takes -k <keyfile>; aspect.json on
		// disk holds only the name, so autospawn maps name → keyfile
		// path here. Empty data-dir falls through to the legacy -home
		// form so other harness binaries that resolve identity from the
		// home dir still work.
		KeyfileDir: keyfileDir,
		BaseEnv: []string{
			"NEXUS_UPSTREAM=" + wsURL,
			// Legacy NEXUS_TOKEN — used only when TokenResolver returns
			// no per-aspect token for this child (graceful degrade).
			"NEXUS_TOKEN=" + token,
		},
		TokenResolver: tokens,
		Logger:        log,
	}

	candidates, err := autospawn.Discover(cfg)
	if err != nil {
		log.Error("auto-spawn discovery failed", "err", err)
		return
	}
	if len(candidates) == 0 {
		log.Info("auto-spawn: no aspect homes found", "dir", dir)
		return
	}
	sup, err := autospawn.Spawn(cfg, candidates)
	if err != nil {
		log.Error("auto-spawn failed", "err", err)
		return
	}
	if supOut != nil {
		supOut.Store(sup)
	}
}

// discoverAspectIDs returns the names of aspects discoverable under
// aspectDirFlag (or NEXUS_ASPECT_DIR). Empty slice when no scan dir
// is configured or the dir is empty / missing — in that case startup
// continues with only the Frame token reconciled, and any aspect that
// later registers via WS authenticates via the legacy master token
// until manually reconciled. Errors are logged and treated as
// "no aspects to reconcile" to keep boot resilient.
// resolveAspectDir picks the aspect directory from --aspect-dir, then
// NEXUS_ASPECT_DIR. Returns the FIRST root for callers that still
// expect a single dir (Frame detection); resolveAspectDirRoots
// returns the full list for the discovery scan. #21: comma-separated
// values let aspects live in multiple paths (e.g. main repo +
// external workspace).
func resolveAspectDir(aspectDirFlag string) string {
	roots := resolveAspectDirRoots(aspectDirFlag)
	if len(roots) == 0 {
		return ""
	}
	return roots[0]
}

// resolveAspectDirRoots splits the comma-separated --aspect-dir /
// NEXUS_ASPECT_DIR into one or more roots. Empty strings are dropped.
// Returns nil when neither source is set.
func resolveAspectDirRoots(aspectDirFlag string) []string {
	raw := aspectDirFlag
	if raw == "" {
		raw = os.Getenv("NEXUS_ASPECT_DIR")
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// discoverAspectHomes scans every root and returns a name → absolute
// home path map. Used by the broker to derive canonical aspect homes
// for the register handler (#21). Returns an empty map (not nil) on
// scan failure so the broker's lookup still works (and falls through
// to the legacy payload.Home path with a warning).
func discoverAspectHomes(aspectDirFlag string, log *slog.Logger) map[string]string {
	roots := resolveAspectDirRoots(aspectDirFlag)
	if len(roots) == 0 {
		return nil
	}
	candidates, err := autospawn.DiscoverRoots(autospawn.Config{}, roots)
	if err != nil {
		log.Warn("discover aspect homes: scan failed", "roots", roots, "err", err)
		return map[string]string{}
	}
	homes := make(map[string]string, len(candidates))
	for _, c := range candidates {
		homes[c.Name] = c.Path
	}
	return homes
}

func discoverAspectIDs(aspectDirFlag string, log *slog.Logger) []string {
	roots := resolveAspectDirRoots(aspectDirFlag)
	if len(roots) == 0 {
		return nil
	}
	candidates, err := autospawn.DiscoverRoots(autospawn.Config{}, roots)
	if err != nil {
		log.Warn("discover aspect ids: scan failed; tokens not reconciled", "roots", roots, "err", err)
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.Name)
	}
	return ids
}

// buildOnPersonalityChange returns the OnPersonalityChange callback
// passed into broker.Config. When the edited aspect is the embedded
// Frame, it calls EmbeddedFrame.RefreshPersonality so the next
// deliberation turn sees the new prompt. For non-Frame aspects the
// callback is a no-op today (remote agentfunnels pick up at next JWT
// re-validation; future WS broadcast will land here too).
//
// Returns nil when no Frame is embedded — broker treats nil as
// "no listener", same effective shape but cheaper.
func buildOnPersonalityChange(ctx context.Context, ef *frame.EmbeddedFrame, log *slog.Logger) func(string, int64) {
	if ef == nil {
		return nil
	}
	frameName := ef.Aspect.Name
	return func(aspectName string, newVersion int64) {
		if aspectName != frameName {
			// Remote aspect; nothing in-process to refresh.
			return
		}
		// Use the broker's parent context, not the request's, so a
		// short-lived HTTP handler cancel doesn't tear down the DB
		// read mid-refresh. Bound the refresh itself by a small
		// timeout so a stuck DB doesn't wedge the listener.
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := ef.RefreshPersonality(refreshCtx); err != nil {
			log.Warn("frame personality refresh failed",
				"aspect", aspectName, "version", newVersion, "err", err)
			return
		}
		log.Info("frame personality refreshed in-process",
			"aspect", aspectName, "version", newVersion)
	}
}

// buildOnNexusMDChange returns the OnNexusMDChange callback for
// broker.Config (Part 9d). Fires after a successful PUT
// /api/admin/nexus-md edit; calls EmbeddedFrame.RefreshCentral so the
// Frame picks up the new central content on its next deliberation
// turn (Part 9b's SystemPromptFn callback path reads the updated
// cache).
//
// Network-wide change: every aspect's composed prompt includes the
// central section, so a future broadcast frame would also fire from
// here. v0.1: in-process Frame refreshes immediately; remote
// agentfunnels pick up at next JWT re-validation (1h TTL).
//
// Returns nil when no Frame is embedded — broker treats nil as
// "no listener", same effective shape but cheaper.
func buildOnNexusMDChange(ctx context.Context, ef *frame.EmbeddedFrame, log *slog.Logger) func(int64) {
	if ef == nil {
		return nil
	}
	return func(newVersion int64) {
		// Same context discipline as OnPersonalityChange — broker
		// parent context, bounded refresh timeout.
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := ef.RefreshCentral(refreshCtx); err != nil {
			log.Warn("frame central refresh failed",
				"version", newVersion, "err", err)
			return
		}
		log.Info("frame central nexus_md refreshed in-process",
			"version", newVersion)
	}
}

// buildOperatorLogin assembles the broker's operator login wiring
// (dashboard-ws-port spec, sub-part 5b1). Returns nil when the
// configuration to make WebAuthn meaningful is missing — empty
// signing secret (no Nexus identity), or no NEXUS_OPERATOR_RPID set.
//
// Why an env var for RPID/origins instead of deriving from --addr:
// the broker may listen on :7888 but the dashboard URL the browser
// uses depends on TLS certs, tailnet hostname, possible front
// proxy. WebAuthn rejects any RPID the browser doesn't see in its
// own origin, so deriving it server-side is fragile. Operator
// supplies via env so deployment topology doesn't require a code
// change. Defaults: RPID empty → returns nil (no operator endpoints
// registered); RPID set without origins → derive a single origin
// from "https://" + RPID (the common case for tailnet hosts on
// the default 443).
func buildOperatorLogin(db *sql.DB, nexusID string, secret []byte, addr string, log *slog.Logger) *broker.OperatorLogin {
	rpID := os.Getenv("NEXUS_OPERATOR_RPID")
	if rpID == "" {
		log.Info("operator login disabled — set NEXUS_OPERATOR_RPID to enable (typically the tailnet hostname)")
		return nil
	}
	if len(secret) == 0 {
		log.Warn("operator login disabled — Nexus identity has no session signing secret")
		return nil
	}

	// Origins: NEXUS_OPERATOR_ORIGINS is comma-separated; default
	// derives from rpID + listen port. WebAuthn matches origins as
	// exact strings (including port), so "https://host" without the
	// port will silently reject every assertion when the dashboard
	// runs on a non-443 port. Diligence 2026-05-11 hit this — derive
	// from --addr by default so the common case (tailnet host, custom
	// port) works without operator config.
	origins := []string{deriveDefaultOrigin(rpID, addr)}
	if env := os.Getenv("NEXUS_OPERATOR_ORIGINS"); env != "" {
		origins = strings.Split(env, ",")
		for i := range origins {
			origins[i] = strings.TrimSpace(origins[i])
		}
	}

	auth, err := operator.NewAuth(rpID, "The Nexus", origins, operator.NewPasskeyStore(db))
	if err != nil {
		log.Error("operator login disabled — webauthn config rejected", "err", err)
		return nil
	}

	log.Info("operator login enabled", "rp_id", rpID, "origins", origins)
	return &broker.OperatorLogin{
		Auth:                 auth,
		SessionSigningSecret: secret,
		JWTTTL:               24 * time.Hour,
		NexusID:              nexusID,
	}
}

// seedThreadIndex populates the in-memory ThreadIndex with the Frame's
// historical posts so reply-routing (route.ShouldRouteToFrame rule 2c)
// survives nexus restarts. The index is in-process; without seeding,
// every reboot loses authorship knowledge and operator replies to any
// pre-restart Frame post route nowhere.
//
// Reads chat_messages where from_agent == frameName, oldest first, no
// limit (replays the full Frame chat history). Topic comes through as
// empty for messages without one; RecordPost handles that.
//
// Errors propagate to the caller, which logs + continues — a seed
// failure degrades reply-routing to this-boot-only but doesn't block
// startup.
func seedThreadIndex(ctx context.Context, store chat.Store, idx *route.ThreadIndex, frameName string, log *slog.Logger) error {
	if store == nil || idx == nil || frameName == "" {
		return nil
	}
	// chat.Store doesn't expose ListByFromAgent today; walk ListPage
	// in batches and filter. This is one-shot at boot, so the cost is
	// negligible relative to a server lifetime.
	var afterID int64
	const batch = 500
	seeded := 0
	for {
		msgs, hasMore, err := store.ListPage(ctx, 0, afterID, batch)
		if err != nil {
			return fmt.Errorf("seed thread index: list chat page: %w", err)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			if m.From == frameName {
				idx.RecordPost(m.ID, m.Topic)
				seeded++
			}
			if m.ID > afterID {
				afterID = m.ID
			}
		}
		if !hasMore {
			break
		}
	}
	log.Info("frame threads index seeded from chat history",
		"frame", frameName, "messages_recorded", seeded)
	return nil
}

// deriveDefaultOrigin builds the WebAuthn origin string for the operator
// dashboard from the rpID + listen addr. WebAuthn matches origins as
// exact strings — including port — so a default of "https://<rpID>"
// silently rejects every assertion when the dashboard runs on a non-443
// port. Diligence 2026-05-11 hit this with rpID=host + addr=:18888.
//
// Rules:
//   - addr like ":18888" or "host:18888" extracts port 18888 → emits
//     "https://<rpID>:18888"
//   - addr empty, port unparseable, or port == 443 → emits
//     "https://<rpID>" (the original default; correct for the standard
//     HTTPS port)
//
// Operator can still override via NEXUS_OPERATOR_ORIGINS for setups
// behind a front proxy where the listen addr doesn't match the
// browser-visible origin.
func deriveDefaultOrigin(rpID, addr string) string {
	if addr == "" {
		return "https://" + rpID
	}
	// addr can be ":18888" or "host:18888"; SplitHostPort handles both.
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || port == "443" {
		return "https://" + rpID
	}
	return "https://" + rpID + ":" + port
}

// buildChatRouter constructs the funnel and returns a ChatRouterCallbacks
// wired to it. Returns nil when no Frame is embedded (legacy / no-aspect-dir
// mode), causing the broker to log + drop chat.send frames.
//
// The provider is determined by the Frame's aspect.json `provider` field.
// v1 supports "claude-api" (and "claude" alias); other providers log a
// warning and fall back to nil (no deliberation). This keeps the Frame
// operational as a routing surface even when the provider isn't recognised.
// usageRecorderAdapter bridges the funnel.UsageRecorder interface to
// usage.SQLStore. Lives in main rather than in usage/ to avoid the
// usage package importing funnel (which would create a cycle through
// framecomms).
type usageRecorderAdapter struct {
	store *usage.SQLStore
}

func (a *usageRecorderAdapter) Record(ctx context.Context, msgID int64, turnID, aspectID, model string, u bridle.Usage) error {
	_, err := a.store.Record(ctx, usage.Record{
		MsgID:        msgID,
		TurnID:       turnID,
		AspectID:     aspectID,
		Model:        model,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	})
	return err
}

// buildOutputFilter resolves the post-hoc filter from aspect.json:
//
//	"filter":                "cheap" | "hard" | "always" | "off" (default "cheap")
//	"filter_provider":       optional override; falls back to the Frame's provider
//	"filter_provider_config.model": optional; falls back to "claude-haiku-4-5" for
//	                          Claude flavors, otherwise the Frame's main model
//
// Empty filter → "cheap" (full triage). "hard" skips the model call.
// "always" / "off" only catch empty replies.
//
// The cheap-tier judge is operator-configurable so non-Claude deployments
// can wire their own (ollama, openai, anthropic-api with haiku). Default
// haiku rather than the Frame's main model so per-turn cost stays bounded.
func buildOutputFilter(cfg schemas.AspectConfig, frameProvider bridle.Provider, frameProviderID bridle.ProviderID, frameModel string, obsHook funnel.ObservabilityHook, aspectHome string, credentialStore *credentials.Store, log *slog.Logger) funnel.OutputFilter {
	aspectName := cfg.Name
	choice := strings.ToLower(strings.TrimSpace(cfg.Filter))
	if choice == "" {
		choice = "cheap"
	}
	switch choice {
	case "off", "always":
		log.Info("frame funnel: filter=always (post every non-empty reply)", "aspect", aspectName)
		return funnel.AlwaysPostFilter{}
	case "hard":
		log.Info("frame funnel: filter=hard (substring/prefix self-suppress only)", "aspect", aspectName)
		return funnel.HardRulesFilter{}
	case "cheap":
		return buildFrameCheapFilter(cfg, frameProvider, frameProviderID, frameModel, obsHook, aspectHome, credentialStore, log)
	default:
		log.Warn("frame funnel: unrecognised filter setting; falling back to cheap",
			"aspect", aspectName, "setting", cfg.Filter)
		return buildFrameCheapFilter(cfg, frameProvider, frameProviderID, frameModel, obsHook, aspectHome, credentialStore, log)
	}
}

// autoRecallConfig maps the aspect.json auto_recall block to the funnel's
// AutoRecallConfig, supplying the runtime's knowledge gateway. nil block →
// zero config (disabled). The funnel applies its own defaults for unset
// TopK/MaxChars.
func autoRecallConfig(c *schemas.AutoRecall, gw funnel.KnowledgeGateway) funnel.AutoRecallConfig {
	if c == nil {
		return funnel.AutoRecallConfig{}
	}
	return funnel.AutoRecallConfig{
		Gateway:  gw,
		Enabled:  c.Enabled,
		TopK:     c.TopK,
		MaxRank:  c.MaxRank,
		MaxChars: c.MaxChars,
	}
}

// buildFrameCheapFilter resolves the Frame's judge inputs (judge provider /
// model from aspect.json, judge credential env from the credentials store)
// and delegates construction to the shared judge package — the same builder
// the out-of-process agentfunnel uses (NEX-365 #2). The judge model comes
// from filter_provider_config.model; the judge provider from filter_provider
// (empty = inherit the Frame's provider).
func buildFrameCheapFilter(cfg schemas.AspectConfig, frameProvider bridle.Provider, frameProviderID bridle.ProviderID, frameModel string, obsHook funnel.ObservabilityHook, aspectHome string, credentialStore *credentials.Store, log *slog.Logger) funnel.OutputFilter {
	judgeModel := ""
	if cfg.FilterProviderConfig != nil {
		if m, ok := cfg.FilterProviderConfig["model"].(string); ok {
			judgeModel = strings.TrimSpace(m)
		}
	}
	return judge.BuildFilter(judge.Spec{
		Label:             "frame funnel",
		MainProvider:      frameProvider,
		MainProviderID:    frameProviderID,
		MainModel:         frameModel,
		JudgeProviderName: strings.TrimSpace(cfg.FilterProvider),
		JudgeModel:        judgeModel,
		JudgeEnv:          resolveFilterCredentialEnv(cfg, credentialStore, log),
		AspectHome:        aspectHome,
		ObsHook:           obsHook,
		Logger:            log,
	})
}

// resolveCompactCredentialEnv mirrors resolveFilterCredentialEnv
// for the compact-tier (rewriter / haiku distiller) path. NEX-301.
// Empty CompactCredential / nil store → returns nil → distiller
// inherits ambient process env. Same best-effort error handling as
// the filter side.
func resolveCompactCredentialEnv(cfg schemas.AspectConfig, store *credentials.Store, log *slog.Logger) map[string]string {
	if cfg.CompactCredential == "" || store == nil {
		return nil
	}
	c, err := store.Get(context.Background(), cfg.CompactCredential)
	if err != nil {
		log.Warn("compact credential: lookup failed; distiller inherits ambient env",
			"aspect", cfg.Name, "credential", cfg.CompactCredential, "err", err)
		return nil
	}
	env, err := store.EnvForCredential(c)
	if err != nil {
		log.Warn("compact credential: env materialization failed; distiller inherits ambient env",
			"aspect", cfg.Name, "credential", cfg.CompactCredential, "err", err)
		return nil
	}
	return env
}

// resolveFilterCredentialEnv looks up the named filter credential and
// returns its env overlay (typically ANTHROPIC_API_KEY +
// ANTHROPIC_BASE_URL for Anthropic-shape providers). Empty
// FilterCredential or nil store → returns nil so the judge inherits
// the ambient process env. Errors are logged + swallowed; failing
// open is the right shape (the worst case is the bare subprocess
// finds no API key and fails its turn, which the filter handles by
// failing open already).
func resolveFilterCredentialEnv(cfg schemas.AspectConfig, store *credentials.Store, log *slog.Logger) map[string]string {
	if cfg.FilterCredential == "" || store == nil {
		return nil
	}
	c, err := store.Get(context.Background(), cfg.FilterCredential)
	if err != nil {
		log.Warn("filter credential: lookup failed; judge inherits ambient env",
			"aspect", cfg.Name, "credential", cfg.FilterCredential, "err", err)
		return nil
	}
	env, err := store.EnvForCredential(c)
	if err != nil {
		log.Warn("filter credential: env materialization failed; judge inherits ambient env",
			"aspect", cfg.Name, "credential", cfg.FilterCredential, "err", err)
		return nil
	}
	return env
}

// applyAspectModelOverrides reads NEX-263's per-aspect model + credential
// override rows from the credentials store and mutates cfg in place. Each
// override field overlays the corresponding keyfile value; null overrides
// leave keyfile values untouched. Designed to be called once at startup
// before any consumer (buildChatRouter, buildFrameCheapFilter,
// buildRewriterRunner) reads cfg, so all three see the resolved values.
//
// Phase 1 wires three model fields (primary / judge / compact) and the
// judge credential. The primary and compact credential overrides are
// stored but not yet runtime-injected; an operator-visible warn is
// emitted when they're set so the gap is observable rather than silent.
//
// NEX-294: judge + compact (model AND credential) now fall back to
// network_defaults when no per-aspect override is set. Primary model
// + primary credential stay per-aspect only by design (whole point of
// primary is differentiation). Log `model_source` distinguishes
// "override" (per-aspect) from "network_default" so operators can
// see which layer fired.
func applyAspectModelOverrides(ctx context.Context, cfg *schemas.AspectConfig, store *credentials.Store, log *slog.Logger) {
	if store == nil || cfg == nil {
		return
	}
	override, err := store.GetAspectModelConfig(ctx, cfg.Name)
	if err != nil {
		log.Warn("aspect model override read failed", "aspect", cfg.Name, "err", err)
		return
	}
	defaults, err := store.GetNetworkDefaults(ctx)
	if err != nil {
		// Non-fatal: log + proceed with per-aspect overrides only.
		// Caller's existing legacy fallback (haiku model, ambient env
		// credential) handles the path that would otherwise have
		// consulted network defaults.
		log.Warn("network defaults read failed; falling back to per-aspect only",
			"aspect", cfg.Name, "err", err)
		defaults = credentials.NetworkDefaults{}
	}

	// Primary model — per-aspect only (no network default by design).
	if override.PrimaryModel != nil {
		if cfg.ProviderConfig == nil {
			cfg.ProviderConfig = map[string]any{}
		}
		prev, _ := cfg.ProviderConfig["model"].(string)
		cfg.ProviderConfig["model"] = *override.PrimaryModel
		log.Info("aspect model override applied",
			"aspect", cfg.Name, "kind", "primary",
			"from", prev, "to", *override.PrimaryModel, "model_source", "override")
	}

	// Judge model — per-aspect override wins, network default
	// applies when override blank.
	if judgeModel, src := pickModelOverride(override.JudgeModel, defaults.JudgeModel); src != "" {
		if cfg.FilterProviderConfig == nil {
			cfg.FilterProviderConfig = map[string]any{}
		}
		prev, _ := cfg.FilterProviderConfig["model"].(string)
		cfg.FilterProviderConfig["model"] = judgeModel
		log.Info("aspect model override applied",
			"aspect", cfg.Name, "kind", "judge",
			"from", prev, "to", judgeModel, "model_source", src)
	}

	// Compact model — same pattern.
	if compactModel, src := pickModelOverride(override.CompactModel, defaults.CompactModel); src != "" {
		if cfg.Rewriter == nil {
			cfg.Rewriter = &schemas.RewriterConfig{}
		}
		prev := cfg.Rewriter.DistillerModel
		cfg.Rewriter.DistillerModel = compactModel
		log.Info("aspect model override applied",
			"aspect", cfg.Name, "kind", "compact",
			"from", prev, "to", compactModel, "model_source", src)
	}

	// Judge credential — per-aspect override > network default.
	if judgeCred, src := pickModelOverride(override.JudgeCredential, defaults.JudgeCredential); src != "" {
		prev := cfg.FilterCredential
		cfg.FilterCredential = judgeCred
		log.Info("aspect credential override applied",
			"aspect", cfg.Name, "kind", "judge",
			"from", prev, "to", judgeCred, "model_source", src)
	}
	// Judge provider — per-aspect override > network default (NEX-365 #3).
	// Selects which provider family the cheap-judge runs on, independent
	// of the aspect's primary provider. aspect.json's explicit
	// filter_provider is operator-intent and WINS over the stored policy
	// plane; this only fills the gap when filter_provider is unset. The
	// paired judge credential (set above) carries the endpoint key + base
	// URL so the judge package (judge.BuildFilter / NativeJudgeProvider)
	// actually targets it.
	if judgeProv, src := pickModelOverride(override.JudgeProvider, defaults.JudgeProvider); src != "" {
		if strings.TrimSpace(cfg.FilterProvider) == "" {
			cfg.FilterProvider = judgeProv
			log.Info("aspect judge provider applied",
				"aspect", cfg.Name, "kind", "judge", "to", judgeProv, "model_source", src)
		} else {
			log.Info("aspect judge provider present but filter_provider set in aspect.json; keeping aspect.json",
				"aspect", cfg.Name, "stored", judgeProv, "aspect_json", cfg.FilterProvider, "model_source", src)
		}
	}
	if override.PrimaryCredential != nil {
		log.Warn("aspect primary credential override stored but not yet wired into runtime",
			"aspect", cfg.Name, "value", *override.PrimaryCredential)
	}
	// Compact credential — per-aspect override > network default
	// (NEX-294). Wired into runtime via the rewriter construction
	// site below (NEX-301).
	if compactCred, src := pickModelOverride(override.CompactCredential, defaults.CompactCredential); src != "" {
		prev := cfg.CompactCredential
		cfg.CompactCredential = compactCred
		log.Info("aspect credential override applied",
			"aspect", cfg.Name, "kind", "compact",
			"from", prev, "to", compactCred, "model_source", src)
	}
}

// mainTurnSamplingFromConfig maps the optional aspect.json
// main_turn_sampling block (schemas.MainTurnSampling) to funnel's
// internal Config.MainTurnSampling shape. Field-for-field copy —
// the two structs have identical layout, kept as separate types so
// the funnel package doesn't take a hard dep on shared/schemas.
//
// nil input -> zero-valued MainTurnSampling (all fields unset),
// which funnel pass-through translates to "leave bridle TurnRequest
// fields unset" -> provider default. NEX-300 main-turn slice.
func mainTurnSamplingFromConfig(s *schemas.MainTurnSampling) funnel.MainTurnSampling {
	if s == nil {
		return funnel.MainTurnSampling{}
	}
	return funnel.MainTurnSampling{
		Temperature:     s.Temperature,
		TopP:            s.TopP,
		TopK:            s.TopK,
		Seed:            s.Seed,
		MaxOutputTokens: s.MaxOutputTokens,
		StopSequences:   s.StopSequences,
	}
}

// pickModelOverride implements the NEX-294 resolution: per-aspect
// override wins when set (non-nil + non-empty); otherwise network
// default applies when set (non-empty); otherwise neither (return
// "" and src=""). Returns (value, source) so callers can log which
// layer fired — useful when an operator asks "why is anvil using
// model X?".
//
// source is "override" | "network_default" | "" (neither).
func pickModelOverride(perAspect *string, networkDefault string) (value, source string) {
	if perAspect != nil && *perAspect != "" {
		return *perAspect, "override"
	}
	if networkDefault != "" {
		return networkDefault, "network_default"
	}
	return "", ""
}

// NEX-365 #2: the cheap-judge construction (resolveJudgeProviderAndModel,
// bareJudgeProvider, nativeJudgeProvider, isClaudeFlavor) moved to the
// shared nexus/frame/funnel/judge package, which now builds the judge for
// both this Frame and the out-of-process agentfunnel. buildProviderByName
// stays below — the rewriter (compact) path still uses it.

// buildProviderByName mirrors the Frame's own provider switch in
// buildChatRouter. Kept narrow — adds providers as the rest of the
// runtime gains them. Returns ok=false on unrecognised names so the
// caller can downgrade gracefully rather than crash.
func buildProviderByName(name string, log *slog.Logger) (bridle.Provider, bridle.ProviderID, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-api", "claude":
		return claudeprovider.New(""), bridle.ProviderID("claude-api"), true
	case "claude-code", "claudecode":
		return claudecodeprovider.New(), bridle.ProviderID("claude-code"), true
	default:
		log.Warn("frame funnel: filter_provider unrecognised", "filter_provider", name)
		return nil, "", false
	}
}

// toolsForProvider returns the bridle.ToolDef set the funnel should
// register on TurnRequest.Tools. Direct-API providers (claude-api,
// future ollama/openai) use the funnel's Go-function tool surface
// (CommsToolDefs) — bridle routes tool_use through ToolRunner. claude-
// code is subprocess-stream: the CLI owns tool execution against its
// own native toolkit; bridle's Tools field doesn't propagate. Passing
// CommsToolDefs to claude-code creates a phantom tool surface where
// the model thinks send_chat/triage etc. exist but can't call them.
// Until the MCP route (#181) ships, claude-code Frames get empty
// Tools and rely on the funnel's auto-post path for reply surfacing.
func toolsForProvider(id bridle.ProviderID) []bridle.ToolDef {
	switch id {
	case "claude-code", "claudecode":
		return nil
	}
	return funnel.CommsToolDefs()
}

// buildRewriterRunner constructs the per-turn jsonl rewriter runner
// for the Frame's funnel. Returns funnel.NoopPostTurn when the
// rewriter is disabled or the configuration would be unworkable
// (non-claude-code provider with no operator override; missing
// distiller config; etc).
//
// Defaults:
//   - claude-code provider → enabled unless aspect.json sets
//     rewriter.enabled = false
//   - any other provider → disabled unless explicitly enabled (and
//     even then, the rewriter is a no-op if there's no jsonl to walk;
//     warn but don't crash)
//
// The session path is resolved lazily through sessionIDFn so the
// funnel's session id rotations (compaction, rewriter-driven reset)
// are picked up automatically.
// NEX-301: compactEnv overlays env vars on the distiller's
// TurnRequest, routing compact-tier calls to whatever auth domain
// the resolved compact_credential points at (DeepSeek, secondary
// Anthropic). Nil = inherit ambient process env (legacy behaviour).
// Caller resolves via resolveCompactCredentialEnv.
func buildRewriterRunner(cfg schemas.AspectConfig, aspectCwd string, frameProviderID bridle.ProviderID, frameProvider bridle.Provider, frameModel string, sessionIDFn func() string, compactEnv map[string]string, log *slog.Logger) funnel.PostTurnHook {
	rwCfg := cfg.Rewriter
	claudeFlavor := judge.IsClaudeFlavor(frameProviderID)
	enabledByDefault := claudeFlavor
	enabled := enabledByDefault
	if rwCfg != nil && rwCfg.Enabled != nil {
		enabled = *rwCfg.Enabled
	}
	if !enabled {
		reason := "non-claude-default"
		if rwCfg != nil && rwCfg.Enabled != nil {
			reason = "explicit-off"
		}
		log.Info("frame funnel: rewriter disabled",
			"aspect", cfg.Name, "provider", frameProviderID, "reason", reason)
		return funnel.NoopPostTurn{}
	}
	// Hard guard: rewriter only makes sense for providers that write a
	// session jsonl on disk. claude-code writes one; direct-API
	// providers (claude-api, ollama-local, openai-api) do not. An
	// operator who explicitly enables rewriter on one of those would
	// trigger a never-ending fail-and-reset loop because DistillTail
	// would always hit ENOENT. Override to off and warn.
	if !claudeFlavor || frameProviderID == "claude-api" || frameProviderID == "claude" {
		log.Warn("frame funnel: rewriter requested but provider has no session jsonl; disabling",
			"aspect", cfg.Name, "provider", frameProviderID,
			"hint", "rewriter only applies to claude-code (subprocess-stream) providers")
		return funnel.NoopPostTurn{}
	}

	// Resolve distiller provider+model. Defaults: same provider as the
	// Frame, claude-haiku-4-5 model when Claude flavor.
	distillerProvider := frameProvider
	distillerProviderID := frameProviderID
	distillerModel := ""
	if rwCfg != nil {
		if rwCfg.DistillerProvider != "" {
			p, id, ok := buildProviderByName(rwCfg.DistillerProvider, log)
			if ok {
				distillerProvider = p
				distillerProviderID = id
			} else {
				log.Warn("frame funnel: rewriter distiller_provider unrecognised; falling back to frame provider",
					"aspect", cfg.Name, "configured", rwCfg.DistillerProvider)
			}
		}
		distillerModel = rwCfg.DistillerModel
	}
	if distillerModel == "" {
		if judge.IsClaudeFlavor(distillerProviderID) {
			distillerModel = "claude-haiku-4-5"
		} else {
			distillerModel = frameModel
		}
	}

	haiku, err := rewriter.NewHaikuDistiller(bridle.NewHarness(distillerProvider), distillerProviderID, distillerModel)
	if err != nil {
		log.Warn("frame funnel: rewriter haiku distiller construction failed; disabling rewriter",
			"aspect", cfg.Name, "err", err)
		return funnel.NoopPostTurn{}
	}
	haiku.AspectID = cfg.Name
	// NEX-301: route compact calls through operator-configured auth
	// domain when CompactCredential resolved to an env overlay.
	haiku.ProviderEnv = compactEnv

	// Thresholds: zero falls back to spec defaults inside rewriter.New.
	var trThreshold, atThreshold int
	if rwCfg != nil {
		trThreshold = rwCfg.ToolResultThreshold
		atThreshold = rwCfg.AssistantTextThreshold
	}

	rw, err := rewriter.New(rewriter.Config{
		SessionPathFn: func() string {
			id := sessionIDFn()
			if id == "" {
				return ""
			}
			return rewriter.SessionPath(aspectCwd, id)
		},
		Distiller:              haiku,
		ModelName:              distillerModel,
		ToolResultThreshold:    trThreshold,
		AssistantTextThreshold: atThreshold,
		Logger:                 log,
	})
	if err != nil {
		log.Warn("frame funnel: rewriter construction failed; disabling",
			"aspect", cfg.Name, "err", err)
		return funnel.NoopPostTurn{}
	}

	log.Info("frame funnel: rewriter enabled",
		"aspect", cfg.Name,
		"distiller_provider", distillerProviderID,
		"distiller_model", distillerModel,
		"tool_result_threshold", trThreshold,
		"assistant_text_threshold", atThreshold)

	return rewriter.NewRunner(rw, log)
}

// buildChatRouter returns the chat-router callbacks plus the gateway it
// wired the funnel to. The caller is expected to assign gateway.Sender
// to the broker after broker.New so in-process Frame posts go through
// Broker.HandleChatSend (the unified chat-send path). When ef is nil
// both returns are nil.
func buildChatRouter(ctx context.Context, ef *frame.EmbeddedFrame, ros *roster.Roster, store chat.Store, triageStore chat.TriageStore, usageStore *usage.SQLStore, knowledgeStore *knowledge.Store, obsHub *observability.Hub, credentialStore *credentials.Store, log *slog.Logger, ledgerSvc *ledger.Service) (*broker.ChatRouterCallbacks, *framecomms.Gateway, *funnel.Funnel) {
	if ef == nil {
		return nil, nil, nil
	}

	provider := ef.Aspect.Config.Provider
	model := ""
	if pc := ef.Aspect.Config.ProviderConfig; pc != nil {
		if m, ok := pc["model"].(string); ok {
			model = m
		}
	}

	var p bridle.Provider
	switch provider {
	case "claude-api", "claude":
		p = claudeprovider.New("")
	case "claude-code", "claudecode", "":
		// Subscription-auth path: shells out to the local `claude` CLI.
		// Default for unset provider since the rebuild deploy runs on
		// subscription, not API key.
		cp := claudecodeprovider.New()
		cp.DisallowedTools = funnel.DisallowedNativeTools
		p = cp
	default:
		log.Warn("frame funnel: unrecognised provider; deliberation disabled",
			"provider", provider, "frame", ef.Aspect.Name)
		return nil, nil, nil
	}

	if model == "" {
		log.Warn("frame funnel: no model configured in aspect.json; deliberation disabled",
			"frame", ef.Aspect.Name)
		return nil, nil, nil
	}

	// F1.4b.4: wire the comms tool surface (Lock 3) and the
	// chat-pulse impl (Lock 5). The Frame's gateway writes via the
	// chat.Store; CommsRunner translates send_chat / react_to /
	// chat.read / announce_file / share_file tool calls into
	// gateway methods. ChatPulser fires real chat-visible status
	// pulses via the same gateway, replacing F1.3's NoopPulser
	// default. The caller is expected to set gateway.Sender after
	// broker.New so SendChat takes the unified Broker.HandleChatSend
	// path (per docs/2026-05-04-unify-frame-aspect-chat-path.md).
	gateway := framecomms.NewGateway(store, ef.Aspect.Name)
	knowledgeGateway := framecomms.NewKnowledgeGateway(knowledgeStore)
	commsRunner := funnel.CommsRunner{
		Gateway:   gateway,
		Knowledge: knowledgeGateway,
		AspectID:  ef.Aspect.Name,
		Triage:    triageStore,
	}
	pulser := &framecomms.ChatPulser{Gateway: gateway}
	recorder := &usageRecorderAdapter{store: usageStore}

	threads := route.NewThreadIndex()
	// Seed the in-memory index from chat history so reply-routing
	// (route.Rule 2c) survives nexus restarts. The threads index is
	// transient; without this seed, every restart drops the Frame's
	// authorship record and operator replies to pre-restart Frame
	// posts route nowhere. Surfaced on 2026-05-11 cutover.
	if seedErr := seedThreadIndex(ctx, store, threads, ef.Aspect.Name, log); seedErr != nil {
		log.Warn("frame threads index seed failed; reply-to-Frame routing limited to this boot",
			"err", seedErr, "frame", ef.Aspect.Name)
	}
	// AspectHome anchors the bridle subprocess (claude-code) cwd so its
	// session jsonl + .mcp.json discovery land in a per-aspect directory
	// instead of whatever cwd nexus.exe inherited from its launcher.
	// Without this, Frame keel's subprocess inherits nexus's cwd, which
	// inherits the launcher session's cwd, which inherits some external
	// claude-code session's .mcp.json — and the Frame posts to chat with
	// the wrong identity. Fixed by the per-request Cwd field in bridle
	// (PR #4 merged 2026-05-12) + this companion site. frame.Detect
	// already resolved this path; reuse it.
	aspectHome := ef.Aspect.Path

	// Phase E: one Grouper per aspect — shared between the funnel and
	// its cheap-judge filter so the dashboard sees main + filter-judge
	// turns on the same stream.
	obsHook := obsHub.GrouperFor(ef.Aspect.Name)
	outputFilter := buildOutputFilter(ef.Aspect.Config, p, bridle.ProviderID(provider), model, obsHook, aspectHome, credentialStore, log)
	// PostTurn hook resolves the funnel's session id lazily: the
	// funnel itself doesn't exist yet (we're constructing its config),
	// so we capture a pointer that gets filled in after funnel.New.
	// The runner only invokes the closure inside AfterTurn, by which
	// time the pointer has been assigned.
	var funnelPtr *funnel.Funnel
	// Rewriter watches the same path claude-code writes to. claude-code
	// derives its projects-directory key from process cwd; with the
	// AspectHome wiring above, that cwd IS aspectHome. So the rewriter
	// reads from aspectHome too. Pre-AspectHome (legacy) used os.Getwd()
	// because there was no per-aspect anchor.
	postTurn := buildRewriterRunner(ef.Aspect.Config, aspectHome, bridle.ProviderID(provider), p, model, func() string {
		if funnelPtr == nil {
			return ""
		}
		return funnelPtr.SessionID()
	}, resolveCompactCredentialEnv(ef.Aspect.Config, credentialStore, log), log)
	f, err := funnel.New(funnel.Config{
		AspectID:   ef.Aspect.Name,
		AspectHome: aspectHome,
		Harness:    bridle.NewHarness(p),
		Provider:   bridle.ProviderID(provider),
		Model:      model,
		// ContextMode (#226.5): the Frame is hardcoded to Global. Its
		// deliberation legitimately spans all incoming chat as one
		// stream of consciousness — operator routing, multi-aspect
		// coordination, cross-thread context are first-class for the
		// Frame. Per-thread isolation belongs to funnel-driven aspects
		// (agentfunnel, runtime/cmd/aspect).
		ContextMode: funnel.ContextGlobal,
		// SystemPromptFn (not SystemPrompt) so RefreshPersonality on
		// the EmbeddedFrame is picked up by the next turn without
		// rebuilding the funnel. Spec §11 in-process refresh path.
		SystemPromptFn: ef.SystemPrompt,
		// Tools: bridle.ToolDef[] is for direct-API providers where bridle
		// routes the model's tool_use through ToolRunner. For claude-code
		// (subprocess-stream), the CLI owns tool execution against its
		// own native toolkit (Bash, Read, Glob, etc.) and bridle's Tools
		// field doesn't propagate. Passing CommsToolDefs() for a claude-
		// code Frame creates a phantom tool surface — the model sees the
		// SystemPrompt promise of send_chat/triage etc. but cannot call
		// them, AND can confuse itself out of using legit native tools.
		// The MCP route (task #181) is the proper fix; for now, empty
		// Tools for claude-code Frames so the model relies on its native
		// toolkit + the funnel's auto-post path for replies.
		Tools:  toolsForProvider(bridle.ProviderID(provider)),
		Runner: funnel.ComposeRunner(commsRunner, &funnel.NullRunner{}),
		// AutoRecall (Commonplace): turn-time recall into the system prompt.
		// Off unless aspect.json opts in; the gateway is the same one the
		// comms runner uses. Day-3 cost/output lever.
		AutoRecall:  autoRecallConfig(ef.Aspect.Config.AutoRecall, knowledgeGateway),
		Filter:      outputFilter,
		ChatGateway: gateway,
		// ThreadReader gives the post-hoc judge the recent thread tail so it
		// can suppress @all / broadcast acknowledgement loops by INTENT (the
		// AI judge's job) rather than the verbatim-only Tier-1 damping. Same
		// gateway; ReadThreadTail = ReadThread(sinceID=0), funnel-bounded.
		ThreadReader: gateway,
		// NEX-239: stream per-text-block to chat for parity with
		// agentfunnel. Operator stance (2026-05-26): the embedded Frame
		// is a standard agentfunnel that happens to auto-start with the
		// broker; behavioural divergence isn't intentional. Without this,
		// pre-tool exploratory text is dropped (claudecode resets
		// finalText on every tool_use) — the same chat-loss pattern
		// NEX-239 reported for anvil before NEX-240 wired streaming for
		// remote agentfunnel aspects.
		StreamTextToChat: true,
		Threads:          threads,
		Pulser:           pulser,
		UsageRecorder:    recorder,
		Triage:           triageStore,
		PostTurn:         postTurn,
		// NEX-300 main-turn slice: per-aspect sampling + output overrides
		// from aspect.json's optional main_turn_sampling block. Empty /
		// absent block produces a zero-valued MainTurnSampling — funnel
		// pass-through leaves bridle's TurnRequest fields unset →
		// provider defaults preserved (back-compat).
		MainTurnSampling: mainTurnSamplingFromConfig(ef.Aspect.Config.MainTurnSampling),
		// Phase E: hand the embedded Frame's funnel the per-aspect
		// Grouper from the broker's observability Hub. Same-process
		// wiring — broker and funnel share the heap, so the Grouper
		// satisfies funnel.ObservabilityHook via structural typing.
		ObservabilityHook: obsHook,
		// #218: route every Frame turn through the credential store so
		// aspects.default_anthropic_credential overlays
		// ANTHROPIC_API_KEY + ANTHROPIC_BASE_URL onto bridle's
		// ProviderEnv. Nil store (legacy / dev) cleanly falls back to
		// the resolver returning (nil, false) → no overlay, subscription
		// / process-env auth wins.
		ProviderEnvResolver: newCredentialEnvResolver(credentialStore, bridle.ProviderID(provider)),
		// NEX-96: persist the seen-msg-id set under AspectHome so the
		// idempotency guarantee survives nexus restart / redeploy.
		// Without this, broker re-delivery after a stale-cursor crash
		// causes duplicate deliberation of already-handled messages.
		IdempotencyFile: filepath.Join(aspectHome, "funnel-seen.json"),
		Logger:          log,
	})
	if err != nil {
		log.Error("frame funnel: construction failed; deliberation disabled",
			"err", err, "frame", ef.Aspect.Name)
		return nil, nil, nil
	}
	// Bind the lazy session-id closure used by the rewriter runner.
	funnelPtr = f

	log.Info("frame funnel: deliberation loop ready",
		"frame", ef.Aspect.Name, "provider", provider, "model", model,
		"tools", len(toolsForProvider(bridle.ProviderID(provider))))

	frameName := ef.Aspect.Name
	return &broker.ChatRouterCallbacks{
		RouteChat: func(rctx context.Context, msgID int64, from, content string, replyTo int64, topic string) {
			// Frame's own posts must never route back to the funnel.
			// HandleChatSend fires RouteChat for every persisted message,
			// including ones the Frame just sent via SendChat — without
			// this guard, a Frame post containing "@frame" would queue a
			// spurious deliberation cycle on the same goroutine.
			//
			// But: record Frame-authored posts in the threads index so
			// rule 2c ("replying to a Frame-authored message") routes
			// future replies back to the funnel. Without this the index
			// stays empty and operator replies to keel's own messages
			// silently never reach keel. Surfaced on 2026-05-11 cutover.
			if from == frameName {
				if threads != nil {
					threads.RecordPost(msgID, topic)
				}
				return
			}
			// Route predicate: only deliberate on messages ShouldRouteToFrame
			// approves. The broker sends us every chat.send frame; we filter
			// here so the funnel only runs turns for messages the Frame cares about.
			routeMsg := route.Message{
				ID:      msgID,
				From:    from,
				Content: content,
				ReplyTo: replyTo,
				Topic:   topic,
			}
			// Build the roster-of-known-aspects for the addressing
			// check. Includes the Frame itself plus every live aspect
			// per the in-memory Roster. Snapshotted per-message so
			// roster churn between turns is reflected immediately.
			rosterNames := []string{frameName}
			if ros != nil {
				for _, n := range ros.AspectNames() {
					if n != frameName {
						rosterNames = append(rosterNames, n)
					}
				}
			}
			if !route.ShouldRouteToFrame(routeMsg, frameName, rosterNames, threads) {
				return
			}
			f.ReceiveWithMsgID(bridle.InboxItem{From: from, Content: content}, msgID)
			// NEX-210: when a Definition of Done is present, wrap
			// deliberation with the goal-loop so the post-turn judge
			// can drive multi-turn pursuit toward DoD completion.
			dod := extractDoD(content)
			if dod != "" {
				ticketID := resolveTicketID(rctx, topic, ledgerSvc, log)
				gl := funnel.NewGoalLoop(f, funnel.GoalConfig{
					TicketID:   ticketID,
					DoD:        dod,
					ThreadRoot: msgID,
				})
				for {
					result, err := gl.Pursue(rctx)
					if err != nil {
						log.Warn("frame funnel: goal-loop error", "err", err, "ticket", ticketID, "msg_id", msgID)
						break
					}
					if result.Done || result.Blocked {
						if result.Blocked {
							log.Info("frame funnel: goal-loop blocked", "ticket", ticketID, "turns", result.TurnsRun)
						}
						break
					}
				}
				return
			}
			// Drain the inbox one msg per turn (#224 FIFO contract).
			// Loop until ErrEmptyInbox so a burst that arrived while the
			// previous turn was running gets fully processed before
			// yielding. Each turn handles one msg in isolation; threads
			// stay separate by construction.
			for {
				_, err := f.Deliberate(rctx, "")
				if errors.Is(err, funnel.ErrEmptyInbox) {
					break
				}
				if err != nil {
					log.Warn("frame funnel: deliberation error", "err", err, "msg_id", msgID)
					break
				}
			}
		},
	}, gateway, f
}

// extractDoD parses a Definition of Done from message content.
// Recognises "Definition of Done:" (with optional leading "## ") as a
// section marker; returns the text from the marker to the next markdown
// header or end of content. Returns an empty dod when no marker is found.
func extractDoD(content string) (dod string) {
	const marker = "Definition of Done"
	idx := strings.Index(content, marker)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(marker):]
	rest = strings.TrimLeft(rest, ":\n\r\t ")
	if nextH2 := strings.Index(rest, "\n## "); nextH2 >= 0 {
		rest = rest[:nextH2]
	} else if nextH1 := strings.Index(rest, "\n# "); nextH1 >= 0 {
		rest = rest[:nextH1]
	}
	return strings.TrimSpace(rest)
}

// resolveTicketID determines the ledger ticket key for an inbound chat
// message. When the topic matches a PROJECT-NNN pattern (e.g. NEX-226) it
// verifies the key exists in the ledger. Otherwise it falls back to
// "unknown" and logs a warning. The queue manager (runQueueManager) is the
// authoritative source; this function is the best-effort fallback for
// chat-routed messages that arrive without queue-manager dispatch context.
func resolveTicketID(ctx context.Context, topic string, ledgerSvc *ledger.Service, log *slog.Logger) string {
	if topic == "" {
		log.Warn("frame: ticket ID not available; topic is empty, using 'unknown'")
		return "unknown"
	}
	if isTicketKey(topic) {
		if ledgerSvc == nil {
			log.Warn("frame: ticket ID not available; ledger service not available for lookup, using 'unknown'", "topic", topic)
			return "unknown"
		}
		if _, err := ledgerSvc.GetIssue(ctx, topic); err == nil {
			return topic
		}
		log.Warn("frame: ticket ID not available; topic matches ticket key pattern but not found in ledger, using 'unknown'", "topic", topic)
		return "unknown"
	}
	log.Warn("frame: ticket ID not available; topic does not match a ticket key pattern, using 'unknown'", "topic", topic)
	return "unknown"
}

// isTicketKey reports whether s matches a PROJECT-NNN shape (e.g. NEX-226).
func isTicketKey(s string) bool {
	if len(s) < 4 {
		return false
	}
	dash := strings.IndexByte(s, '-')
	if dash < 1 || dash >= len(s)-1 {
		return false
	}
	for _, c := range s[:dash] {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	for _, c := range s[dash+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// runQueueManager is the NEX-176 keel-as-queue-manager loop. It polls
// the ledger for ready tickets and dispatches work to assignee aspects.
// When a ticket has a Definition of Done, the queue manager injects a
// synthetic work item into the Frame's funnel so the NEX-210 goal-loop
// can drive multi-turn pursuit.
func runQueueManager(ctx context.Context, f *funnel.Funnel, ledgerSvc *ledger.Service, sender *broker.Broker, frameName string, log *slog.Logger) {
	const pollInterval = 30 * time.Second
	// Track tickets we've already dispatched so we don't double-send.
	active := make(map[string]bool)

	t := time.NewTicker(pollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// ListReady returns tickets in "To Do" or "In Progress" status
		// assigned to this Frame (directly or via team). The Frame's
		// own name is used as the polling identity — the queue manager
		// is the Frame acting as dispatcher.
		refs, err := ledgerSvc.ListReady(ctx, frameName, nil)
		if err != nil {
			log.Warn("queue-manager: ledger ListReady failed", "err", err)
			continue
		}

		seen := make(map[string]bool)
		for _, ref := range refs {
			seen[ref.Key] = true

			if active[ref.Key] {
				continue // already dispatched
			}

			// Fetch the full issue to get DefinitionOfDone + assignee.
			issue, err := ledgerSvc.GetIssue(ctx, ref.Key)
			if err != nil {
				log.Warn("queue-manager: GetIssue failed", "err", err, "key", ref.Key)
				continue
			}
			if issue.AssigneeAspect == "" {
				continue // can't dispatch without an assignee
			}
			if strings.TrimSpace(issue.DefinitionOfDone) == "" {
				continue // can't goal-pursue without a DoD
			}

			dispatchContent := buildDispatchBrief(issue)

			// Self-assigned tickets: inject directly into the Frame's
			// own funnel and drive via goal-loop. Chat-send would
			// route from frameName → RouteChat, and RouteChat skips
			// messages where from==frameName, so the Frame would
			// never process its own tickets through chat.
			if issue.AssigneeAspect == frameName {
				f.Receive(bridle.InboxItem{
					From:    frameName + " (queue-manager)",
					Content: dispatchContent,
				})
				gl := funnel.NewGoalLoop(f, funnel.GoalConfig{
					TicketID: issue.Key,
					DoD:      issue.DefinitionOfDone,
				})
				for {
					result, err := gl.Pursue(ctx)
					if err != nil {
						log.Warn("queue-manager: goal-loop error", "err", err, "key", issue.Key)
						break
					}
					if result.Done || result.Blocked {
						log.Info("queue-manager: goal-loop complete",
							"key", issue.Key, "turns", result.TurnsRun,
							"reason", result.Reason)
						break
					}
				}
			} else {
				// Remote aspect: send via chat. The assignee's WS
				// funnel picks it up; @-mention + DoD section
				// triggers the aspect's own goal-loop.
				if _, err := sender.HandleChatSend(ctx, frameName, dispatchContent, 0, issue.Key); err != nil {
					log.Warn("queue-manager: chat send failed", "err", err, "key", issue.Key)
					continue
				}
			}

			active[ref.Key] = true
			log.Info("queue-manager: dispatched ticket",
				"key", issue.Key,
				"assignee", issue.AssigneeAspect,
				"priority", issue.Priority,
			)
		}

		// Prune tracked tickets that are no longer in the ready pool.
		for key := range active {
			if !seen[key] {
				delete(active, key)
				log.Info("queue-manager: ticket left ready pool", "key", key)
			}
		}
	}
}

func reaper(ctx context.Context, r *roster.Roster, b *broker.Broker, staleAfter, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// Refresh heartbeats for aspects with a live WS connection
			// before sweeping. Lock 2: an open connection IS the
			// heartbeat under the WS transport, so the reaper would
			// otherwise mark every connected aspect stale after 30s.
			if b != nil {
				r.RefreshHeartbeats(b.ConnectedAspects(), now)
			}
			stale, down := r.ReapStale(now, staleAfter)
			for _, name := range stale {
				log.Warn("aspect stale", "name", name)
			}
			for _, name := range down {
				log.Error("aspect down", "name", name)
			}
		}
	}
}
