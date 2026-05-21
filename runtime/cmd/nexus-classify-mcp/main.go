// Command nexus-classify-mcp exposes AI-powered classification tools
// via stdio MCP. Uses the nexus/classification package under the hood
// with a bridle provider for model access. Credentials come from the
// provider's standard env vars (OPENAI_API_KEY etc.) — no keyfile or
// broker auth needed.
//
// Tools exposed (v0.1):
//
//	classify.pr_triage — classify a git diff as trivial/needs-review/suspicious
//
// Future NEX-243 lanes (comms digest, activity summary, ticket triage)
// land here as additional classify.* tools.
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

	"github.com/CarriedWorldUniverse/bridle"
	bridleopenai "github.com/CarriedWorldUniverse/bridle/provider/openai"
	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
)

const mcpName = "nexus-classify"
const mcpVersion = "0.1.0"

func main() {
	var (
		providerFlag = flag.String("provider", "openai-api", "bridle provider: openai-api")
		logLevel     = flag.String("log-level", "info", "slog level: debug|info|warn|error")
		logFile      = flag.String("log-file", "", "Write logs here instead of stderr; stdout is reserved for MCP")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-classify-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	_, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prov, provID, err := buildProvider(*providerFlag)
	if err != nil {
		log.Error("provider setup failed", "err", err, "provider", *providerFlag)
		os.Exit(2)
	}

	harness := bridle.NewHarness(prov)
	classifier := &classification.PRTriage{
		Harness:  harness,
		Provider: provID,
		Model:    "deepseek-chat",
		Logger:   log,
	}

	srv := mcpserver.NewMCPServer(mcpName, mcpVersion,
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, classifier, log)

	log.Info("nexus-classify-mcp ready", "provider", *providerFlag)

	if err := mcpserver.ServeStdio(srv); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!strings.Contains(err.Error(), "EOF") {
		log.Error("MCP stdio loop ended", "err", err)
	}
}

func buildProvider(name string) (bridle.Provider, bridle.ProviderID, error) {
	switch name {
	case "openai-api":
		return bridleopenai.New(""), bridle.ProviderOpenAI, nil
	default:
		return nil, "", fmt.Errorf("unknown provider: %q", name)
	}
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
