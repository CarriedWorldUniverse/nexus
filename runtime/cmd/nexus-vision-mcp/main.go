// Command nexus-vision-mcp gives pool agents eyes. It serves two stdio-MCP
// tools — read_image and read_video — that route an image (or sampled video
// frames) to a local multimodal model through litellm's OpenAI-compatible
// gateway (default the sovereign gemma-4-12b "vision" route on dMon's 5090)
// and return a plain-text description. DeepSeek has no vision on its API and
// Gemini stays art-only, so the default is deliberately the local model; the
// endpoint/model are env-overridable so a future API vision model is a config
// change, not a code change.
//
// Env (all optional; defaults target the in-cluster litellm gateway):
//
//	VISION_BASE_URL   OpenAI-compatible base, default http://litellm.model-stack.svc.cluster.local:4000/v1
//	VISION_MODEL      model/route name, default "vision" (litellm → gemma-4-12b)
//	VISION_API_KEY    bearer, default "dummy" (litellm/vLLM are keyless locally)
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
)

const mcpName = "nexus-vision"

func main() {
	var (
		logLevel = flag.String("log-level", "info", "slog level")
		logFile  = flag.String("log-file", "", "Write logs here.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-vision-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = ctx

	cfg := visionConfigFromEnv()
	srv := mcpserver.NewMCPServer(mcpName, "0.1.0",
		mcpserver.WithLogging(),
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, log, cfg)
	log.Info("nexus-vision-mcp ready", "base_url", cfg.BaseURL, "model", cfg.Model)

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
