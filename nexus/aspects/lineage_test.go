package aspects

import "testing"

func TestLineageNames(t *testing.T) {
	cases := []struct {
		name    string
		derived bool
		base    string
	}{
		{"plumb", false, "plumb"},
		{"plumb.bob", true, "plumb"},
		{"plumb.fathom", true, "plumb"},
		{"shadow.umbra", true, "shadow"},
		{"maren-art.brine", true, "maren-art"}, // hyphenated base survives
		{"anvil.hand-7", true, "anvil"},        // overflow token is still derived
		{"plumb.", false, "plumb."},            // empty suffix is not a lineage
		{".umbra", false, ".umbra"},            // empty base is not a lineage
	}
	for _, c := range cases {
		if got := IsDerivedName(c.name); got != c.derived {
			t.Errorf("IsDerivedName(%q) = %v, want %v", c.name, got, c.derived)
		}
		if got := BaseName(c.name); got != c.base {
			t.Errorf("BaseName(%q) = %q, want %q", c.name, got, c.base)
		}
	}
}

func TestDerivedName(t *testing.T) {
	if got := DerivedName("plumb", "fathom"); got != "plumb.fathom" {
		t.Fatalf("DerivedName = %q", got)
	}
	if !IsDerivedName(DerivedName("plumb", "fathom")) {
		t.Fatal("DerivedName output must satisfy IsDerivedName")
	}
	if BaseName(DerivedName("plumb", "fathom")) != "plumb" {
		t.Fatal("BaseName must invert DerivedName")
	}
}

func TestOverflowHandName(t *testing.T) {
	got := OverflowHandName("shadow", 5)
	if got != "shadow.hand-5" {
		t.Fatalf("OverflowHandName = %q", got)
	}
	if !IsDerivedName(got) || BaseName(got) != "shadow" {
		t.Fatalf("overflow name must round-trip as a derived name of shadow, got base %q", BaseName(got))
	}
}

func TestHandNamePool(t *testing.T) {
	if pool := HandNamePool("shadow"); len(pool) == 0 || pool[0] != "umbra" {
		t.Fatalf("shadow pool unexpected: %v", pool)
	}
	if pool := HandNamePool("nobody-aspect"); len(pool) != 0 {
		t.Fatalf("unknown aspect must have empty pool, got %v", pool)
	}
}
