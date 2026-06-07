package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
)

// submitDispatch handles an intercepted !dispatch message.
// The message is NOT persisted to ChatStore; it goes directly to the runner.
func (b *Broker) submitDispatch(ctx context.Context, from, content string) error {
	if b.runner == nil {
		return errors.New("broker: no runner configured for dispatch")
	}
	brief, err := dispatch.ParseBrief([]byte(content))
	if err != nil {
		return fmt.Errorf("broker: bad dispatch brief: %w", err)
	}
	_, err = b.runner.Submit(ctx, brief)
	return err
}
