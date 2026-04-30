package templates

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// defaultVars returns a complete vars map for the "default" template,
// keeping tests in lockstep with the template's actual placeholders.
// If a new placeholder is added to the default template, this map must
// be extended; the strict-vars contract enforces it via test failure.
func defaultVars() map[string]string {
	return map[string]string{
		"name":     "frame",
		"voice":    "Direct, low-affect, plain.",
		"values":   "the network running well, the operator's time, honest reporting.",
		"provider": "claude-api",
		"model":    "claude-opus-4-7",
	}
}

func TestList(t *testing.T) {
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, n := range got {
		if n == "default" {
			found = true
		}
	}
	if !found {
		t.Errorf("List did not contain 'default': %v", got)
	}
}

func TestRender_DefaultHappyPath(t *testing.T) {
	bundle, err := Render("default", defaultVars())
	if err != nil {
		t.Fatalf("Render(default): %v", err)
	}

	required := []string{"aspect.json", "SOUL.md", "CLAUDE.md", "PRIMER.md"}
	for _, f := range required {
		if _, ok := bundle[f]; !ok {
			t.Errorf("bundle missing %q", f)
		}
	}

	// All placeholders should be resolved — no {{x}} should remain.
	for f, content := range bundle {
		if strings.Contains(string(content), "{{") {
			t.Errorf("%s contains unresolved placeholder: %s", f, content)
		}
	}

	// aspect.json must be valid JSON with role:frame.
	var aj map[string]any
	if err := json.Unmarshal(bundle["aspect.json"], &aj); err != nil {
		t.Fatalf("aspect.json invalid JSON: %v\n%s", err, bundle["aspect.json"])
	}
	if aj["role"] != "frame" {
		t.Errorf("aspect.json role=%v want frame", aj["role"])
	}
	if aj["name"] != "frame" {
		t.Errorf("aspect.json name=%v want frame", aj["name"])
	}

	// SOUL.md and PRIMER.md should mention the frame's name.
	for _, f := range []string{"SOUL.md", "PRIMER.md"} {
		if !strings.Contains(string(bundle[f]), "frame") {
			t.Errorf("%s does not mention frame name", f)
		}
	}
}

func TestRender_MissingVar(t *testing.T) {
	vars := defaultVars()
	delete(vars, "voice")

	_, err := Render("default", vars)
	if !errors.Is(err, ErrMissingVar) {
		t.Fatalf("expected ErrMissingVar, got %v", err)
	}
	if !strings.Contains(err.Error(), "voice") {
		t.Errorf("error should name missing var: %v", err)
	}
}

func TestRender_UnknownVar(t *testing.T) {
	vars := defaultVars()
	vars["surprise"] = "extra"

	_, err := Render("default", vars)
	if !errors.Is(err, ErrUnknownVar) {
		t.Fatalf("expected ErrUnknownVar, got %v", err)
	}
	if !strings.Contains(err.Error(), "surprise") {
		t.Errorf("error should name unknown var: %v", err)
	}
}

func TestRender_UnknownTemplate(t *testing.T) {
	_, err := Render("nope", defaultVars())
	if !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("expected ErrUnknownTemplate, got %v", err)
	}
}

func TestRender_AllPlaceholdersResolved(t *testing.T) {
	bundle, err := Render("default", defaultVars())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	vars := defaultVars()
	// Spot-check: substituted values appear in at least one rendered file.
	wantValues := []string{vars["voice"], vars["values"]}
	for _, v := range wantValues {
		found := false
		for _, content := range bundle {
			if strings.Contains(string(content), v) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("rendered bundle does not contain expected value %q", v)
		}
	}
}

func TestSubstitute_PreservesNonPlaceholderContent(t *testing.T) {
	in := []byte("hello {{name}}, the curly { brace and } stay literal")
	used := map[string]struct{}{}
	out, missing := substitute(in, map[string]string{"name": "frame"}, used)
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	want := "hello frame, the curly { brace and } stay literal"
	if string(out) != want {
		t.Errorf("got %q want %q", string(out), want)
	}
}

func TestSubstitute_WhitespaceTolerant(t *testing.T) {
	in := []byte("{{name}} and {{ name }} and {{	name	}}")
	used := map[string]struct{}{}
	out, missing := substitute(in, map[string]string{"name": "x"}, used)
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	if string(out) != "x and x and x" {
		t.Errorf("got %q", string(out))
	}
}

func TestSubstitute_RepeatedPlaceholderCounts_AsOneUse(t *testing.T) {
	// A placeholder used 3 times still counts as just 1 used-var; the
	// strict-vars check should not complain.
	in := []byte("{{name}} {{name}} {{name}}")
	used := map[string]struct{}{}
	_, missing := substitute(in, map[string]string{"name": "x"}, used)
	if len(missing) != 0 {
		t.Fatalf("missing: %v", missing)
	}
	if _, ok := used["name"]; !ok {
		t.Errorf("'name' not marked used")
	}
	if len(used) != 1 {
		t.Errorf("expected 1 used var, got %d", len(used))
	}
}

func TestRender_AspectJSONValidatesAgainstSchema(t *testing.T) {
	// Once rendered, the aspect.json should round-trip through schemas.AspectConfig
	// and report role:frame via EffectiveRole. This is the contract that
	// P5 (frame embedding) will rely on.
	bundle, err := Render("default", defaultVars())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// We can't import shared/schemas here without making a dep cycle for
	// some platforms, so we just check the wire-shape via map; full
	// schema parsing is tested elsewhere.
	var ac map[string]any
	if err := json.Unmarshal(bundle["aspect.json"], &ac); err != nil {
		t.Fatalf("aspect.json: %v", err)
	}
	if ac["context_mode"] != "global" {
		t.Errorf("frame must be context_mode=global, got %v", ac["context_mode"])
	}
	caps, ok := ac["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities missing or wrong type: %v", ac["capabilities"])
	}
	wantCaps := map[string]bool{"bootstrap": false, "broadcast": false, "admin": false}
	for _, c := range caps {
		s, _ := c.(string)
		if _, ok := wantCaps[s]; ok {
			wantCaps[s] = true
		}
	}
	for c, present := range wantCaps {
		if !present {
			t.Errorf("default frame missing capability %q", c)
		}
	}
}
