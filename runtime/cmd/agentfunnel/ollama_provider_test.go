package main

import (
	"strings"
	"testing"
	"time"
)

// mapGetenv returns a getenv func backed by a plain map, so the parse
// paths in ollamaFromEnv are exercised without touching process env.
func mapGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestOllamaFromEnv_AllSet(t *testing.T) {
	p, err := ollamaFromEnv(mapGetenv(map[string]string{
		"OLLAMA_BASE_URL":   "http://dmon:11434",
		"OLLAMA_KEEP_ALIVE": "45m",
		"OLLAMA_NUM_CTX":    "16384",
	}))
	if err != nil {
		t.Fatalf("ollamaFromEnv: %v", err)
	}
	if p.KeepAlive != 45*time.Minute {
		t.Errorf("KeepAlive = %v; want 45m", p.KeepAlive)
	}
	if p.NumCtx != 16384 {
		t.Errorf("NumCtx = %d; want 16384", p.NumCtx)
	}
}

func TestOllamaFromEnv_NegativeKeepAlivePassesThrough(t *testing.T) {
	// "-1s" is ollama's "keep the model loaded forever" — it must reach
	// the provider as a negative duration, not be clamped or rejected.
	p, err := ollamaFromEnv(mapGetenv(map[string]string{
		"OLLAMA_KEEP_ALIVE": "-1s",
	}))
	if err != nil {
		t.Fatalf("ollamaFromEnv: %v", err)
	}
	if p.KeepAlive != -time.Second {
		t.Errorf("KeepAlive = %v; want -1s", p.KeepAlive)
	}
}

func TestOllamaFromEnv_EmptyEnvLeavesBridleDefaults(t *testing.T) {
	// Zero KeepAlive/NumCtx mean "bridle's defaults apply" (30m
	// keep_alive, model-default num_ctx) — the funnel must not invent
	// its own values on top.
	p, err := ollamaFromEnv(mapGetenv(nil))
	if err != nil {
		t.Fatalf("ollamaFromEnv: %v", err)
	}
	if p.KeepAlive != 0 {
		t.Errorf("KeepAlive = %v; want 0 (bridle default)", p.KeepAlive)
	}
	if p.NumCtx != 0 {
		t.Errorf("NumCtx = %d; want 0 (model default)", p.NumCtx)
	}
}

func TestOllamaFromEnv_MalformedEnvFailsLoudly(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"keep_alive not a duration", map[string]string{"OLLAMA_KEEP_ALIVE": "30 minutes"}, "OLLAMA_KEEP_ALIVE"},
		{"keep_alive bare number", map[string]string{"OLLAMA_KEEP_ALIVE": "1800"}, "OLLAMA_KEEP_ALIVE"},
		{"num_ctx not an int", map[string]string{"OLLAMA_NUM_CTX": "16k"}, "OLLAMA_NUM_CTX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ollamaFromEnv(mapGetenv(tc.env))
			if err == nil {
				t.Fatalf("ollamaFromEnv: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func TestBuildProvider_OllamaIDs(t *testing.T) {
	// Both the short ID and bridle's ProviderID string route to the
	// native lane. Pin the env empty so a developer machine's ollama
	// config can't leak into the assertion.
	t.Setenv("OLLAMA_KEEP_ALIVE", "")
	t.Setenv("OLLAMA_NUM_CTX", "")
	for _, id := range []string{"ollama", "ollama-local"} {
		p, err := buildProvider(id, "")
		if err != nil {
			t.Errorf("buildProvider(%q): %v", id, err)
			continue
		}
		if p == nil {
			t.Errorf("buildProvider(%q): nil provider", id)
		}
	}
}

func TestBuildProvider_OllamaMalformedEnvFails(t *testing.T) {
	t.Setenv("OLLAMA_KEEP_ALIVE", "not-a-duration")
	if _, err := buildProvider("ollama", ""); err == nil {
		t.Fatal("buildProvider with malformed OLLAMA_KEEP_ALIVE: want error, got nil")
	}
}
