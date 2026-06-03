// Command agent is the single-aspect runtime binary. Soon to be
// renamed to harness per the transport spec; keeping the directory
// name for now to preserve git history. The hand-mode flag arrives
// in a later part.
//
// Usage:
//
//	agent -home <aspect-home-folder>
//
// Reads <home>/aspect.json, loads the configured provider (currently
// only claude-api), opens the session tree, dials upstream (Nexus
// directly or a local Outpost per NEXUS_OUTPOST/NEXUS_UPSTREAM env),
// registers, handles turn frames.
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

	"github.com/CarriedWorldUniverse/nexus/runtime/agent"
	"github.com/CarriedWorldUniverse/nexus/runtime/handexec"
	"github.com/CarriedWorldUniverse/nexus/runtime/heraldkeyfile"
	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
	claudeapi "github.com/CarriedWorldUniverse/nexus/runtime/providers/claude-api"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func main() {
	home := flag.String("home", "", "aspect home folder (must contain aspect.json)")
	tokenEnv := flag.String("token-env", "NEXUS_TOKEN", "env var holding the shared bearer token")
	handMode := flag.Bool("hand", false, "run in hand mode: read a dispatch request from stdin, execute once, exit")
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

	provider, err := buildProvider(cfg, absHome)
	if err != nil {
		log.Error("build provider", "err", err)
		os.Exit(1)
	}

	// Hand mode: single-turn execution, read from stdin, write to
	// stdout, exit. No WS connect, no registration — the spawning
	// dispatcher already holds the audit trail.
	if *handMode {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := handexec.Run(ctx, absHome, cfg, provider); err != nil {
			log.Error("handexec.Run", "err", err)
			os.Exit(1)
		}
		return
	}

	token := os.Getenv(*tokenEnv)
	if token == "" {
		log.Error("missing auth token", "env_var", *tokenEnv)
		os.Exit(2)
	}

	upstreamURL, isExplicitOutpost, err := resolveUpstream()
	if err != nil {
		log.Error("resolve upstream", "err", err)
		os.Exit(2)
	}

	var heraldKF *heraldkeyfile.Keyfile
	if p := os.Getenv("NEXUS_HERALD_KEYFILE"); p != "" {
		heraldKF, err = heraldkeyfile.Load(p)
		if err != nil {
			log.Error("load herald keyfile", "err", err)
			os.Exit(2)
		}
		log.Info("herald bootstrap keyfile loaded", "slug", heraldKF.Slug, "agent", heraldKF.KeyID)
	}

	a, err := agent.New(agent.Config{
		Home:                      absHome,
		Aspect:                    cfg,
		Provider:                  provider,
		UpstreamURL:               upstreamURL,
		UpstreamIsExplicitOutpost: isExplicitOutpost,
		AuthToken:                 token,
		HeraldKeyfile:             heraldKF,
		Logger:                    log,
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

// resolveUpstream applies the transport spec §3.1 precedence:
// NEXUS_OUTPOST if set → connect via local Outpost (fail-loudly).
// Else NEXUS_UPSTREAM → direct to Nexus.
// Else error.
// Returns (url, isExplicitOutpost, err).
func resolveUpstream() (string, bool, error) {
	if v := os.Getenv("NEXUS_OUTPOST"); v != "" {
		return v, true, nil
	}
	if v := os.Getenv("NEXUS_UPSTREAM"); v != "" {
		return v, false, nil
	}
	return "", false, errors.New("neither NEXUS_OUTPOST nor NEXUS_UPSTREAM is set")
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
func buildProvider(cfg schemas.AspectConfig, home string) (providers.Provider, error) {
	switch cfg.Provider {
	case claudeapi.ProviderName, "":
		return claudeapi.NewFromAspectHome(home)
	default:
		return nil, fmt.Errorf("unsupported provider %q (only claude-api wired in v1)", cfg.Provider)
	}
}
