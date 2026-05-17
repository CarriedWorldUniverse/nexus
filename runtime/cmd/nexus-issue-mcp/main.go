// Command nexus-issue-mcp bridges the in-process nexus issue tracker
// to stdio MCP. One process == one aspect identity. The keyfile
// provides the JWT auth path to reach the tracker via the nexus.exe
// HTTPS listener; no Jira credentials needed.
//
// Tools exposed:
//
//	issue.create  — create an issue
//	issue.get     — fetch by key (resolves aliases)
//	issue.search  — structured filter search
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

const aspectMCPName = "nexus-issue"

func main() {
	var (
		keyfilePath  = flag.String("keyfile", "", "Path to the aspect keyfile JSON.")
		nexusURLFlag = flag.String("nexus-url", "", "Override the HTTPS base URL.")
		insecureSkip = flag.Bool("insecure-skip-verify", false, "Skip TLS verify (dev only).")
		logLevel     = flag.String("log-level", "info", "slog level")
		logFile      = flag.String("log-file", "", "Write logs here.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-issue-mcp: logger setup: %v\n", err)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kc := keyfile.NewClient()
	if *insecureSkip {
		kc.HTTP = &http.Client{Timeout: 10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	}
	res, err := kc.Validate(ctx, kf)
	if err != nil {
		log.Error("keyfile validate failed", "err", err)
		os.Exit(2)
	}
	log.Info("keyfile validation succeeded", "aspect", res.AspectName, "nexus_id", kf.Envelope.NexusID)

	httpsBase := *nexusURLFlag
	if httpsBase == "" {
		httpsBase = strings.Replace(res.NexusURL, "wss://", "https://", 1)
		httpsBase = strings.TrimSuffix(httpsBase, "/connect")
	}

	client := newClient(httpsBase, res.SessionJWT, *insecureSkip, log)

	srv := mcpserver.NewMCPServer(aspectMCPName, "0.1.0",
		mcpserver.WithLogging(),
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, client, log)

	log.Info("nexus-issue-mcp ready", "aspect", res.AspectName, "base", httpsBase)

	if err := mcpserver.ServeStdio(srv); err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
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
