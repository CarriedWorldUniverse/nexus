package frame

import (
	"context"
	"testing"

	"github.com/nexus-cw/nexus/nexus/aspects"
	"github.com/nexus-cw/nexus/nexus/broker"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// freshAspectsStore opens a fresh test DB with the bootstrapped schema
// and returns an aspects.SQLStore wrapping it. Mirrors the helper in
// nexus/aspects/aspects_test.go.
func freshAspectsStore(t *testing.T) *aspects.SQLStore {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return aspects.NewSQLStore(db)
}

// fakePubkey returns a 32-byte placeholder for the aspects.Insert call
// (Insert requires a pubkey of valid size; the personality wiring path
// doesn't care about its content).
func fakePubkey() []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = 0xAB
	}
	return out
}

// TestEmbed_LoadsPersonality — operator pre-populated aspect_personalities
// for "frame"; Embed must find it and EmbeddedFrame.SystemPrompt must
// return the composed bundle (or NexusMD fallback when composed is "").
func TestEmbed_LoadsPersonality(t *testing.T) {
	store := freshAspectsStore(t)
	ctx := context.Background()

	// Set up the aspect row + a personality row for "frame".
	if err := store.Insert(ctx, aspects.Aspect{
		Name: "frame", AspectPubkey: fakePubkey(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert aspect: %v", err)
	}
	if err := store.PersonalitySet(ctx, aspects.Personality{
		AspectName: "frame",
		NexusMD:    "## frame core",
		SoulMD:     "core soul",
		PrimerMD:   "boot primer",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}

	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(ctx, EmbedConfig{
		Detected:         newFrameAspect(t, "frame"),
		Roster:           r,
		TokenStore:       ts,
		PersonalityStore: store,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	// Composed cache is "" right after PersonalitySet (Part 2's
	// invalidation); SystemPrompt falls back to concat of all three
	// sections so SoulMD + PrimerMD aren't silently dropped.
	want := "## frame core\n\n---\n\ncore soul\n\n---\n\nboot primer"
	if got := e.SystemPrompt(); got != want {
		t.Errorf("SystemPrompt = %q; want concat fallback %q", got, want)
	}
}

// TestEmbed_FallbackOmitsEmptySections — when SoulMD is empty, the
// concat skips it (no double-separator) so the Frame doesn't end up
// with "core\n\n---\n\n\n\n---\n\nprimer".
func TestEmbed_FallbackOmitsEmptySections(t *testing.T) {
	e := &EmbeddedFrame{
		personality: &aspects.Personality{
			NexusMD:  "core",
			SoulMD:   "",
			PrimerMD: "primer",
		},
	}
	if got := e.SystemPrompt(); got != "core\n\n---\n\nprimer" {
		t.Errorf("SystemPrompt with empty SoulMD = %q; want clean 2-section concat", got)
	}
}

// TestEmbed_PrefersComposedOverNexusMD — when Composed is non-empty
// (Part 7's renderer will populate this), SystemPrompt returns it
// rather than NexusMD alone. Tested via a manual EmbeddedFrame where
// we can inject a personality with a non-empty Composed; can't reach
// it through the Store API alone since PersonalitySet always
// invalidates Composed.
func TestEmbed_PrefersComposedOverNexusMD(t *testing.T) {
	e := &EmbeddedFrame{
		personality: &aspects.Personality{
			AspectName: "frame",
			NexusMD:    "fallback",
			Composed:   "FULLY COMPOSED PROMPT",
		},
	}
	if got := e.SystemPrompt(); got != "FULLY COMPOSED PROMPT" {
		t.Errorf("SystemPrompt = %q; want composed", got)
	}
}

// TestEmbed_NoPersonality_RunsWithEmptyPrompt — no personality row
// for the Frame name. Embed must succeed (Frame can boot prompt-less)
// but SystemPrompt returns "".
func TestEmbed_NoPersonality_RunsWithEmptyPrompt(t *testing.T) {
	store := freshAspectsStore(t)

	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(context.Background(), EmbedConfig{
		Detected:         newFrameAspect(t, "frame"),
		Roster:           r,
		TokenStore:       ts,
		PersonalityStore: store, // empty — no rows
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := e.SystemPrompt(); got != "" {
		t.Errorf("SystemPrompt = %q; want empty (no row populated yet)", got)
	}
}

// TestEmbed_NoPersonalityStore_LegacyPath — when PersonalityStore is
// nil, Embed must still succeed (legacy callers and bootstrap-mode
// pre-Part-2 paths pass nil). SystemPrompt returns "".
func TestEmbed_NoPersonalityStore_LegacyPath(t *testing.T) {
	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "frame"),
		Roster:     r,
		TokenStore: ts,
		// PersonalityStore omitted — legacy path.
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := e.SystemPrompt(); got != "" {
		t.Errorf("SystemPrompt = %q; want empty (legacy nil-store path)", got)
	}
}

// TestEmbed_RefreshPersonality — Part 7's in-process refresh path.
// After Embed, operator edits the personality (PersonalitySet); the
// next RefreshPersonality call swaps the cached bundle so SystemPrompt
// reflects the new content.
func TestEmbed_RefreshPersonality(t *testing.T) {
	store := freshAspectsStore(t)
	ctx := context.Background()

	if err := store.Insert(ctx, aspects.Aspect{
		Name: "frame", AspectPubkey: fakePubkey(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert aspect: %v", err)
	}
	if err := store.PersonalitySet(ctx, aspects.Personality{
		AspectName: "frame", NexusMD: "v1 prompt",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}

	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(ctx, EmbedConfig{
		Detected:         newFrameAspect(t, "frame"),
		Roster:           r,
		TokenStore:       ts,
		PersonalityStore: store,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := e.SystemPrompt(); got != "v1 prompt" {
		t.Errorf("initial SystemPrompt = %q; want v1 prompt", got)
	}

	// Operator updates the row.
	if err := store.PersonalitySet(ctx, aspects.Personality{
		AspectName: "frame", NexusMD: "v2 prompt",
	}); err != nil {
		t.Fatalf("PersonalitySet v2: %v", err)
	}

	// Pre-refresh: cached value still v1.
	if got := e.SystemPrompt(); got != "v1 prompt" {
		t.Errorf("pre-refresh SystemPrompt = %q; want v1 (no refresh called yet)", got)
	}

	// Post-refresh: new value visible.
	if err := e.RefreshPersonality(ctx); err != nil {
		t.Fatalf("RefreshPersonality: %v", err)
	}
	if got := e.SystemPrompt(); got != "v2 prompt" {
		t.Errorf("post-refresh SystemPrompt = %q; want v2 prompt", got)
	}
}

// TestEmbed_ComposedDoesNotDoubleBakeCentral — pins the seam for the
// future Part 7 renderer that will populate aspect_personalities.composed.
//
// Invariant: when SystemPrompt is called with both centralNexusMD set
// AND personality.Composed set, the result is `central + composed`,
// NOT `central + (central + per-aspect)`. This means the renderer
// must NOT bake central content into Composed — central is layered
// at read time.
//
// This test passes today against the current code (one concat layer).
// Its job is to fail loudly if Part 7's renderer author bakes central
// into Composed without realising SystemPrompt also adds it.
func TestEmbed_ComposedDoesNotDoubleBakeCentral(t *testing.T) {
	e := &EmbeddedFrame{
		centralNexusMD: "## central network scope",
		personality: &aspects.Personality{
			Composed: "per-aspect rendered prompt",
		},
	}
	want := "## central network scope\n\n---\n\nper-aspect rendered prompt"
	if got := e.SystemPrompt(); got != want {
		t.Errorf("SystemPrompt = %q\nwant %q", got, want)
	}
}

// TestEmbed_LayersCentralAbovePerAspect — Part 9b: when both
// nexus_settings.nexus_md AND aspect_personalities content exist,
// SystemPrompt concatenates them with the central chunk first.
func TestEmbed_LayersCentralAbovePerAspect(t *testing.T) {
	store := freshAspectsStore(t)
	settings := aspects.NewSQLSettingsStore(store.DBForTest())
	ctx := context.Background()

	if err := store.Insert(ctx, aspects.Aspect{
		Name: "frame", AspectPubkey: fakePubkey(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := store.PersonalitySet(ctx, aspects.Personality{
		AspectName: "frame",
		NexusMD:    "frame-specific delta",
		SoulMD:     "frame voice",
		PrimerMD:   "frame primer",
	}); err != nil {
		t.Fatalf("PersonalitySet: %v", err)
	}
	if _, err := settings.SetNexusMD(ctx, "## central network scope"); err != nil {
		t.Fatalf("SetNexusMD: %v", err)
	}

	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(ctx, EmbedConfig{
		Detected:         newFrameAspect(t, "frame"),
		Roster:           r,
		TokenStore:       ts,
		PersonalityStore: store,
		SettingsStore:    settings,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	want := "## central network scope\n\n---\n\n" +
		"frame-specific delta\n\n---\n\n" +
		"frame voice\n\n---\n\n" +
		"frame primer"
	if got := e.SystemPrompt(); got != want {
		t.Errorf("SystemPrompt = %q\nwant %q", got, want)
	}
}

// TestEmbed_RefreshCentral — Part 9b: RefreshCentral re-reads
// nexus_settings and updates the cached central content.
func TestEmbed_RefreshCentral(t *testing.T) {
	store := freshAspectsStore(t)
	settings := aspects.NewSQLSettingsStore(store.DBForTest())
	ctx := context.Background()

	if err := store.Insert(ctx, aspects.Aspect{
		Name: "frame", AspectPubkey: fakePubkey(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := settings.SetNexusMD(ctx, "v1"); err != nil {
		t.Fatalf("SetNexusMD v1: %v", err)
	}

	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(ctx, EmbedConfig{
		Detected:         newFrameAspect(t, "frame"),
		Roster:           r,
		TokenStore:       ts,
		PersonalityStore: store,
		SettingsStore:    settings,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := e.SystemPrompt(); got != "v1" {
		t.Errorf("initial SystemPrompt = %q; want v1", got)
	}
	if e.CentralVersion() != 1 {
		t.Errorf("initial CentralVersion = %d; want 1", e.CentralVersion())
	}

	// Operator updates central via the future Part 9c admin endpoint
	// (here simulated by calling SetNexusMD directly).
	if _, err := settings.SetNexusMD(ctx, "v2"); err != nil {
		t.Fatalf("SetNexusMD v2: %v", err)
	}

	// Pre-refresh: cached value still v1.
	if got := e.SystemPrompt(); got != "v1" {
		t.Errorf("pre-refresh = %q; want v1 (no refresh called)", got)
	}

	// Post-refresh: new value.
	if err := e.RefreshCentral(ctx); err != nil {
		t.Fatalf("RefreshCentral: %v", err)
	}
	if got := e.SystemPrompt(); got != "v2" {
		t.Errorf("post-refresh = %q; want v2", got)
	}
	if e.CentralVersion() != 2 {
		t.Errorf("post-refresh CentralVersion = %d; want 2", e.CentralVersion())
	}
}

// TestEmbed_RefreshCentral_NoStore — legacy mode (no SettingsStore)
// returns nil silently rather than erroring.
func TestEmbed_RefreshCentral_NoStore(t *testing.T) {
	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "frame"),
		Roster:     r,
		TokenStore: ts,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if err := e.RefreshCentral(context.Background()); err != nil {
		t.Errorf("RefreshCentral with nil settings: want silent no-op; got %v", err)
	}
	if got := e.CentralVersion(); got != 0 {
		t.Errorf("CentralVersion legacy = %d; want 0", got)
	}
}

// TestEmbed_RefreshPersonality_NoStore — RefreshPersonality on a
// legacy-mode EmbeddedFrame (no store at Embed time) returns an error
// rather than silently no-oping.
func TestEmbed_RefreshPersonality_NoStore(t *testing.T) {
	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "frame"),
		Roster:     r,
		TokenStore: ts,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if err := e.RefreshPersonality(context.Background()); err == nil {
		t.Error("RefreshPersonality with nil store: want error; got nil")
	}
}

