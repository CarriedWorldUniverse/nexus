// Command shadow-runner drives shadow's autonomous orchestrate loop (NEX-642):
// a heartbeat-coalescing Runner that re-invokes a fresh `claude -p` running the
// orchestrate drain skill. Stateless reasoner; all work-state lives in ledger.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch/shadowrunner"
)

func pendingExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func setPending(path string, on bool) error {
	if on {
		return os.WriteFile(path, []byte("1"), 0o644)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const defaultDrainPrompt = "Use the orchestrate skill to drain the ledger work queue: read the ready set, " +
	"decompose ready goals, dispatch ready leaf tasks, review landed PRs, auto-merge green low-risk ones, " +
	"escalate the rest. Then exit."

func main() {
	heartbeat := flag.Duration("heartbeat", 12*time.Minute, "unconditional resync drain interval")
	claudeBin := flag.String("claude", "claude", "path to the claude CLI")
	prompt := flag.String("prompt", defaultDrainPrompt, "drain prompt")
	stateDir := flag.String("state-dir", envOr("CW_RUNNER_STATE_DIR", "/var/lib/shadow-runner"), "pending-bit dir")
	timeout := flag.Duration("drain-timeout", 30*time.Minute, "max wall-clock per drain")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		log.Warn("shadow-runner: could not create state dir; pending bit will not persist", "dir", *stateDir, "err", err)
	}
	pendingPath := filepath.Join(*stateDir, "pending")

	drain := func(ctx context.Context) error {
		// Mark a drain as in-progress so a crash mid-drain re-drains on restart.
		if err := setPending(pendingPath, true); err != nil {
			log.Warn("shadow-runner: set pending failed", "err", err)
		}
		dctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		cmd := exec.CommandContext(dctx, *claudeBin, "-p", *prompt)
		cmd.Stdout = os.Stdout // captured by the pod log
		cmd.Stderr = os.Stderr
		log.Info("shadow-runner: invoking claude drain")
		err := cmd.Run()
		log.Info("shadow-runner: drain finished", "err", err)
		if cerr := setPending(pendingPath, false); cerr != nil {
			log.Warn("shadow-runner: clear pending failed", "err", cerr)
		}
		return err
	}

	r := shadowrunner.New(shadowrunner.Config{Heartbeat: *heartbeat, Log: log}, drain)

	// Restore across restart: if we died mid-drain (pending file present), re-drain now.
	if pendingExists(pendingPath) {
		log.Info("shadow-runner: pending bit found on start — triggering drain")
		r.Trigger()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	r.Run(ctx)
	log.Info("shadow-runner: stopped")
}
