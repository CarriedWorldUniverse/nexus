package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dry-parse <path-to-aspect.json>")
		os.Exit(1)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	var cfg schemas.AspectConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}
	if cfg.Name == "" {
		fmt.Fprintln(os.Stderr, "loader-fail: missing name")
		os.Exit(1)
	}
	if cfg.EffectiveRole() != schemas.RoleAspect && cfg.EffectiveRole() != schemas.RoleFrame {
		fmt.Fprintln(os.Stderr, "loader-fail: unknown role:", cfg.Role)
		os.Exit(1)
	}
	switch cfg.ContextMode {
	case schemas.ContextGlobal, schemas.ContextThread, schemas.ContextStateless:
	default:
		fmt.Fprintln(os.Stderr, "loader-fail: context_mode:", cfg.ContextMode)
		os.Exit(1)
	}
	fmt.Printf("OK: name=%s role=%s context_mode=%s provider=%s model=%v port=%d\n",
		cfg.Name, cfg.EffectiveRole(), cfg.ContextMode, cfg.Provider,
		cfg.ProviderConfig["model"], cfg.Port)
	fmt.Printf("    capabilities=%v\n", cfg.Capabilities)
	fmt.Printf("    nexus_url_env=%q auth_token_env=%q\n", cfg.NexusURLEnv, cfg.AuthTokenEnv)
}
