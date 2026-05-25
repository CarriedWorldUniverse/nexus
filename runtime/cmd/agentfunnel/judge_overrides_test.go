package main

import (
	"reflect"
	"testing"

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
