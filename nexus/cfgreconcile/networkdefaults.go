package cfgreconcile

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

// NetworkDefaultsPath is the single almanac key holding the network-wide
// judge/compact defaults document.
const NetworkDefaultsPath = "cwb/nexus/network-defaults"

// NetworkDefaultsStore is the narrow surface on the credentials backend.
// Satisfied by *credentials.Store.
type NetworkDefaultsStore interface {
	GetNetworkDefaults(ctx context.Context) (credentials.NetworkDefaults, error)
	SetNetworkDefaultField(ctx context.Context, column, value string) error
}

// networkDefaultsDoc is the almanac document shape. Pointer fields so an
// omitted key leaves that column untouched (mirrors the admin setter); an
// explicit "" clears it. Keys match the network_defaults columns.
type networkDefaultsDoc struct {
	JudgeModel        *string `json:"judge_model"`
	JudgeCredential   *string `json:"judge_credential"`
	JudgeProvider     *string `json:"judge_provider"`
	CompactModel      *string `json:"compact_model"`
	CompactCredential *string `json:"compact_credential"`
}

// NetworkDefaults reconciles the network-wide judge/compact defaults (INC-4b).
// almanac is truth; the credentials store (sqld singleton row) is the cache.
type NetworkDefaults struct {
	r     Reader
	store NetworkDefaultsStore
	log   *slog.Logger
}

// NewNetworkDefaults builds the network-defaults reconciler.
func NewNetworkDefaults(r Reader, store NetworkDefaultsStore, log *slog.Logger) *NetworkDefaults {
	return &NetworkDefaults{r: r, store: store, log: log}
}

func (*NetworkDefaults) Name() string { return "network-defaults" }

// ReconcileOnce write-throughs each changed field of the almanac doc into the
// store. No doc → no-op (store kept). An almanac read or store read error
// aborts the pass. A per-field write error (e.g. unknown credential) is
// skipped+logged so one bad field neither clobbers the others nor aborts.
func (rc *NetworkDefaults) ReconcileOnce(ctx context.Context) (int, error) {
	raw, ok, err := rc.r.Value(ctx, NetworkDefaultsPath)
	if err != nil {
		return 0, err
	}
	if !ok || raw == "" {
		return 0, nil // almanac doesn't manage it yet → leave the store as-is
	}
	var d networkDefaultsDoc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		rc.log.Warn("cfgreconcile: skip malformed network-defaults doc", "err", err)
		return 0, nil
	}
	cur, err := rc.store.GetNetworkDefaults(ctx)
	if err != nil {
		return 0, err
	}
	fields := []struct {
		col  string
		want *string
		have string
	}{
		{"judge_model", d.JudgeModel, cur.JudgeModel},
		{"judge_credential", d.JudgeCredential, cur.JudgeCredential},
		{"judge_provider", d.JudgeProvider, cur.JudgeProvider},
		{"compact_model", d.CompactModel, cur.CompactModel},
		{"compact_credential", d.CompactCredential, cur.CompactCredential},
	}
	updated := 0
	for _, f := range fields {
		if f.want == nil || *f.want == f.have {
			continue
		}
		if err := rc.store.SetNetworkDefaultField(ctx, f.col, *f.want); err != nil {
			rc.log.Warn("cfgreconcile: skip network-defaults field", "column", f.col, "err", err)
			continue
		}
		rc.log.Info("cfgreconcile: network-default synced from almanac", "column", f.col, "value", *f.want, "was", f.have)
		updated++
	}
	return updated, nil
}
