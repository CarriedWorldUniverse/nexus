package broker

import (
	"reflect"
	"testing"
)

// known is a tiny set used across the parse tests.
func known(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestParseConveneBasic(t *testing.T) {
	cmd, err := parseConveneCommand(
		"!convene plumb anvil — should bridle adopt a registry for hand images?",
		known("plumb", "anvil"),
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(cmd.Participants, []string{"plumb", "anvil"}) {
		t.Errorf("participants = %v, want [plumb anvil]", cmd.Participants)
	}
	if cmd.Problem != "should bridle adopt a registry for hand images?" {
		t.Errorf("problem = %q", cmd.Problem)
	}
	// Facilitator defaults to the sender; the parser leaves it empty for the
	// caller (HandleChatSend) to resolve from the sender/operator rule.
	if cmd.Facilitator != "" {
		t.Errorf("facilitator = %q, want empty (caller resolves)", cmd.Facilitator)
	}
}

func TestParseConveneColonSeparator(t *testing.T) {
	cmd, err := parseConveneCommand("!convene plumb anvil : pick a lockfile format", known("plumb", "anvil"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cmd.Problem != "pick a lockfile format" {
		t.Errorf("problem = %q", cmd.Problem)
	}
}

func TestParseConveneCommaList(t *testing.T) {
	cmd, err := parseConveneCommand("!convene plumb, anvil, keel — design X", known("plumb", "anvil", "keel"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(cmd.Participants, []string{"plumb", "anvil", "keel"}) {
		t.Errorf("participants = %v", cmd.Participants)
	}
}

func TestParseConveneFacilitatorOverride(t *testing.T) {
	cmd, err := parseConveneCommand("!convene facilitator=keel plumb anvil — design X", known("plumb", "anvil", "keel"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cmd.Facilitator != "keel" {
		t.Errorf("facilitator = %q, want keel", cmd.Facilitator)
	}
	if !reflect.DeepEqual(cmd.Participants, []string{"plumb", "anvil"}) {
		t.Errorf("participants = %v, want [plumb anvil] (facilitator stripped)", cmd.Participants)
	}
}

func TestParseConveneLensSegments(t *testing.T) {
	cmd, err := parseConveneCommand(
		"!convene plumb anvil lens:plumb=play the skeptic — design X",
		known("plumb", "anvil"),
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cmd.Lenses["plumb"] != "play the skeptic" {
		t.Errorf("lens[plumb] = %q, want 'play the skeptic'", cmd.Lenses["plumb"])
	}
	if cmd.Problem != "design X" {
		t.Errorf("problem = %q", cmd.Problem)
	}
}

func TestParseConveneRejectsEmptyLensText(t *testing.T) {
	// lens:plumb= with no following text resolves to an empty lens; that's a
	// malformed segment, not a silent empty lens.
	_, err := parseConveneCommand(
		"!convene plumb anvil lens:plumb= — design X",
		known("plumb", "anvil"),
	)
	if err == nil {
		t.Fatal("expected error for empty lens text (lens:plumb=)")
	}
}

func TestParseConveneLensEqualsThenWords(t *testing.T) {
	// lens:plumb= followed by words: the words become the lens text and it
	// parses fine (the trailing = is just where Cut splits).
	cmd, err := parseConveneCommand(
		"!convene plumb anvil lens:plumb= careful about X — design X",
		known("plumb", "anvil"),
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cmd.Lenses["plumb"] != "careful about X" {
		t.Errorf("lens[plumb] = %q, want 'careful about X'", cmd.Lenses["plumb"])
	}
}

func TestParseConveneRejectsUnknownAspect(t *testing.T) {
	_, err := parseConveneCommand("!convene plumb ghost — x", known("plumb", "anvil"))
	if err == nil {
		t.Fatal("expected error for unknown aspect 'ghost'")
	}
}

func TestParseConveneRejectsDerivedName(t *testing.T) {
	// Derived hand identities (<parent>.<word>) carry a dot and are not
	// conveneable — only base aspects argue at the roundtable.
	_, err := parseConveneCommand("!convene plumb shadow.umbra — x", known("plumb", "shadow"))
	if err == nil {
		t.Fatal("expected error for derived name 'shadow.umbra'")
	}
}

func TestParseConveneRequiresTwoParticipants(t *testing.T) {
	_, err := parseConveneCommand("!convene plumb — x", known("plumb", "anvil"))
	if err == nil {
		t.Fatal("expected error: convene needs >=2 participants")
	}
}

func TestParseConveneRequiresProblem(t *testing.T) {
	_, err := parseConveneCommand("!convene plumb anvil —", known("plumb", "anvil"))
	if err == nil {
		t.Fatal("expected error: empty problem statement")
	}
}

func TestParseConveneRequiresSeparator(t *testing.T) {
	_, err := parseConveneCommand("!convene plumb anvil no separator here", known("plumb", "anvil"))
	if err == nil {
		t.Fatal("expected error: missing — / : separator")
	}
}

func TestParseConveneFacilitatorMustBeKnown(t *testing.T) {
	_, err := parseConveneCommand("!convene facilitator=ghost plumb anvil — x", known("plumb", "anvil"))
	if err == nil {
		t.Fatal("expected error: facilitator override must be a known aspect")
	}
}
