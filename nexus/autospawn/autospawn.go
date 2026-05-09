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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

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

	// HarnessPath is the absolute path to the harness binary.
	// Required — autospawn doesn't search PATH to avoid ambiguity
	// over which harness to use in multi-installation hosts.
	HarnessPath string

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
		// Frame aspects (role: frame) are embedded in the Nexus process
		// directly — see nexus/frame package. They must NOT be
		// subprocess-spawned; otherwise the broker roster would see two
		// registrations under the same name (one in-process, one from
		// the spawned harness) and collide.
		if aspectCfg.EffectiveRole() == schemas.RoleFrame {
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

// Spawn starts a harness subprocess for each candidate. Fire-and-
// forget: we os.exec, attach stdout/stderr to log prefixers, return.
// If a child dies, we do not restart.
func Spawn(cfg Config, candidates []Candidate) error {
	if cfg.HarnessPath == "" {
		return errors.New("autospawn.Spawn: HarnessPath required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	for _, c := range candidates {
		cmd := exec.Command(cfg.HarnessPath, "-home", c.Path)
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

		// Reap the child once both pipes drain. Without cmd.Wait, Unix
		// leaves zombies and Windows leaks the OS process handle —
		// long-running brokers across many aspect restarts hit
		// fd/handle limits otherwise (#26).
		go func(cmd *exec.Cmd, name string) {
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
		}(cmd, c.Name)
	}
	return nil
}

// envAllowlist is the set of parent env variables forwarded to
// autospawned children (#30). Anything else in os.Environ() is
// dropped. Per operator decision (chat #9686) we ship a minimal list
// and adjust later if a provider needs more:
//
//   PATH         — required for the harness binary to find tools
//   HOME         — Unix home directory; provider configs read from here
//   USERPROFILE  — Windows equivalent of HOME
//   TEMP         — used by some providers for scratch files
//
// Per-aspect NEXUS_TOKEN is added separately by the token resolver;
// any NEXUS_TOKEN in BaseEnv (the legacy graceful-degrade path) is
// also passed through. Other NEXUS_* env vars are NOT forwarded
// wholesale — explicit BaseEnv entries are the audited path.
var envAllowlist = map[string]struct{}{
	"PATH":        {},
	"HOME":        {},
	"USERPROFILE": {},
	"TEMP":        {},
}

// scrubParentEnv applies envAllowlist to a parent env slice.
// Preserves order of the allowed keys' first occurrence (Go's exec
// honors LAST occurrence anyway, but a stable order makes test
// output deterministic). Tokens / app config that need to flow to
// children must go through BaseEnv where the operator can audit
// what's set.
func scrubParentEnv(parent []string) []string {
	out := make([]string, 0, len(envAllowlist))
	for _, kv := range parent {
		i := indexOfEqual(kv)
		if i < 0 {
			continue
		}
		key := kv[:i]
		if _, ok := envAllowlist[key]; ok {
			out = append(out, kv)
		}
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
// scrubbed parent env (per envAllowlist, #30) + BaseEnv, plus a
// per-aspect NEXUS_TOKEN appended last when the resolver yields one.
// Go's os.Exec applies the LAST occurrence of a duplicate key as the
// effective value, so the per-aspect token overrides any NEXUS_TOKEN
// in BaseEnv. When the resolver returns false, BaseEnv's NEXUS_TOKEN
// remains in effect — the legacy graceful-degrade path for aspects
// not yet reconciled.
//
// Pure helper for unit testing — no syscall side effects, takes the
// "parent env" as a parameter so tests can pass their own.
func childEnv(parent, base []string, tokens AspectTokenResolver, aspect string) []string {
	scrubbed := scrubParentEnv(parent)
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
