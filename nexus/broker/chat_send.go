// chat_send.go — the canonical chat.send code path.
//
// Every origin of a chat message lands in HandleChatSend:
//
//   - Out-of-process aspect WS:  handleChatSendFrame (ws.go) → HandleChatSend
//   - Operator browser WS:        handleChatSendFrame (same path) → HandleChatSend
//
// Every chat.send results in:
//   1. INSERT chat_messages row (ChatStore.Insert)
//   2. Compute recipients per Lock 2 (RecipientPolicy.Compute)
//   3. Fan out chat.deliver frames to each recipient's live WS
//   4. Emit chat observability frames
//
// Without this single path:
//   - Operator/aspect chat.send didn't persist (FK errors on usage.Record)
//   - Live chat.deliver fan-out was missing entirely (only replay-on-register
//     delivered messages)

package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

// HandleChatSend persists an inbound chat message and fans it out to
// recipients. Returns the assigned chat msg_id so callers can reference
// it for reply chains, usage attribution, etc.
//
// Errors propagate to the caller. The WS shim treats them as warn-and-
// drop because chat.send is fire-and-forget per transport spec.
func (b *Broker) HandleChatSend(ctx context.Context, from, content string, replyTo int64, topic string) (int64, error) {
	if b.cfg.ChatStore == nil {
		return 0, errors.New("broker.HandleChatSend: ChatStore not configured")
	}
	if from == "" {
		return 0, errors.New("broker.HandleChatSend: from required")
	}

	// 1. Persist. ChatStore.Insert mints id + server timestamp; both
	// flow into the chat.deliver frames so recipients can age-check
	// (Lock 6 received_at semantics).
	msg, err := b.cfg.ChatStore.Insert(ctx, from, content, replyTo, topic)
	if err != nil {
		return 0, fmt.Errorf("broker.HandleChatSend: insert: %w", err)
	}

	// 2. Compute recipients. RecipientPolicy.Compute excludes the
	// sender, preventing self-delivery loops. When no policy is
	// configured, fan-out is skipped silently; persistence still
	// succeeds and replay can serve future reconnects.
	var recipients []string
	if b.cfg.RecipientPolicy != nil {
		recipients = b.cfg.RecipientPolicy.Compute(from, content, replyTo)
	}

	// 3. Fan out chat.deliver to each recipient's live WS connection.
	// Aspects not currently connected miss the live frame; they pick
	// it up on next register via Lock 6 since_msg_id replay.
	// Build the chat.deliver envelope once, reuse it for both the
	// per-aspect fan-out below and the operator broadcast at the
	// tail. Reason is the per-aspect best-fit; operators see the
	// same reason but render the chat feed regardless of policy.
	reason := "mention"
	if replyTo > 0 {
		reason = "reply"
	}
	deliverEnv, deliverErr := frames.New(frames.KindChatDeliver, frames.ChatDeliverPayload{
		ID:      int(msg.ID),
		From:    from,
		Content: content,
		ReplyTo: int(replyTo),
		// RFC3339Nano matches replay (ws.go replayAddressedSince) and
		// chat.read so cursor-equality comparisons across the three
		// surfaces don't break on sub-second precision.
		ReceivedAt: msg.CreatedAt.UTC().Format(time.RFC3339Nano),
		Reason:     reason,
		Replay:     false,
		// ThreadRoot carries the linked-list thread identity (#226)
		// so the receiving aspect's funnel can key per-thread session
		// state. Resolved during Insert.
		ThreadRoot: int(msg.ThreadRootMsgID),
	})
	if deliverErr != nil {
		// Build failure means the per-aspect AND operator paths
		// both miss this delivery. Replay covers aspects on
		// reconnect; operators refresh-on-load. Log and continue.
		b.log.Warn("chat.deliver: build frame", "err", deliverErr, "msg_id", msg.ID)
	} else {
		for _, rec := range recipients {
			c := b.dispatcher.connFor(rec)
			if c == nil {
				// Not connected. Replay covers reconnect; skip silently.
				continue
			}
			c.send(deliverEnv)
		}

		// Operator broadcast (5d). Distinct from the per-aspect
		// loop: the operator's view is "everything," not policy-
		// scoped. Subscribers gated via c.subscribedChat in the
		// fan-out predicate.
		b.broadcastChatDeliverToOperators(deliverEnv)
	}

	// 4. Observability (Phase B): emit ChatFrames for the sender
	// (outbound) and each computed recipient (inbound). Lazy-create
	// groupers per aspect via the Hub. Non-aspect senders (operator,
	// frame) still get a Grouper today — they only become visible if
	// someone subscribes; filtering by registered roster is deferred.
	if b.observability != nil {
		obsMsg := chat.Message{
			ID:        msg.ID,
			From:      from,
			Content:   content,
			ReplyTo:   replyTo,
			Topic:     topic,
			CreatedAt: msg.CreatedAt,
		}
		if g := b.observability.GrouperFor(from); g != nil {
			g.OnChat(obsMsg, observability.DirectionOutbound)
		}
		for _, rec := range recipients {
			if g := b.observability.GrouperFor(rec); g != nil {
				g.OnChat(obsMsg, observability.DirectionInbound)
			}
		}
	}

	return msg.ID, nil
}
