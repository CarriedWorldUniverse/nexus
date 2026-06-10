// wake.go — the wake-on-mention controller (roundtable spec component 1).
//
// Napping aspects are registered-but-asleep: their Deployment is scaled
// to zero, their inbox accumulates in the ChatStore, and chat addressed
// to them wakes the pod. The controller owns exactly the scale-up half:
// HandleChatSend calls MaybeWake for every computed recipient without a
// live WS conn; if that aspect's policy is wake-on-mention, the
// Deployment is scaled 0→1. The triggering message needs no special
// handling — Lock 6 since_msg_id replay delivers it when the woken
// aspect registers.
//
// The scale-down half lives in the idle reaper; roster status flips to
// napping there, not here — wake just scales.

package broker

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Wake policies (Config.AspectWakePolicy values). An aspect with no
// policy entry has no wake behavior at all — exactly today's semantics.
const (
	// WakePolicyAlwaysOn marks aspects whose pod must never be scaled
	// to zero (keel; maren — agy keyring requires a live pod). Neither
	// the wake controller nor the idle reaper touches them.
	WakePolicyAlwaysOn = "always-on"
	// WakePolicyWakeOnMention is the napping shape: chat addressed to
	// the aspect scales its Deployment 0→1; the idle reaper scales it
	// back to zero when quiet.
	WakePolicyWakeOnMention = "wake-on-mention"
	// WakePolicyDispatchOnly aspects only exist while holding dispatch
	// work (the ticket-builder shape) — chat never wakes them.
	WakePolicyDispatchOnly = "dispatch-only"
)

// wakeDebounce is the window during which repeat mentions of the same
// aspect do not issue another scale-up — the pod is presumed to be
// scheduling/booting from the first wake.
const wakeDebounce = 60 * time.Second

// deploymentScaler is the slice of dispatch.K8s the controller needs.
// Narrowed to an interface so tests inject a fake.
type deploymentScaler interface {
	ScaleDeployment(ctx context.Context, name string, replicas int32) error
}

// wakeController scales napping wake-on-mention aspects back up when
// chat arrives for them. Nil-safe: a nil *wakeController (wake not
// configured) no-ops on MaybeWake, so callers don't gate.
type wakeController struct {
	scaler      deploymentScaler
	policies    map[string]string // aspect → wake policy; read-only after New
	deployments map[string]string // aspect → Deployment name; absent = aspect name
	log         *slog.Logger
	now         func() time.Time // injectable clock for the debounce tests

	mu       sync.Mutex
	lastWake map[string]time.Time // aspect → last successful scale-up issue
}

func newWakeController(scaler deploymentScaler, policies, deployments map[string]string, log *slog.Logger) *wakeController {
	if log == nil {
		log = slog.Default()
	}
	return &wakeController{
		scaler:      scaler,
		policies:    policies,
		deployments: deployments,
		log:         log,
		now:         time.Now,
		lastWake:    make(map[string]time.Time),
	}
}

// MaybeWake scales the aspect's Deployment to 1 if its policy is
// wake-on-mention and no wake is already in flight (debounced). Failure
// is logged, never propagated — the message is already persisted and
// replay delivers it on whatever register eventually happens.
func (w *wakeController) MaybeWake(ctx context.Context, aspect string) {
	if w == nil {
		return
	}
	if w.policies[aspect] != WakePolicyWakeOnMention {
		return
	}

	now := w.now()
	w.mu.Lock()
	if last, ok := w.lastWake[aspect]; ok && now.Sub(last) < wakeDebounce {
		w.mu.Unlock()
		return
	}
	// Stamp before scaling so a concurrent mention can't double-wake
	// while this call is on the wire.
	w.lastWake[aspect] = now
	w.mu.Unlock()

	deployment := w.deployments[aspect]
	if deployment == "" {
		deployment = aspect
	}
	if err := w.scaler.ScaleDeployment(ctx, deployment, 1); err != nil {
		// Disarm the debounce so the next mention retries — a failed
		// wake must not wedge the aspect unreachable for a full window.
		w.mu.Lock()
		delete(w.lastWake, aspect)
		w.mu.Unlock()
		w.log.Warn("wake: scale-up failed — message persisted, replay delivers on register",
			"aspect", aspect, "deployment", deployment, "err", err)
		return
	}
	w.log.Info("wake: scaled deployment for napping aspect", "aspect", aspect, "deployment", deployment)
}
