package cfgreconcile

import (
	"context"
	"log/slog"
	"strings"
)

// WakePolicyPrefix is the almanac namespace for per-aspect wake policy; each
// direct child is an aspect name whose value is the policy string.
const WakePolicyPrefix = "cwb/nexus/wake-policy/"

// validWakePolicies mirrors the broker's WakePolicy* constants (wake.go).
var validWakePolicies = map[string]bool{
	"always-on":       true,
	"wake-on-mention": true,
	"dispatch-only":   true,
}

// WakePolicySetter is the narrow surface on the wake controller. SetWakePolicy
// updates an aspect's live policy and reports whether it actually changed.
// Satisfied by *broker.Broker (delegates to the wake controller; nil-safe).
type WakePolicySetter interface {
	SetWakePolicy(aspect, policy string) (changed bool)
}

// WakePolicy reconciles per-aspect wake policy (INC-4b). almanac is truth; the
// wake controller's in-memory map is the live state it drives.
type WakePolicy struct {
	r      Reader
	setter WakePolicySetter
	log    *slog.Logger
}

// NewWakePolicy builds the wake-policy reconciler.
func NewWakePolicy(r Reader, setter WakePolicySetter, log *slog.Logger) *WakePolicy {
	return &WakePolicy{r: r, setter: setter, log: log}
}

func (*WakePolicy) Name() string { return "wake-policy" }

// ReconcileOnce applies each almanac wake-policy key to the wake controller,
// counting actual changes. An almanac list error aborts the pass (live policies
// kept). An invalid policy value is skipped+logged.
func (rc *WakePolicy) ReconcileOnce(ctx context.Context) (int, error) {
	snap, err := rc.r.Snapshot(ctx, WakePolicyPrefix)
	if err != nil {
		return 0, err
	}
	updated := 0
	for aspect, raw := range snap {
		policy := strings.TrimSpace(raw)
		if !validWakePolicies[policy] {
			rc.log.Warn("cfgreconcile: skip invalid wake policy", "aspect", aspect, "policy", policy)
			continue
		}
		if rc.setter.SetWakePolicy(aspect, policy) {
			rc.log.Info("cfgreconcile: wake-policy synced from almanac", "aspect", aspect, "policy", policy)
			updated++
		}
	}
	return updated, nil
}
