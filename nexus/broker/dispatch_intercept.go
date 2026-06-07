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
		return fmt.Errorf("broker: bad dispatch brief: %w", err)
	}
	if thread != "" {
		brief.Thread = thread
	}
	// A queued brief (agent busy) returns ("", nil) — not an error.
	_, err = b.runner.Submit(ctx, brief)
	return err
}
