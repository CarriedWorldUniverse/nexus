package main

import (
	"context"

	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
)

// brokerChatSender lets the broker-inline dispatch Runner post its status
// lines ("dispatch submitted/queued", "builder spawned", "completed") through
// the broker's own chat path — the in-process equivalent of the deleted
// dispatch-controller's wsasp send-chat. It posts as the "dispatch" system
// identity to the brief's thread topic.
//
// Status lines never start with "!dispatch", so HandleChatSend does not
// re-intercept them as dispatch commands. Satisfies dispatch.ChatSender, which
// dispatch.NewWsPoster wraps into the Runner's Poster.
type brokerChatSender struct{ b *broker.Broker }

func (s brokerChatSender) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
	return s.b.HandleChatSend(ctx, "dispatch", content, replyTo, topic)
}

// brokerAuditPoster posts THROUGH the broker's chat path AS a named
// sender, returning the stored msg id. The dispatch Runner uses it for
// spawn audit-thread roots (from=<parent>, NEX-571) — the same
// post-as-thread-root machinery !dispatch uses, originated broker-side.
// Spawn summaries never start with "!dispatch", so HandleChatSend does
// not re-intercept them. Satisfies dispatch.AuditPoster.
type brokerAuditPoster struct{ b *broker.Broker }

func (s brokerAuditPoster) PostFrom(ctx context.Context, from, content string, replyTo int64, topic string) (int64, error) {
	return s.b.HandleChatSend(ctx, from, content, replyTo, topic)
}
