// wake.go — the wake-on-mention controller (roundtable spec component 1).
//
// Napping aspects are registered-but-asleep: their Deployment is scaled
// to zero, their inbox accumulates in the ChatStore, and chat addressed
// to them wakes the pod. The controller owns exactly the scale-up half:
// HandleChatSend calls MaybeWake for every computed recipient without a
// live WS conn; if that aspect's policy is wake-on-mention, the
// Deployment is scaled 0→1.
//
// Delivering the triggering message DOES need special handling. Lock 6
// replay is opt-in (NEX-131: RequestReplay && SinceMsgID>0), and a
// cold-started aspect's wsasp sendRegister sets neither — so a woken
// aspect would register with an empty inbox and the message that woke it
// would never arrive. To close that gap the controller records a
// pending-wake watermark (pendingWake[aspect] = triggering msg id) on
// every wake; on that aspect's next register the WS handler force-replays
// addressed messages at-or-after the watermark, regardless of the
// client's RequestReplay flag, then clears the watermark. This delivers
// the triggering message (and anything else addressed during the brief
// nap→wake gap) without changing the opt-in default for normal
// reconnects.
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
	// pendingWake maps a woken aspect → the chat msg id that woke it.
	// Set on wake (even on scale failure — the pod may already be coming
	// up), read+cleared on that aspect's next register, where it forces a
	// replay of addressed messages at-or-after the watermark so the
	// triggering message is actually delivered. Guarded by mu alongside
	// lastWake.
	pendingWake map[string]int64
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
		pendingWake: make(map[string]int64),
	}
}

// takePendingWake returns and clears the pending-wake watermark for
// aspect, reporting whether one was set. The register path calls this to
// decide whether to force a replay of the message(s) that woke the
// aspect. Read-and-clear under mu so a single register consumes the
// watermark exactly once.
func (w *wakeController) takePendingWake(aspect string) (int64, bool) {
	if w == nil {
		return 0, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	since, ok := w.pendingWake[aspect]
	if ok {
		delete(w.pendingWake, aspect)
	}
	return since, ok
}

// MaybeWake scales the aspect's Deployment to 1 if its policy is
// wake-on-mention and no wake is already in flight (debounced), and
// records a pending-wake watermark so the triggering message msgID is
// force-replayed to the aspect on its next register. The debounce stamp
// happens synchronously under the mutex so concurrent mentions can't
// double-wake; the scale call itself runs in a goroutine so the chat hot
// path (HandleChatSend) never blocks on the apiserver. Scale failure is
// logged, never propagated — the message is already persisted and the
// recorded watermark delivers it on whatever register eventually happens.
func (w *wakeController) MaybeWake(ctx context.Context, aspect string, msgID int64) {
	if w == nil {
		return
	}
	if w.policies[aspect] != WakePolicyWakeOnMention {
		return
	}

	now := w.now()
	w.mu.Lock()
	// Record the pending-wake watermark regardless of the debounce: a
	// later mention that lands while the pod is still booting must not be
	// skipped, but the watermark must stay at the OLDEST undelivered
	// message so the replay catches everything since the nap. AddressedSince
	// returns msg ids strictly greater than the cursor, so store msgID-1 to
	// make the triggering message itself replayable. First-write wins; once
	// a watermark is set it is only cleared by a register.
	if msgID > 0 {
		if _, pending := w.pendingWake[aspect]; !pending {
			w.pendingWake[aspect] = msgID - 1
		}
	}
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
	// ctx is the broker lifetime context (not per-request), so it is a
	// sound parent: in-flight scales die with the broker, and the
	// timeout bounds a single apiserver round-trip.
	go func() {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := w.scaler.ScaleDeployment(sctx, deployment, 1); err != nil {
			// Disarm the debounce so the next mention retries — a failed
			// wake must not wedge the aspect unreachable for a full
			// window. Compare-and-delete on our own stamp: if a newer
			// wake restamped while this call was on the wire, that stamp
			// must survive this failure.
			w.mu.Lock()
			if w.lastWake[aspect].Equal(now) {
				delete(w.lastWake, aspect)
			}
			w.mu.Unlock()
			w.log.Warn("wake: scale-up failed — message persisted, replay delivers on register",
				"aspect", aspect, "deployment", deployment, "err", err)
			return
		}
		w.log.Info("wake: scaled deployment for napping aspect", "aspect", aspect, "deployment", deployment)
	}()
}
