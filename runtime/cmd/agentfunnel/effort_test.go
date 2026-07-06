package main

import (
	"testing"

	claudecodeprovider "github.com/CarriedWorldUniverse/bridle/provider/claudecode"
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

// TestEffortToCLIFlag covers the reasoning-EFFORT knob's claude-code CLI
// leg (2026-07-06): low/medium/high identity-map onto the claude CLI's
// own --effort argument (the CLI additionally accepts xhigh/max, but
// RoleBrain never emits them and the operator's guidance caps reasoning
// at high, so this package never needs to produce them).
func TestEffortToCLIFlag(t *testing.T) {
	cases := []struct {
		effort   string
		wantFlag string
		wantOK   bool
	}{
		{"low", "low", true},
		{"medium", "medium", true},
		{"high", "high", true},
		{"", "", false},
		{"xhigh", "", false}, // valid to the CLI, but not one RoleBrain emits
		{"max", "", false},
		{"ultra", "", false},
		{"LOW", "", false}, // case-sensitive; no normalization
	}
	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			flag, ok := effortToCLIFlag(tc.effort)
			if flag != tc.wantFlag || ok != tc.wantOK {
				t.Errorf("effortToCLIFlag(%q) = (%q, %v), want (%q, %v)", tc.effort, flag, ok, tc.wantFlag, tc.wantOK)
			}
		})
	}
}

// TestApplyEffortCLIArg covers applyEffortCLIArg: CW_EFFORT drives the
// claude-code provider's ExtraArgs with ["--effort", <level>] on a valid
// value; empty/invalid CW_EFFORT leaves ExtraArgs untouched (main.go's
// doc comment — CLI default effort applies).
func TestApplyEffortCLIArg(t *testing.T) {
	cases := []struct {
		name          string
		env           map[string]string
		wantExtraArgs []string
	}{
		{
			name:          "low effort",
			env:           map[string]string{"CW_EFFORT": "low"},
			wantExtraArgs: []string{"--effort", "low"},
		},
		{
			name:          "medium effort",
			env:           map[string]string{"CW_EFFORT": "medium"},
			wantExtraArgs: []string{"--effort", "medium"},
		},
		{
			name:          "high effort",
			env:           map[string]string{"CW_EFFORT": "high"},
			wantExtraArgs: []string{"--effort", "high"},
		},
		{
			name:          "no CW_EFFORT set — untouched",
			env:           map[string]string{},
			wantExtraArgs: nil,
		},
		{
			name:          "unrecognized effort value — warn+ignore",
			env:           map[string]string{"CW_EFFORT": "ultra"},
			wantExtraArgs: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := claudecodeprovider.New()
			applyEffortCLIArg(p, fakeGetenv(tc.env), testLogger())
			if len(p.ExtraArgs) != len(tc.wantExtraArgs) {
				t.Fatalf("ExtraArgs = %v, want %v", p.ExtraArgs, tc.wantExtraArgs)
			}
			for i := range tc.wantExtraArgs {
				if p.ExtraArgs[i] != tc.wantExtraArgs[i] {
					t.Errorf("ExtraArgs = %v, want %v", p.ExtraArgs, tc.wantExtraArgs)
				}
			}
		})
	}
}

// TestBuildProvider_EffortRouting covers the split briefed in main.go's
// buildProvider: the claude-code branch sets ExtraArgs from CW_EFFORT and
// leaves ThinkingBudgetTokens alone (that field isn't even part of the
// provider — it lives on funnel.MainTurnSampling, applied separately by
// applyEffortOverride); the claude-api branch is the inverse (unchanged
// #423 behaviour, verified here only for ExtraArgs' absence since
// claudeprovider has no such field to assert against directly).
func TestBuildProvider_EffortRouting(t *testing.T) {
	t.Setenv("CW_EFFORT", "high")
	t.Run("claude-code gets ExtraArgs", func(t *testing.T) {
		p, err := buildProvider("claude-code", "", testLogger())
		if err != nil {
			t.Fatalf("buildProvider: %v", err)
		}
		cp, ok := p.(*claudecodeprovider.Provider)
		if !ok {
			t.Fatalf("buildProvider(\"claude-code\") returned %T, want *claudecodeprovider.Provider", p)
		}
		want := []string{"--effort", "high"}
		if len(cp.ExtraArgs) != len(want) || cp.ExtraArgs[0] != want[0] || cp.ExtraArgs[1] != want[1] {
			t.Errorf("ExtraArgs = %v, want %v", cp.ExtraArgs, want)
		}
	})
	t.Run("claude-api does not gain ExtraArgs (no such field/effect)", func(t *testing.T) {
		p, err := buildProvider("claude-api", "", testLogger())
		if err != nil {
			t.Fatalf("buildProvider: %v", err)
		}
		if _, ok := p.(*claudecodeprovider.Provider); ok {
			t.Fatalf("buildProvider(\"claude-api\") unexpectedly returned a claudecode provider")
		}
	})
}
