package orchestrator

import "testing"

func TestRoleBrainResolver(t *testing.T) {
	r := RoleBrainResolver{Brains: map[string]RoleBrain{
		"builder-complex": {Provider: "claude-code", Model: "claude-sonnet-4-6", Effort: "high"},
	}}

	rolePrompt, skills, policy, provider, model, effort := r.Resolve("builder-complex")
	if provider != "claude-code" || model != "claude-sonnet-4-6" || effort != "high" {
		t.Errorf("Resolve(builder-complex) provider/model/effort = %q/%q/%q, want claude-code/claude-sonnet-4-6/high", provider, model, effort)
	}
	if rolePrompt != "" || skills != nil || policy != nil {
		t.Errorf("RoleBrainResolver must never resolve prompt/skills/policy: got %q, %v, %v", rolePrompt, skills, policy)
	}

	_, _, _, provider, model, effort = r.Resolve("builder")
	if provider != "" || model != "" || effort != "" {
		t.Errorf("Resolve(builder) (no override configured) = %q/%q/%q, want empty/empty/empty", provider, model, effort)
	}

	// A zero-value RoleBrainResolver (nil Brains map) must not panic and
	// must resolve every role to no override.
	var zero RoleBrainResolver
	_, _, _, provider, model, effort = zero.Resolve("anything")
	if provider != "" || model != "" || effort != "" {
		t.Errorf("zero-value RoleBrainResolver.Resolve = %q/%q/%q, want empty/empty/empty", provider, model, effort)
	}
}
