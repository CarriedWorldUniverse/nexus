// Command nexus is the central Nexus process: broker, orchestrator, and
// (future) embedded frame-agent. v1 covers broker + in-memory roster +
// the stale-reap sweep; the orchestrator and frame-agent slots in as
// later spec migration steps.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nexus-cw/nexus/nexus/autospawn"
	"github.com/nexus-cw/nexus/nexus/broker"
	"github.com/nexus-cw/nexus/nexus/handqueue"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/nexus/sessions"
	"github.com/nexus-cw/nexus/nexus/storage"
)

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

	token := os.Getenv(*tokenEnv)
	if token == "" {
		logger.Error("missing auth token", "env_var", *tokenEnv)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := storage.Open(ctx, *dataDir, logger)
	if err != nil {
		logger.Error("storage open failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	r := roster.New()
	proj := sessions.New(db)

	// Hand dispatch queue. Executor spawns harness subprocesses in
	// hand mode. Resolves aspect home paths from the roster — v1
	// only dispatches to aspects whose home is on this Nexus host;
	// cross-host hand dispatch lands when Outposts gain their own
	// queues.
	queue, err := handqueue.New(handqueue.Config{
		MaxConcurrent: 5,
		Executor: &handqueue.SpawnExecutor{
			HomeResolver: handqueue.AspectHomeResolverFunc(func(aspect string) (string, bool) {
				for _, a := range r.List() {
					if a.Name == aspect {
						return a.Home, true
					}
				}
				return "", false
			}),
			ExtraEnv: []string{
				"NEXUS_TOKEN=" + token,
			},
		},
		Logger: logger,
	})
	if err != nil {
		logger.Error("handqueue.New", "err", err)
		os.Exit(1)
	}

	b := broker.New(broker.Config{
		Addr:       *addr,
		AuthToken:  token,
		StaleAfter: *staleAfter,
		Logger:     logger,
		Projection: proj,
		HandQueue:  queue,
	}, r)

	// Stale-reap sweep. Runs until ctx cancels.
	go reaper(ctx, r, *staleAfter, *reapEvery, logger)

	// Auto-spawn: after the broker has bound its listener (brief
	// delay), scan the aspect dir and fire off harness children.
	// Non-blocking; failures are logged per-aspect.
	go runAutoSpawn(ctx, logger, *aspectDir, *harnessPath, *addr, token)

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
func runAutoSpawn(ctx context.Context, log *slog.Logger, aspectDirFlag, harnessPathFlag, brokerAddr, token string) {
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
			"NEXUS_TOKEN=" + token,
		},
		Logger: log,
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
