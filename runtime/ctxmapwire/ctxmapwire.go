// Package ctxmapwire is the shared glue for attaching ctxmap working memory
// (bridle/ctxmap) to a runtime's bridle.Harness. Both the aspect runtime and
// agentfunnel use it, so the env config and engine lifecycle live in one place.
//
// The llama.cpp-backed extractor is behind the `ctxmap_llama` build tag; the
// default build compiles this package cgo-free and Build() is a warn-if-asked
// no-op. Everything is fail-open: any init error disables memory and the runtime
// proceeds exactly as before (the map is a cache, never load-bearing).
package ctxmapwire

import (
	"io"
	"os"
	"path/filepath"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// Config is resolved from the environment. Off unless CTXMAP_ENABLED is truthy.
type Config struct {
	Enabled   bool
	ModelsDir string
	Extract   string // Qwen3-1.7B extraction model
	Kind      string // Qwen3-4B judgment model
	Embed     string // nomic embedding model
	StorePath string // sqlite store (persistent, per-aspect)
	Session   string // session/project id
	Threads   int
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Resolve builds the config from the environment. Model paths default to
// <CTXMAP_MODELS>/<name> (CTXMAP_MODELS defaults to the /models PVC mount),
// each individually overridable; the store persists under the aspect home.
func Resolve(aspectHome, aspectName string) Config {
	enabled := false
	switch os.Getenv("CTXMAP_ENABLED") {
	case "1", "true", "yes", "on":
		enabled = true
	}
	models := envOr("CTXMAP_MODELS", "/models")
	threads := 8
	if n, ok := positiveInt(os.Getenv("CTXMAP_THREADS")); ok {
		threads = n
	}
	return Config{
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

func positiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, n > 0
}

// Handle is an attached-or-not ctxmap engine. AttachTo wires the shared engine
// to a harness — call once per harness the runtime builds (agentfunnel rebuilds
// its harness on a binding refresh; the engine survives, the turn counter with
// it). Close tears everything down at shutdown.
type Handle interface {
	AttachTo(h *bridle.Harness)
	io.Closer
}

// Nop is the disabled handle: attaches nothing, closes cleanly. It is the value
// runtimes hold before Build and in the default (non-llama) build.
func Nop() Handle { return nopHandle{} }

type nopHandle struct{}

func (nopHandle) AttachTo(*bridle.Harness) {}
func (nopHandle) Close() error            { return nil }
