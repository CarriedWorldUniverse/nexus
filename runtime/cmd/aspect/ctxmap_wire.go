package main

import (
	"os"
	"path/filepath"
)

// ctxmap working-memory wiring for the interactive aspect runtime.
//
// The engine lives for the whole process (one aspect = one long-running,
// multi-turn conversation = one persistent per-aspect store), which is the
// regime ctxmap's dialogue memory was validated in. It attaches to the single
// bridle.Harness via existing hook seams; the llama.cpp-backed extractor is
// behind the `ctxmap_llama` build tag, so the DEFAULT build carries none of the
// cgo weight and this whole feature is a no-op unless both (a) the binary is
// built with the tag and (b) CTXMAP_ENABLED is set. Fail-open throughout: any
// init error disables memory and the aspect runs exactly as before (the map is
// a cache, never load-bearing).

type ctxmapWireConfig struct {
	Enabled   bool
	ModelsDir string // directory holding the gguf models
	Extract   string // extraction model path (Qwen3-1.7B)
	Kind      string // judgment model path (Qwen3-4B)
	Embed     string // embedding model path (nomic)
	StorePath string // sqlite store path (persistent, per-aspect)
	Session   string // session/project id
	Threads   int
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveCtxmapConfig builds the wiring config from the environment. Off unless
// CTXMAP_ENABLED is truthy. Model paths default to <CTXMAP_MODELS>/<name>, with
// CTXMAP_MODELS defaulting to /models (the PVC mount); each model path is
// individually overridable. The store persists per-aspect under the aspect home.
func resolveCtxmapConfig(aspectHome, aspectName string) ctxmapWireConfig {
	enabled := false
	switch os.Getenv("CTXMAP_ENABLED") {
	case "1", "true", "yes", "on":
		enabled = true
	}
	models := envOr("CTXMAP_MODELS", "/models")
	threads := 8
	if t := os.Getenv("CTXMAP_THREADS"); t != "" {
		if n, err := parsePositiveInt(t); err == nil {
			threads = n
		}
	}
	return ctxmapWireConfig{
		Enabled:   enabled,
		ModelsDir: models,
		Extract:   envOr("CTXMAP_EXTRACT_MODEL", filepath.Join(models, "Qwen3-1.7B-Q8_0.gguf")),
		Kind:      envOr("CTXMAP_KIND_MODEL", filepath.Join(models, "Qwen3-4B-Q8_0.gguf")),
		Embed:     envOr("CTXMAP_EMBED_MODEL", filepath.Join(models, "nomic-embed-text-v1.5.Q8_0.gguf")),
		StorePath: envOr("CTXMAP_STORE", filepath.Join(aspectHome, "ctxmap.db")),
		Session:   aspectName,
		Threads:   threads,
	}
}

func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, os.ErrInvalid
	}
	return n, nil
}

// nopCloser is the disabled / stub attachment result.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }
