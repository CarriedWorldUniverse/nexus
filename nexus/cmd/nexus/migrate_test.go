package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus-cw/nexus/nexus/aspects"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// migrateFixture builds an aspect-dir with one or more home subdirs
// for the migrate tests to walk.
type migrateFixture struct {
	dir string
}

func newMigrateFixture(t *testing.T) *migrateFixture {
	t.Helper()
	return &migrateFixture{dir: t.TempDir()}
}

// addHome creates <fixture>/<name>/{aspect.json, NEXUS.md, SOUL.md, PRIMER.md}.
func (f *migrateFixture) addHome(t *testing.T, name, provider, model, nexusMD, soulMD, primerMD string, opts ...func(map[string]any)) {
	t.Helper()
	home := filepath.Join(f.dir, name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := map[string]any{
		"name":            name,
		"context_mode":    "thread",
		"provider":        provider,
		"provider_config": map[string]any{"model": model},
		"capabilities":    []string{"a", "b"},
	}
	for _, opt := range opts {
		opt(cfg)
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "aspect.json"), raw, 0o644); err != nil {
		t.Fatalf("write aspect.json: %v", err)
	}
	if nexusMD != "" {
		_ = os.WriteFile(filepath.Join(home, "NEXUS.md"), []byte(nexusMD), 0o644)
	}
	if soulMD != "" {
		_ = os.WriteFile(filepath.Join(home, "SOUL.md"), []byte(soulMD), 0o644)
	}
	if primerMD != "" {
		_ = os.WriteFile(filepath.Join(home, "PRIMER.md"), []byte(primerMD), 0o644)
	}
}

// addLegacyClaudeMD overrides NEXUS.md with a CLAUDE.md instead, to
// exercise the back-compat fallback in readMDFile.
func (f *migrateFixture) addLegacyClaudeMD(t *testing.T, name, content string) {
	t.Helper()
	home := filepath.Join(f.dir, name)
	if err := os.WriteFile(filepath.Join(home, "CLAUDE.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
}

// freshAspectsStore opens a clean DB-backed Store for the test.
func freshAspectsStoreForTest(t *testing.T) (*aspects.SQLStore, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	return aspects.NewSQLStore(db), func() { db.Close() }
}

// TestScanAspectDir_Walks — only directories with aspect.json are
// returned; bare directories are skipped.
func TestScanAspectDir_Walks(t *testing.T) {
	f := newMigrateFixture(t)
	f.addHome(t, "plumb", "claude-api", "claude-opus-4-7", "## plumb", "soul", "primer")
	f.addHome(t, "wren", "claude-code", "claude-haiku-4-5", "## wren", "", "")
	// Bare dir without aspect.json — must be skipped.
	if err := os.MkdirAll(filepath.Join(f.dir, "not-an-aspect"), 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}

	homes, err := scanAspectDir(f.dir)
	if err != nil {
		t.Fatalf("scanAspectDir: %v", err)
	}
	if len(homes) != 2 {
		t.Errorf("got %d homes; want 2", len(homes))
	}
	names := map[string]bool{}
	for _, h := range homes {
		names[h.name] = true
	}
	if !names["plumb"] || !names["wren"] {
		t.Errorf("missing expected names: %+v", names)
	}
}

// TestMigrateOne_FreshInsert — happy path. Aspect not in DB → row
// inserted with version=1, placeholder pubkey, personality populated.
func TestMigrateOne_FreshInsert(t *testing.T) {
	f := newMigrateFixture(t)
	f.addHome(t, "plumb", "claude-api", "claude-opus-4-7", "## plumb", "soul", "primer")
	store, closeDB := freshAspectsStoreForTest(t)
	t.Cleanup(closeDB)

	homes, _ := scanAspectDir(f.dir)
	summary, err := migrateOne(context.Background(), store, homes[0], false, false, nil)
	if err != nil {
		t.Fatalf("migrateOne: %v", err)
	}
	if summary.action != "inserted" {
		t.Errorf("action = %q; want inserted", summary.action)
	}

	got, err := store.Get(context.Background(), "plumb")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Provider != "claude-api" || got.Model != "claude-opus-4-7" {
		t.Errorf("provider/model: %q/%q", got.Provider, got.Model)
	}
	if got.CurrentKeyfileVersion != 1 {
		t.Errorf("version = %d; want 1", got.CurrentKeyfileVersion)
	}
	if len(got.AspectPubkey) != 32 {
		t.Errorf("pubkey len = %d; want 32 (placeholder)", len(got.AspectPubkey))
	}

	p, err := store.PersonalityGet(context.Background(), "plumb")
	if err != nil {
		t.Fatalf("PersonalityGet: %v", err)
	}
	if p.NexusMD != "## plumb" || p.SoulMD != "soul" || p.PrimerMD != "primer" {
		t.Errorf("personality round-trip wrong: %+v", p)
	}
}

// TestMigrateOne_SkipExistingWithoutOverwrite — second run is a no-op
// unless --overwrite is set.
func TestMigrateOne_SkipExistingWithoutOverwrite(t *testing.T) {
	f := newMigrateFixture(t)
	f.addHome(t, "plumb", "claude-api", "claude-opus-4-7", "## plumb v1", "", "")
	store, closeDB := freshAspectsStoreForTest(t)
	t.Cleanup(closeDB)

	homes, _ := scanAspectDir(f.dir)
	if _, err := migrateOne(context.Background(), store, homes[0], false, false, nil); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Update the disk content; without --overwrite the DB stays at v1.
	_ = os.WriteFile(filepath.Join(f.dir, "plumb", "NEXUS.md"), []byte("## plumb v2"), 0o644)

	summary, err := migrateOne(context.Background(), store, homes[0], false, false, nil)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if summary.action != "skipped" {
		t.Errorf("action = %q; want skipped", summary.action)
	}
	p, _ := store.PersonalityGet(context.Background(), "plumb")
	if p.NexusMD != "## plumb v1" {
		t.Errorf("DB content changed despite skip: %q", p.NexusMD)
	}
}

// TestMigrateOne_OverwriteReplacesContent — with --overwrite, the
// second migrate updates the row contents while keeping the existing
// keyfile version (no inadvertent rotation).
func TestMigrateOne_OverwriteReplacesContent(t *testing.T) {
	f := newMigrateFixture(t)
	f.addHome(t, "plumb", "claude-api", "claude-opus-4-7", "## plumb v1", "", "")
	store, closeDB := freshAspectsStoreForTest(t)
	t.Cleanup(closeDB)

	homes, _ := scanAspectDir(f.dir)
	if _, err := migrateOne(context.Background(), store, homes[0], false, false, nil); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	originalRow, _ := store.Get(context.Background(), "plumb")

	_ = os.WriteFile(filepath.Join(f.dir, "plumb", "NEXUS.md"), []byte("## plumb v2"), 0o644)
	homes, _ = scanAspectDir(f.dir)
	summary, err := migrateOne(context.Background(), store, homes[0], true, false, nil)
	if err != nil {
		t.Fatalf("overwrite migrate: %v", err)
	}
	if summary.action != "overwritten" {
		t.Errorf("action = %q; want overwritten", summary.action)
	}

	got, _ := store.Get(context.Background(), "plumb")
	if got.CurrentKeyfileVersion != originalRow.CurrentKeyfileVersion {
		t.Errorf("version changed on overwrite: was %d, now %d (overwrite must NOT rotate keyfile)",
			originalRow.CurrentKeyfileVersion, got.CurrentKeyfileVersion)
	}
	p, _ := store.PersonalityGet(context.Background(), "plumb")
	if p.NexusMD != "## plumb v2" {
		t.Errorf("personality not updated: %q", p.NexusMD)
	}
}

// TestMigrateOne_LegacyClaudeMDFallback — spec §10: NEXUS.md preferred,
// CLAUDE.md is the back-compat fallback. If only CLAUDE.md is present,
// its content lands in nexus_md.
func TestMigrateOne_LegacyClaudeMDFallback(t *testing.T) {
	f := newMigrateFixture(t)
	// Create the home with NO NEXUS.md but WITH CLAUDE.md.
	f.addHome(t, "legacy", "claude-api", "claude-opus-4-7", "" /* no NEXUS.md */, "", "")
	f.addLegacyClaudeMD(t, "legacy", "## legacy from CLAUDE.md")

	store, closeDB := freshAspectsStoreForTest(t)
	t.Cleanup(closeDB)
	homes, _ := scanAspectDir(f.dir)
	if _, err := migrateOne(context.Background(), store, homes[0], false, false, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, _ := store.PersonalityGet(context.Background(), "legacy")
	if p.NexusMD != "## legacy from CLAUDE.md" {
		t.Errorf("CLAUDE.md fallback failed: NexusMD = %q", p.NexusMD)
	}
}

// TestMigrateOne_DryRunNoWrites — --dry-run reports without touching
// the DB.
func TestMigrateOne_DryRunNoWrites(t *testing.T) {
	f := newMigrateFixture(t)
	f.addHome(t, "plumb", "claude-api", "claude-opus-4-7", "## plumb", "", "")
	store, closeDB := freshAspectsStoreForTest(t)
	t.Cleanup(closeDB)
	homes, _ := scanAspectDir(f.dir)
	summary, err := migrateOne(context.Background(), store, homes[0], false, true, nil)
	if err != nil {
		t.Fatalf("dry-run migrate: %v", err)
	}
	if summary.action != "inserted" {
		t.Errorf("dry-run action = %q; want inserted", summary.action)
	}
	if _, err := store.Get(context.Background(), "plumb"); err == nil {
		t.Error("dry-run wrote to DB; should be no-op")
	}
}

// TestMigrateOne_NoPersonalityFiles — aspect.json present but no md
// files. Should still insert the aspects row; personality row is
// skipped (operator runs `nexus personality edit` later).
func TestMigrateOne_NoPersonalityFiles(t *testing.T) {
	f := newMigrateFixture(t)
	f.addHome(t, "blank", "claude-api", "claude-opus-4-7", "", "", "")
	store, closeDB := freshAspectsStoreForTest(t)
	t.Cleanup(closeDB)
	homes, _ := scanAspectDir(f.dir)
	if _, err := migrateOne(context.Background(), store, homes[0], false, false, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Get(context.Background(), "blank"); err != nil {
		t.Errorf("aspect not in DB: %v", err)
	}
	if _, err := store.PersonalityGet(context.Background(), "blank"); err == nil {
		t.Error("personality row created for aspect with no md files; want absent")
	}
}
