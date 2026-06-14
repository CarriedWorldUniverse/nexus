package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

// runDrain performs ONE one-shot autonomous orchestrate drain, then exits. By
// the time it runs, agentfunnel has validated the keyfile, materialised
// shadow's MCP profile into .mcp.json in cwd, and applied any broker provider
// creds — so a plain `claude -p` here is a fully-tooled agentic run (jira /
// comms / dispatch MCPs + bash for gh). This is the heartbeat CronJob's worker:
// the k8s CronJob is the timer, the JiraGate is the cost gate, this is the drain.
//
// It never returns — it os.Exits with the drain's status so the pod terminates
// per fire.
func runDrain(log *slog.Logger, res *keyfile.ValidationResult, claudePath, prompt string) {
	// Export the session JWT so cw (the git credential helper) + the comms /
	// dispatch seam authenticate during the drain — same as builder mode.
	if res.SessionJWT != "" {
		if err := os.Setenv("CW_TOKEN", res.SessionJWT); err != nil {
			log.Warn("drain: failed to export CW_TOKEN", "err", err)
		}
	}
	// Bridge gh/git so PR review + squash-merge work in the drain. Non-fatal
	// (a pre-wired image may already have a working helper) but logged loudly —
	// merges fail later without it.
	if out, err := exec.CommandContext(context.Background(), "cw", "setup-git", "github").CombinedOutput(); err != nil {
		log.Error("drain: cw setup-git github failed — gh not bridged, PR merge may fail",
			"err", err, "out", strings.TrimSpace(string(out)))
	} else {
		log.Info("drain: gh/git bridged for PR review + merge")
	}

	bin := claudePath
	if bin == "" {
		bin = "claude"
	}
	timeout := 30 * time.Minute
	if v := os.Getenv("CW_DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	log.Info("drain: invoking claude one-shot orchestrate drain",
		"aspect", res.AspectName, "timeout", timeout)
	// --dangerously-skip-permissions: the drain is autonomous in a sandboxed pod
	// (the deployment sets IS_SANDBOX=1); the orchestrate procedure's own
	// escalation gates — not interactive prompts — bound what it may do.
	cmd := exec.CommandContext(ctx, bin, "-p", "--dangerously-skip-permissions", prompt)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Error("drain: claude drain exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("drain: complete")
	os.Exit(0)
}
