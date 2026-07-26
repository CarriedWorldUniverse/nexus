package aspects

import (
	"context"
	"testing"
)

// resolveWorkerRig stands up a base aspect plus a settings row carrying
// both central variants. ResolveByName is the path a JWT-booted worker
// takes (broker/validate_endpoint.go calls it for exactly this case), so
// this is where the NEX-826 split has to hold or the feature does
// nothing for the identities it was built for.
func resolveWorkerRig(t *testing.T, interactive, worker string) (*SQLStore, *SQLSettingsStore) {
	t.Helper()
	store := freshStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name:         "shadow",
		AspectPubkey: fakePubkey(9),
		Provider:     "claude-api",
		Model:        "claude-opus",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PersonalitySet(ctx, Personality{AspectName: "shadow", SoulMD: "shadow soul"}); err != nil {
		t.Fatal(err)
	}
	settings := NewSQLSettingsStore(store.DBForTest())
	if interactive != "" {
		if _, err := settings.SetNexusMD(ctx, interactive); err != nil {
			t.Fatal(err)
		}
	}
	if worker != "" {
		if _, err := settings.SetNexusMDWorker(ctx, worker); err != nil {
			t.Fatal(err)
		}
	}
	return store, settings
}

// A dispatched run must receive worker policy while still inheriting its parent's
// persona. Those two facts pull in opposite directions — persona keys on
// BaseName, policy keys on the full name — so they are asserted together
// to keep a future "simplification" from collapsing both onto one key.
func TestResolveByName_DispatchedRunGetsWorkerCentral(t *testing.T) {
	store, settings := resolveWorkerRig(t, "## interactive", "## headless")

	got, err := ResolveByName(context.Background(),
		ResolveConfigByName{Store: store, Settings: settings}, "shadow.umbra")
	if err != nil {
		t.Fatal(err)
	}
	if got.CentralNexusMD != "## headless" {
		t.Errorf("dispatched run got central %q, want the worker variant", got.CentralNexusMD)
	}
	if got.Personality == nil || got.Personality.SoulMD != "shadow soul" {
		t.Errorf("dispatched run must still inherit the parent persona, got %+v", got.Personality)
	}
}

// The parent aspect, resolved through the same store, must be unaffected.
func TestResolveByName_AspectKeepsInteractiveCentral(t *testing.T) {
	store, settings := resolveWorkerRig(t, "## interactive", "## headless")

	got, err := ResolveByName(context.Background(),
		ResolveConfigByName{Store: store, Settings: settings}, "shadow")
	if err != nil {
		t.Fatal(err)
	}
	if got.CentralNexusMD != "## interactive" {
		t.Errorf("aspect got central %q, want the interactive text", got.CentralNexusMD)
	}
}

// Pool workers are the other headless shape: `<personality>-<role>`.
// This one is easy to lose, because it does not look like a derived name
// unless you know IsWorkerName exists.
func TestResolveByName_PoolWorkerGetsWorkerCentral(t *testing.T) {
	store := freshStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name:         "anvil",
		AspectPubkey: fakePubkey(11),
		Provider:     "openai",
		Model:        "ornith",
	}); err != nil {
		t.Fatal(err)
	}
	settings := NewSQLSettingsStore(store.DBForTest())
	if _, err := settings.SetNexusMD(ctx, "## interactive"); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.SetNexusMDWorker(ctx, "## headless"); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveByName(ctx, ResolveConfigByName{Store: store, Settings: settings}, "anvil-builder")
	if err != nil {
		t.Fatal(err)
	}
	if got.CentralNexusMD != "## headless" {
		t.Errorf("pool worker got central %q, want the worker variant", got.CentralNexusMD)
	}
}

// The migration-safety case: with no worker variant configured, a run
// resolves exactly as it did before NEX-826. Deploying the schema change
// must not alter a single identity's prompt until content is written.
func TestResolveByName_NoWorkerVariant_UnchangedBehaviour(t *testing.T) {
	store, settings := resolveWorkerRig(t, "## interactive", "")

	for _, name := range []string{"shadow", "shadow.umbra"} {
		got, err := ResolveByName(context.Background(),
			ResolveConfigByName{Store: store, Settings: settings}, name)
		if err != nil {
			t.Fatal(err)
		}
		if got.CentralNexusMD != "## interactive" {
			t.Errorf("%s got central %q, want the interactive text (pre-NEX-826 behaviour)",
				name, got.CentralNexusMD)
		}
	}
}
