package broker

import (
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// TestRenderParticipantBrief — the brief substitutes the participant, the
// problem, an explicit lens, the co-deliberators, and the facilitator, and
// leads with the @mention that wakes the participant.
func TestRenderParticipantBrief(t *testing.T) {
	b := renderParticipantBrief("plumb", "shadow", "pick a lockfile format",
		"play the skeptic", []string{"plumb", "anvil"})
	if !strings.HasPrefix(b, "@plumb") {
		t.Errorf("brief must lead with @plumb (wake mention): %q", b)
	}
	for _, want := range []string{"pick a lockfile format", "play the skeptic", "anvil", "@shadow"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief missing %q: %q", want, b)
		}
	}
}

func TestRenderParticipantBriefDefaultLens(t *testing.T) {
	b := renderParticipantBrief("anvil", "shadow", "X", "", []string{"plumb", "anvil"})
	if !strings.Contains(b, "your standing perspective") {
		t.Errorf("empty lens should fall back to standing perspective: %q", b)
	}
}

// TestRenderFacilitatorBrief — the facilitation contract (round cadence,
// convergence test, CONSENSUS: format, stuck→decision-point, close) lives
// in one place; assert the load-bearing pieces render.
func TestRenderFacilitatorBrief(t *testing.T) {
	b := renderFacilitatorBrief("shadow", "cv-1", "design X", []string{"plumb", "anvil"})
	for _, want := range []string{"@shadow", "cv-1", "design X", "CONSENSUS:", "convene.close", "shadow"} {
		if !strings.Contains(b, want) {
			t.Errorf("facilitator brief missing %q: %q", want, b)
		}
	}
	// Mediation contract: the operator is not firehosed.
	if !strings.Contains(strings.ToLower(b), "operator is not in this thread") {
		t.Errorf("facilitator brief missing the mediation contract: %q", b)
	}
}

func TestResolveFacilitator(t *testing.T) {
	cases := []struct{ override, sender, want string }{
		{"keel", "shadow", "keel"}, // explicit override wins
		{"", "plumb", "plumb"},     // convener facilitates
		{"", "operator", "shadow"}, // operator → shadow
	}
	for _, c := range cases {
		if got := resolveFacilitator(c.override, c.sender); got != c.want {
			t.Errorf("resolveFacilitator(%q,%q) = %q, want %q", c.override, c.sender, got, c.want)
		}
	}
}

// TestConveneFramesKnown — the new convene frame kinds are recognised so
// the WS read loop routes them rather than logging-and-dropping.
func TestConveneFramesKnown(t *testing.T) {
	for _, k := range []frames.Kind{
		frames.KindConveneClose, frames.KindConveneCloseResult,
		frames.KindConvenesList, frames.KindConvenesListResult,
	} {
		if !frames.IsKnown(k) {
			t.Errorf("frames.IsKnown(%q) = false, want true", k)
		}
	}
}
