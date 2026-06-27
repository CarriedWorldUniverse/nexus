// Package pbreconcile keeps the broker's aspect provider-bindings in sync with
// almanac — the configuration source of truth ("configure almanac, almanac
// configures everything"). almanac is authoritative; the broker's aspects
// store (sqld) is a local cache/fallback so the broker still boots and resolves
// bindings when almanac is unreachable.
//
// Flow: every interval, list almanac keys under cwb/nexus/provider-bindings/,
// parse {"provider","model"}, and write-through any change to the aspects store
// via SetProviderAndModel. The existing read path (Store.Get → .Provider/.Model
// in aspects.ResolveByName / Validate) then serves the almanac value with ZERO
// caller changes. A change made with
//
//	cw config set cwb/nexus/provider-bindings/<aspect> '{"provider":"openai","model":"ornith"}'
//
// takes effect within one interval — no redeploy, no raw-DB edit.
//
// Why write-through instead of a read-time overlay: it leaves the hot read path
// (Store.Get) untouched and makes sqld a true last-known cache — a cold boot
// with almanac down still resolves the most recent binding. Poll now; NEX-622
// (almanac Watch) will replace the ticker behind Run without touching callers.
package pbreconcile

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// Prefix is the almanac path namespace for per-aspect provider-bindings. Each
// direct child is an aspect name whose value is a binding JSON document.
const Prefix = "cwb/nexus/provider-bindings/"

// Reader is the read-only almanac view the reconciler needs: a flat snapshot of
// the binding namespace keyed by aspect name (value = raw JSON). Mirrors mason's
// snapshot semantics; faked in tests.
type Reader interface {
	Snapshot(ctx context.Context) (map[string]string, error)
}

// Store is the narrow surface on the aspects backend — operations, not
// representation (P2). Satisfied by *aspects.SQLStore.
type Store interface {
	Get(ctx context.Context, name string) (*aspects.Aspect, error)
	SetProviderAndModel(ctx context.Context, name, provider, model string) error
}

// supportedProviders mirrors broker.supportedProviders (admin_provider_binding.go).
// Duplicated rather than imported to keep this package free of any broker
// dependency; keep the two in sync when adding a backend. A binding naming an
// unsupported provider is skipped + logged so a typo never poisons the store.
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

// Reconciler write-throughs almanac provider-bindings into the aspects store.
type Reconciler struct {
	r     Reader
	store Store
	log   *slog.Logger
}

// New builds a Reconciler. log must be non-nil.
func New(r Reader, store Store, log *slog.Logger) *Reconciler {
	return &Reconciler{r: r, store: store, log: log}
}

// ReconcileOnce lists almanac bindings and write-throughs every change into the
// store. Returns the count of bindings actually updated.
//
// Failure policy: an almanac list error aborts the pass and is returned — the
// store keeps its last-known values (boots-standalone / survives almanac
// downtime). A per-aspect problem (malformed JSON, incomplete fields,
// unsupported provider, aspect absent from the store) is logged and skipped so
// one bad key neither zeroes a binding nor aborts the whole pass.
func (rc *Reconciler) ReconcileOnce(ctx context.Context) (int, error) {
	snap, err := rc.r.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	updated := 0
	for aspect, raw := range snap {
		var b binding
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			rc.log.Warn("pbreconcile: skip malformed binding", "aspect", aspect, "err", err)
			continue
		}
		if b.Provider == "" || b.Model == "" {
			rc.log.Warn("pbreconcile: skip incomplete binding (need provider+model)", "aspect", aspect)
			continue
		}
		if !supportedProviders[b.Provider] {
			rc.log.Warn("pbreconcile: skip unsupported provider", "aspect", aspect, "provider", b.Provider)
			continue
		}
		cur, err := rc.store.Get(ctx, aspect)
		if err != nil {
			// Unknown to the store (ErrNotFound) or a transient read error:
			// never create aspects from config — only update existing bindings.
			rc.log.Warn("pbreconcile: skip — aspect not resolvable in store", "aspect", aspect, "err", err)
			continue
		}
		if cur.Provider == b.Provider && cur.Model == b.Model {
			continue // already in sync
		}
		if err := rc.store.SetProviderAndModel(ctx, aspect, b.Provider, b.Model); err != nil {
			rc.log.Error("pbreconcile: write-through failed", "aspect", aspect, "err", err)
			continue
		}
		rc.log.Info("pbreconcile: provider-binding synced from almanac",
			"aspect", aspect, "provider", b.Provider, "model", b.Model,
			"was_provider", cur.Provider, "was_model", cur.Model)
		updated++
	}
	return updated, nil
}

// Run reconciles immediately, then every interval until ctx is cancelled.
func (rc *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if n, err := rc.ReconcileOnce(ctx); err != nil {
		rc.log.Warn("pbreconcile: initial pass failed (using store cache)", "err", err)
	} else if n > 0 {
		rc.log.Info("pbreconcile: initial sync", "updated", n)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := rc.ReconcileOnce(ctx); err != nil {
				rc.log.Warn("pbreconcile: pass failed (using store cache)", "err", err)
			}
		}
	}
}
