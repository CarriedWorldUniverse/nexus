package frame

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func writeAspect(t *testing.T, dir, name string, cfg schemas.AspectConfig) {
	t.Helper()
	homeDir := filepath.Join(dir, name)
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", homeDir, err)
	}
	if cfg.Name == "" {
		cfg.Name = name
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "aspect.json"), raw, 0o644); err != nil {
		t.Fatalf("write aspect.json for %s: %v", name, err)
	}
}

func writeRaw(t *testing.T, dir, name, content string) {
	t.Helper()
	homeDir := filepath.Join(dir, name)
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", homeDir, err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "aspect.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write aspect.json for %s: %v", name, err)
	}
}

func TestDetect_NoAgentsDir(t *testing.T) {
	d, err := Detect(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if d.Frame != nil {
		t.Fatalf("expected no frame, got %+v", d.Frame)
	}
}

func TestDetect_EmptyAgentsDir(t *testing.T) {
	d, err := Detect(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame != nil {
		t.Fatalf("expected no frame, got %+v", d.Frame)
	}
}

func TestDetect_NoFrameOnlyAspects(t *testing.T) {
	dir := t.TempDir()
	writeAspect(t, dir, "wren", schemas.AspectConfig{
		ContextMode: schemas.ContextThread,
		Provider:    "claude-api",
	})
	writeAspect(t, dir, "anvil", schemas.AspectConfig{
		Role:        schemas.RoleAspect,
		ContextMode: schemas.ContextThread,
		Provider:    "claude-api",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame != nil {
		t.Fatalf("expected no frame, got %+v", d.Frame)
	}
}

func TestDetect_OneFrame(t *testing.T) {
	dir := t.TempDir()
	writeAspect(t, dir, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})
	writeAspect(t, dir, "wren", schemas.AspectConfig{
		ContextMode: schemas.ContextThread,
		Provider:    "claude-api",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame == nil {
		t.Fatalf("expected frame, got nil")
	}
	if d.Frame.Name != "keel" {
		t.Fatalf("expected name=keel, got %q", d.Frame.Name)
	}
	if !filepath.IsAbs(d.Frame.Path) {
		t.Fatalf("expected absolute path, got %q", d.Frame.Path)
	}
	if d.Frame.Config.EffectiveRole() != schemas.RoleFrame {
		t.Fatalf("expected role=frame, got %q", d.Frame.Config.EffectiveRole())
	}
}

func TestDetect_MultipleFrames(t *testing.T) {
	dir := t.TempDir()
	writeAspect(t, dir, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})
	writeAspect(t, dir, "frame2", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	_, err := Detect(dir)
	if err == nil {
		t.Fatalf("expected ErrMultipleFrames, got nil")
	}
	if !errors.Is(err, ErrMultipleFrames) {
		t.Fatalf("expected ErrMultipleFrames in chain, got %v", err)
	}
}

func TestDetect_MalformedJSONSkipped(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, "broken", "{ not valid json")
	writeAspect(t, dir, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("expected malformed-json to be skipped silently, got err %v", err)
	}
	if d.Frame == nil || d.Frame.Name != "keel" {
		t.Fatalf("expected frame=keel, got %+v", d.Frame)
	}
}

func TestDetect_MissingAspectJSONSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "no-config"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAspect(t, dir, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame == nil || d.Frame.Name != "keel" {
		t.Fatalf("expected frame=keel, got %+v", d.Frame)
	}
}

func TestDetect_EmptyNameSkipped(t *testing.T) {
	// An aspect.json with role:frame but no name field should be skipped
	// — a Frame without a name has no chat handle, which is incoherent.
	dir := t.TempDir()
	writeRaw(t, dir, "no-name", `{"role":"frame","context_mode":"global"}`)

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame != nil {
		t.Fatalf("expected nameless frame to be skipped, got %+v", d.Frame)
	}
}

func TestDetect_EmptyRoleTreatedAsAspect(t *testing.T) {
	// Back-compat: existing aspect.json files don't have a role field.
	// They must still be treated as RoleAspect, not RoleFrame.
	dir := t.TempDir()
	writeRaw(t, dir, "wren", `{"name":"wren","context_mode":"thread","provider":"claude-api"}`)

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame != nil {
		t.Fatalf("expected no frame (empty role = aspect), got %+v", d.Frame)
	}
}

func TestDetect_UnknownRoleNotTreatedAsFrame(t *testing.T) {
	// A typo'd role string (e.g. "fraem") must NOT match RoleFrame and
	// must NOT be silently coerced to RoleAspect. The aspect is skipped
	// from frame consideration; a warning is logged. This protects the
	// operator from a typo silently dropping them into bootstrap mode.
	dir := t.TempDir()
	writeRaw(t, dir, "typo", `{"name":"typo","role":"fraem","context_mode":"global"}`)

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame != nil {
		t.Fatalf("typo'd role must not be treated as Frame, got %+v", d.Frame)
	}
}

func TestDetect_UnknownRoleAlongsideRealFrame(t *testing.T) {
	// A typo'd aspect coexisting with a valid frame must not interfere
	// with detection of the valid frame.
	dir := t.TempDir()
	writeRaw(t, dir, "typo", `{"name":"typo","role":"fraem","context_mode":"global"}`)
	writeAspect(t, dir, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame == nil || d.Frame.Name != "keel" {
		t.Fatalf("expected frame=keel, got %+v", d.Frame)
	}
}

func TestDetect_NonDirEntriesIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAspect(t, dir, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if d.Frame == nil || d.Frame.Name != "keel" {
		t.Fatalf("expected frame=keel, got %+v", d.Frame)
	}
}
