// Token usage recording — Lock 4 attribution surface (operator
// #9254/#9258). Funnel calls UsageRecorder after each turn.end
// with the triggering chat msg_id and the bridle Usage; the
// concrete recorder lives in cmd/nexus startup wiring it to
// usage.SQLStore (kept external so the funnel doesn't import
// nexus/usage and create a circular dep through framecomms).

package funnel

import (
	"context"

	"github.com/CarriedWorldUniverse/bridle"
)

// UsageRecorder persists per-turn token attribution. Implementations
// MUST be safe to call from the deliberation goroutine.
//
// MsgID is the chat msg_id that triggered the deliberation cycle, or
// 0 for turns that ran without a chat trigger (compaction summarize,
// internal ops). TurnID is the funnel-local handle from Lock 5
// lifecycle events; AspectID is the funnel's configured AspectID.
//
// Errors are logged at the funnel layer but don't fail the
// deliberation — forensics can't block the chat path.
type UsageRecorder interface {
	Record(ctx context.Context, msgID int64, turnID, aspectID, model string, usage bridle.Usage) error
}

// NoopUsageRecorder is the default when no recorder is wired. Drops
// every record on the floor. Used in tests and as a safe fallback.
type NoopUsageRecorder struct{}

// Record drops the call.
func (NoopUsageRecorder) Record(_ context.Context, _ int64, _, _, _ string, _ bridle.Usage) error {
	return nil
}
