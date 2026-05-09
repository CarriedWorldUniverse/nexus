// Package frame holds the Nexus's Frame role: detection, embedding,
// admin surface, chat routing rules. See docs/2026-04-28-frame-role-spec.md
// and docs/2026-05-01-frame-65-build-plan.md.
//
// This file (P1 of §6.5) implements Frame detection at Nexus startup —
// scanning the agents directory for an aspect.json with role: frame.
// Branching into bootstrap-vs-normal mode based on the result is the
// caller's job; this package only reports what it found.
package frame

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// Detected is the result of a Detect scan over an agents directory.
//
//   - Frame is non-nil when exactly one role:frame aspect was found.
//     The caller branches into normal mode and embeds this Frame.
//   - Frame is nil when no role:frame aspect was found. The caller
//     branches into bootstrap mode (P2).
//
// Multiple frames is an error (frame-role spec §3.4: one per Nexus).
// Malformed aspect.json files are silently skipped — same shape as
// autospawn.Discover, which is the existing precedent for "this isn't
// our directory, move on."
type Detected struct {
	Frame *FrameAspect
}

// FrameAspect is the resolved Frame: its absolute home path plus the
// parsed aspect.json. Mirrors autospawn.Candidate but is named for the
// role to avoid implying autospawn semantics — Frames are never spawned
// as subprocesses, they embed in the Nexus process (P5).
type FrameAspect struct {
	Path   string                 // absolute path to the home folder
	Name   string                 // aspect.json:name
	Config schemas.AspectConfig   // full parsed config
}

// ErrMultipleFrames is returned when more than one role:frame aspect is
// present in the scan directory. Per spec §3.4, exactly one Frame per
// Nexus. Operators must pick one and remove (or rename role on) the
// others before Nexus will start in normal mode.
var ErrMultipleFrames = errors.New("frame: multiple role:frame aspects found — exactly one is required")

// Detect scans agentsDir for aspect homes and returns Detected. agentsDir
// is the directory containing one subdirectory per aspect home (the same
// shape autospawn.Discover scans). A non-existent agentsDir is treated
// as "no frames" — the caller should branch into bootstrap mode.
//
// Errors:
//   - ErrMultipleFrames if >1 frames found.
//   - I/O errors on the directory listing itself (other than not-exist).
//
// Malformed aspect.json files within agentsDir are skipped silently —
// they are not the Frame whether they intended to be or not, and we do
// not want a typo in some unrelated aspect's config to block Nexus from
// starting.
func Detect(agentsDir string) (Detected, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Detected{}, nil
		}
		return Detected{}, fmt.Errorf("frame: read %s: %w", agentsDir, err)
	}

	var found []FrameAspect

	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		homePath := filepath.Join(agentsDir, ent.Name())
		jsonPath := filepath.Join(homePath, "aspect.json")
		raw, rerr := os.ReadFile(jsonPath)
		if rerr != nil {
			continue
		}
		var cfg schemas.AspectConfig
		if jerr := json.Unmarshal(raw, &cfg); jerr != nil {
			continue
		}
		if cfg.Name == "" {
			continue
		}
		role := cfg.EffectiveRole()
		if !role.Known() {
			// Likely typo (e.g. role: "fraem"). Surface so the operator sees
			// it instead of silently treating as RoleAspect. Don't fail
			// startup — they may have *meant* the typo and we want the rest
			// of the network to come up.
			slog.Warn("frame: unknown role on aspect — treating as non-frame; check for typo",
				"aspect", cfg.Name, "path", homePath, "role", string(role))
			continue
		}
		if role != schemas.RoleFrame {
			continue
		}
		abs, aerr := filepath.Abs(homePath)
		if aerr != nil {
			abs = homePath
		}
		found = append(found, FrameAspect{
			Path:   abs,
			Name:   cfg.Name,
			Config: cfg,
		})
	}

	switch len(found) {
	case 0:
		return Detected{}, nil
	case 1:
		fa := found[0]
		return Detected{Frame: &fa}, nil
	default:
		names := make([]string, 0, len(found))
		for _, f := range found {
			names = append(names, f.Name)
		}
		return Detected{}, fmt.Errorf("%w: %v", ErrMultipleFrames, names)
	}
}
