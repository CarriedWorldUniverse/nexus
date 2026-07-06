package main

import (
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// TestEffortToBudgetTokens covers the reasoning-EFFORT knob's
// low/medium/high -> budget_tokens table (2026-07-06).
func TestEffortToBudgetTokens(t *testing.T) {
	cases := []struct {
		effort     string
		wantBudget int
		wantOK     bool
	}{
		{"low", effortBudgetLow, true},
		{"medium", effortBudgetMedium, true},
		{"high", effortBudgetHigh, true},
		{"", 0, false},
		{"ultra", 0, false},
		{"LOW", 0, false}, // case-sensitive; no normalization
	}
	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			budget, ok := effortToBudgetTokens(tc.effort)
			if budget != tc.wantBudget || ok != tc.wantOK {
				t.Errorf("effortToBudgetTokens(%q) = (%d, %v), want (%d, %v)", tc.effort, budget, ok, tc.wantBudget, tc.wantOK)
			}
		})
	}
	// The table itself must stay Anthropic-valid (budget_tokens >= 1024).
	for _, b := range []int{effortBudgetLow, effortBudgetMedium, effortBudgetHigh} {
		if b < 1024 {
			t.Errorf("effort budget %d is below Anthropic's 1024 minimum", b)
		}
	}
}

// TestApplyEffortOverride covers applyEffortOverride's precedence: CW_EFFORT
// maps to ThinkingBudgetTokens ONLY on the claude-api provider path; every
// other provider is a logged no-op (main.go's doc comment); an unrecognized
// effort value is warned+ignored; unset CW_EFFORT leaves sampling untouched.
func TestApplyEffortOverride(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		env        map[string]string
		wantBudget int
	}{
		{
			name:       "claude-api + high effort — budget applied",
			provider:   "claude-api",
			env:        map[string]string{"CW_EFFORT": "high"},
			wantBudget: effortBudgetHigh,
		},
		{
			name:       "claude (alt provider string) + medium — budget applied",
			provider:   "claude",
			env:        map[string]string{"CW_EFFORT": "medium"},
			wantBudget: effortBudgetMedium,
		},
		{
			name:       "claude-code CLI — no-op regardless of effort",
			provider:   "claude-code",
			env:        map[string]string{"CW_EFFORT": "high"},
			wantBudget: 0,
		},
		{
			name:       "openai/Ornith — no-op regardless of effort",
			provider:   "openai",
			env:        map[string]string{"CW_EFFORT": "low"},
			wantBudget: 0,
		},
		{
			name:       "no CW_EFFORT set — untouched",
			provider:   "claude-api",
			env:        map[string]string{},
			wantBudget: 0,
		},
		{
			name:       "unrecognized effort value — warn+ignore",
			provider:   "claude-api",
			env:        map[string]string{"CW_EFFORT": "ultra"},
			wantBudget: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sampling := funnel.MainTurnSampling{}
			applyEffortOverride(&sampling, tc.provider, fakeGetenv(tc.env), testLogger())
			if sampling.ThinkingBudgetTokens != tc.wantBudget {
				t.Errorf("ThinkingBudgetTokens = %d, want %d", sampling.ThinkingBudgetTokens, tc.wantBudget)
			}
		})
	}
}
