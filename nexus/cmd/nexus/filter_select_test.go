package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// stubProvider implements bridle.Provider with no behavior — only used so
// buildOutputFilter has a non-nil provider to wrap when constructing
// CheapModelFilter. The cheap path isn't actually exercised in these
// tests; we're checking the type selected.
type stubProvider struct{}

func (stubProvider) Name() bridle.ProviderID { return "stub" }
func (stubProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{}
}
func (stubProvider) RunTurn(ctx context.Context, req bridle.ProviderRequest, sink bridle.EventSink) (bridle.ProviderResult, error) {
	return bridle.ProviderResult{}, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func cfgWith(filter, filterProvider, filterModel string) schemas.AspectConfig {
	cfg := schemas.AspectConfig{Name: "test", Filter: filter, FilterProvider: filterProvider}
	if filterModel != "" {
		cfg.FilterProviderConfig = map[string]any{"model": filterModel}
	}
	return cfg
}

func TestBuildOutputFilter_DefaultIsCheap(t *testing.T) {
	got := buildOutputFilter(cfgWith("", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	if _, ok := got.(funnel.HardRulesFilter); !ok {
		t.Fatalf("default: expected HardRulesFilter, got %T", got)
	}
}

func TestBuildOutputFilter_Cheap_InheritsFrameProvider(t *testing.T) {
	got := buildOutputFilter(cfgWith("cheap", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	hr, ok := got.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("cheap: expected HardRulesFilter wrapper, got %T", got)
	}
	cmf, ok := hr.Inner.(*funnel.CheapModelFilter)
	if !ok {
		t.Fatalf("cheap: expected Inner=CheapModelFilter, got %T", hr.Inner)
	}
	if cmf.Provider != "stub" {
		t.Errorf("cheap: expected inherited provider stub, got %q", cmf.Provider)
	}
	// stub is not Claude flavor → falls back to Frame model
	if cmf.Model != "stub-model" {
		t.Errorf("cheap: expected fallback to Frame model stub-model, got %q", cmf.Model)
	}
}

func TestBuildOutputFilter_Cheap_ClaudeFrameDefaultsToHaiku(t *testing.T) {
	// NEX-369: claude-code keeps the bare "haiku" CLI shorthand (a versioned
	// id makes the CLI run as a full agent, not a classifier — see #194);
	// native claude-api/claude expand to a full Anthropic model id because
	// a bare "haiku" 404s the SDK → the judge degrades + fails open.
	want := map[bridle.ProviderID]string{
		"claude-code": "haiku",
		"claudecode":  "haiku",
		"claude-api":  "claude-haiku-4-5-20251001",
		"claude":      "claude-haiku-4-5-20251001",
	}
	for id, exp := range want {
		got := buildOutputFilter(cfgWith("cheap", "", ""), stubProvider{}, id, "claude-opus-4-7", nil, "", nil, quietLogger())
		hr := got.(funnel.HardRulesFilter)
		cmf := hr.Inner.(*funnel.CheapModelFilter)
		if cmf.Model != exp {
			t.Errorf("Claude flavor %q default: expected %q, got %q", id, exp, cmf.Model)
		}
	}
}

func TestBuildOutputFilter_Cheap_OperatorOverridesProvider(t *testing.T) {
	got := buildOutputFilter(cfgWith("cheap", "claude-api", "claude-haiku-4-5"), stubProvider{}, "claude-code", "claude-opus-4-7", nil, "", nil, quietLogger())
	hr := got.(funnel.HardRulesFilter)
	cmf, ok := hr.Inner.(*funnel.CheapModelFilter)
	if !ok {
		t.Fatalf("expected CheapModelFilter, got %T", hr.Inner)
	}
	if cmf.Provider != "claude-api" {
		t.Errorf("expected override provider claude-api, got %q", cmf.Provider)
	}
	if cmf.Model != "claude-haiku-4-5" {
		t.Errorf("expected override model claude-haiku-4-5, got %q", cmf.Model)
	}
}

func TestBuildOutputFilter_Cheap_OverrideProviderWithoutModelFallsToHaiku(t *testing.T) {
	got := buildOutputFilter(cfgWith("cheap", "claude-api", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	hr := got.(funnel.HardRulesFilter)
	cmf := hr.Inner.(*funnel.CheapModelFilter)
	if cmf.Provider != "claude-api" {
		t.Errorf("expected provider claude-api, got %q", cmf.Provider)
	}
	// NEX-369: native claude-api judge expands the bare default to a full id.
	if cmf.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Claude override no model: expected claude-haiku-4-5-20251001, got %q", cmf.Model)
	}
}

func TestBuildOutputFilter_Cheap_UnknownOverrideProviderDowngrades(t *testing.T) {
	got := buildOutputFilter(cfgWith("cheap", "voodoo-llm", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	hr, ok := got.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("expected HardRulesFilter (downgrade), got %T", got)
	}
	if hr.Inner != nil {
		t.Errorf("expected Inner=nil after downgrade, got %T", hr.Inner)
	}
}

func TestBuildOutputFilter_Hard(t *testing.T) {
	got := buildOutputFilter(cfgWith("hard", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	hr, ok := got.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("hard: expected HardRulesFilter, got %T", got)
	}
	if hr.Inner != nil {
		t.Fatalf("hard: expected Inner=nil, got %T", hr.Inner)
	}
}

func TestBuildOutputFilter_Always(t *testing.T) {
	got := buildOutputFilter(cfgWith("always", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	if _, ok := got.(funnel.AlwaysPostFilter); !ok {
		t.Fatalf("always: expected AlwaysPostFilter, got %T", got)
	}
}

func TestBuildOutputFilter_Off(t *testing.T) {
	got := buildOutputFilter(cfgWith("off", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	if _, ok := got.(funnel.AlwaysPostFilter); !ok {
		t.Fatalf("off: expected AlwaysPostFilter, got %T", got)
	}
}

func TestBuildOutputFilter_UnknownFallsBackToCheap(t *testing.T) {
	got := buildOutputFilter(cfgWith("nonsense", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	if _, ok := got.(funnel.HardRulesFilter); !ok {
		t.Fatalf("unknown: expected fallback to HardRulesFilter, got %T", got)
	}
}

// NEX-365 #3: nativeJudgeProvider rebuilds a native Anthropic-shape
// judge provider from the resolved judge-credential env so cross-provider
// judging (a Claude aspect judged by a DeepSeek Anthropic-shape endpoint)
// actually targets the judge endpoint — the native SDK reads creds at
// CONSTRUCTION, not per-turn, so ProviderEnv alone can't redirect it.
func TestNativeJudgeProvider(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-deepseek",
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
	}

	// claude-api + judge env → a freshly-built native claude provider,
	// NOT the inherited (stub) one.
	got := nativeJudgeProvider(stubProvider{}, "claude-api", env)
	if got.Name() != "claude-api" {
		t.Errorf("claude-api + env: provider Name = %q, want claude-api (rebuilt)", got.Name())
	}
	if _, isStub := got.(stubProvider); isStub {
		t.Error("claude-api + env: judge still on the inherited stub provider; should be rebuilt with the judge creds")
	}

	// "claude" alias behaves the same.
	if got := nativeJudgeProvider(stubProvider{}, "claude", env); got.Name() != "claude-api" {
		t.Errorf("claude alias: Name = %q, want claude-api", got.Name())
	}

	// claude-code is the subprocess family — left to bareJudgeProvider +
	// ProviderEnv, so nativeJudgeProvider must pass it through untouched.
	if got := nativeJudgeProvider(stubProvider{}, "claude-code", env); got.Name() != "stub" {
		t.Errorf("claude-code: provider should pass through (Name=%q), bareJudgeProvider owns it", got.Name())
	}

	// No judge env → nothing to pin; inherit (passthrough), preserving
	// pre-NEX-365 behaviour.
	if got := nativeJudgeProvider(stubProvider{}, "claude-api", nil); got.Name() != "stub" {
		t.Errorf("claude-api + no env: should pass through (Name=%q)", got.Name())
	}

	// Non-Claude id → passthrough.
	if got := nativeJudgeProvider(stubProvider{}, "openai", env); got.Name() != "stub" {
		t.Errorf("openai: should pass through (Name=%q)", got.Name())
	}
}

func TestBuildOutputFilter_CaseInsensitive(t *testing.T) {
	got := buildOutputFilter(cfgWith("HARD", "", ""), stubProvider{}, "stub", "stub-model", nil, "", nil, quietLogger())
	hr, ok := got.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("HARD: expected HardRulesFilter, got %T", got)
	}
	if hr.Inner != nil {
		t.Fatalf("HARD: expected Inner=nil, got %T", hr.Inner)
	}
}
