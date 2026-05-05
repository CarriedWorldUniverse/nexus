// Command nexus is the central Nexus process: broker, orchestrator, and
// (future) embedded frame-agent. v1 covers broker + in-memory roster +
// the stale-reap sweep; the orchestrator and frame-agent slots in as
// later spec migration steps.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	bridle "github.com/nexus-cw/bridle"
	claudeprovider "github.com/nexus-cw/bridle/provider/claude"
	claudecodeprovider "github.com/nexus-cw/bridle/provider/claudecode"
	"github.com/nexus-cw/nexus/nexus/autospawn"
	"github.com/nexus-cw/nexus/nexus/broker"
	"github.com/nexus-cw/nexus/nexus/chat"
	"github.com/nexus-cw/nexus/nexus/frame"
	"github.com/nexus-cw/nexus/nexus/frame/framecomms"
	"github.com/nexus-cw/nexus/nexus/frame/funnel"
	"github.com/nexus-cw/nexus/nexus/frame/route"
	"github.com/nexus-cw/nexus/nexus/handqueue"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/nexus/sessions"
	"github.com/nexus-cw/nexus/nexus/storage"
	"github.com/nexus-cw/nexus/nexus/usage"
)

// exitCodeBootstrapDone signals a successful first-boot setup. Supervisor
// scripts (or operator) restart the process; on the next boot, the new
// Frame is detected and Nexus comes up in normal mode.
const exitCodeBootstrapDone = 64

func main() {
	// Subcommand dispatch — `nexus cert <verb>` peels off here before
	// the broker flagset is parsed. Other subcommands land beside it.
	if len(os.Args) >= 2 && os.Args[1] == "cert" {
		os.Exit(runCertSubcommand(os.Args[2:]))
	}
	addr := flag.String("addr", ":7888", "broker listen address")
	tokenEnv := flag.String("token-env", "NEXUS_TOKEN", "env var holding the shared bearer token")
	staleAfter := flag.Duration("stale-after", 30*time.Second, "aspect becomes stale after this gap without heartbeat")
	reapEvery := flag.Duration("reap-every", 10*time.Second, "how often to sweep for stale aspects")
	dataDir := flag.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	aspectDir := flag.String("aspect-dir", "", "comma-separated directories to scan for aspect homes (falls back to NEXUS_ASPECT_DIR env; disabled if neither set). The broker uses this as the source of truth for aspect homes (#21).")
	harnessPath := flag.String("harness-path", "", "path to the harness binary used for auto-spawn (falls back to NEXUS_HARNESS env)")
	// Defaults from env so explicit `--tls-cert=` (empty) is honored
	// as the operator's intent (fail-fast at broker startup) rather
	// than silently falling back to env.
	tlsCert := flag.String("tls-cert", os.Getenv("NEXUS_TLS_CERT"), "path to TLS server cert PEM (default: NEXUS_TLS_CERT env). Required.")
	tlsKey := flag.String("tls-key", os.Getenv("NEXUS_TLS_KEY"), "path to TLS server key PEM (default: NEXUS_TLS_KEY env). Required.")
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
			Detected:   detectedFrame,
			Roster:     r,
			TokenStore: tokenStore,
			DB:         db,
			Logger:     logger,
		})
		if err != nil {
			logger.Error("frame embed failed", "err", err)
			os.Exit(1)
		}
		embeddedFrame = ef
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
	chatRouter, frameGateway := buildChatRouter(ctx, embeddedFrame, chatStore, usage.NewSQLStore(db), logger)

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

	// #21: derive canonical aspect homes from the discovery scan so
	// the register handler can override payload.Home (closes the
	// cmd.Dir control vector for stolen aspect tokens).
	aspectHomes := discoverAspectHomes(*aspectDir, logger)

	b := broker.New(broker.Config{
		Addr:              *addr,
		AuthToken:         token,
		AllowLegacyMaster: allowLegacy,
		Tokens:            tokenStore,
		StaleAfter:        *staleAfter,
		Logger:            logger,
		Projection:        proj,
		HandQueue:         queue,
		Admin:             adminCallbacks,
		ChatRouter:        chatRouter,
		Replayer:          replayer,
		ChatStore:         chatStore,
		RecipientPolicy:   recipientPolicy,
		AspectHomes:       aspectHomes,
		TLSCertFile:       *tlsCert,
		TLSKeyFile:        *tlsKey,
	}, r)

	// Wire the embedded Frame's chat gateway to broker.HandleChatSend so
	// in-process Frame posts persist + fan-out via the same canonical
	// path as out-of-process aspect WS frames (per
	// docs/2026-05-04-unify-frame-aspect-chat-path.md). When no Frame
	// is embedded, frameGateway is nil and this is a no-op.
	if frameGateway != nil {
		frameGateway.Sender = b
	}

	// Stale-reap sweep. Runs until ctx cancels. Reaper queries the
	// broker's dispatcher to refresh heartbeats for live WS-connected
	// aspects before the sweep — under the WS transport an open
	// connection IS the heartbeat per Lock 2.
	go reaper(ctx, r, b, *staleAfter, *reapEvery, logger)

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

	// Auto-spawn: after the broker has bound its listener (brief
	// delay), scan the aspect dir and fire off harness children.
	// Non-blocking; failures are logged per-aspect.
	go runAutoSpawn(ctx, logger, *aspectDir, *harnessPath, *addr, token,
		autospawn.AspectTokenResolverFunc(tokenResolverFunc))

	if err := b.ListenAndServe(ctx); err != nil {
		logger.Error("broker exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("nexus stopped")
}

// runAutoSpawn discovers aspect homes under aspectDir (or
// NEXUS_ASPECT_DIR env) and spawns a harness for each. Skipped if
// no dir is configured. Runs after a short delay so the broker's
// listener has bound before children try to dial in.
func runAutoSpawn(ctx context.Context, log *slog.Logger, aspectDirFlag, harnessPathFlag, brokerAddr, token string, tokens autospawn.AspectTokenResolver) {
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

	cfg := autospawn.Config{
		ScanDir:     dir,
		HarnessPath: harnessPath,
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
	if err := autospawn.Spawn(cfg, candidates); err != nil {
		log.Error("auto-spawn failed", "err", err)
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

// buildChatRouter returns the chat-router callbacks plus the gateway it
// wired the funnel to. The caller is expected to assign gateway.Sender
// to the broker after broker.New so in-process Frame posts go through
// Broker.HandleChatSend (the unified chat-send path). When ef is nil
// both returns are nil.
func buildChatRouter(ctx context.Context, ef *frame.EmbeddedFrame, store chat.Store, usageStore *usage.SQLStore, log *slog.Logger) (*broker.ChatRouterCallbacks, *framecomms.Gateway) {
	if ef == nil {
		return nil, nil
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
		p = claudecodeprovider.New()
	default:
		log.Warn("frame funnel: unrecognised provider; deliberation disabled",
			"provider", provider, "frame", ef.Aspect.Name)
		return nil, nil
	}

	if model == "" {
		log.Warn("frame funnel: no model configured in aspect.json; deliberation disabled",
			"frame", ef.Aspect.Name)
		return nil, nil
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
	commsRunner := funnel.CommsRunner{Gateway: gateway}
	pulser := &framecomms.ChatPulser{Gateway: gateway}
	recorder := &usageRecorderAdapter{store: usageStore}

	threads := route.NewThreadIndex()
	f, err := funnel.New(funnel.Config{
		AspectID:      ef.Aspect.Name,
		Harness:       bridle.NewHarness(p),
		Provider:      bridle.ProviderID(provider),
		Model:         model,
		Tools:         funnel.CommsToolDefs(),
		Runner:        funnel.ComposeRunner(commsRunner, &funnel.NullRunner{}),
		ChatGateway:   gateway,
		Threads:       threads,
		Pulser:        pulser,
		UsageRecorder: recorder,
		Logger:        log,
	})
	if err != nil {
		log.Error("frame funnel: construction failed; deliberation disabled",
			"err", err, "frame", ef.Aspect.Name)
		return nil, nil
	}

	log.Info("frame funnel: deliberation loop ready",
		"frame", ef.Aspect.Name, "provider", provider, "model", model,
		"tools", len(funnel.CommsToolDefs()))

	frameName := ef.Aspect.Name
	return &broker.ChatRouterCallbacks{
		RouteChat: func(rctx context.Context, msgID int64, from, content string, replyTo int64, topic string) {
			// Frame's own posts must never route back to the funnel.
			// HandleChatSend fires RouteChat for every persisted message,
			// including ones the Frame just sent via SendChat — without
			// this guard, a Frame post containing "@frame" would queue a
			// spurious deliberation cycle on the same goroutine.
			if from == frameName {
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
			if !route.ShouldRouteToFrame(routeMsg, frameName, threads) {
				return
			}
			f.ReceiveWithMsgID(bridle.InboxItem{From: from, Content: content}, msgID)
			if _, err := f.Deliberate(rctx, ""); err != nil {
				log.Warn("frame funnel: deliberation error", "err", err, "msg_id", msgID)
			}
		},
	}, gateway
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
