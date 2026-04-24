package ollamalocal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/runtime/providers"
)

func TestNewDefaults(t *testing.T) {
	t.Setenv("OLLAMA_URL", "")
	t.Setenv("OLLAMA_EMBED_MODEL", "")

	p := New(Config{})
	if p.URL() != DefaultURL {
		t.Errorf("URL = %q, want %q", p.URL(), DefaultURL)
	}
	if p.model != DefaultEmbedModel {
		t.Errorf("model = %q, want %q", p.model, DefaultEmbedModel)
	}
	if p.dim != DefaultEmbedDim {
		t.Errorf("dim = %d, want %d", p.dim, DefaultEmbedDim)
	}
}

func TestNewEnvOverride(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://ollama.internal:9999")
	t.Setenv("OLLAMA_EMBED_MODEL", "custom-model")

	p := New(Config{})
	if p.URL() != "http://ollama.internal:9999" {
		t.Errorf("URL env override not applied: %q", p.URL())
	}
	if p.model != "custom-model" {
		t.Errorf("model env override not applied: %q", p.model)
	}
}

func TestNewConfigBeatsEnv(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://from-env:1111")
	t.Setenv("OLLAMA_EMBED_MODEL", "env-model")

	p := New(Config{URL: "http://from-config:2222", Model: "config-model"})
	if p.URL() != "http://from-config:2222" {
		t.Errorf("Config URL should win over env, got %q", p.URL())
	}
	if p.model != "config-model" {
		t.Errorf("Config model should win over env, got %q", p.model)
	}
}

func TestEmbedSuccess(t *testing.T) {
	wantVec := []float32{0.1, 0.2, 0.3, 0.4}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Model == "" || req.Prompt == "" {
			http.Error(w, "missing fields", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: wantVec})
	}))
	defer srv.Close()

	p := New(Config{URL: srv.URL})
	res, err := p.Embed(context.Background(), providers.EmbedRequest{Text: "broker restart sequence"})
	if err != nil {
		t.Fatalf("Embed err = %v", err)
	}
	if res.Dim != len(wantVec) {
		t.Errorf("Dim = %d, want %d", res.Dim, len(wantVec))
	}
	if len(res.Vector) != len(wantVec) {
		t.Fatalf("vector len = %d, want %d", len(res.Vector), len(wantVec))
	}
	for i := range wantVec {
		if res.Vector[i] != wantVec[i] {
			t.Errorf("vec[%d] = %v, want %v", i, res.Vector[i], wantVec[i])
		}
	}
	if res.Model != DefaultEmbedModel {
		t.Errorf("Model = %q, want %q", res.Model, DefaultEmbedModel)
	}
}

func TestEmbedEmptyText(t *testing.T) {
	p := New(Config{URL: "http://unused"})
	_, err := p.Embed(context.Background(), providers.EmbedRequest{Text: ""})
	if !errors.Is(err, providers.ErrProvider) {
		t.Errorf("empty-text err = %v, want ErrProvider", err)
	}
}

func TestEmbedRequestModelOverride(t *testing.T) {
	var seenModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenModel = req.Model
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{0.1}})
	}))
	defer srv.Close()

	p := New(Config{URL: srv.URL, Model: "adapter-default"})
	_, err := p.Embed(context.Background(), providers.EmbedRequest{
		Text:  "hi",
		Model: "per-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenModel != "per-request" {
		t.Errorf("server saw model = %q, want per-request override", seenModel)
	}
}

func TestEmbedUnreachable(t *testing.T) {
	p := New(Config{
		URL:     "http://127.0.0.1:1", // port 1 — nothing listens here
		Timeout: 500 * time.Millisecond,
	})
	_, err := p.Embed(context.Background(), providers.EmbedRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected error for unreachable Ollama")
	}
	if !errors.Is(err, providers.ErrProvider) {
		t.Errorf("err = %v, want wrapped ErrProvider", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("err message should name unreachable: %v", err)
	}
}

func TestEmbedNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := New(Config{URL: srv.URL})
	_, err := p.Embed(context.Background(), providers.EmbedRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected error for 500 from Ollama")
	}
	if !errors.Is(err, providers.ErrProvider) {
		t.Errorf("err = %v, want wrapped ErrProvider", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err should include status code: %v", err)
	}
}

func TestEmbedClientTimeout(t *testing.T) {
	// Server intentionally slow: 1s delay, client timeout 100ms.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{0.1}})
	}))
	defer srv.Close()

	p := New(Config{URL: srv.URL, Timeout: 100 * time.Millisecond})
	_, err := p.Embed(context.Background(), providers.EmbedRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, providers.ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout (http.Client.Timeout wraps DeadlineExceeded in *url.Error)", err)
	}
}

func TestEmbedCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{0.1}})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	p := New(Config{URL: srv.URL, Timeout: 10 * time.Second})
	_, err := p.Embed(ctx, providers.EmbedRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if !errors.Is(err, providers.ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

func TestEmbedEmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Embedding: []float32{}})
	}))
	defer srv.Close()

	p := New(Config{URL: srv.URL})
	_, err := p.Embed(context.Background(), providers.EmbedRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected error for empty embedding")
	}
	if !errors.Is(err, providers.ErrProvider) {
		t.Errorf("err = %v, want wrapped ErrProvider", err)
	}
}

func TestCapabilities(t *testing.T) {
	p := New(Config{})
	caps := p.Capabilities()
	if caps.Chat {
		t.Error("Chat should be false for pure-embedding adapter")
	}
	if !caps.Embeddings {
		t.Error("Embeddings should be true")
	}
	if caps.EmbeddingModel != DefaultEmbedModel {
		t.Errorf("EmbeddingModel = %q, want %q", caps.EmbeddingModel, DefaultEmbedModel)
	}
	if caps.EmbeddingDim != DefaultEmbedDim {
		t.Errorf("EmbeddingDim = %d, want %d", caps.EmbeddingDim, DefaultEmbedDim)
	}
	if caps.ToolUse || caps.Vision || caps.SupportsTriage {
		t.Error("embedding adapter should not advertise chat-side capabilities")
	}
}

func TestChatMethodsUnsupported(t *testing.T) {
	p := New(Config{})
	ctx := context.Background()

	if _, err := p.Invoke(ctx, providers.InvokeRequest{}); !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("Invoke err = %v, want ErrUnsupported", err)
	}
	if _, err := p.Stream(ctx, providers.InvokeRequest{}); !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("Stream err = %v, want ErrUnsupported", err)
	}
	if _, err := p.TokenCount(ctx, "", "x"); !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("TokenCount err = %v, want ErrUnsupported", err)
	}
	if _, err := p.Compact(ctx, nil, ""); !errors.Is(err, providers.ErrUnsupported) {
		t.Errorf("Compact err = %v, want ErrUnsupported", err)
	}
}

func TestTriageModelEmpty(t *testing.T) {
	p := New(Config{})
	if got := p.TriageModel(); got != "" {
		t.Errorf("TriageModel = %q, want empty", got)
	}
}

func TestModelsReturnsConfigured(t *testing.T) {
	p := New(Config{Model: "custom-embed"})
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Errorf("Models returned %d, want 1", len(models))
	}
	if models[0].ID != "custom-embed" {
		t.Errorf("Models[0].ID = %q, want custom-embed", models[0].ID)
	}
}

// TestEmbedAgainstRealOllama exercises the real Ollama instance when
// OLLAMA_LIVE is set. Enables the full path: real HTTP, real WASM
// embedding model, real 768-dim vector.
func TestEmbedAgainstRealOllama(t *testing.T) {
	if os.Getenv("OLLAMA_LIVE") == "" {
		t.Skip("OLLAMA_LIVE not set — skipping live integration test")
	}

	p := New(Config{})
	res, err := p.Embed(context.Background(), providers.EmbedRequest{
		Text: "broker restart sequence: stop aspects first, broker last",
	})
	if err != nil {
		t.Fatalf("live Embed err = %v", err)
	}
	if res.Dim != DefaultEmbedDim {
		t.Errorf("live Dim = %d, want %d (DefaultEmbedDim)", res.Dim, DefaultEmbedDim)
	}
	if len(res.Vector) != DefaultEmbedDim {
		t.Errorf("live vector len = %d, want %d", len(res.Vector), DefaultEmbedDim)
	}
	t.Logf("live embedding: dim=%d first=%v", res.Dim, res.Vector[:5])
}
