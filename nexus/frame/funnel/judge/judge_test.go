package judge

import (
	"context"
	"io"
	"log/slog"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
)

// stubProvider is a no-op bridle.Provider used to stand in for an aspect's
// primary provider; we only assert which provider/model the builder selects.
type stubProvider struct{ name bridle.ProviderID }

func (s stubProvider) Name() bridle.ProviderID { return s.name }
func (stubProvider) Capabilities() bridle.ProviderCapabilities {
	return bridle.ProviderCapabilities{}
}
func (stubProvider) RunTurn(context.Context, bridle.ProviderRequest, bridle.EventSink) (bridle.ProviderResult, error) {
	return bridle.ProviderResult{}, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func anthropicEnv() map[string]string {
	return map[string]string{
		"ANTHROPIC_API_KEY":  "sk-deepseek",
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
	}
}

func TestIsClaudeFlavor(t *testing.T) {
	for _, id := range []bridle.ProviderID{"claude-api", "claude-code", "claude", "claudecode"} {
		if !IsClaudeFlavor(id) {
			t.Errorf("IsClaudeFlavor(%q) = false, want true", id)
		}
	}
	for _, id := range []bridle.ProviderID{"openai", "stub", ""} {
		if IsClaudeFlavor(id) {
			t.Errorf("IsClaudeFlavor(%q) = true, want false", id)
		}
	}
}

func TestBuildProvider(t *testing.T) {
	// claude-api with judge env → native provider (Name claude-api).
	if p, id, ok := BuildProvider("claude-api", anthropicEnv(), quiet()); !ok || p == nil || id != "claude-api" {
		t.Errorf("claude-api: p=%v id=%q ok=%v, want non-nil/claude-api/true", p, id, ok)
	}
	// "claude" alias → same native family.
	if _, id, ok := BuildProvider("claude", anthropicEnv(), quiet()); !ok || id != "claude-api" {
		t.Errorf("claude alias: id=%q ok=%v, want claude-api/true", id, ok)
	}
	// claude-api with NO env still builds (ambient-env native provider).
	if p, _, ok := BuildProvider("claude-api", nil, quiet()); !ok || p == nil {
		t.Errorf("claude-api no env should still build; ok=%v p=%v", ok, p)
	}
	// claude-code → subprocess family.
	if _, id, ok := BuildProvider("claude-code", nil, quiet()); !ok || id != "claude-code" {
		t.Errorf("claude-code: id=%q ok=%v, want claude-code/true", id, ok)
	}
	// openai → openai family.
	if p, id, ok := BuildProvider("openai", map[string]string{"OPENAI_API_KEY": "k"}, quiet()); !ok || p == nil || id != "openai" {
		t.Errorf("openai: p=%v id=%q ok=%v, want non-nil/openai/true", p, id, ok)
	}
	// Unrecognised → ok=false.
	if p, _, ok := BuildProvider("llama-local", nil, quiet()); ok || p != nil {
		t.Errorf("unrecognised should not build; p=%v ok=%v", p, ok)
	}
}

func TestNativeJudgeProvider(t *testing.T) {
	// claude-api + env → rebuilt native provider, not the inherited stub.
	got := NativeJudgeProvider(stubProvider{name: "stub"}, "claude-api", anthropicEnv())
	if got.Name() != "claude-api" {
		t.Errorf("claude-api + env: Name = %q, want claude-api (rebuilt)", got.Name())
	}
	if _, isStub := got.(stubProvider); isStub {
		t.Error("claude-api + env: should be rebuilt with judge creds, not the inherited provider")
	}
	// "claude" alias behaves the same.
	if got := NativeJudgeProvider(stubProvider{name: "stub"}, "claude", anthropicEnv()); got.Name() != "claude-api" {
		t.Errorf("claude alias: Name = %q, want claude-api", got.Name())
	}
	// claude-code passes through (BareJudgeProvider + ProviderEnv owns it).
	if got := NativeJudgeProvider(stubProvider{name: "stub"}, "claude-code", anthropicEnv()); got.Name() != "stub" {
		t.Errorf("claude-code: should pass through (Name=%q)", got.Name())
	}
	// No env → passthrough.
	if got := NativeJudgeProvider(stubProvider{name: "stub"}, "claude-api", nil); got.Name() != "stub" {
		t.Errorf("claude-api + no env: should pass through (Name=%q)", got.Name())
	}
	// Non-Claude id → passthrough.
	if got := NativeJudgeProvider(stubProvider{name: "stub"}, "openai", anthropicEnv()); got.Name() != "stub" {
		t.Errorf("openai: should pass through (Name=%q)", got.Name())
	}
}

func TestBareJudgeProvider(t *testing.T) {
	// claude-code → fresh bare claudecode provider (Name claude-code), not
	// the inherited one.
	got := BareJudgeProvider(stubProvider{name: "stub"}, "claude-code")
	if got.Name() != "claude-code" {
		t.Errorf("claude-code: Name = %q, want claude-code (fresh bare provider)", got.Name())
	}
	if _, isStub := got.(stubProvider); isStub {
		t.Error("claude-code: should build a fresh bare provider, not reuse the inherited one")
	}
	// Non-claudecode → passthrough (Bare is a CLI-only knob).
	if got := BareJudgeProvider(stubProvider{name: "stub"}, "claude-api"); got.Name() != "stub" {
		t.Errorf("claude-api: should pass through (Name=%q)", got.Name())
	}
}

func cheapOf(t *testing.T, f funnel.OutputFilter) *funnel.CheapModelFilter {
	t.Helper()
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok {
		t.Fatalf("want funnel.HardRulesFilter, got %T", f)
	}
	cmf, ok := hard.Inner.(*funnel.CheapModelFilter)
	if !ok || cmf == nil {
		t.Fatalf("want *funnel.CheapModelFilter inner, got %T", hard.Inner)
	}
	return cmf
}

func TestBuildFilter_InheritsMainProvider(t *testing.T) {
	// Non-Claude main provider, no override → judge inherits it, model falls
	// back to MainModel (no haiku tier off-Claude).
	f := BuildFilter(Spec{
		Label: "test", MainProvider: stubProvider{name: "stub"}, MainProviderID: "stub",
		MainModel: "stub-model", Logger: quiet(),
	})
	cmf := cheapOf(t, f)
	if cmf.Provider != "stub" || cmf.Model != "stub-model" {
		t.Errorf("inherit: Provider=%q Model=%q, want stub/stub-model", cmf.Provider, cmf.Model)
	}
}

func TestBuildFilter_ClaudeInheritDefaultsHaiku(t *testing.T) {
	// claude-api main, no override → bare "haiku" expanded to a full id.
	f := BuildFilter(Spec{
		Label: "test", MainProvider: stubProvider{name: "claude-api"}, MainProviderID: "claude-api",
		MainModel: "claude-opus-4-8", Logger: quiet(),
	})
	if cmf := cheapOf(t, f); cmf.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("claude-api inherit: Model=%q, want claude-haiku-4-5-20251001 (expanded)", cmf.Model)
	}
	// claude-code keeps the bare CLI shorthand.
	f2 := BuildFilter(Spec{
		Label: "test", MainProvider: stubProvider{name: "claude-code"}, MainProviderID: "claude-code",
		MainModel: "x", Logger: quiet(),
	})
	if cmf := cheapOf(t, f2); cmf.Model != "haiku" {
		t.Errorf("claude-code inherit: Model=%q, want haiku (CLI shorthand)", cmf.Model)
	}
}

func TestBuildFilter_CrossProviderOverride(t *testing.T) {
	// claude-code aspect judged by a native claude-api (DeepSeek) endpoint.
	f := BuildFilter(Spec{
		Label: "test", MainProvider: stubProvider{name: "claude-code"}, MainProviderID: "claude-code",
		MainModel: "x", JudgeProviderName: "claude-api", JudgeModel: "deepseek-v4-flash",
		JudgeEnv: anthropicEnv(), Logger: quiet(),
	})
	cmf := cheapOf(t, f)
	if cmf.Provider != "claude-api" {
		t.Errorf("override Provider=%q, want claude-api (routed off claude-code)", cmf.Provider)
	}
	if cmf.Model != "deepseek-v4-flash" {
		t.Errorf("override Model=%q, want deepseek-v4-flash", cmf.Model)
	}
	if cmf.Harness == nil {
		t.Error("judge harness nil — provider not built")
	}
}

func TestBuildFilter_UnknownOverrideDowngrades(t *testing.T) {
	f := BuildFilter(Spec{
		Label: "test", MainProvider: stubProvider{name: "stub"}, MainProviderID: "stub",
		MainModel: "m", JudgeProviderName: "voodoo-llm", Logger: quiet(),
	})
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok || hard.Inner != nil {
		t.Errorf("unknown override should downgrade to bare HardRulesFilter; got %#v", f)
	}
}

func TestBuildFilter_MisconfigFallsToHardRules(t *testing.T) {
	// Non-Claude, no override, empty MainModel → no judge model resolvable →
	// bare hard rules (never silent always-post).
	f := BuildFilter(Spec{
		Label: "test", MainProvider: stubProvider{name: "stub"}, MainProviderID: "stub",
		MainModel: "", Logger: quiet(),
	})
	hard, ok := f.(funnel.HardRulesFilter)
	if !ok || hard.Inner != nil {
		t.Errorf("misconfig should be bare HardRulesFilter; got %#v", f)
	}
}

// --- BuildAcceptanceVerifier (Unit B — verified task_done, NET-22/23/24) ---
// Mirrors BuildFilter's resolution tests above: same Spec, same provider/
// model resolution, just a different constructed type (and nil rather than
// a hard-rules downgrade, since there's no rules-only fallback for a
// classifier that judges against caller-supplied criteria text).

func TestBuildAcceptanceVerifier_InheritsMainProvider(t *testing.T) {
	v := BuildAcceptanceVerifier(Spec{
		Label: "test", MainProvider: stubProvider{name: "stub"}, MainProviderID: "stub",
		MainModel: "stub-model", Logger: quiet(),
	})
	if v == nil {
		t.Fatal("expected a non-nil verifier")
	}
	if v.Provider != "stub" || v.Model != "stub-model" {
		t.Errorf("inherit: Provider=%q Model=%q, want stub/stub-model", v.Provider, v.Model)
	}
	if v.Harness == nil {
		t.Error("verifier harness nil — provider not built")
	}
}

func TestBuildAcceptanceVerifier_ClaudeInheritDefaultsHaiku(t *testing.T) {
	v := BuildAcceptanceVerifier(Spec{
		Label: "test", MainProvider: stubProvider{name: "claude-api"}, MainProviderID: "claude-api",
		MainModel: "claude-opus-4-8", Logger: quiet(),
	})
	if v == nil || v.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("claude-api inherit: verifier=%#v, want Model=claude-haiku-4-5-20251001 (expanded)", v)
	}
}

func TestBuildAcceptanceVerifier_UnknownOverrideReturnsNil(t *testing.T) {
	v := BuildAcceptanceVerifier(Spec{
		Label: "test", MainProvider: stubProvider{name: "stub"}, MainProviderID: "stub",
		MainModel: "m", JudgeProviderName: "voodoo-llm", Logger: quiet(),
	})
	if v != nil {
		t.Errorf("unknown override must return nil (caller fails open); got %#v", v)
	}
}

func TestBuildAcceptanceVerifier_MisconfigReturnsNil(t *testing.T) {
	v := BuildAcceptanceVerifier(Spec{
		Label: "test", MainProvider: stubProvider{name: "stub"}, MainProviderID: "stub",
		MainModel: "", Logger: quiet(),
	})
	if v != nil {
		t.Errorf("no resolvable judge model must return nil; got %#v", v)
	}
}
