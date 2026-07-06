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

func TestSplitWorker(t *testing.T) {
	cases := []struct {
		name        string
		personality string
		role        string
		ok          bool
	}{
		{"anvil-builder", "anvil", "builder", true},
		{"maren-painter", "maren", "painter", true},
		{"plumb-security-reviewer", "plumb", "security-reviewer", true}, // longest role suffix wins
		{"keel-modeller", "keel", "modeller", true},
		{"harrow-tester", "harrow", "tester", true},
		{"shadow-builder", "", "", false}, // shadow is the orchestrator, not a worker personality
		{"maren-art", "", "", false},      // ordinary hyphenated aspect name, not a worker
		{"anvil-plumber", "", "", false},  // unknown role
		{"nobody-builder", "", "", false}, // unknown personality
		{"anvil", "", "", false},          // bare personality is not a worker identity
		{"plumb.bob", "", "", false},      // dotted hand is not a worker identity
	}
	for _, c := range cases {
		p, r, ok := SplitWorker(c.name)
		if ok != c.ok || p != c.personality || r != c.role {
			t.Errorf("SplitWorker(%q) = (%q,%q,%v), want (%q,%q,%v)", c.name, p, r, ok, c.personality, c.role, c.ok)
		}
		if IsWorkerName(c.name) != c.ok {
			t.Errorf("IsWorkerName(%q) = %v, want %v", c.name, IsWorkerName(c.name), c.ok)
		}
	}
}

func TestPersonalityOf(t *testing.T) {
	cases := map[string]string{
		"anvil-builder":           "anvil",     // worker → personality
		"plumb-security-reviewer": "plumb",     // worker → personality
		"shadow.umbra":            "shadow",    // dotted hand → base aspect
		"maren-art":               "maren-art", // ordinary aspect → itself
		"harrow":                  "harrow",    // bare aspect → itself
	}
	for name, want := range cases {
		if got := PersonalityOf(name); got != want {
			t.Errorf("PersonalityOf(%q) = %q, want %q", name, got, want)
		}
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
