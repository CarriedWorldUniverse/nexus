package main

import (
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

// TestComposeSystemPrompt_RolePrompt is a table test of the role-at-spawn
// prompt overlay (M1 Unit 3): rolePrompt, when non-empty, is inserted
// ABOVE the (thin) personality — after central (org-wide base knowledge)
// but before aspect/personality (decoration). An empty rolePrompt (the
// default — no Role on the dispatched brief) must reproduce today's
// exact composeSystemPrompt output.
func TestComposeSystemPrompt_RolePrompt(t *testing.T) {
	tests := []struct {
		name       string
		res        *keyfile.ValidationResult
		rolePrompt string
		want       string
	}{
		{
			name:       "nil result returns empty regardless of role prompt",
			res:        nil,
			rolePrompt: "you are a builder",
			want:       "",
		},
		{
			name: "empty role prompt reproduces pre-role-at-spawn output",
			res: &keyfile.ValidationResult{
				CentralNexusMD: "central",
				Personality:    keyfile.PersonalityBundle{Composed: "composed personality"},
			},
			rolePrompt: "",
			want:       "central\n\n---\n\ncomposed personality",
		},
		{
			name: "role prompt inserted above personality, below central",
			res: &keyfile.ValidationResult{
				CentralNexusMD: "central",
				Personality:    keyfile.PersonalityBundle{Composed: "composed personality"},
			},
			rolePrompt: "you are a builder",
			want:       "central\n\n---\n\nyou are a builder\n\n---\n\ncomposed personality",
		},
		{
			name: "role prompt with no central and decomposed personality sections",
			res: &keyfile.ValidationResult{
				Personality: keyfile.PersonalityBundle{NexusMD: "nexus", SoulMD: "soul", PrimerMD: "primer"},
			},
			rolePrompt: "you are a reviewer",
			want:       "you are a reviewer\n\n---\n\nnexus\n\n---\n\nsoul\n\n---\n\nprimer",
		},
		{
			name:       "role prompt alone when every other section is empty",
			res:        &keyfile.ValidationResult{},
			rolePrompt: "you are a security-reviewer",
			want:       "you are a security-reviewer",
		},
		{
			name:       "everything empty still returns empty",
			res:        &keyfile.ValidationResult{},
			rolePrompt: "",
			want:       "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := composeSystemPrompt(tc.res, tc.rolePrompt)
			if got != tc.want {
				t.Errorf("composeSystemPrompt() = %q, want %q", got, tc.want)
			}
			if tc.rolePrompt != "" && tc.res != nil && !strings.Contains(got, tc.rolePrompt) {
				t.Errorf("composed prompt missing role prompt: %q", got)
			}
		})
	}
}
