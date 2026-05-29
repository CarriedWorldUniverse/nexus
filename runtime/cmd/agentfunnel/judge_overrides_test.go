package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/runtime/brokercreds"
)

// NEX-293: providerBundleToEnv mirrors the broker-side
// credentials.Store.EnvForCredential mapping. Agentfunnel can't
// import the credentials package directly (broker-only), so the
// mapping is duplicated here; this test pins it against the same
// canonical shapes the broker uses so drift fails the build.
func TestProviderBundleToEnv(t *testing.T) {
	cases := []struct {
		name   string
		bundle brokercreds.ProviderBundle
		want   map[string]string
	}{
		{
			name: "anthropic with base URL",
			bundle: brokercreds.ProviderBundle{
				APIShape: "anthropic",
				Key:      "sk-deepseek-abc",
				BaseURL:  "https://api.deepseek.com/anthropic",
			},
			want: map[string]string{
				"ANTHROPIC_API_KEY":  "sk-deepseek-abc",
				"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
			},
		},
		{
			name: "anthropic without base URL (default endpoint)",
			bundle: brokercreds.ProviderBundle{
				APIShape: "anthropic",
				Key:      "sk-ant-abc",
			},
			want: map[string]string{
				"ANTHROPIC_API_KEY": "sk-ant-abc",
			},
		},
		{
			name: "openai with base URL",
			bundle: brokercreds.ProviderBundle{
				APIShape: "openai",
				Key:      "sk-openai-xyz",
				BaseURL:  "https://api.openai.com/v1",
			},
			want: map[string]string{
				"OPENAI_API_KEY":  "sk-openai-xyz",
				"OPENAI_BASE_URL": "https://api.openai.com/v1",
			},
		},
		{
			name: "unknown shape falls through to nil so caller inherits ambient env",
			bundle: brokercreds.ProviderBundle{
				APIShape: "weirdshape",
				Key:      "k",
			},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := providerBundleToEnv(c.bundle)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("providerBundleToEnv(%+v) = %v, want %v", c.bundle, got, c.want)
			}
		})
	}
}

// NEX-293: envKeyNames sorts so log output is stable across runs
// and never includes credential values. Sanity test pinning both
// invariants.
func TestEnvKeyNames(t *testing.T) {
	got := envKeyNames(map[string]string{
		"ANTHROPIC_API_KEY":  "sk-secret",
		"ANTHROPIC_BASE_URL": "https://example.com",
	})
	want := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envKeyNames = %v, want %v (sorted)", got, want)
	}
	for _, k := range got {
		if k == "sk-secret" || k == "https://example.com" {
			t.Errorf("envKeyNames must return keys only, not values; got %q", k)
		}
	}
	if got := envKeyNames(nil); got != nil {
		t.Errorf("nil env should produce nil; got %v", got)
	}
	if got := envKeyNames(map[string]string{}); got != nil {
		t.Errorf("empty env should produce nil; got %v", got)
	}
}

// NEX-302: agentfunnel reads MainTurnSampling from its aspect's
// aspect.json (autospawn convention: file lives at the aspect's home
// dir). Parses cleanly + populates funnel.MainTurnSampling.
func TestReadMainTurnSamplingFromAspectJSON_Populated(t *testing.T) {
	dir := t.TempDir()
	body := `{
		"name": "anvil",
		"provider": "claude-code",
		"main_turn_sampling": {
			"temperature":       0.7,
			"top_p":             0.95,
			"top_k":             40,
			"seed":              1234,
			"max_output_tokens": 2048,
			"stop_sequences":    ["</done>", "STOP"]
		}
	}`
	if err := writeTestFile(dir, "aspect.json", body); err != nil {
		t.Fatalf("write aspect.json: %v", err)
	}
	got := readMainTurnSamplingFromAspectJSON(dir, newTestLogger())
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", got.Temperature)
	}
	if got.TopP == nil || *got.TopP != 0.95 {
		t.Errorf("TopP = %v, want 0.95", got.TopP)
	}
	if got.TopK == nil || *got.TopK != 40 {
		t.Errorf("TopK = %v, want 40", got.TopK)
	}
	if got.Seed == nil || *got.Seed != 1234 {
		t.Errorf("Seed = %v, want 1234", got.Seed)
	}
	if got.MaxOutputTokens != 2048 {
		t.Errorf("MaxOutputTokens = %d, want 2048", got.MaxOutputTokens)
	}
	if len(got.StopSequences) != 2 || got.StopSequences[0] != "</done>" {
		t.Errorf("StopSequences = %v, want [</done> STOP]", got.StopSequences)
	}
}

// NEX-302 back-compat: aspect.json without a main_turn_sampling
// block returns zero MainTurnSampling — provider defaults preserved.
func TestReadMainTurnSamplingFromAspectJSON_AbsentBlock(t *testing.T) {
	dir := t.TempDir()
	body := `{"name": "anvil", "provider": "claude-code"}`
	if err := writeTestFile(dir, "aspect.json", body); err != nil {
		t.Fatal(err)
	}
	got := readMainTurnSamplingFromAspectJSON(dir, newTestLogger())
	if got.Temperature != nil || got.TopP != nil || got.TopK != nil ||
		got.Seed != nil || got.MaxOutputTokens != 0 || len(got.StopSequences) != 0 {
		t.Errorf("absent block should produce zero MainTurnSampling; got %+v", got)
	}
}

// NEX-302 back-compat: missing aspect.json file is the common case
// for many autospawn setups — return zero, log at debug, never error.
func TestReadMainTurnSamplingFromAspectJSON_MissingFile(t *testing.T) {
	dir := t.TempDir()
	// No aspect.json written.
	got := readMainTurnSamplingFromAspectJSON(dir, newTestLogger())
	if got.Temperature != nil || got.MaxOutputTokens != 0 || len(got.StopSequences) != 0 {
		t.Errorf("missing file should produce zero MainTurnSampling; got %+v", got)
	}
}

// NEX-302: malformed JSON is logged + returns zero (never crashes
// the aspect startup just because aspect.json got corrupted).
func TestReadMainTurnSamplingFromAspectJSON_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := writeTestFile(dir, "aspect.json", "{ this is not valid json"); err != nil {
		t.Fatal(err)
	}
	got := readMainTurnSamplingFromAspectJSON(dir, newTestLogger())
	if got.Temperature != nil || got.MaxOutputTokens != 0 || len(got.StopSequences) != 0 {
		t.Errorf("malformed JSON should produce zero MainTurnSampling; got %+v", got)
	}
}

// NEX (judge-for-all-providers): buildAgentFunnelFilter must build a
// cheap-judge for non-Claude providers too, defaulting the judge model
// to the aspect's own main model (no haiku tier exists off-Claude).
func TestBuildAgentFunnelFilter_NonClaudeGetsJudge(t *testing.T) {
	f := buildAgentFunnelFilter(nil, "openai", "", "", nil, "deepseek-chat", newTestLogger(), nil)
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("want funnel.HardRulesFilter, got %T", f)
	}
	cheap, ok := hard.Inner.(*funnel.CheapModelFilter)
	if !ok || cheap == nil {
		t.Fatalf("want non-nil *funnel.CheapModelFilter inner, got %T", hard.Inner)
	}
	if cheap.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", cheap.Provider)
	}
	if cheap.Model != "deepseek-chat" {
		t.Errorf("Model = %q, want deepseek-chat (mainModel fallback)", cheap.Model)
	}
}

// judgeModelOverride wins over both the haiku and mainModel defaults.
func TestBuildAgentFunnelFilter_OverrideWins(t *testing.T) {
	f := buildAgentFunnelFilter(nil, "openai", "", "x", nil, "deepseek-chat", newTestLogger(), nil)
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("want funnel.HardRulesFilter, got %T", f)
	}
	cheap, ok := hard.Inner.(*funnel.CheapModelFilter)
	if !ok || cheap == nil {
		t.Fatalf("want non-nil *funnel.CheapModelFilter inner, got %T", hard.Inner)
	}
	if cheap.Model != "x" {
		t.Errorf("Model = %q, want x (override wins)", cheap.Model)
	}
}

// Claude flavor with no override defaults the judge to the haiku tier,
// ignoring mainModel. NEX-369: on native claude-api the bare "haiku" is
// expanded to a full Anthropic model id (a bare "haiku" 404s the SDK),
// while claude-code keeps the CLI shorthand.
func TestBuildAgentFunnelFilter_ClaudeDefaultsHaiku(t *testing.T) {
	// Native claude-api → full id.
	f := buildAgentFunnelFilter(nil, "claude-api", "", "", nil, "whatever", newTestLogger(), nil)
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("want funnel.HardRulesFilter, got %T", f)
	}
	cheap, ok := hard.Inner.(*funnel.CheapModelFilter)
	if !ok || cheap == nil {
		t.Fatalf("want non-nil *funnel.CheapModelFilter inner, got %T", hard.Inner)
	}
	if cheap.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("claude-api Model = %q, want claude-haiku-4-5-20251001 (NEX-369 expanded)", cheap.Model)
	}

	// claude-code keeps the bare shorthand its CLI expects.
	f2 := buildAgentFunnelFilter(nil, "claude-code", "", "", nil, "whatever", newTestLogger(), nil)
	cheap2 := f2.(funnel.HardRulesFilter).Inner.(*funnel.CheapModelFilter)
	if cheap2.Model != "haiku" {
		t.Errorf("claude-code Model = %q, want haiku (CLI shorthand preserved)", cheap2.Model)
	}
}

// Misconfig: non-Claude with no override AND no mainModel can't resolve
// a judge model, so it falls to bare hard-rules (Inner nil) rather than
// silently always-posting.
func TestBuildAgentFunnelFilter_MisconfigFallsToHardRules(t *testing.T) {
	f := buildAgentFunnelFilter(nil, "openai", "", "", nil, "", newTestLogger(), nil)
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("want funnel.HardRulesFilter, got %T", f)
	}
	if hard.Inner != nil {
		t.Errorf("misconfig should produce bare HardRulesFilter; Inner = %#v, want nil", hard.Inner)
	}
}

// NEX-365 #3: a judge_provider override routes the cheap-judge to a
// DIFFERENT provider family than the aspect's primary. Here a claude-code
// aspect is judged by a native claude-api (DeepSeek Anthropic-shape)
// endpoint: the CheapModelFilter must run on claude-api with the
// operator's judge model, NOT the aspect's main claude-code provider.
func TestBuildAgentFunnelFilter_JudgeProviderOverride_CrossProvider(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-deepseek",
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
	}
	f := buildAgentFunnelFilter(nil, "claude-code", "claude-api", "deepseek-v4-flash", env, "whatever", newTestLogger(), nil)
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("want funnel.HardRulesFilter, got %T", f)
	}
	cheap, ok := hard.Inner.(*funnel.CheapModelFilter)
	if !ok || cheap == nil {
		t.Fatalf("want *funnel.CheapModelFilter inner, got %T", hard.Inner)
	}
	if cheap.Provider != "claude-api" {
		t.Errorf("judge Provider = %q, want claude-api (routed away from claude-code main)", cheap.Provider)
	}
	if cheap.Model != "deepseek-v4-flash" {
		t.Errorf("judge Model = %q, want deepseek-v4-flash", cheap.Model)
	}
	// The judge must have a harness built on the standalone provider, not
	// the (nil) main provider — proves we didn't reuse the aspect's.
	if cheap.Harness == nil {
		t.Fatal("judge harness nil — provider not built")
	}
}

// NEX-365 #3 (parity with the Frame): even WITHOUT an explicit
// judge_provider, a judge credential must redirect a native claude-api
// judge to its endpoint — the native SDK ignores per-turn ProviderEnv, so
// nativeJudgeProvider rebuilds the provider from the judge env. Without
// this the judge silently runs on the aspect's main endpoint.
func TestNativeJudgeProvider_AgentFunnel(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-deepseek",
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
	}
	// claude-api + judge env → rebuilt native provider (Name claude-api),
	// not the inherited nil.
	if got := nativeJudgeProvider(nil, "claude-api", env); got == nil || got.Name() != "claude-api" {
		t.Errorf("claude-api + env: got %v, want a rebuilt claude-api provider", got)
	}
	// claude-code passes through (subprocess + ProviderEnv owns it).
	if got := nativeJudgeProvider(nil, "claude-code", env); got != nil {
		t.Errorf("claude-code should pass through unchanged (got %v)", got)
	}
	// No env → passthrough.
	if got := nativeJudgeProvider(nil, "claude-api", nil); got != nil {
		t.Errorf("no env should pass through unchanged (got %v)", got)
	}
}

// buildAgentFunnelJudgeProvider pins native Anthropic creds from the
// judge env at construction (the native SDK can't be redirected per-turn
// via ProviderEnv). Unrecognised names return ok=false so the caller
// keeps the aspect's main provider.
func TestBuildAgentFunnelJudgeProvider(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-x",
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
	}
	if p, id, ok := buildAgentFunnelJudgeProvider("claude-api", env, newTestLogger()); !ok || p == nil || id != "claude-api" {
		t.Errorf("claude-api: p=%v id=%q ok=%v, want non-nil/claude-api/true", p, id, ok)
	}
	// "claude" alias resolves to the same native family.
	if _, id, ok := buildAgentFunnelJudgeProvider("claude", env, newTestLogger()); !ok || id != "claude-api" {
		t.Errorf("claude alias: id=%q ok=%v, want claude-api/true", id, ok)
	}
	// claude-code judge → subprocess family.
	if _, id, ok := buildAgentFunnelJudgeProvider("claude-code", nil, newTestLogger()); !ok || id != "claude-code" {
		t.Errorf("claude-code: id=%q ok=%v, want claude-code/true", id, ok)
	}
	// Unrecognised → ok=false, caller falls back to the main provider.
	if p, _, ok := buildAgentFunnelJudgeProvider("llama-local", env, newTestLogger()); ok || p != nil {
		t.Errorf("unrecognised judge_provider should not build; got p=%v ok=%v", p, ok)
	}
}

func writeTestFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
