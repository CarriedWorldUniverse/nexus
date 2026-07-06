// Command nexus-skills-mcp serves the canonical nexus dev-lifecycle skill
// store over stdio MCP. It embeds skills/<name>/SKILL.md and exposes
// search_skills + get_skill for progressive disclosure.
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

const aspectMCPName = "nexus-skills"

func main() {
	var (
		logLevel       = flag.String("log-level", "info", "slog level")
		logFile        = flag.String("log-file", "", "Write logs here.")
		skillAllowlist = flag.String("skill-allowlist", "", "comma-separated skill names to serve for this spawn (role-at-spawn skill gating, M1 Unit 3; empty = all skills, the back-compat default). Falls back to the CW_SKILL_ALLOWLIST env var, which BuildJob sets from dispatch.Brief.SkillAllowlist.")
	)
	flag.Parse()

	log, closeLog, err := buildLogger(*logLevel, *logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nexus-skills-mcp: logger setup: %v\n", err)
		os.Exit(1)
	}
	defer closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = ctx

	allow := skillAllowlistFrom(*skillAllowlist, os.Getenv("CW_SKILL_ALLOWLIST"))
	srv := mcpserver.NewMCPServer(aspectMCPName, "0.1.0",
		mcpserver.WithLogging(),
		mcpserver.WithToolCapabilities(true),
	)
	registerTools(srv, log, allow)
	log.Info("nexus-skills-mcp ready", "skill_allowlist", allow)

	if err := mcpserver.ServeStdio(srv); err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "EOF") {
		log.Error("MCP stdio loop ended", "err", err)
	}
}

// skillAllowlistFrom resolves the skill-gating allow list: the -skill-
// allowlist flag wins when set, else the CW_SKILL_ALLOWLIST env var (the
// value BuildJob injects from dispatch.Brief.SkillAllowlist), else empty
// (all skills — today's ungated behavior). Both sources are a
// comma-separated list of exact skill names; blank entries are dropped.
func skillAllowlistFrom(flagVal, envVal string) []string {
	raw := flagVal
	if raw == "" {
		raw = envVal
	}
	if raw == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
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
