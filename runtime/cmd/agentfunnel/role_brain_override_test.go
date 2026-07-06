package main

import (
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

// fakeGetenv builds a getenv func over a plain map, for tests that don't
// want to touch the real process environment (t.Setenv works fine too, but
// a fake keeps this table-driven and parallel-safe).
func fakeGetenv(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

// TestApplyRoleBrainOverride covers the role-tier-brains (2026-07-06)
// precedence: CW_PROVIDER/CW_MODEL, when present, win over whatever the
// broker validate/resolve response resolved (the personality row's own
// binding) — see main.go's applyRoleBrainOverride call site.
func TestApplyRoleBrainOverride(t *testing.T) {
	cases := []struct {
		name         string
		resProvider  string
		resModel     string
		env          map[string]string
		wantProvider string
		wantModel    string
	}{
		{
			name:         "no override env — resolve response passes through unchanged (the fallback)",
			resProvider:  "openai",
			resModel:     "ornith",
			env:          map[string]string{},
			wantProvider: "openai",
			wantModel:    "ornith",
		},
		{
			name:         "both CW_PROVIDER and CW_MODEL set — both win",
			resProvider:  "openai",
			resModel:     "ornith",
			env:          map[string]string{"CW_PROVIDER": "claude-code", "CW_MODEL": "claude-sonnet-4-6"},
			wantProvider: "claude-code",
			wantModel:    "claude-sonnet-4-6",
		},
		{
			name:         "only CW_PROVIDER set — model falls back to the resolve response",
			resProvider:  "openai",
			resModel:     "ornith",
			env:          map[string]string{"CW_PROVIDER": "codex-cli"},
			wantProvider: "codex-cli",
			wantModel:    "ornith",
		},
		{
			name:         "only CW_MODEL set — provider falls back to the resolve response",
			resProvider:  "openai",
			resModel:     "ornith",
			env:          map[string]string{"CW_MODEL": "gpt-5"},
			wantProvider: "openai",
			wantModel:    "gpt-5",
		},
		{
			name:         "empty-string env values are treated as unset",
			resProvider:  "openai",
			resModel:     "ornith",
			env:          map[string]string{"CW_PROVIDER": "", "CW_MODEL": ""},
			wantProvider: "openai",
			wantModel:    "ornith",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &keyfile.ValidationResult{Provider: tc.resProvider, Model: tc.resModel}
			applyRoleBrainOverride(res, fakeGetenv(tc.env))
			if res.Provider != tc.wantProvider {
				t.Errorf("Provider = %q, want %q", res.Provider, tc.wantProvider)
			}
			if res.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", res.Model, tc.wantModel)
			}
		})
	}
}
