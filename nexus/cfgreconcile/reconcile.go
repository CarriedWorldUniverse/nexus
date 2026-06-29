// Package cfgreconcile keeps live broker configuration in sync with almanac —
// the configuration source of truth ("configure almanac, almanac configures
// everything"). almanac is authoritative; the broker's local stores (sqld
// aspects/credentials, the in-memory wake policy map) are caches/fallbacks so
// the broker still boots and serves when almanac is unreachable.
//
// Each config domain is a small DomainReconciler that, on a poll, reads its
// almanac keys and write-throughs any change into the broker's local state.
// The existing read paths then serve the almanac value with ZERO caller changes.
// A change made with `cw config set <path>` takes effect within one interval —
// no redeploy, no raw-DB edit. Domains today:
//
//	cwb/nexus/provider-bindings/<aspect>  → aspects store   (INC-4a)
//	cwb/nexus/network-defaults            → credentials store (INC-4b)
//	cwb/nexus/wake-policy/<aspect>        → wake controller  (INC-4b)
//
// Poll now; NEX-622 (almanac Watch) will replace the ticker behind RunAll
// without touching the reconcilers.
package cfgreconcile

import (
	"context"
	"log/slog"
	"time"
)

// Reader is the read-only almanac view the reconcilers need. Snapshot lists a
// prefix as a flat map keyed by the leaf segment (value = raw config value);
// Value fetches a single key (ok=false when absent). Faked in tests.
type Reader interface {
	Snapshot(ctx context.Context, prefix string) (map[string]string, error)
	Value(ctx context.Context, path string) (value string, ok bool, err error)
}

// DomainReconciler reconciles one config domain from almanac into the broker.
// ReconcileOnce returns the number of items actually changed this pass, or an
// error if the pass should be treated as failed (local state kept as-is).
type DomainReconciler interface {
	Name() string
	ReconcileOnce(ctx context.Context) (int, error)
}

// RunAll reconciles every domain immediately, then again every interval until
// ctx is cancelled. A domain whose pass errors is logged and skipped (its local
// state stays last-known — boots-standalone / survives almanac downtime); the
// other domains still reconcile.
func RunAll(ctx context.Context, interval time.Duration, log *slog.Logger, rs ...DomainReconciler) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	pass := func(initial bool) {
		for _, r := range rs {
			n, err := r.ReconcileOnce(ctx)
			switch {
			case err != nil:
				log.Warn("cfgreconcile: pass failed (using local cache)", "domain", r.Name(), "err", err)
			case n > 0:
				log.Info("cfgreconcile: synced from almanac", "domain", r.Name(), "updated", n, "initial", initial)
			}
		}
	}
	pass(true)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass(false)
		}
	}
}
