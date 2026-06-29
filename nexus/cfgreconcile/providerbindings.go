package cfgreconcile

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// ProviderBindingPrefix is the almanac namespace for per-aspect provider
// bindings; each direct child is an aspect name whose value is a binding JSON.
const ProviderBindingPrefix = "cwb/nexus/provider-bindings/"

// AspectStore is the narrow surface on the aspects backend (operations, not
// representation). Satisfied by *aspects.SQLStore.
type AspectStore interface {
	Get(ctx context.Context, name string) (*aspects.Aspect, error)
	SetProviderAndModel(ctx context.Context, name, provider, model string) error
}

// supportedProviders mirrors broker.supportedProviders (admin_provider_binding.go).
// Duplicated to keep this package broker-dependency-free; keep in sync when
// adding a backend. A binding naming an unsupported provider is skipped+logged.
var supportedProviders = map[string]bool{
	"claude": true, "claude-api": true, "claudecode": true, "claude-code": true,
	"openai": true, "ollama": true, "ollama-local": true,
	"codex": true, "codex-cli": true, "codexcli": true,
	"antigravity-cli": true, "antigravity": true, "agy": true,
}

type binding struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ProviderBindings reconciles aspect provider-bindings (INC-4a). almanac is
// truth; the aspects store (sqld) is the boots-standalone cache.
type ProviderBindings struct {
	r     Reader
	store AspectStore
	log   *slog.Logger
}

// NewProviderBindings builds the provider-binding reconciler.
func NewProviderBindings(r Reader, store AspectStore, log *slog.Logger) *ProviderBindings {
	return &ProviderBindings{r: r, store: store, log: log}
}

func (*ProviderBindings) Name() string { return "provider-bindings" }

// ReconcileOnce write-throughs every changed binding into the aspects store.
// An almanac list error aborts the pass (store keeps last-known). A per-aspect
// problem (malformed/incomplete/unsupported/unknown-aspect) is skipped+logged
// so one bad key never zeroes a binding or aborts the pass.
func (rc *ProviderBindings) ReconcileOnce(ctx context.Context) (int, error) {
	snap, err := rc.r.Snapshot(ctx, ProviderBindingPrefix)
	if err != nil {
		return 0, err
	}
	updated := 0
	for aspect, raw := range snap {
		var b binding
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			rc.log.Warn("cfgreconcile: skip malformed provider-binding", "aspect", aspect, "err", err)
			continue
		}
		if b.Provider == "" || b.Model == "" {
			rc.log.Warn("cfgreconcile: skip incomplete provider-binding (need provider+model)", "aspect", aspect)
			continue
		}
		if !supportedProviders[b.Provider] {
			rc.log.Warn("cfgreconcile: skip unsupported provider", "aspect", aspect, "provider", b.Provider)
			continue
		}
		cur, err := rc.store.Get(ctx, aspect)
		if err != nil {
			rc.log.Warn("cfgreconcile: skip — aspect not resolvable in store", "aspect", aspect, "err", err)
			continue
		}
		if cur.Provider == b.Provider && cur.Model == b.Model {
			continue
		}
		if err := rc.store.SetProviderAndModel(ctx, aspect, b.Provider, b.Model); err != nil {
			rc.log.Error("cfgreconcile: provider-binding write-through failed", "aspect", aspect, "err", err)
			continue
		}
		rc.log.Info("cfgreconcile: provider-binding synced from almanac",
			"aspect", aspect, "provider", b.Provider, "model", b.Model,
			"was_provider", cur.Provider, "was_model", cur.Model)
		updated++
	}
	return updated, nil
}
