package aspects

import (
	"context"
	"sort"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// freshDB opens a clean DB-backed test fixture and returns the raw
// *sql.DB plus a SQLSettingsStore wrapping it.
func freshSettingsRig(t *testing.T) (*SQLSettingsStore, *SQLStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSQLSettingsStore(db), NewSQLStore(db)
}

// TestSettings_GetReturnsDefault — first read after table creation
// returns the default zero-content row at version=0 (not 1, so first
// Set always advances version regardless of whether Get was called).
func TestSettings_GetReturnsDefault(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	got, err := ss.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.NexusMD != "" {
		t.Errorf("default NexusMD = %q; want empty", got.NexusMD)
	}
	if got.Version != 0 {
		t.Errorf("default Version = %d; want 0 (so first Set lands at 1)", got.Version)
	}
}

// TestSettings_SetNexusMD_FirstWriteLandsAtOne — fresh table, no Get
// first. INSERT arm of the upsert fires; version=1.
func TestSettings_SetNexusMD_FirstWriteLandsAtOne(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	v, err := ss.SetNexusMD(context.Background(), "## central content")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v != 1 {
		t.Errorf("first set version = %d; want 1", v)
	}
}

// TestSettings_SetNexusMD_BumpsOnSubsequent — second write bumps to 2.
func TestSettings_SetNexusMD_BumpsOnSubsequent(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	ctx := context.Background()
	if _, err := ss.SetNexusMD(ctx, "v1"); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	v, err := ss.SetNexusMD(ctx, "v2")
	if err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	if v != 2 {
		t.Errorf("second set version = %d; want 2", v)
	}
	got, _ := ss.Get(ctx)
	if got.NexusMD != "v2" || got.Version != 2 {
		t.Errorf("Get after second set = %+v", got)
	}
}

// TestSettings_SetNexusMD_BumpsVersionAfterDefaultGet — Get
// materialises the row at version=0; SetNexusMD then bumps 0→1 via
// the ON CONFLICT path, ending at the same version=1 as a fresh-table
// SetNexusMD without prior Get. The two cold-paths converge.
func TestSettings_SetNexusMD_BumpsVersionAfterDefaultGet(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	ctx := context.Background()
	if _, err := ss.Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	v, err := ss.SetNexusMD(ctx, "central")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v != 1 {
		t.Errorf("version after default-Get + Set = %d; want 1 (first real content)", v)
	}
}

// TestMigrate_SkipsWhenAlreadyPopulated — running migrate twice is a
// no-op on the second call.
func TestMigrate_SkipsWhenAlreadyPopulated(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	// Seed an aspect with content, then pre-populate central.
	if err := store.Insert(ctx, Aspect{
		Name: "keel", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert keel: %v", err)
	}
	if err := store.PersonalitySet(ctx, Personality{
		AspectName: "keel", NexusMD: "keel central",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}
	// Pre-populate central with different content to verify migrate
	// doesn't overwrite it.
	if _, err := ss.SetNexusMD(ctx, "manually-set central"); err != nil {
		t.Fatalf("SetNexusMD: %v", err)
	}

	res, err := MigrateCentralFromAspect(ctx, ss.db, "keel")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Migrate didn't skip on populated table: %+v", res)
	}
	got, _ := ss.Get(ctx)
	if got.NexusMD != "manually-set central" {
		t.Errorf("Migrate clobbered existing content: %q", got.NexusMD)
	}
}

// TestMigrate_PrefersFrameContent — keel has content, plumb has
// different content. Migrate must seed from keel (the preferredFrame).
func TestMigrate_PrefersFrameContent(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	for _, name := range []string{"keel", "plumb"} {
		if err := store.Insert(ctx, Aspect{
			Name: name, AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
		}); err != nil {
			t.Fatalf("Insert %q: %v", name, err)
		}
	}
	if err := store.PersonalitySet(ctx, Personality{AspectName: "keel", NexusMD: "keel-content"}); err != nil {
		t.Fatalf("set keel: %v", err)
	}
	if err := store.PersonalitySet(ctx, Personality{AspectName: "plumb", NexusMD: "plumb-content"}); err != nil {
		t.Fatalf("set plumb: %v", err)
	}

	res, err := MigrateCentralFromAspect(ctx, ss.db, "keel")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.SeededFrom != "keel" {
		t.Errorf("SeededFrom = %q; want keel", res.SeededFrom)
	}
	got, _ := ss.Get(ctx)
	if got.NexusMD != "keel-content" {
		t.Errorf("NexusMD = %q; want keel-content", got.NexusMD)
	}
	if len(res.DivergentAspects) != 1 || res.DivergentAspects[0] != "plumb" {
		t.Errorf("DivergentAspects = %v; want [plumb]", res.DivergentAspects)
	}
}

// TestMigrate_FallsBackToMostRecent — preferredFrame absent; migrate
// falls back to the most-recently-updated row with non-empty nexus_md.
func TestMigrate_FallsBackToMostRecent(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	for _, name := range []string{"plumb", "wren"} {
		if err := store.Insert(ctx, Aspect{
			Name: name, AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
		}); err != nil {
			t.Fatalf("Insert %q: %v", name, err)
		}
	}
	if err := store.PersonalitySet(ctx, Personality{AspectName: "plumb", NexusMD: "plumb-old"}); err != nil {
		t.Fatalf("set plumb: %v", err)
	}
	// wren written second → most-recent.
	if err := store.PersonalitySet(ctx, Personality{AspectName: "wren", NexusMD: "wren-new"}); err != nil {
		t.Fatalf("set wren: %v", err)
	}

	res, err := MigrateCentralFromAspect(ctx, ss.db, "keel" /* absent */)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.SeededFrom != "wren" {
		t.Errorf("SeededFrom = %q; want wren (most-recent)", res.SeededFrom)
	}
	got, _ := ss.Get(ctx)
	if got.NexusMD != "wren-new" {
		t.Errorf("NexusMD = %q; want wren-new", got.NexusMD)
	}
}

// TestMigrate_NoSourceContent — no aspects have nexus_md; migrate is
// a no-op (Skipped, with reason).
func TestMigrate_NoSourceContent(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name: "blank", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// No PersonalitySet — blank aspect has no personality row at all.
	res, err := MigrateCentralFromAspect(ctx, ss.db, "keel")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Migrate didn't skip with no source content: %+v", res)
	}
	got, _ := ss.Get(ctx)
	if got.NexusMD != "" {
		t.Errorf("central populated despite no source: %q", got.NexusMD)
	}
}

// TestMigrate_IdempotentViaSchemaMetaMarker — operator blanks central
// via SetNexusMD("") and restarts. The schema_meta marker prevents
// re-seeding from per-aspect rows that may have been pruned to short
// deltas post-migration.
func TestMigrate_IdempotentViaSchemaMetaMarker(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name: "keel", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := store.PersonalitySet(ctx, Personality{
		AspectName: "keel", NexusMD: "full network-wide content",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}

	// First run: seed central from keel.
	res, err := MigrateCentralFromAspect(ctx, ss.db, "keel")
	if err != nil {
		t.Fatalf("Migrate 1: %v", err)
	}
	if res.SeededFrom != "keel" {
		t.Errorf("first migrate seededFrom = %q; want keel", res.SeededFrom)
	}

	// Operator blanks central content (simulating Part 9c admin
	// endpoint clearing it before a rewrite, then process restart).
	if _, err := ss.SetNexusMD(ctx, ""); err != nil {
		t.Fatalf("blank: %v", err)
	}

	// Operator also pruned keel's per-aspect content to a short delta
	// (the post-migration cleanup the spec calls for).
	if err := store.PersonalitySet(ctx, Personality{
		AspectName: "keel", NexusMD: "I am the Frame.",
	}); err != nil {
		t.Fatalf("prune keel: %v", err)
	}

	// Second run: marker says skip, central stays blank rather than
	// re-seeding from keel's now-short delta. Without the marker, the
	// empty-string check would re-seed and clobber operator intent.
	res2, err := MigrateCentralFromAspect(ctx, ss.db, "keel")
	if err != nil {
		t.Fatalf("Migrate 2: %v", err)
	}
	if !res2.Skipped {
		t.Errorf("second migrate didn't skip: %+v", res2)
	}
	got, _ := ss.Get(ctx)
	if got.NexusMD != "" {
		t.Errorf("central re-seeded despite marker: %q", got.NexusMD)
	}
}

// TestMigrate_LeavesPerAspectUntouched — per spec §6 (revised), the
// migration is "soft": per-aspect rows are not modified. Operator
// prunes manually afterward.
func TestMigrate_LeavesPerAspectUntouched(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name: "keel", AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := store.PersonalitySet(ctx, Personality{
		AspectName: "keel", NexusMD: "keel-central", SoulMD: "soul",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}

	if _, err := MigrateCentralFromAspect(ctx, ss.db, "keel"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// keel's row should be unchanged after migration.
	p, err := store.PersonalityGet(ctx, "keel")
	if err != nil {
		t.Fatalf("PersonalityGet: %v", err)
	}
	if p.NexusMD != "keel-central" {
		t.Errorf("per-aspect nexus_md modified by migrate: %q", p.NexusMD)
	}
	if p.SoulMD != "soul" {
		t.Errorf("per-aspect soul_md modified by migrate: %q", p.SoulMD)
	}
}

// TestMigrate_DivergentAspectsSortedDeterministic — the divergent list
// must be stable across runs so operator log diffs are clean.
func TestMigrate_DivergentAspectsSortedDeterministic(t *testing.T) {
	ss, store := freshSettingsRig(t)
	ctx := context.Background()
	for _, name := range []string{"keel", "wren", "anvil", "plumb"} {
		if err := store.Insert(ctx, Aspect{
			Name: name, AspectPubkey: fakePubkey(1), Provider: "p", Model: "m",
		}); err != nil {
			t.Fatalf("Insert %q: %v", name, err)
		}
	}
	_ = store.PersonalitySet(ctx, Personality{AspectName: "keel", NexusMD: "shared"})
	_ = store.PersonalitySet(ctx, Personality{AspectName: "wren", NexusMD: "wren-specific"})
	_ = store.PersonalitySet(ctx, Personality{AspectName: "anvil", NexusMD: "anvil-specific"})
	_ = store.PersonalitySet(ctx, Personality{AspectName: "plumb", NexusMD: "shared"}) // matches keel; not divergent

	res, err := MigrateCentralFromAspect(ctx, ss.db, "keel")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := []string{"anvil", "wren"}
	if !sort.StringsAreSorted(res.DivergentAspects) {
		t.Errorf("DivergentAspects not sorted: %v", res.DivergentAspects)
	}
	if len(res.DivergentAspects) != len(want) {
		t.Fatalf("DivergentAspects len = %d; want %d (got %v)",
			len(res.DivergentAspects), len(want), res.DivergentAspects)
	}
	for i, w := range want {
		if res.DivergentAspects[i] != w {
			t.Errorf("DivergentAspects[%d] = %q; want %q", i, res.DivergentAspects[i], w)
		}
	}
}
