package aspects

import (
	"context"
	"errors"
	"testing"
)

// ResolveByName on a BASE aspect name returns its own persona +
// provider/model — the keyfile-less counterpart of Validate's steps 7-8.
func TestResolveByName_BaseAspect(t *testing.T) {
	store := freshStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name:         "plumb",
		AspectPubkey: fakePubkey(7),
		Provider:     "deepseek",
		Model:        "deepseek-chat",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PersonalitySet(ctx, Personality{AspectName: "plumb", SoulMD: "I am plumb."}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveByName(ctx, ResolveConfigByName{Store: store}, "plumb")
	if err != nil {
		t.Fatal(err)
	}
	if got.AspectName != "plumb" || got.Provider != "deepseek" || got.Model != "deepseek-chat" {
		t.Errorf("resolved = %+v", got)
	}
	if got.Personality == nil || got.Personality.SoulMD != "I am plumb." {
		t.Errorf("persona = %+v", got.Personality)
	}
}

// A derived hand name resolves the BASE aspect's persona AND provider
// (provider inheritance) while keeping the derived name on AspectName
// (truthful lineage).
func TestResolveByName_DerivedInheritsParent(t *testing.T) {
	store := freshStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name:         "shadow",
		AspectPubkey: fakePubkey(7),
		Provider:     "claude-api",
		Model:        "claude-opus",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PersonalitySet(ctx, Personality{AspectName: "shadow", SoulMD: "shadow soul"}); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveByName(ctx, ResolveConfigByName{Store: store}, "shadow.umbra")
	if err != nil {
		t.Fatal(err)
	}
	if got.AspectName != "shadow.umbra" {
		t.Errorf("AspectName = %q, want the derived name (truthful lineage)", got.AspectName)
	}
	if got.Provider != "claude-api" || got.Model != "claude-opus" {
		t.Errorf("hand must inherit parent provider/model, got %q/%q", got.Provider, got.Model)
	}
	if got.Personality == nil || got.Personality.SoulMD != "shadow soul" {
		t.Errorf("hand must serve the parent persona, got %+v", got.Personality)
	}
}

func TestResolveByName_UnknownAspect(t *testing.T) {
	store := freshStore(t)
	if _, err := ResolveByName(context.Background(), ResolveConfigByName{Store: store}, "ghost.umbra"); !errors.Is(err, ErrUnknownAspect) {
		t.Fatalf("err = %v, want ErrUnknownAspect", err)
	}
}
