package broker

import (
	"context"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func newJudgeCfgStore(t *testing.T) *credentials.Store {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := credentials.NewStore(db, []byte("test-session-signing-secret-32-bytes-padded"))
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}
	return s
}

// NEX-373: the validate endpoint resolves the EFFECTIVE judge config
// (network default here) + the judge credential's env, so the aspect builds
// its judge from the validate response rather than a startup WS fetch that
// raced the WS connect.
func TestResolveJudgeConfig_NetworkDefault(t *testing.T) {
	ctx := context.Background()
	s := newJudgeCfgStore(t)
	if err := s.Set(ctx, credentials.UpsertParams{
		Name: "deepseek-flash", Kind: credentials.KindProvider,
		Bundle:         map[string]any{"api_shape": "openai", "base_url": "https://api.deepseek.com/v1", "key": "sk-x"},
		AllowedAspects: []string{"*"}, Mode: credentials.ModeFetch,
	}); err != nil {
		t.Fatalf("seed cred: %v", err)
	}
	for col, val := range map[string]string{
		"judge_provider":   "openai",
		"judge_model":      "deepseek-v4-flash",
		"judge_credential": "deepseek-flash",
	} {
		if err := s.SetNetworkDefaultField(ctx, col, val); err != nil {
			t.Fatalf("set %s: %v", col, err)
		}
	}

	provider, model, env := resolveJudgeConfig(ctx, s, "anvil", nil)
	if provider != "openai" || model != "deepseek-v4-flash" {
		t.Errorf("provider=%q model=%q, want openai/deepseek-v4-flash", provider, model)
	}
	if env["OPENAI_API_KEY"] != "sk-x" || env["OPENAI_BASE_URL"] != "https://api.deepseek.com/v1" {
		t.Errorf("judge env not delivered: %v", env)
	}
}

// Mode-gate: a proxy-mode judge credential resolves provider+model but its
// env must NOT be handed to an out-of-process aspect (mirrors
// resolveProviderEnv — the key stays inside nexus).
func TestResolveJudgeConfig_ProxyModeEnvWithheld(t *testing.T) {
	ctx := context.Background()
	s := newJudgeCfgStore(t)
	if err := s.Set(ctx, credentials.UpsertParams{
		Name: "ds-proxy", Kind: credentials.KindProvider,
		Bundle:         map[string]any{"api_shape": "openai", "base_url": "https://api.deepseek.com/v1", "key": "sk-x"},
		AllowedAspects: []string{"*"}, Mode: credentials.ModeProxy,
	}); err != nil {
		t.Fatalf("seed cred: %v", err)
	}
	for col, val := range map[string]string{
		"judge_provider":   "openai",
		"judge_model":      "deepseek-v4-flash",
		"judge_credential": "ds-proxy",
	} {
		if err := s.SetNetworkDefaultField(ctx, col, val); err != nil {
			t.Fatalf("set %s: %v", col, err)
		}
	}

	provider, model, env := resolveJudgeConfig(ctx, s, "anvil", nil)
	if provider != "openai" || model != "deepseek-v4-flash" {
		t.Errorf("provider/model should still resolve: %q/%q", provider, model)
	}
	if env != nil {
		t.Errorf("proxy-mode judge cred env must NOT be delivered off-box; got %v", env)
	}
}

// No judge policy set → all empty (aspect's judge inherits its main provider).
func TestResolveJudgeConfig_Unset(t *testing.T) {
	ctx := context.Background()
	s := newJudgeCfgStore(t)
	provider, model, env := resolveJudgeConfig(ctx, s, "anvil", nil)
	if provider != "" || model != "" || env != nil {
		t.Errorf("unset: got provider=%q model=%q env=%v, want all empty", provider, model, env)
	}
}
