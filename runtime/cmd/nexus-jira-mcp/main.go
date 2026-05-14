// Command nexus-jira-mcp bridges Atlassian Jira REST to stdio MCP.
// One process == one aspect's Atlassian identity. Credentials are
// pulled from the same keyfile nexus-comms-mcp reads (the .jira
// section, unencrypted alongside the envelope).
//
// Tools exposed:
//
//   jira.search          — generic JQL search
//   jira.get             — fetch a single issue
//   jira.list_my_issues  — what's assigned to me, optionally filtered by status
//   jira.list_ready      — Ready/To Do work this aspect could claim
//   jira.claim           — set self as assignee + move to In Progress
//   jira.comment         — post a plain-text comment
//   jira.update_status   — transition by status name
//   jira.create          — file an Epic / Story / Task / Subtask / Bug
//   jira.complete        — transition to Done (or In Review when awaitReview=true)
//
// Identity is whoever the keyfile.Jira block authenticates as. For
// shadow's keyfile, that's shadow's Atlassian account. The MCP host
// gets exactly the surface the keyfile owner can do — no privilege
// escalation, no service-account impersonation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

const aspectMCPName = "nexus-jira"

func main() {
	var (
		keyfilePath = flag.String("keyfile", "", "Path to the aspect keyfile JSON. The .jira block carries Atlassian credentials.")
		logLevel    = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile     = flag.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for the MCP protocol stream.")
		probe       = flag.Bool("probe", false, "Don't start MCP — call /rest/api/3/myself and exit. Useful for credential smoke tests.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-jira-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	if *keyfilePath == "" {
		log.Error("missing -keyfile")
		os.Exit(2)
	}
	kf, err := loadKeyfile(*keyfilePath)
	if err != nil {
		log.Error("keyfile load failed", "err", err, "path", *keyfilePath)
		os.Exit(2)
	}
	if kf.Jira == nil {
		log.Error("keyfile has no .jira block — add Site/Email/APIToken/ProjectKey to enable", "path", *keyfilePath)
		os.Exit(2)
	}
	client := newJiraClient(kf.Jira.Site, kf.Jira.Email, kf.Jira.APIToken, kf.Jira.ProjectKey, nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Credential smoke test — confirms we can reach Atlassian + auth
	// works before starting the MCP loop.
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	accountID, err := client.MyAccountID(probeCtx)
	cancel()
	if err != nil {
		log.Error("jira credential probe failed", "err", err)
		os.Exit(3)
	}
	log.Info("nexus-jira-mcp ready",
		"site", kf.Jira.Site,
		"email", kf.Jira.Email,
		"account_id", accountID,
		"project_key", kf.Jira.ProjectKey)

	if *probe {
		return
	}

	srv := mcpserver.NewMCPServer("nexus-jira", "0.1.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, client, log)

	if err := mcpserver.ServeStdio(srv); err != nil {
		// Stdio close (peer hung up) is the normal shutdown path;
		// don't dignify with an error.
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
			log.Error("MCP stdio loop ended", "err", err)
		}
	}
}

// loadKeyfile reads the keyfile JSON and resolves the path relative to
// the working directory so callers can pass plain "shadow.keyfile.json"
// the same way nexus-comms-mcp does.
func loadKeyfile(path string) (*keyfile.Keyfile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var kf keyfile.Keyfile
	if err := json.Unmarshal(buf, &kf); err != nil {
		return nil, fmt.Errorf("parse keyfile: %w", err)
	}
	return &kf, nil
}

// buildLogger returns a slog.Logger pointed at logFile (when set) or
// stderr (default). NEVER stdout — MCP claims that channel.
func buildLogger(levelStr, logFile string) (*slog.Logger, func(), error) {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, func() {}, fmt.Errorf("bad log level: %s", levelStr)
	}
	out := os.Stderr
	closer := func() {}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, func() {}, err
		}
		out = f
		closer = func() { _ = f.Close() }
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), closer, nil
}

// Compile-time guard so mcpgo's tool helpers stay referenced.
var _ = mcpgo.NewTool
