package orchestrator

import "testing"

func TestRoleBrainResolver(t *testing.T) {
	r := RoleBrainResolver{Brains: map[string]RoleBrain{
		"builder-complex": {Provider: "claude-code", Model: "claude-sonnet-4-6"},
	}}

	rolePrompt, skills, policy, provider, model := r.Resolve("builder-complex")
	if provider != "claude-code" || model != "claude-sonnet-4-6" {
		t.Errorf("Resolve(builder-complex) provider/model = %q/%q, want claude-code/claude-sonnet-4-6", provider, model)
	}
	if rolePrompt != "" || skills != nil || policy != nil {
		t.Errorf("RoleBrainResolver must never resolve prompt/skills/policy: got %q, %v, %v", rolePrompt, skills, policy)
	}

	_, _, _, provider, model = r.Resolve("builder")
	if provider != "" || model != "" {
		t.Errorf("Resolve(builder) (no override configured) = %q/%q, want empty/empty", provider, model)
	}

	// A zero-value RoleBrainResolver (nil Brains map) must not panic and
	// must resolve every role to no override.
	var zero RoleBrainResolver
	_, _, _, provider, model = zero.Resolve("anything")
	if provider != "" || model != "" {
		t.Errorf("zero-value RoleBrainResolver.Resolve = %q/%q, want empty/empty", provider, model)
	}
}
