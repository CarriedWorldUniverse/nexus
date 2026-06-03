package funnel

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

// NEX-365 / #202 regression: every comms tool advertised by CommsToolDefs
// MUST route to the CommsRunner — it must never fall through to the next
// (local) runner, which is what produced `unknown tool "triage"` when the
// router's hand-maintained switch drifted from the defs. The router now
// derives its set from CommsToolDefs, so this can't silently drift again.
func TestComposedRunner_RoutesAllCommsTools(t *testing.T) {
	reachedNext := false
	next := stubToolRunner(func(_ context.Context, _ bridle.ToolCall) (json.RawMessage, error) {
		reachedNext = true
		return json.RawMessage(`{}`), nil
	})
	// CommsRunner has a nil gateway — comms.Run will error, but we only care
	// WHERE the call routed, not whether the work succeeded.
	r := ComposeRunner(CommsRunner{}, next)

	for _, d := range CommsToolDefs() {
		reachedNext = false
		_, _ = r.Run(context.Background(), bridle.ToolCall{Name: d.Name})
		if reachedNext {
			t.Errorf("comms tool %q fell through to the next runner — router/defs drift (the #202 class)", d.Name)
		}
	}

	// Legacy aliases (handled but not advertised) must still route to comms.
	for _, alias := range commsToolAliases {
		reachedNext = false
		_, _ = r.Run(context.Background(), bridle.ToolCall{Name: alias})
		if reachedNext {
			t.Errorf("comms alias %q fell through to the next runner", alias)
		}
	}

	// Sanity: a genuinely non-comms tool DOES fall through (routing isn't a
	// catch-all that would swallow local tools).
	reachedNext = false
	_, _ = r.Run(context.Background(), bridle.ToolCall{Name: "definitely_not_a_comms_tool"})
	if !reachedNext {
		t.Error("a non-comms tool should fall through to the next runner")
	}
}
