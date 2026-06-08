package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// submitDispatch handles an intercepted !dispatch post. The post itself is
// stored by the caller (HandleChatSend) as the audit-thread root; this routes
// the parsed brief to the Runner, threading the worker's replies under that
// post via `thread`. The Runner runs the work as the named agent.
func (b *Broker) submitDispatch(ctx context.Context, from, content, thread string) error {
	if b.runner == nil {
		return errors.New("broker: no runner configured for dispatch")
	}
	brief, err := dispatch.ParseBrief([]byte(content))
	if err != nil {
		b.log.Warn("dispatch: brief parse failed", "err", err, "from", from)
		return fmt.Errorf("broker: bad dispatch brief: %w", err)
	}
	if thread != "" {
		brief.Thread = thread
	}
	b.log.Info("dispatch: submitting brief to runner",
		"agent", brief.Agent, "ticket", brief.Ticket, "repo", brief.Repo,
		"provider", brief.Provider, "thread", brief.Thread)
	// A queued brief (agent busy) returns ("", nil) — not an error.
	runID, err := b.runner.Submit(ctx, brief)
	b.log.Info("dispatch: runner.Submit returned",
		"agent", brief.Agent, "ticket", brief.Ticket, "run_id", runID, "err", err)
	return err
}
