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

func TestDiscoverSkipsFrame(t *testing.T) {
	// Frame aspects (role: frame) embed in the Nexus process; they must
	// not also be subprocess-spawned, or the broker roster collides on
	// the name. Discover skips them — embedding is the frame package's
	// job, not autospawn's.
	base := t.TempDir()

	writeAspect(t, base, "keel", schemas.AspectConfig{
		Role:        schemas.RoleFrame,
		ContextMode: schemas.ContextGlobal,
		Provider:    "claude-api",
	})
	writeAspect(t, base, "wren", schemas.AspectConfig{
		ContextMode: schemas.ContextThread,
		Provider:    "claude-api",
	})

	got, err := Discover(Config{ScanDir: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "wren" {
		t.Errorf("discovered %+v, want only [wren] (frame skipped)", got)
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

// lastNexusToken returns the effective NEXUS_TOKEN from an env slice,
// applying os.Exec's last-wins rule for duplicate keys.
func lastNexusToken(env []string) string {
	const key = "NEXUS_TOKEN="
	got := ""
	for _, e := range env {
		if len(e) > len(key) && e[:len(key)] == key {
			got = e[len(key):]
		}
	}
	return got
}

// TestChildEnvPerAspectTokenWins — when TokenResolver yields a token
// for the aspect, that NEXUS_TOKEN must be the effective one (last
// occurrence wins), overriding the legacy shared token in BaseEnv.
func TestChildEnvPerAspectTokenWins(t *testing.T) {
	parent := []string{"PATH=/usr/bin"}
	base := []string{"NEXUS_UPSTREAM=ws://x", "NEXUS_TOKEN=shared"}
	tr := AspectTokenResolverFunc(func(a string) (string, bool) {
		if a == "wren" {
			return "wren-tok", true
		}
		return "", false
	})

	env := childEnv(parent, base, tr, "wren")
	if got := lastNexusToken(env); got != "wren-tok" {
		t.Errorf("effective NEXUS_TOKEN = %q, want wren-tok", got)
	}
}

// TestChildEnvFallbackOnResolverMiss — when TokenResolver returns
// false (unknown aspect), BaseEnv's legacy NEXUS_TOKEN remains in
// effect. Deliberate graceful-degrade per task spec.
func TestChildEnvFallbackOnResolverMiss(t *testing.T) {
	parent := []string{"PATH=/usr/bin"}
	base := []string{"NEXUS_TOKEN=shared-fallback"}
	tr := AspectTokenResolverFunc(func(string) (string, bool) {
		return "", false
	})

	env := childEnv(parent, base, tr, "ghost")
	if got := lastNexusToken(env); got != "shared-fallback" {
		t.Errorf("effective NEXUS_TOKEN = %q, want shared-fallback", got)
	}
}

// TestChildEnvNilResolver — back-compat: when no resolver is set, the
// env is exactly parent + BaseEnv.
func TestChildEnvNilResolver(t *testing.T) {
	parent := []string{"PATH=/usr/bin"}
	base := []string{"NEXUS_TOKEN=legacy"}

	env := childEnv(parent, base, nil, "wren")
	if got := lastNexusToken(env); got != "legacy" {
		t.Errorf("effective NEXUS_TOKEN = %q, want legacy", got)
	}
}

// TestChildEnvEmptyTokenIsMiss — resolver returning ("", true) is
// treated as a miss; we don't append an empty NEXUS_TOKEN= entry that
// would clobber the BaseEnv fallback.
func TestChildEnvEmptyTokenIsMiss(t *testing.T) {
	parent := []string{}
	base := []string{"NEXUS_TOKEN=fallback"}
	tr := AspectTokenResolverFunc(func(string) (string, bool) {
		return "", true // pathological — empty but ok=true
	})

	env := childEnv(parent, base, tr, "wren")
	if got := lastNexusToken(env); got != "fallback" {
		t.Errorf("effective NEXUS_TOKEN = %q, want fallback", got)
	}
}
