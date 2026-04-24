// Command agent is the single-aspect runtime binary.
//
// Usage:
//
//	agent -home <aspect-home-folder> [-nexus <url>] [-token-env <var>]
//
// Reads <home>/aspect.json, loads the configured provider (currently
// only claude-api), opens the session tree, registers with Nexus,
// runs the heartbeat loop, serves POST /turn.
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
	"syscall"

	"github.com/nexus-cw/nexus/runtime/agent"
	"github.com/nexus-cw/nexus/runtime/providers"
	claudeapi "github.com/nexus-cw/nexus/runtime/providers/claude-api"
	"github.com/nexus-cw/nexus/shared/schemas"
)

func main() {
	home := flag.String("home", "", "aspect home folder (must contain aspect.json)")
	nexusURL := flag.String("nexus", "http://localhost:7888", "Nexus base URL")
	tokenEnv := flag.String("token-env", "NEXUS_TOKEN", "env var holding the shared bearer token")
	listen := flag.String("listen", ":0", "agent turn-endpoint bind address (\":0\" picks an ephemeral port)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *home == "" {
		log.Error("missing -home flag")
		os.Exit(2)
	}
	absHome, err := filepath.Abs(*home)
	if err != nil {
		log.Error("resolve home", "err", err)
		os.Exit(2)
	}

	cfg, err := loadAspectConfig(absHome)
	if err != nil {
		log.Error("load aspect.json", "err", err)
		os.Exit(2)
	}

	token := os.Getenv(*tokenEnv)
	if token == "" {
		log.Error("missing auth token", "env_var", *tokenEnv)
		os.Exit(2)
	}

	provider, err := buildProvider(cfg, absHome)
	if err != nil {
		log.Error("build provider", "err", err)
		os.Exit(1)
	}

	a, err := agent.New(agent.Config{
		Home:       absHome,
		Aspect:     cfg,
		Provider:   provider,
		NexusURL:   *nexusURL,
		AuthToken:  token,
		Logger:     log,
		ListenAddr: *listen,
	})
	if err != nil {
		log.Error("agent new", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Start(ctx); err != nil {
		log.Error("agent start", "err", err)
		os.Exit(1)
	}
	log.Info("agent stopped")
}

// loadAspectConfig parses aspect.json in the home folder.
func loadAspectConfig(home string) (schemas.AspectConfig, error) {
	path := filepath.Join(home, "aspect.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return schemas.AspectConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg schemas.AspectConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return schemas.AspectConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		return schemas.AspectConfig{}, errors.New("aspect.json: missing name")
	}
	return cfg, nil
}

// buildProvider constructs the provider adapter named in aspect.json.
// Only claude-api wired in v1; openai-api / gemini-api / ollama-local
// (for chat if ever added) can slot in here.
func buildProvider(cfg schemas.AspectConfig, home string) (providers.Provider, error) {
	switch cfg.Provider {
	case claudeapi.ProviderName, "":
		return claudeapi.NewFromAspectHome(home)
	default:
		return nil, fmt.Errorf("unsupported provider %q (only claude-api wired in v1)", cfg.Provider)
	}
}
