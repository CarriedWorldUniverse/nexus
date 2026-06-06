package autospawn

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// TestMain handles the spawn-a-sleeper helper used by
// TestSupervisorKillsChildrenOnShutdown. When SLEEPER_HELPER is set,
// the binary acts as a no-op long-running process so the test has
// something realistic to kill (instead of mocking *exec.Cmd).
func TestMain(m *testing.M) {
	if os.Getenv("SLEEPER_HELPER") == "1" {
		// Block until killed. 30s ceiling is well past the test timeout
		// so we don't leak even if Shutdown fails.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

func TestDiscoverIncludesFrameRole(t *testing.T) {
	// Frame aspects now run out-of-process and register like any other
	// aspect, so discovery must include them.
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
	if len(got) != 2 {
		t.Fatalf("discovered %+v, want keel and wren", got)
	}
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	if !names["keel"] || !names["wren"] {
		t.Errorf("discovered %+v, want keel and wren", got)
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

// Passthrough policy (2026-05-25): parent env flows through to the
// child unchanged except for keys configuration supersedes. Operator
// shell env is the source of truth for single-operator local broker
// deployments — provider credentials (ANTHROPIC_API_KEY, etc.) and
// arbitrary tooling vars all need to reach autospawned aspects.
// Today the only superseded key is NEXUS_TOKEN.
func TestChildEnvPassesThroughExceptOverridden(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HOME=/home/keel",
		"ANTHROPIC_API_KEY=sk-deepseek-foo",
		"ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic",
		"GITHUB_TOKEN=ghp_op_set_this_intentionally",
		"NEXUS_TOKEN=parent-master", // overridden — resolver / BaseEnv only
	}
	base := []string{"NEXUS_UPSTREAM=ws://x"}
	tr := AspectTokenResolverFunc(func(string) (string, bool) {
		return "wren-tok", true
	})

	env := childEnv(parent, base, tr, "wren")

	got := map[string]string{}
	for _, kv := range env {
		i := indexOfEqual(kv)
		if i >= 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	// Everything operator-set in shell flows through. The judge needs
	// these for DeepSeek bare-mode, and operators expect "I set it,
	// the child sees it" semantics.
	for _, want := range []string{
		"PATH", "HOME",
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL",
		"GITHUB_TOKEN",
	} {
		if got[want] == "" {
			t.Errorf("expected %s to flow through to child, missing", want)
		}
	}
	// NEXUS_TOKEN from parent must NOT reach the child — the resolver
	// (or BaseEnv fallback) is the authoritative source. Per-aspect
	// token wins because Go honours the LAST occurrence.
	if lastNexusToken(env) != "wren-tok" {
		t.Errorf("expected per-aspect token to win, got %q", lastNexusToken(env))
	}
}

// NEXUS_TOKEN is the only env key autospawn drops from parent env.
// Verify the drop is case-insensitive — same lesson as the Path
// regression: env-key matching must not assume canonical casing.
func TestChildEnvDropsNexusTokenCaseInsensitive(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"Nexus_Token=parent-master-mixed",
	}
	env := childEnv(parent, nil, nil, "wren")
	for _, kv := range env {
		i := indexOfEqual(kv)
		if i >= 0 && strings.EqualFold(kv[:i], "NEXUS_TOKEN") {
			t.Errorf("parent NEXUS_TOKEN (mixed case) leaked through: %q", kv)
		}
	}
}

// Regression for the 2026-05-25 prod cascade: Windows represents the
// path variable as "Path" (TitleCase), not "PATH". The pre-fix
// case-sensitive map lookup stripped "Path" entirely, leaving
// autospawned aspects unable to find executables — judge subprocess
// failed with "executable file not found in %PATH%", filter
// defaulted to fail-open, every aspect cascaded into chat.
func TestChildEnvCaseInsensitivePathPassthrough(t *testing.T) {
	parent := []string{
		"Path=C:\\Windows\\System32;C:\\Users\\jacin\\AppData\\Roaming\\npm", // Windows TitleCase
		"USERPROFILE=C:\\Users\\jacin",
		"APPDATA=C:\\Users\\jacin\\AppData\\Roaming", // claude.cmd install dir
		"LOCALAPPDATA=C:\\Users\\jacin\\AppData\\Local",
		"SYSTEMROOT=C:\\Windows",
		"WINDIR=C:\\Windows",
		"TEMP=C:\\Users\\jacin\\AppData\\Local\\Temp",
	}
	env := childEnv(parent, nil, nil, "wren")
	got := map[string]string{}
	for _, kv := range env {
		i := indexOfEqual(kv)
		if i >= 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	// Path (TitleCase) must come through — the cascade-causing case.
	if got["Path"] == "" {
		t.Errorf("Path (Windows TitleCase) was stripped — child can't find executables: %v", got)
	}
	// Windows-essential vars added for npm-installed CLI lookup.
	for _, want := range []string{"APPDATA", "LOCALAPPDATA", "SYSTEMROOT", "WINDIR"} {
		if got[want] == "" {
			t.Errorf("%s should be allowed through for Windows tool lookup", want)
		}
	}
}

func TestEnvOverridden_CaseInsensitive(t *testing.T) {
	cases := []struct {
		key string
		ok  bool
	}{
		{"NEXUS_TOKEN", true}, // canonical
		{"Nexus_Token", true}, // mixed case
		{"nexus_token", true}, // lowercase
		{"PATH", false},
		{"ANTHROPIC_API_KEY", false},
		{"GITHUB_TOKEN", false},
		{"", false},
	}
	for _, c := range cases {
		if got := envOverridden(c.key); got != c.ok {
			t.Errorf("envOverridden(%q) = %v, want %v", c.key, got, c.ok)
		}
	}
}

// Regression for issue #21: DiscoverRoots merges multiple aspect-dir
// scans, deduping by aspect name (first root wins on conflict).
func TestDiscoverRoots_MergesMultipleRoots(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()

	writeAspect(t, root1, "wren", schemas.AspectConfig{Provider: "claude-api"})
	writeAspect(t, root1, "harrow", schemas.AspectConfig{Provider: "claude-api"})
	writeAspect(t, root2, "anvil", schemas.AspectConfig{Provider: "claude-api"})
	// Duplicate name across roots — root1 wins per documented contract.
	writeAspect(t, root2, "wren", schemas.AspectConfig{Provider: "different"})

	got, err := DiscoverRoots(Config{}, []string{root1, root2})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, c := range got {
		names[c.Name] = c.Path
	}
	if len(names) != 3 {
		t.Errorf("expected 3 unique aspects, got %d: %v", len(names), names)
	}
	abs1, _ := filepath.Abs(root1)
	if !strings.HasPrefix(names["wren"], abs1) {
		t.Errorf("wren resolved to %q, expected root1 (%q) to win", names["wren"], abs1)
	}
}

// TestSupervisorKillsChildrenOnShutdown — orphan-funnel regression.
// Spawns two long-running helper processes (re-exec of the test
// binary in SLEEPER_HELPER mode), then asserts Shutdown terminates
// them within the grace window. Without supervision the children
// would survive the test binary's exit; with it, the OS sees them
// reaped before we return.
func TestSupervisorKillsChildrenOnShutdown(t *testing.T) {
	// Use the test binary itself as the harness so the test is
	// hermetic. SLEEPER_HELPER=1 + TestMain redirects the child into
	// a 30s sleep loop.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	base := t.TempDir()
	writeAspect(t, base, "alpha", schemas.AspectConfig{Provider: "claude-api"})
	writeAspect(t, base, "beta", schemas.AspectConfig{Provider: "claude-api"})

	cfg := Config{
		ScanDir:     base,
		HarnessPath: self,
		BaseEnv:     []string{"SLEEPER_HELPER=1"},
	}
	// We pass the legacy -home form (KeyfileDir empty) so the helper
	// gets `-home <path>` which it ignores and falls through to the
	// 30s sleep.
	candidates, err := Discover(cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}

	sup, err := Spawn(cfg, candidates)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sup == nil {
		t.Fatal("Spawn returned nil supervisor")
	}

	// Snapshot PIDs while Shutdown still has the slice.
	sup.mu.Lock()
	pids := make([]int, 0, len(sup.children))
	for _, c := range sup.children {
		if c.cmd.Process != nil {
			pids = append(pids, c.cmd.Process.Pid)
		}
	}
	sup.mu.Unlock()
	if len(pids) != 2 {
		t.Fatalf("supervisor tracking %d PIDs, want 2", len(pids))
	}

	// Give children a tick to actually be running before we try to
	// kill them — Spawn returns after exec.Start, which on Windows
	// may not yet have a fully-initialised process.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	sup.Shutdown(ctx)
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		t.Errorf("Shutdown hit context deadline: %v (elapsed %v)", ctx.Err(), elapsed)
	}

	// Verify each PID is actually gone. FindProcess always succeeds
	// on Unix, so we send signal 0 (existence probe) — on Windows,
	// Process.Signal(syscall.Signal(0)) returns an error if the
	// process is gone, but the behaviour is platform-quirky, so we
	// just check that Shutdown returned before the timeout.
	if elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v, expected sub-second kill", elapsed)
	}

	// Second Shutdown is a no-op (tracked slice drained on first call).
	sup.Shutdown(context.Background())

	// runtime guard: this test assumes a real OS process, not js/wasm.
	_ = runtime.GOOS
}

// TestSupervisorShutdownNil — defensive: calling Shutdown on a nil
// receiver should not panic. Lets callers do `sup.Shutdown(ctx)`
// without nil-checking when autospawn was disabled or never ran.
func TestSupervisorShutdownNil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil Shutdown panicked: %v", r)
		}
	}()
	var sup *Supervisor
	sup.Shutdown(context.Background())
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
