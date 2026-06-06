// Package autospawn scans a directory for aspect home folders and
// fires off a harness subprocess per aspect. Fire-and-forget per
// transport spec §7 — no supervision, no restart. Container
// orchestration (Docker, k8s, systemd) is the supervisor layer.
//
// Used by both Nexus and Outpost on startup to bring configured
// aspects up automatically.
package autospawn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// AspectTokenResolver looks up the per-aspect bearer token for the
// child process. Mirrors handqueue.AspectTokenResolver: when an aspect
// is being autospawned, that aspect's harness must authenticate to the
// broker AS that aspect (per-aspect identity §5.4), not via a shared
// master token. Spawn injects the resolver-supplied token as
// NEXUS_TOKEN in the child env, overriding any NEXUS_TOKEN passed via
// BaseEnv.
//
// Returns (token, true) when the store has an entry for the aspect;
// (empty, false) on miss. On miss, Spawn falls back to whatever
// NEXUS_TOKEN is in BaseEnv — typically the legacy shared token —
// so boot ordering and unknown-aspect cases degrade gracefully
// instead of failing closed. (Once all aspects have been reconciled,
// the legacy fallback can be dropped — see follow-on cleanup task.)
type AspectTokenResolver interface {
	TokenFor(aspect string) (string, bool)
}

// AspectTokenResolverFunc adapts a plain function to AspectTokenResolver.
type AspectTokenResolverFunc func(aspect string) (string, bool)

// TokenFor implements AspectTokenResolver.
func (f AspectTokenResolverFunc) TokenFor(aspect string) (string, bool) {
	return f(aspect)
}

// Config tunes the scan + spawn behaviour.
type Config struct {
	// ScanDir is the directory to scan. Defaults to "./aspects".
	ScanDir string

	// HarnessPath is the absolute path to the agentfunnel binary.
	// Required — autospawn doesn't search PATH to avoid ambiguity
	// over which harness to use in multi-installation hosts.
	HarnessPath string

	// AgoraPath is the absolute path to the agora binary. When an
	// aspect's PrimarySurface is "agora", autospawn uses this path
	// instead of HarnessPath. Empty → agora-surface aspects fall
	// back to HarnessPath with a warning.
	AgoraPath string

	// KeyfileDir is the directory that holds per-aspect keyfile JSON.
	// When non-empty, autospawn invokes the harness as
	// `harness -k <KeyfileDir>/<name>.keyfile.json` (the agentfunnel
	// contract). When empty, autospawn falls back to the legacy
	// `-home <aspect-path>` form retained for harness binaries that
	// resolve identity from the home dir themselves.
	KeyfileDir string

	// BaseEnv is propagated to every spawned child. NEXUS_UPSTREAM
	// / NEXUS_OUTPOST / NEXUS_TOKEN live here. Parent's os.Environ
	// is also inherited so $PATH and basic settings work.
	//
	// When TokenResolver is set and yields a per-aspect token, that
	// per-aspect NEXUS_TOKEN overrides any NEXUS_TOKEN entry in
	// BaseEnv for that child. BaseEnv's NEXUS_TOKEN therefore acts
	// as the legacy fallback for aspects the resolver doesn't know
	// about (e.g. unrecognised in the token store at startup).
	BaseEnv []string

	// TokenResolver maps aspect name → bearer token. Optional; when
	// nil, every child inherits BaseEnv's NEXUS_TOKEN unchanged
	// (legacy shared-token mode). When set, each child's NEXUS_TOKEN
	// is the resolver-supplied per-aspect token, with the BaseEnv
	// value reserved for the resolver-miss fallback path.
	TokenResolver AspectTokenResolver

	// Logger is optional.
	Logger *slog.Logger
}

// DefaultScanDir is used when Config.ScanDir is empty.
const DefaultScanDir = "./aspects"

// Candidate is a discovered aspect home.
type Candidate struct {
	Path   string
	Name   string
	Config schemas.AspectConfig
}

// DiscoverRoots scans multiple aspect-dir roots and returns the
// combined candidate list. Per #21 the broker uses this to derive
// canonical aspect homes — the source of truth for "where does
// aspect X live" is the broker's scan, not the aspect's
// self-reported register payload. Aspects discovered in earlier
// roots win on duplicate names; the operator should not configure
// the same aspect under multiple roots.
func DiscoverRoots(cfg Config, roots []string) ([]Candidate, error) {
	if len(roots) == 0 {
		return Discover(cfg)
	}
	seen := make(map[string]struct{})
	var combined []Candidate
	for _, root := range roots {
		one := cfg
		one.ScanDir = root
		got, err := Discover(one)
		if err != nil {
			return nil, fmt.Errorf("autospawn: scan %q: %w", root, err)
		}
		for _, c := range got {
			if _, dup := seen[c.Name]; dup {
				continue
			}
			seen[c.Name] = struct{}{}
			combined = append(combined, c)
		}
	}
	return combined, nil
}

// Discover lists all subdirectories of ScanDir that contain a
// valid aspect.json and are not opted-out (auto_spawn: false).
func Discover(cfg Config) ([]Candidate, error) {
	dir := cfg.ScanDir
	if dir == "" {
		dir = DefaultScanDir
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no scan dir = no candidates
		}
		return nil, fmt.Errorf("autospawn: read %s: %w", dir, err)
	}

	var candidates []Candidate
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		aspectPath := filepath.Join(dir, ent.Name())
		jsonPath := filepath.Join(aspectPath, "aspect.json")
		raw, err := os.ReadFile(jsonPath)
		if err != nil {
			continue // not an aspect home, skip silently
		}
		var aspectCfg schemas.AspectConfig
		if err := json.Unmarshal(raw, &aspectCfg); err != nil {
			continue // malformed; skip
		}
		if aspectCfg.Name == "" {
			continue
		}
		// Anything other than an aspect-like role (operator, typos)
		// must not be spawned.
		role := aspectCfg.EffectiveRole()
		if role != schemas.RoleAspect && role != schemas.RoleFrame {
			continue
		}
		// auto_spawn: false opt-out lives under metadata for
		// forward-compat without having to extend the canonical
		// schema.
		if optOut(aspectCfg) {
			continue
		}
		abs, err := filepath.Abs(aspectPath)
		if err != nil {
			abs = aspectPath
		}
		candidates = append(candidates, Candidate{
			Path:   abs,
			Name:   aspectCfg.Name,
			Config: aspectCfg,
		})
	}
	return candidates, nil
}

// optOut checks for metadata.auto_spawn == false.
func optOut(cfg schemas.AspectConfig) bool {
	if cfg.Metadata == nil {
		return false
	}
	if v, ok := cfg.Metadata["auto_spawn"]; ok {
		if b, ok := v.(bool); ok && !b {
			return true
		}
	}
	return false
}

// Supervisor tracks the harness subprocesses Spawn has started so that
// the parent process can kill them on shutdown. Without this, Windows
// (no SIGTERM, no process groups) leaks one funnel per aspect per
// nexus run — restart loops accumulate orphans in Task Manager.
type Supervisor struct {
	mu       sync.Mutex
	children []supervisedChild
	log      *slog.Logger
}

type supervisedChild struct {
	name string
	cmd  *exec.Cmd
	// done closes when the reaper goroutine sees the process exit.
	done chan struct{}
}

// track records a started child for later Shutdown.
func (s *Supervisor) track(name string, cmd *exec.Cmd, done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.children = append(s.children, supervisedChild{name: name, cmd: cmd, done: done})
}

// Shutdown kills every tracked child and waits for them to exit (or
// for ctx to fire). On Unix a graceful SIGTERM would be nicer, but
// agentfunnel doesn't currently install a signal handler and Windows
// has no SIGTERM at all — Process.Kill (SIGKILL / TerminateProcess) is
// the portable contract. Caller is responsible for ordering: shut down
// the broker first so children get clean WS-close events before being
// terminated.
//
// Safe to call once; subsequent calls are no-ops because the tracked
// slice is drained.
func (s *Supervisor) Shutdown(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	children := s.children
	s.children = nil
	s.mu.Unlock()

	log := s.log
	if log == nil {
		log = slog.Default()
	}

	for _, ch := range children {
		if ch.cmd.Process == nil {
			continue
		}
		if err := ch.cmd.Process.Kill(); err != nil {
			// Already-exited is the common "error" here — the reaper
			// goroutine got there first. Not worth surfacing as a
			// warning.
			log.Debug("autospawn: kill returned",
				"aspect", ch.name, "pid", ch.cmd.Process.Pid, "err", err)
		} else {
			log.Info("autospawn: killed child on shutdown",
				"aspect", ch.name, "pid", ch.cmd.Process.Pid)
		}
	}

	// Wait for the reaper goroutines to drain so the parent doesn't
	// exit while pipes are still being read. On ctx expiry we keep
	// iterating the rest of the children — Kill was already sent to
	// every PID above, so the remaining items have nothing useful to
	// block on, but we do want a log line per skipped child so a slow
	// pipe drain doesn't silently mask itself.
	for _, ch := range children {
		select {
		case <-ch.done:
		case <-ctx.Done():
			log.Warn("autospawn: shutdown context expired before child reaped",
				"aspect", ch.name)
		}
	}
}

// Spawn starts a harness subprocess for each candidate. Returns a
// Supervisor that tracks the children so the parent can kill them on
// shutdown (Shutdown). Without supervision, Windows nexus restarts
// leak one funnel per aspect per run — Task Manager fills up with
// orphaned agentfunnel.exe.
//
// If a child dies on its own, we still do NOT restart it (the spec
// §7 "container-orchestrator supervises" contract is unchanged). The
// supervisor is only for the parent-driven kill on exit.
func Spawn(cfg Config, candidates []Candidate) (*Supervisor, error) {
	if cfg.HarnessPath == "" {
		return nil, errors.New("autospawn.Spawn: HarnessPath required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	sup := &Supervisor{log: log}

	for _, c := range candidates {
		surface := c.Config.EffectivePrimarySurface()
		var cmd *exec.Cmd
		if cfg.KeyfileDir != "" {
			keyfilePath := filepath.Join(cfg.KeyfileDir, c.Name+".keyfile.json")
			switch surface {
			case schemas.SurfaceAgora:
				if cfg.AgoraPath != "" {
					cmd = exec.Command(cfg.AgoraPath, "--keyfile", keyfilePath)
				} else {
					log.Warn("autospawn: aspect wants agora but no AgoraPath configured; falling back to funnel",
						"aspect", c.Name)
					cmd = exec.Command(cfg.HarnessPath, "-k", keyfilePath)
				}
			default:
				cmd = exec.Command(cfg.HarnessPath, "-k", keyfilePath)
			}
		} else {
			cmd = exec.Command(cfg.HarnessPath, "-home", c.Path)
		}
		// Anchor cwd to the aspect's home directory. Without this every
		// spawned agentfunnel inherits the broker's cwd, which means
		// claude-code subprocesses (which derive their session jsonl
		// path from cwd) write every aspect's sessions to the SAME
		// projects/<broker-cwd-slug>/ dir — mixed across all aspects.
		// Surfaced 2026-05-14 (operator).
		//
		// This makes per-aspect Cwd actually work end-to-end. funnel/
		// bridle code already passes req.Cwd to claudecode.cmd.Dir, but
		// only when the funnel knows the aspect home — and agentfunnel
		// didn't set AspectHome on funnel.Config. Fixing at the spawn
		// layer (here) is the smallest correct change: the child's
		// os.Getwd() now returns the aspect dir, so even paths that
		// don't go through req.Cwd land in the right place.
		cmd.Dir = c.Path
		cmd.Env = childEnv(os.Environ(), cfg.BaseEnv, cfg.TokenResolver, c.Name)

		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			log.Error("autospawn: failed to start", "aspect", c.Name, "err", err)
			continue
		}

		// Fan stdout and stderr through tagged log forwarders. Each
		// child spawns two goroutines that run until the pipe closes
		// (when the child exits).
		pipeDone := make(chan struct{}, 2)
		go func() { logPipe(stdout, c.Name, log, false); pipeDone <- struct{}{} }()
		go func() { logPipe(stderr, c.Name, log, true); pipeDone <- struct{}{} }()

		log.Info("autospawn: started", "aspect", c.Name, "home", c.Path, "pid", cmd.Process.Pid)

		done := make(chan struct{})
		sup.track(c.Name, cmd, done)

		// Reap the child once both pipes drain. Without cmd.Wait, Unix
		// leaves zombies and Windows leaks the OS process handle —
		// long-running brokers across many aspect restarts hit
		// fd/handle limits otherwise (#26).
		go func(cmd *exec.Cmd, name string, done chan struct{}) {
			defer close(done)
			<-pipeDone
			<-pipeDone
			err := cmd.Wait()
			if err != nil {
				log.Warn("autospawn: child exited",
					"aspect", name, "pid", cmd.Process.Pid, "err", err)
			} else {
				log.Info("autospawn: child exited cleanly",
					"aspect", name, "pid", cmd.Process.Pid)
			}
		}(cmd, c.Name, done)
	}
	return sup, nil
}

// envOverriddenByConfig names parent env keys that autospawn drops
// because configuration provides an authoritative replacement
// downstream. Anything not in this set is forwarded to the child
// unchanged — the operator's shell environment is the source of
// truth, since this broker is single-operator local infrastructure
// (not a multi-tenant sandbox).
//
// Today the only superseded key is NEXUS_TOKEN: the AspectTokenResolver
// emits a per-aspect token, and the legacy graceful-degrade path uses
// BaseEnv's NEXUS_TOKEN. Letting the parent process's NEXUS_TOKEN leak
// through would defeat per-aspect isolation when the resolver yields
// nothing — the parent's master token would be visible to the child.
//
// Lookup is case-insensitive (strings.EqualFold). On Windows, env keys
// like "Path" are TitleCase; an earlier allowlist regression
// (2026-05-25 prod cascade) hinged on this and we preserve the
// case-insensitive semantics here.
//
// Earlier design (#30) was the inverse — allowlist by default, drop
// everything else. That made sense for a hypothetical multi-tenant
// sandbox; for the single-operator local broker it stripped legitimate
// provider env (ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, etc.) that
// operators set in their shell expecting flow-through to children.
// Reversed per operator decision 2026-05-25.
var envOverriddenByConfig = []string{
	"NEXUS_TOKEN",
}

// envOverridden reports whether key is replaced downstream by
// configuration, in which case autospawn drops the parent's value.
// Case-insensitive — see envOverriddenByConfig.
func envOverridden(key string) bool {
	for _, k := range envOverriddenByConfig {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// passthroughParentEnv forwards parent env to the child, dropping
// only keys configuration supersedes (see envOverriddenByConfig).
// Preserves first-occurrence order so tests are deterministic;
// Go's exec honours LAST occurrence anyway.
func passthroughParentEnv(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		i := indexOfEqual(kv)
		if i < 0 {
			continue
		}
		if envOverridden(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// indexOfEqual returns the index of the first '=' in s, or -1.
func indexOfEqual(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return i
		}
	}
	return -1
}

// childEnv builds the environment slice for an autospawned child:
// passthrough parent env (minus keys overridden by config, see
// envOverriddenByConfig) + BaseEnv, plus a per-aspect NEXUS_TOKEN
// appended last when the resolver yields one. Go's os.Exec applies
// the LAST occurrence of a duplicate key as the effective value, so
// the per-aspect token overrides any NEXUS_TOKEN in BaseEnv. When
// the resolver returns false, BaseEnv's NEXUS_TOKEN remains in
// effect — the legacy graceful-degrade path for aspects not yet
// reconciled.
//
// Pure helper for unit testing — no syscall side effects, takes the
// "parent env" as a parameter so tests can pass their own.
func childEnv(parent, base []string, tokens AspectTokenResolver, aspect string) []string {
	scrubbed := passthroughParentEnv(parent)
	out := make([]string, 0, len(scrubbed)+len(base)+1)
	out = append(out, scrubbed...)
	out = append(out, base...)
	if tokens != nil {
		if tok, ok := tokens.TokenFor(aspect); ok && tok != "" {
			out = append(out, "NEXUS_TOKEN="+tok)
		}
	}
	return out
}

// logPipe reads lines from the child and emits them with an aspect
// prefix. Spec §7.4: the parent is the forensic trail.
func logPipe(r io.ReadCloser, aspect string, log *slog.Logger, isStderr bool) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if isStderr {
			log.Info("child stderr", "aspect", aspect, "line", line)
		} else {
			log.Info("child stdout", "aspect", aspect, "line", line)
		}
	}
}
