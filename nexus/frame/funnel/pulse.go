// Funnel-enforced status pulses. Per Lock 5 of the aspect-funnel
// architecture: long ops (compaction, large tool chains, provider
// retry/backoff) emit a chat-visible status pulse BEFORE starting,
// so silence-during-work is distinguishable from stuck/crashed.
//
// Two layers, both funnel-enforced (not aspect-author discretion —
// per anvil #9124, convention-driven status pulses break under
// pressure exactly when you need them most):
//
//  1. Lifecycle events (Lock 5, F1.2) — machine-readable telemetry
//     to dashboard. Fired regardless of duration. Already wired.
//
//  2. Chat status pulses (this file) — human-readable send_chat
//     fired BEFORE ops crossing a likely-noticeable threshold (default
//     10s, immediate for any compaction). Wraps the long-op call
//     site so the aspect can't skip the pulse.
//
// F1.3 lands the contract and compaction's integration. F1.4 wires
// the real send_chat transport via the comms tool surface; until
// then the default StatusPulser is a NoopPulser and the tests use
// recordingPulser. The funnel's compaction code calls Pulser.Fire
// before bridle.Summarize regardless of which sink is connected.

package funnel

import (
	"context"
	"time"
)

// PulseKind names the long-op category. Sinks may render differently
// per kind; new kinds are added by appending here.
type PulseKind string

const (
	// PulseKindCompact fires before the funnel's summarization turn.
	// Mandatory regardless of expected duration — compactions are
	// always operator-visible because the deliberation pauses while
	// the cheap-model summarize call runs.
	PulseKindCompact PulseKind = "compact"

	// PulseKindToolChain fires before a tool-call round projected to
	// run > threshold. Reserved — wired by F1.4 once tool-call timing
	// estimates are available.
	PulseKindToolChain PulseKind = "tool_chain"

	// PulseKindProviderRetry fires before a backoff/retry that the
	// provider signals will take > threshold. Reserved — bridle owns
	// retry; funnel surfaces once bridle exposes the hook.
	PulseKindProviderRetry PulseKind = "provider_retry"
)

// StatusPulse is what the funnel passes to a StatusPulser. Carries
// enough context for the sink to render a human-readable message
// without having to reach back into funnel state.
type StatusPulse struct {
	// Kind names what kind of long op is starting. Sinks may use
	// this to format a category-specific message.
	Kind PulseKind

	// AspectID identifies which Frame/aspect is doing the work, so
	// the chat surface can attribute the pulse correctly.
	AspectID string

	// Reason is a short human-readable description of what's about
	// to happen. Sinks render this verbatim into chat — it should
	// already be operator-readable. Examples:
	//   "compacting context (160k tokens, ~30s)"
	//   "reviewing 200 file diffs, may take a minute"
	Reason string

	// EstimatedDuration is the funnel's best guess at how long the
	// op will run. Sinks may include this in the rendered message;
	// dashboards consuming the same pulse can use it for progress
	// bars. Zero means "unknown — long enough to matter."
	EstimatedDuration time.Duration

	// TurnID joins to the Lock 5 lifecycle events for this turn so
	// telemetry can correlate the chat-visible pulse with the
	// machine-readable compact.start / turn.start events.
	TurnID string
}

// StatusPulser receives chat-visible pulses. The default in v1 is
// NoopPulser (until F1.4 wires the real send_chat path); production
// implementations buffer the pulse to the WS outbound queue or call
// the in-process Frame's chat router directly.
//
// Fire MUST be safe to call from the deliberation goroutine. Fire
// SHOULD be non-blocking — the funnel wraps it in a bounded timeout
// (pulseTimeout) to enforce that, but a sink that returns quickly
// keeps the deliberation tight.
//
// Fire is best-effort. If the chat post fails to land (network
// down, broker rejects, etc.), the funnel does NOT block the long
// op waiting for confirmation. The op proceeds; the lifecycle event
// from Lock 5 still fires regardless and gives the dashboard a
// fallback signal.
type StatusPulser interface {
	Fire(ctx context.Context, p StatusPulse)
}

// NoopPulser discards every pulse. Default until F1.4 wires the real
// transport. Tests should use recordingPulser instead.
type NoopPulser struct{}

// Fire drops the pulse on the floor.
func (NoopPulser) Fire(_ context.Context, _ StatusPulse) {}

// estimatedCompactDuration is the funnel's rough guess for a
// summarize turn. Anvil's compaction work in agent-network ticket
// #102 logged ~2m13s for production sessions; we use 2m as the
// hint rather than a more optimistic number because undershooting
// makes the operator wonder if it's stuck — exactly the failure
// mode Lock 5 exists to prevent.
const estimatedCompactDuration = 2 * time.Minute

// pulseTimeout caps how long the funnel waits for a Pulser. Status
// pulses are observability — they cannot hold up the long op they're
// announcing. Same shape as emit()'s bounded wait, slightly longer
// budget because the pulse may legitimately involve a network write.
const pulseTimeout = 250 * time.Millisecond

// pulse runs the configured StatusPulser with the funnel's safety
// guarantees: panic recovery (a broken pulse path can never break
// deliberation) and a bounded wall clock (a slow pulser can never
// stall the very long op it's announcing).
//
// The Fire call receives a pulse-scoped context with the
// pulseTimeout deadline applied — well-behaved sinks that respect
// ctx will exit on their own when the budget expires, even after
// pulse() has returned. The goroutine + select is the hard backstop
// for sinks that ignore context entirely.
func (f *Funnel) pulse(ctx context.Context, p StatusPulse) {
	if f.cfg.Pulser == nil {
		return
	}
	p.AspectID = f.cfg.AspectID

	pulseCtx, cancel := context.WithTimeout(ctx, pulseTimeout)
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				f.log.Warn("funnel: pulser panicked; suppressing",
					"kind", p.Kind, "panic", r)
			}
			close(done)
			cancel() // ensure pulseCtx is always released
		}()
		f.cfg.Pulser.Fire(pulseCtx, p)
	}()
	select {
	case <-done:
	case <-time.After(pulseTimeout):
		f.log.Warn("funnel: pulser slow; abandoning",
			"kind", p.Kind, "timeout", pulseTimeout)
		cancel() // signal the abandoned goroutine to wind down
	case <-ctx.Done():
		f.log.Warn("funnel: context cancelled during pulse", "kind", p.Kind)
		cancel()
	}
}
