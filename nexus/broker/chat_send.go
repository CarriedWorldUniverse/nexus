// chat_send.go — the canonical chat.send code path.
//
// Every origin of a chat message lands in HandleChatSend:
//
//   - Out-of-process aspect WS:  handleChatSendFrame (ws.go) → HandleChatSend
//   - Operator browser WS:        handleChatSendFrame (same path) → HandleChatSend
//   - In-process Frame gateway:   framecomms.Gateway.SendChat → HandleChatSend
//
// Every chat.send results in:
//   1. INSERT chat_messages row (ChatStore.Insert)
//   2. Compute recipients per Lock 2 (RecipientPolicy.Compute)
//   3. Fan out chat.deliver frames to each recipient's live WS
//   4. Fire ChatRouter.RouteChat to trigger Frame's deliberation funnel
//      (legacy callback; eventually retires when the Frame becomes
//      a chat.deliver recipient like every other aspect)
//
// Without this single path:
//   - Operator/aspect chat.send didn't persist (FK errors on usage.Record)
//   - Live chat.deliver fan-out was missing entirely (only replay-on-register
//     delivered messages)
//   - framecomms.Gateway and the WS path had diverged implementations of
//     "post a chat message"

package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexus-cw/nexus/nexus/frames"
)

// HandleChatSend persists an inbound chat message and fans it out to
// recipients. Returns the assigned chat msg_id so callers can reference
// it for reply chains, usage attribution, etc.
//
// Errors propagate to the caller. The WS shim treats them as warn-and-
// drop (chat.send is fire-and-forget per transport spec); the
// in-process Frame gateway propagates them up to the funnel runner so
// the model sees a tool error.
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
	// sender, so a Frame post never routes back to the Frame — no
	// self-loop. When no policy is configured, fan-out is skipped
	// silently (legacy mode); only persistence + ChatRouter callback
	// fire.
	var recipients []string
	if b.cfg.RecipientPolicy != nil {
		recipients = b.cfg.RecipientPolicy.Compute(from, content, replyTo)
	}

	// 3. Fan out chat.deliver to each recipient's live WS connection.
	// Aspects not currently connected miss the live frame; they pick
	// it up on next register via Lock 6 since_msg_id replay.
	for _, rec := range recipients {
		c := b.dispatcher.connFor(rec)
		if c == nil {
			// Not connected. Replay covers reconnect; skip silently.
			continue
		}
		// The frame's reason field tracks why this aspect was
		// included (mention | reply | thread | all). For v1 we
		// recompute a single best-fit reason; refining the policy
		// to surface per-recipient reason is a follow-up.
		reason := "mention"
		if replyTo > 0 {
			reason = "reply"
		}
		env, err := frames.New(frames.KindChatDeliver, frames.ChatDeliverPayload{
			ID:         int(msg.ID),
			From:       from,
			Content:    content,
			ReplyTo:    int(replyTo),
			// RFC3339Nano matches replay (ws.go replayAddressedSince) and
			// chat.read so cursor-equality comparisons across the three
			// surfaces don't break on sub-second precision.
			ReceivedAt: msg.CreatedAt.UTC().Format(time.RFC3339Nano),
			Reason:     reason,
			Replay:     false,
		})
		if err != nil {
			b.log.Warn("chat.deliver: build frame", "err", err, "to", rec)
			continue
		}
		c.send(env)
	}

	// 4. Frame's funnel trigger (legacy ChatRouter callback). The
	// callback runs the Frame's deliberation loop when the message
	// matches the Frame's interest predicate. Eventually the Frame
	// becomes one of the chat.deliver recipients above and this
	// callback retires.
	if b.cfg.ChatRouter != nil && b.cfg.ChatRouter.RouteChat != nil {
		// Detach the goroutine from the caller's ctx. The Frame's
		// SendChat path passes the funnel's per-turn deliberation ctx;
		// when that turn ends the ctx cancels, which would kill any
		// nested deliberation RouteChat triggers. Match the dispatch
		// handler's pattern: prefer broker-lifetime ctx, fall back to
		// Background.
		routerCtx := b.ctx
		if routerCtx == nil {
			routerCtx = context.Background()
		}
		go b.cfg.ChatRouter.RouteChat(routerCtx, msg.ID, from, content, replyTo, topic)
	}

	return msg.ID, nil
}
