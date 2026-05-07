package aspects

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestEditPersonality_FirstEdit — operator opens the editor for an
// aspect that has no personality row yet. EditPersonality writes the
// row, returns OldVersion=0 and NewVersion=1.
func TestEditPersonality_FirstEdit(t *testing.T) {
	store := freshStore(t)
	ctx := context.Background()

	if err := store.Insert(ctx, Aspect{
		Name: "plumb", AspectPubkey: fakePubkey(1),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	change, err := EditPersonality(ctx, store, "plumb",
		"## plumb operational", "voice & values", "network primer")
	if err != nil {
		t.Fatalf("EditPersonality: %v", err)
	}
	if change.OldVersion != 0 {
		t.Errorf("OldVersion = %d; want 0 (first edit)", change.OldVersion)
	}
	if change.NewVersion != 1 {
		t.Errorf("NewVersion = %d; want 1", change.NewVersion)
	}

	got, err := store.PersonalityGet(ctx, "plumb")
	if err != nil {
		t.Fatalf("PersonalityGet: %v", err)
	}
	if got.NexusMD != "## plumb operational" || got.SoulMD != "voice & values" || got.PrimerMD != "network primer" {
		t.Errorf("personality round-trip wrong: %+v", got)
	}
}

// TestEditPersonality_BumpsVersionOnSubsequent — second edit on the
// same aspect bumps the version monotonically.
func TestEditPersonality_BumpsVersionOnSubsequent(t *testing.T) {
	store := freshStore(t)
	ctx := context.Background()
	if err := store.Insert(ctx, Aspect{
		Name: "plumb", AspectPubkey: fakePubkey(1),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	first, err := EditPersonality(ctx, store, "plumb", "v1 nexus", "v1 soul", "v1 primer")
	if err != nil {
		t.Fatalf("EditPersonality 1: %v", err)
	}
	second, err := EditPersonality(ctx, store, "plumb", "v2 nexus", "v2 soul", "v2 primer")
	if err != nil {
		t.Fatalf("EditPersonality 2: %v", err)
	}
	if second.OldVersion != first.NewVersion {
		t.Errorf("second OldVersion = %d; want first NewVersion %d", second.OldVersion, first.NewVersion)
	}
	if second.NewVersion <= first.NewVersion {
		t.Errorf("version did not bump: first=%d second=%d", first.NewVersion, second.NewVersion)
	}
}

// TestEditPersonality_AspectNotFound — the FK guard fires when the
// parent aspect row doesn't exist. Caller renders "mint it first".
func TestEditPersonality_AspectNotFound(t *testing.T) {
	store := freshStore(t)
	_, err := EditPersonality(context.Background(), store, "ghost",
		"x", "y", "z")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("EditPersonality(ghost) = %v; want wrapped ErrNotFound", err)
	}
}

// TestEditPersonality_InvalidArgs — nil store / empty name fail
// loudly without touching the DB.
func TestEditPersonality_InvalidArgs(t *testing.T) {
	store := freshStore(t)
	cases := []struct {
		name      string
		s         Store
		aspect    string
		errSubstr string
	}{
		{"nil store", nil, "plumb", "store nil"},
		{"empty name", store, "", "name required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EditPersonality(context.Background(), tc.s, tc.aspect, "", "", "")
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("got %v; want error containing %q", err, tc.errSubstr)
			}
		})
	}
}

// TestEditBlob_RoundTrip — Marshal → Unmarshal preserves all three
// sections, including content with internal blank lines.
func TestEditBlob_RoundTrip(t *testing.T) {
	original := &Personality{
		NexusMD:  "## section\n\nfirst para\n\nsecond para",
		SoulMD:   "voice\n- one\n- two",
		PrimerMD: "single line",
	}
	blob := MarshalEditBlob(original)
	nexusMD, soulMD, primerMD, err := UnmarshalEditBlob(blob)
	if err != nil {
		t.Fatalf("UnmarshalEditBlob: %v", err)
	}
	if nexusMD != original.NexusMD {
		t.Errorf("NexusMD = %q; want %q", nexusMD, original.NexusMD)
	}
	if soulMD != original.SoulMD {
		t.Errorf("SoulMD = %q; want %q", soulMD, original.SoulMD)
	}
	if primerMD != original.PrimerMD {
		t.Errorf("PrimerMD = %q; want %q", primerMD, original.PrimerMD)
	}
}

// TestEditBlob_EmptySections — operator clears all three fields. Must
// round-trip cleanly.
func TestEditBlob_EmptySections(t *testing.T) {
	blob := MarshalEditBlob(&Personality{})
	n, s, p, err := UnmarshalEditBlob(blob)
	if err != nil {
		t.Fatalf("UnmarshalEditBlob empty: %v", err)
	}
	if n != "" || s != "" || p != "" {
		t.Errorf("empty round-trip nonzero: nexus=%q soul=%q primer=%q", n, s, p)
	}
}

// TestEditBlob_MissingHeaders — operator deletes a header by mistake.
// Parser must return an error naming which header is missing rather
// than silently merging sections.
func TestEditBlob_MissingHeaders(t *testing.T) {
	cases := []struct {
		name, blob, want string
	}{
		{
			"missing NEXUS",
			"# === SOUL.md ===\nx\n# === PRIMER.md ===\ny",
			"NEXUS.md",
		},
		{
			"missing SOUL",
			"# === NEXUS.md ===\nx\n# === PRIMER.md ===\ny",
			"SOUL.md",
		},
		{
			"missing PRIMER",
			"# === NEXUS.md ===\nx\n# === SOUL.md ===\ny",
			"PRIMER.md",
		},
		{
			"completely empty blob",
			"",
			"NEXUS.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := UnmarshalEditBlob(tc.blob)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v; want error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestEditBlob_HeadersOutOfOrder — SOUL before NEXUS. Parser is
// strict about order (the marshalled form is always NEXUS → SOUL →
// PRIMER), so out-of-order is a user error.
func TestEditBlob_HeadersOutOfOrder(t *testing.T) {
	blob := "# === SOUL.md ===\ns\n# === NEXUS.md ===\nn\n# === PRIMER.md ===\np"
	_, _, _, err := UnmarshalEditBlob(blob)
	if err == nil {
		t.Error("expected error for out-of-order headers; got nil")
	}
}

// TestEditBlob_TrimsTrailingWhitespace — the marshalled blob has a
// blank line between sections; Unmarshal must trim it without
// truncating intentional internal whitespace.
func TestEditBlob_TrimsTrailingWhitespace(t *testing.T) {
	blob := "# === NEXUS.md ===\nfoo\n\n   \n# === SOUL.md ===\nbar\n# === PRIMER.md ===\nbaz\n"
	n, _, _, err := UnmarshalEditBlob(blob)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if n != "foo" {
		t.Errorf("NexusMD = %q; want %q (trailing whitespace not trimmed)", n, "foo")
	}
}
