package framecomms

import (
	"context"

	"github.com/nexus-cw/nexus/nexus/frame/funnel"
)

// ChatPulser is the in-process funnel.StatusPulser that posts
// status pulses as actual chat messages via the Gateway. Replaces
// F1.3's NoopPulser default once the gateway is wired into the
// Frame's startup config.
//
// Per Lock 5 of the architecture, status pulses are operator-
// visible chat posts that announce long ops BEFORE they start, so
// silence-during-work is distinguishable from stuck/crashed.
//
// The Pulser is fire-and-forget — Fire returns whether the
// underlying SendChat succeeded or not; the funnel's pulse() helper
// already wraps the call in a 250ms timeout + panic recovery, so
// the pulser doesn't need its own safety wrapper.
type ChatPulser struct {
	Gateway *Gateway
}

// Fire posts the pulse's Reason as a chat message via the Gateway.
// On any error the pulser logs nothing and returns silently — the
// telemetry layer is best-effort, and the lifecycle event from
// Lock 5 will still fire regardless and give the dashboard a
// fallback signal.
func (p *ChatPulser) Fire(ctx context.Context, sp funnel.StatusPulse) {
	if p == nil || p.Gateway == nil {
		return
	}
	// SendChat error is intentionally swallowed: a pulse is best-
	// effort observability, not a critical path. If it fails, the
	// funnel's machine-readable lifecycle event still fires and the
	// dashboard's activity strip shows the long op anyway. Logging
	// here would be redundant — the gateway already logs storage
	// errors via its callers.
	_, _ = p.Gateway.SendChat(ctx, sp.Reason, 0, "")
}
