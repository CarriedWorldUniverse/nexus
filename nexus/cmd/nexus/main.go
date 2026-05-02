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
	"syscall"
	"time"

	bridle "github.com/nexus-cw/bridle"
	claudeprovider "github.com/nexus-cw/bridle/provider/claude"
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
)

// exitCodeBootstrapDone signals a successful first-boot setup. Supervisor
// scripts (or operator) restart the process; on the next boot, the new
// Frame is detected and Nexus comes up in normal mode.
const exitCodeBootstrapDone = 64

func main() {
	addr := flag.String("addr", ":7888", "broker listen address")
	tokenEnv := flag.String("token-env", "NEXUS_TOKEN", "env var holding the shared bearer token")
	staleAfter := flag.Duration("stale-after", 30*time.Second, "aspect becomes stale after this gap without heartbeat")
	reapEvery := flag.Duration("reap-every", 10*time.Second, "how often to sweep for stale aspects")
	dataDir := flag.String("data-dir", "", "data directory holding nexus.db (falls back to NEXUS_DATA_DIR env, then ./data)")
	aspectDir := flag.String("aspect-dir", "", "directory to scan for aspect homes to auto-spawn (falls back to NEXUS_ASPECT_DIR env; disabled if neither set)")
	harnessPath := flag.String("harness-path", "", "path to the harness binary used for auto-spawn (falls back to NEXUS_HARNESS env)")
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
	chatRouter := buildChatRouter(ctx, embeddedFrame, chat.NewSQLStore(db), logger)

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

	b := broker.New(broker.Config{
		Addr:       *addr,
		AuthToken:  token,
		Tokens:     tokenStore,
		StaleAfter: *staleAfter,
		Logger:     logger,
		Projection: proj,
		HandQueue:  queue,
		Admin:      adminCallbacks,
		ChatRouter: chatRouter,
	}, r)

	// Stale-reap sweep. Runs until ctx cancels.
	go reaper(ctx, r, *staleAfter, *reapEvery, logger)

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
	wsURL := "ws://" + upstream + "/connect"

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
// NEXUS_ASPECT_DIR. Returns "" when neither is set, in which case the
// caller skips frame detection (and bootstrap mode is unreachable —
// operator must point Nexus at an agents dir for first-boot to work).
func resolveAspectDir(aspectDirFlag string) string {
	if aspectDirFlag != "" {
		return aspectDirFlag
	}
	return os.Getenv("NEXUS_ASPECT_DIR")
}

func discoverAspectIDs(aspectDirFlag string, log *slog.Logger) []string {
	dir := aspectDirFlag
	if dir == "" {
		dir = os.Getenv("NEXUS_ASPECT_DIR")
	}
	if dir == "" {
		return nil
	}
	candidates, err := autospawn.Discover(autospawn.Config{ScanDir: dir})
	if err != nil {
		log.Warn("discover aspect ids: scan failed; tokens not reconciled", "dir", dir, "err", err)
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
func buildChatRouter(ctx context.Context, ef *frame.EmbeddedFrame, store chat.Store, log *slog.Logger) *broker.ChatRouterCallbacks {
	if ef == nil {
		return nil
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
	default:
		log.Warn("frame funnel: unrecognised provider; deliberation disabled",
			"provider", provider, "frame", ef.Aspect.Name)
		return nil
	}

	if model == "" {
		log.Warn("frame funnel: no model configured in aspect.json; deliberation disabled",
			"frame", ef.Aspect.Name)
		return nil
	}

	// F1.4b.4: wire the comms tool surface (Lock 3) and the
	// chat-pulse impl (Lock 5). The Frame's gateway writes via the
	// chat.Store; CommsRunner translates send_chat / react_to /
	// chat.read / announce_file / share_file tool calls into
	// gateway methods. ChatPulser fires real chat-visible status
	// pulses via the same gateway, replacing F1.3's NoopPulser
	// default.
	gateway := framecomms.NewGateway(store, ef.Aspect.Name)
	commsRunner := funnel.CommsRunner{Gateway: gateway}
	pulser := &framecomms.ChatPulser{Gateway: gateway}

	threads := route.NewThreadIndex()
	f, err := funnel.New(funnel.Config{
		AspectID: ef.Aspect.Name,
		Harness:  bridle.NewHarness(p),
		Provider: bridle.ProviderID(provider),
		Model:    model,
		Tools:    funnel.CommsToolDefs(),
		Runner:   funnel.ComposeRunner(commsRunner, &funnel.NullRunner{}),
		Threads:  threads,
		Pulser:   pulser,
		Logger:   log,
	})
	if err != nil {
		log.Error("frame funnel: construction failed; deliberation disabled",
			"err", err, "frame", ef.Aspect.Name)
		return nil
	}

	log.Info("frame funnel: deliberation loop ready",
		"frame", ef.Aspect.Name, "provider", provider, "model", model,
		"tools", len(funnel.CommsToolDefs()))

	frameName := ef.Aspect.Name
	return &broker.ChatRouterCallbacks{
		RouteChat: func(rctx context.Context, msgID int64, from, content string, replyTo int64, topic string) {
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
			f.Receive(bridle.InboxItem{From: from, Content: content})
			if _, err := f.Deliberate(rctx, ""); err != nil {
				log.Warn("frame funnel: deliberation error", "err", err, "msg_id", msgID)
			}
		},
	}
}

func reaper(ctx context.Context, r *roster.Roster, staleAfter, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
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
