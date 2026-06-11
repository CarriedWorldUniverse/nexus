package roster

import (
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// Derived hand identities register as ordinary roster entries with a
// lineage marker; first-class aspects carry none (NEX-571).
func TestRegisterStampsLineageForDerivedNames(t *testing.T) {
	r := New()

	hand, _, err := r.Register(&schemas.RegisterRequest{Name: "plumb.sub-2", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Register derived: %v", err)
	}
	if hand.Lineage != "plumb" {
		t.Errorf("derived Lineage = %q, want plumb", hand.Lineage)
	}

	base, _, err := r.Register(&schemas.RegisterRequest{Name: "plumb", SessionID: "s2"})
	if err != nil {
		t.Fatalf("Register base: %v", err)
	}
	if base.Lineage != "" {
		t.Errorf("base Lineage = %q, want empty", base.Lineage)
	}

	// Listings expose the marker.
	for _, a := range r.List() {
		want := ""
		if a.Name == "plumb.sub-2" {
			want = "plumb"
		}
		if a.Lineage != want {
			t.Errorf("List() %s Lineage = %q, want %q", a.Name, a.Lineage, want)
		}
	}
}

// One-session-per-name still holds PER DERIVED NAME: the parent and
// each hand hold independent slots, but a second session can't steal a
// live hand's name.
func TestOneSessionPerDerivedName(t *testing.T) {
	r := New()
	if _, _, err := r.Register(&schemas.RegisterRequest{Name: "plumb", SessionID: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Register(&schemas.RegisterRequest{Name: "plumb.sub-1", SessionID: "h1"}); err != nil {
		t.Fatalf("hand must register beside its live parent: %v", err)
	}
	if _, _, err := r.Register(&schemas.RegisterRequest{Name: "plumb.sub-2", SessionID: "h2"}); err != nil {
		t.Fatalf("second hand slot must register: %v", err)
	}
	_, _, err := r.Register(&schemas.RegisterRequest{Name: "plumb.sub-1", SessionID: "intruder"})
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("err = %v, want ErrAlreadyRegistered for a live derived name", err)
	}
}
