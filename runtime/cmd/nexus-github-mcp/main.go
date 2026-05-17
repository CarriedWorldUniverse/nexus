// Command nexus-github-mcp bridges GitHub's REST API to stdio MCP.
// One process == one aspect's GitHub identity. Credentials come from
// the keyfile's `github` block (username, email, PAT, default_org).
//
// The aspect-side keyfile is the single source of truth — no broker
// fetch, no env var coupling, no host-shared `gh` auth. Each aspect's
// commits and PRs are attributed to their own GitHub identity.
//
// Tools exposed (v1):
//
//	github.pr_create     — open a PR
//	github.pr_view       — fetch a PR's metadata
//	github.pr_list       — list PRs in a repo (state + filter)
//	github.pr_merge      — squash-merge a PR
//	github.pr_checks     — list CI check results on a PR
//	github.pr_diff       — fetch a PR's diff
//	github.issue_create  — open an issue
//	github.issue_view    — fetch an issue
//	github.issue_list    — list issues
//	github.run_view      — fetch a workflow run
//	github.api           — escape hatch for arbitrary REST calls
//
// All calls authenticate with the keyfile-supplied PAT using HTTPS
// Basic auth (username + PAT). PAT scopes expected: `repo`,
// `workflow`, `read:org`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

const aspectMCPName = "nexus-github"

func main() {
	var (
		keyfilePath = flag.String("keyfile", "", "Path to the aspect keyfile JSON. Required: must contain a `github` block.")
		logLevel    = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile     = flag.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for the MCP protocol stream.")
		probe       = flag.Bool("probe", false, "Don't start MCP — call GET /user and exit. Useful for credential smoke tests.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-github-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	if *keyfilePath == "" {
		log.Error("missing -keyfile")
		os.Exit(2)
	}
	kf, err := keyfile.Load(*keyfilePath)
	if err != nil {
		log.Error("keyfile load failed", "err", err, "path", *keyfilePath)
		os.Exit(2)
	}
	if kf.GitHub == nil {
		log.Error("keyfile has no github block", "path", *keyfilePath)
		os.Exit(2)
	}
	if kf.GitHub.Username == "" || kf.GitHub.PAT == "" {
		log.Error("keyfile.github missing required fields", "username_set", kf.GitHub.Username != "", "pat_set", kf.GitHub.PAT != "")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := newClient(kf.GitHub.Username, kf.GitHub.PAT, kf.GitHub.DefaultOrg, log)

	if *probe {
		user, err := c.WhoAmI(ctx)
		if err != nil {
			log.Error("probe failed", "err", err)
			os.Exit(3)
		}
		log.Info("probe ok", "login", user.Login, "id", user.ID)
		return
	}

	srv := mcpserver.NewMCPServer(aspectMCPName, "0.1.0",
		mcpserver.WithLogging(),
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, c, log)

	log.Info("nexus-github-mcp ready",
		"username", kf.GitHub.Username,
		"email", kf.GitHub.Email,
		"default_org", kf.GitHub.DefaultOrg,
	)

	if err := mcpserver.ServeStdio(srv); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!strings.Contains(err.Error(), "EOF") {
		log.Error("MCP stdio loop ended", "err", err)
	}
}

func buildLogger(level, file string) (*slog.Logger, func(), error) {
	w := os.Stderr
	closer := func() {}
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		w = f
		closer = func() { _ = f.Close() }
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), closer, nil
}
