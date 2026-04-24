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

	"github.com/nexus-cw/nexus/shared/schemas"
)

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
	BaseEnv []string

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
		cmd.Env = append(os.Environ(), cfg.BaseEnv...)

		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()

		if err := cmd.Start(); err != nil {
			log.Error("autospawn: failed to start", "aspect", c.Name, "err", err)
			continue
		}

		// Fan stdout and stderr through tagged log forwarders. Each
		// child spawns two goroutines that run until the pipe closes
		// (when the child exits).
		go logPipe(stdout, c.Name, log, false)
		go logPipe(stderr, c.Name, log, true)

		log.Info("autospawn: started", "aspect", c.Name, "home", c.Path, "pid", cmd.Process.Pid)
	}
	return nil
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
