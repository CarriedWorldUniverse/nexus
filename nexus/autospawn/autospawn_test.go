package autospawn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus-cw/nexus/shared/schemas"
)

func writeAspect(t *testing.T, base, name string, cfg schemas.AspectConfig) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Name = name
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "aspect.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiscoverFindsAspects(t *testing.T) {
	base := t.TempDir()
	writeAspect(t, base, "wren", schemas.AspectConfig{
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})
	writeAspect(t, base, "forge", schemas.AspectConfig{
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	got, err := Discover(Config{ScanDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("discovered %d, want 2", len(got))
	}
}

func TestDiscoverSkipsInvalid(t *testing.T) {
	base := t.TempDir()

	// Valid aspect.
	writeAspect(t, base, "valid", schemas.AspectConfig{
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})

	// Directory without aspect.json.
	os.MkdirAll(filepath.Join(base, "empty-dir"), 0o755)

	// Directory with malformed aspect.json.
	malformedDir := filepath.Join(base, "malformed")
	os.MkdirAll(malformedDir, 0o755)
	os.WriteFile(filepath.Join(malformedDir, "aspect.json"), []byte("not json"), 0o644)

	// Regular file (not a directory) — ignored.
	os.WriteFile(filepath.Join(base, "file.txt"), []byte("x"), 0o644)

	got, err := Discover(Config{ScanDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "valid" {
		t.Errorf("discovered %+v, want only [valid]", got)
	}
}

func TestDiscoverOptOut(t *testing.T) {
	base := t.TempDir()

	writeAspect(t, base, "enabled", schemas.AspectConfig{
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})
	writeAspect(t, base, "disabled", schemas.AspectConfig{
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
		Metadata:    map[string]any{"auto_spawn": false},
	})

	got, err := Discover(Config{ScanDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "enabled" {
		t.Errorf("discovered %+v, want only [enabled]", got)
	}
}

func TestDiscoverNonexistentDir(t *testing.T) {
	// Non-existent scan dir returns empty candidates and no error.
	got, err := Discover(Config{ScanDir: "/this/does/not/exist"})
	if err != nil {
		t.Errorf("want nil error for missing dir, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d candidates, want 0", len(got))
	}
}

func TestDiscoverEmptyDefaultsToStandardDir(t *testing.T) {
	// When ScanDir is empty, it falls back to DefaultScanDir which
	// probably doesn't exist in the test working directory; we just
	// want no error.
	_, err := Discover(Config{ScanDir: ""})
	if err != nil {
		t.Errorf("want nil error, got %v", err)
	}
}
