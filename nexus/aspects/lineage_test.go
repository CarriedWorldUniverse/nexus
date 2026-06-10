package aspects

import "testing"

func TestLineageNames(t *testing.T) {
	cases := []struct {
		name    string
		derived bool
		base    string
	}{
		{"plumb", false, "plumb"},
		{"plumb.sub-1", true, "plumb"},
		{"plumb.sub-12", true, "plumb"},
		{"maren-art.sub-4", true, "maren-art"},
		{"plumb.sub-", false, "plumb.sub-"},
		{"plumb.sub-x", false, "plumb.sub-x"},
		{".sub-1", false, ".sub-1"}, // empty base is not a lineage
		{"a.sub-1.sub-2", true, "a.sub-1"},
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
	if got := DerivedName("plumb", 3); got != "plumb.sub-3" {
		t.Fatalf("DerivedName = %q", got)
	}
	if !IsDerivedName(DerivedName("plumb", 3)) {
		t.Fatal("DerivedName output must satisfy IsDerivedName")
	}
	if BaseName(DerivedName("plumb", 3)) != "plumb" {
		t.Fatal("BaseName must invert DerivedName")
	}
}
