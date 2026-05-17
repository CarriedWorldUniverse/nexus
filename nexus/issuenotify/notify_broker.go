package issuenotify

import (
	"context"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
)

// BrokerNotifier implements ledger.Notifier via the broker's HandleChatSend.
// DMs are delivered via @mention routing; operator stream messages go to
// the designated topic thread.
type BrokerNotifier struct {
	Broker       *broker.Broker
	OperatorAddr string // e.g. "operator"
	StreamTopic  string // e.g. "issue-activity"
}

func (b *BrokerNotifier) NotifyAspect(ctx context.Context, aspect, message string) error {
	// Prepend @aspect so the broker's RecipientPolicy routes it.
	content := fmt.Sprintf("@%s %s", aspect, message)
	_, err := b.Broker.HandleChatSend(ctx, "nexus-issues", content, 0, "")
	return err
}

func (b *BrokerNotifier) NotifyOperatorStream(ctx context.Context, message string) error {
	_, err := b.Broker.HandleChatSend(ctx, "nexus-issues", message, 0, b.StreamTopic)
	return err
}
