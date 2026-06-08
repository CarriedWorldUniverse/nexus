// Command nexus is the central Nexus process: broker, dispatch, and admin API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/ledger"
	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/autospawn"
	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/cwb/cwbproxy"
	"github.com/CarriedWorldUniverse/nexus/nexus/handqueue"
	"github.com/CarriedWorldUniverse/nexus/nexus/identity"
	"github.com/CarriedWorldUniverse/nexus/nexus/issuesrest"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability/jsonlsink"
	"github.com/CarriedWorldUniverse/nexus/nexus/operator"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/runs"
	"github.com/CarriedWorldUniverse/nexus/nexus/sessions"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	"k8s.io/client-go/kubernetes"
)

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
	if mres, merr := aspects.MigrateCentralFromAspect(ctx, db, ""); merr != nil {
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
	aspectIDs := discoverAspectIDs(*aspectDir, logger)
	if len(aspectIDs) > 0 {
		if err := tokenStore.ReconcileAgentTokens(ctx, db, aspectIDs); err != nil {
			logger.Error("token reconcile (aspects)", "err", err)
			os.Exit(1)
		}
		logger.Info("token reconcile (aspects)", "count", len(aspectIDs))
	}
	chatStore := chat.NewSQLStore(db)
	runsStore := runs.NewSQLStore(db)
	knowledgeStore := knowledge.New(db, logger)
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

	adminCallbacks := &broker.AdminCallbacks{
		Shutdown: func(_ context.Context) error {
			logger.Info("broker admin shutdown requested")
			stop()
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
		// Compact / Rewind: nil → 501 not_implemented until the broker
		// owns concrete backing operations for them.
	}

	// RecipientPolicy: who receives chat.deliver for each chat.send.
	// Used both by live fan-out (broker.HandleChatSend) and Lock 6
	// replay (broker.Replayer). One policy, two consumers.
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

	// NEXUS_CWB_EDGE: the interchange (CWB edge) base URL. Optional /
	// dark-by-default — when unset, the per-aspect token custodian is
	// not constructed (HeraldEdge empty) AND the CWB reverse-proxy routes
	// are not registered, so behavior is unchanged. When set, both
	// surfaces are enabled.
	cwbEdge := os.Getenv("NEXUS_CWB_EDGE")

	if authBypass {
		logger.Warn("operator auth bypass ENABLED — DO NOT use in production",
			"reason", "NEXUS_AUTH_BYPASS=1 set in environment")
	}

	// #21: derive canonical aspect homes from the discovery scan so
	// the register handler can override payload.Home (closes the
	// cmd.Dir control vector for stolen aspect tokens).
	aspectHomes := discoverAspectHomes(*aspectDir, logger)

	dispatchCfg := dispatch.JobConfig{
		Image:         os.Getenv("CW_BUILDER_IMAGE"),
		Namespace:     envOrDefault("CW_K8S_NAMESPACE", "nexus"),
		NodeIP:        os.Getenv("CW_NODE_IP"),
		BrokerHost:    os.Getenv("CW_BROKER_HOST"),
		BriefTimeout:  envOrDefault("CW_BRIEF_TIMEOUT", "30m"),
		GitCredName:   os.Getenv("CW_GIT_CRED_NAME"),
		ActivityDir:   filepath.Join(*dataDir, "activity"),
		LynxAIBaseURL: os.Getenv("LYNXAI_BASE_URL"),
		LynxAIKey:     os.Getenv("LYNXAI_KEY"),
	}
	// Dispatch Runner: each !dispatch runs AS the named agent (mounts that
	// agent's keyfile → its persona + attribution). Built unconditionally so
	// !dispatch works whenever the broker runs; the kube client is wired only
	// in-cluster (else dispatch is an inert no-op, not a crash). MaxConc caps
	// total concurrent runs across agents (per-agent concurrency is 1).
	maxConc := 4
	if v := os.Getenv("CW_MAX_CONCURRENT"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			maxConc = n
		}
	}
	var runner dispatch.Submitter
	dispatchRunner := &dispatch.Runner{Cfg: dispatchCfg, MaxConc: maxConc}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if k8s, kerr := dispatch.NewInClusterK8s(dispatchCfg.Namespace); kerr != nil {
			slog.Error("dispatch: in-cluster k8s client failed — Runner will not spawn jobs", "err", kerr)
		} else {
			dispatchRunner.K8sIface = k8s
			slog.Info("dispatch: in-cluster k8s client wired", "namespace", dispatchCfg.Namespace, "max_concurrent", maxConc)
		}
	} else {
		slog.Info("dispatch: not in-cluster (KUBERNETES_SERVICE_HOST unset) — Runner has no k8s client (no job spawn)")
	}
	if err := dispatchRunner.Init(ctx); err != nil {
		slog.Error("dispatch runner init failed — dispatch disabled", "err", err)
		dispatchRunner = nil
	} else {
		runner = dispatchRunner
	}
	var k8sReader kubernetes.Interface
	var k8sNamespace string
	if dispatchRunner != nil {
		if k8s, ok := dispatchRunner.K8sIface.(*dispatch.K8s); ok && k8s != nil {
			k8sReader = k8s.Client
			k8sNamespace = k8s.Namespace
		}
	}

	activityLogDir := filepath.Join(*dataDir, "activity")
	b := broker.New(broker.Config{
		Addr:               *addr,
		AuthToken:          token,
		AllowLegacyMaster:  allowLegacy,
		OperatorAuthBypass: authBypass,
		Tokens:             tokenStore,
		HeraldEdge:         cwbEdge,
		StaleAfter:         *staleAfter,
		Logger:             logger,
		Projection:         proj,
		HandQueue:          queue,
		Admin:              adminCallbacks,
		Replayer:           replayer,
		ChatStore:          chatStore,
		RunsStore:          runsStore,
		ActivityLogDir:     activityLogDir,
		K8sReader:          k8sReader,
		K8sNamespace:       k8sNamespace,
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
		// Operator login (dashboard-ws-port spec §2.2 / 5b1).
		// Constructed only when the Nexus has identity (signing
		// secret available) AND the operator endpoints are wanted.
		// We build it unconditionally when KeyfileValidator is
		// present — same prerequisite — so the dashboard SPA can
		// reach /api/operator/* once the broker is up.
		OperatorLogin: buildOperatorLogin(db, nexusIdentity.NexusID, nexusIdentity.SessionSigningSecret, *addr, logger),
		Observability: obsHub,
		Runner:        runner,
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
			mux.Handle(issuesrest.ProjectsPath, projectsHandler(ledgerSvc))
			mux.Handle("/healthz/issues", ledgerHandler)
			// NEXUS_CWB_EDGE: pass-through reverse-proxy to the CWB edge
			// (interchange) for /herald/ /cairn/ /ledger/ /knowledge/.
			// Dark-by-default — only registered when the edge is set.
			if cwbEdge != "" {
				if err := cwbproxy.Register(mux, cwbEdge); err != nil {
					logger.Error("cwb reverse-proxy", "err", err)
					os.Exit(1)
				}
				logger.Info("CWB reverse-proxy enabled", "edge", cwbEdge)
			}
		},
	}, r)

	// Wire the dispatch Runner's status Poster to the broker's own chat path
	// (the in-process equivalent of the deleted controller's wsasp send-chat)
	// and start the Job watch loop. Done post-New because the Poster needs the
	// broker. WatchLoop only runs when a k8s client is wired (in-cluster).
	if dispatchRunner != nil {
		dispatchRunner.Poster = dispatch.NewWsPoster(ctx, brokerChatSender{b: b})
		if dispatchRunner.K8sIface != nil {
			go func() { _ = dispatchRunner.WatchLoop(ctx) }()
			logger.Info("dispatch: Job watch loop started")
		}
	}

	// Activity log persistence: chain a jsonlsink writer onto the Hub's
	// fan-out alongside the live broker broadcast. Co-exists with
	// in-memory tail (Hub.Buffer) — adds a durable file-per-aspect-
	// per-day record so post-incident debugging has evidence to read.
	// Writes happen on background goroutines per aspect; channel-full
	// drops with logged warning rather than blocking emit. Closed by
	// the cleanup goroutine on shutdown.
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
		if !storage.ResolveOpenConfig(*dataDir).UsesLocalFile {
			logger.Info("storage watcher: skipped for remote libSQL database", "env_var", storage.EnvDBDSN)
			return
		}
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
		if !storage.ResolveOpenConfig(*dataDir).UsesLocalFile {
			logger.Info("storage verifier: skipped for remote libSQL database", "env_var", storage.EnvDBDSN)
			return
		}
		dbPath := storage.ResolvePath(*dataDir)
		err := storage.WatchWriteDurability(ctx, dbPath, db, 0 /*default interval*/, logger, stop)
		if errors.Is(err, storage.ErrWriteDurabilityFailed) {
			logger.Error("storage verifier: write-durability failure detected — broker shutting down for supervisor restart", "path", dbPath)
		} else if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("storage verifier: stopped with non-fatal error", "err", err, "path", dbPath)
		}
	}()

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

// envOrDefault returns os.Getenv(key) when non-empty, otherwise def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
