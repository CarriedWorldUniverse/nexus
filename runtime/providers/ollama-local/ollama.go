// Package ollamalocal implements the embeddings half of the Nexus
// provider contract against a locally-hosted Ollama instance.
//
// Chat (Invoke/Stream) returns ErrUnsupported — this adapter is
// embeddings-only. Compact + TokenCount are also unsupported; those
// belong to whichever chat provider the aspect is bound to.
//
// See provider-adapter spec §9.4 and registration spec §2.8.
package ollamalocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
)

// ProviderName is the adapter identifier used in aspect.json and in
// Nexus config (e.g. `knowledge.embedding_provider: "ollama-local"`).
const ProviderName = "ollama-local"

// DefaultURL matches operator #7673 — the standard Docker-to-host
// address for the existing Ollama container. `localhost:11434` is the
// same endpoint when the broker runs on the same host without Docker
// networking indirection.
const DefaultURL = "http://localhost:11434"

// DefaultEmbedModel is locked to nomic-embed-text per operator #7676:
// technical-tuned, 768-dim, matches this KB's actual content (ops
// notes, architecture, incidents). Swapping requires re-embedding
// the whole corpus — see one-way-door note in provider-adapter spec.
const DefaultEmbedModel = "nomic-embed-text"

// DefaultEmbedDim is the vector length produced by DefaultEmbedModel.
const DefaultEmbedDim = 768

// DefaultTimeout caps each embed call. Ollama running locally is
// fast but model load on the first call after an idle period can
// take a few seconds.
const DefaultTimeout = 30 * time.Second

// Provider implements providers.Provider for Ollama's embeddings path.
type Provider struct {
	url    string
	model  string
	dim    int
	client *http.Client
}

// Config configures a Provider. Zero-valued fields resolve via:
// Config > env var > Default* constant. Specifically: URL falls back
// to $OLLAMA_URL, then DefaultURL. Model falls back to
// $OLLAMA_EMBED_MODEL, then DefaultEmbedModel. Dim and Timeout fall
// back directly to their defaults (no env).
type Config struct {
	URL     string
	Model   string
	Dim     int
	Timeout time.Duration
	Client  *http.Client
}

// New constructs a Provider. URL/model/dim from env if Config omits
// them (OLLAMA_URL, OLLAMA_EMBED_MODEL).
func New(cfg Config) *Provider {
	url := cfg.URL
	if url == "" {
		url = os.Getenv("OLLAMA_URL")
	}
	if url == "" {
		url = DefaultURL
	}
	model := cfg.Model
	if model == "" {
		model = os.Getenv("OLLAMA_EMBED_MODEL")
	}
	if model == "" {
		model = DefaultEmbedModel
	}
	dim := cfg.Dim
	if dim == 0 {
		dim = DefaultEmbedDim
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	return &Provider{url: url, model: model, dim: dim, client: client}
}

// -------------------------------------------------------------------
// Provider interface — chat methods return ErrUnsupported
// -------------------------------------------------------------------

func (p *Provider) Invoke(ctx context.Context, req providers.InvokeRequest) (providers.InvokeResult, error) {
	return providers.InvokeResult{}, providers.ErrUnsupported
}

func (p *Provider) Stream(ctx context.Context, req providers.InvokeRequest) (providers.StreamIterator, error) {
	return nil, providers.ErrUnsupported
}

func (p *Provider) TokenCount(ctx context.Context, model string, payload string) (int, error) {
	return 0, providers.ErrUnsupported
}

func (p *Provider) Compact(ctx context.Context, entries []providers.Entry, hint string) (providers.CompactionResult, error) {
	return providers.CompactionResult{}, providers.ErrUnsupported
}

// Embed calls Ollama's /api/embeddings endpoint.
func (p *Provider) Embed(ctx context.Context, req providers.EmbedRequest) (providers.EmbedResult, error) {
	if req.Text == "" {
		return providers.EmbedResult{}, fmt.Errorf("%w: empty text", providers.ErrProvider)
	}
	model := req.Model
	if model == "" {
		model = p.model
	}

	body, err := json.Marshal(embedRequest{Model: model, Prompt: req.Text})
	if err != nil {
		return providers.EmbedResult{}, fmt.Errorf("%w: marshal: %v", providers.ErrProvider, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return providers.EmbedResult{}, fmt.Errorf("%w: new request: %v", providers.ErrProvider, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// Timeout detection: ctx deadline / cancellation, and also
		// http.Client.Timeout which wraps context.DeadlineExceeded
		// inside *url.Error. `errors.Is` does not unwrap through
		// *url.Error automatically, so check Timeout() explicitly.
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			return providers.EmbedResult{}, fmt.Errorf("%w: %v", providers.ErrTimeout, err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return providers.EmbedResult{}, fmt.Errorf("%w: %v", providers.ErrTimeout, err)
		}
		// Connection refused / DNS / network-layer failures all fall
		// here — Ollama unreachable is the common case, surface it
		// clearly so ops can act.
		return providers.EmbedResult{}, fmt.Errorf("%w: Ollama unreachable at %s: %v",
			providers.ErrProvider, p.url, err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return providers.EmbedResult{}, fmt.Errorf("%w: read body: %v", providers.ErrProvider, err)
	}

	if resp.StatusCode != http.StatusOK {
		return providers.EmbedResult{}, fmt.Errorf("%w: ollama HTTP %d: %s",
			providers.ErrProvider, resp.StatusCode, truncate(string(rawBody), 200))
	}

	var parsed embedResponse
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return providers.EmbedResult{}, fmt.Errorf("%w: parse response: %v (body: %s)",
			providers.ErrProvider, err, truncate(string(rawBody), 200))
	}
	if len(parsed.Embedding) == 0 {
		return providers.EmbedResult{}, fmt.Errorf("%w: empty embedding in response", providers.ErrProvider)
	}

	return providers.EmbedResult{
		Vector: parsed.Embedding,
		Model:  model,
		Dim:    len(parsed.Embedding),
	}, nil
}

// Capabilities — pure-embeddings adapter. Chat=false deliberately.
func (p *Provider) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		Streaming:          false,
		ToolUse:            false,
		Vision:             false,
		LongContext:        false,
		InSessionModelSwap: false,
		ThinkingLevels:     nil,
		MaxContextTokens:   0,
		SupportsTriage:     false,

		Embeddings:     true,
		EmbeddingModel: p.model,
		EmbeddingDim:   p.dim,
		Chat:           false,
	}
}

// Models returns the single embedding model this adapter is bound to.
// Ollama's `/api/tags` would return every model installed locally;
// for Nexus's purposes we only care about the one configured here.
func (p *Provider) Models(ctx context.Context) ([]providers.Model, error) {
	return []providers.Model{{ID: p.model, DisplayName: p.model, MaxContextTokens: 0}}, nil
}

// TriageModel — embeddings adapters don't have triage models.
func (p *Provider) TriageModel() string { return "" }

// URL returns the configured Ollama endpoint. Useful for logging.
func (p *Provider) URL() string { return p.url }

// -------------------------------------------------------------------
// Wire types — Ollama's /api/embeddings
// -------------------------------------------------------------------

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
