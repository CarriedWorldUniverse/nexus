// Command nexus-imap-mcp bridges an aspect's IMAP mailbox to stdio
// MCP. Credentials come from the same keyfile nexus-comms-mcp and
// nexus-jira-mcp read (the .imap block). One process per aspect.
//
// Tools exposed:
//
//   imap.list_folders   — enumerate mailboxes the user can see
//   imap.fetch_recent   — recent messages from a folder, filterable
//   imap.fetch_otp      — pull a 6-digit code from a recent message
//   imap.move           — move a UID to another folder (auto-creates)
//   imap.delete         — expunge a UID
//   imap.mark           — set/clear IMAP flags (Seen, Flagged, ...)
//
// Today only shadow uses this — to drive Atlassian first-login OTP
// flows on behalf of other aspects — but every aspect's keyfile can
// carry its own .imap block.

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

func main() {
	var (
		keyfilePath = flag.String("keyfile", "", "Path to the aspect keyfile JSON. The .imap block carries mailbox credentials.")
		logLevel    = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile     = flag.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for the MCP protocol stream.")
		probe       = flag.Bool("probe", false, "Don't start MCP — connect, login, SELECT INBOX, and exit. Useful for credential smoke tests.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-imap-mcp: logger setup: %v\n", err)
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
	if kf.IMAP == nil {
		log.Error("keyfile has no .imap block — add host/port/username/password to enable", "path", *keyfilePath)
		os.Exit(2)
	}

	client := NewClient(kf.IMAP.Host, kf.IMAP.Port, kf.IMAP.Username, kf.IMAP.Password)
	defaultFolder := kf.IMAP.DefaultFolder
	if defaultFolder == "" {
		defaultFolder = "INBOX"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := client.Probe(probeCtx); err != nil {
		cancel()
		log.Error("imap credential probe failed", "err", err)
		os.Exit(3)
	}
	cancel()
	log.Info("nexus-imap-mcp ready",
		"host", kf.IMAP.Host,
		"username", kf.IMAP.Username,
		"default_folder", defaultFolder)

	if *probe {
		return
	}

	srv := mcpserver.NewMCPServer("nexus-imap", "0.1.0",
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, client, defaultFolder, log)

	if err := mcpserver.ServeStdio(srv); err != nil {
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
			log.Error("MCP stdio loop ended", "err", err)
		}
	}
}

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

var _ = mcpgo.NewTool
