// Package framecomms provides the in-process ChatGateway used by
// the embedded Frame's funnel. Implements funnel.ChatGateway against
// the chat.Store from F1.4b.1.
//
// Why a separate package: putting the gateway in `frame/funnel` would
// create a circular dependency once F1.4b.4 wires the gateway from
// cmd/nexus startup. Putting it in `nexus/chat` would force chat to
// depend on funnel for the interface. A small adapter package owns
// the seam between the two — funnel defines the interface, chat
// owns persistence, framecomms wires them together.

package framecomms

import (
	"context"
	"fmt"

	"github.com/nexus-cw/nexus/nexus/chat"
	"github.com/nexus-cw/nexus/nexus/frame/funnel"
)

// Gateway is the in-process funnel.ChatGateway. It writes via a
// chat.Store and reads thread history the same way. ReactTo,
// AnnounceFile, and ShareFile are stub-returns for now (F1.4b.3
// adds the storage shape and wiring); they exist so the interface
// is fully implemented and the runner doesn't have to special-case
// them, but calls to those methods today return a "not yet
// implemented" error the model can read in the tool result.
//
// The Gateway intentionally does NOT trigger Frame deliberation on
// SendChat — when keel-as-Frame writes via send_chat, that's an
// outbound post (the Frame is the sender). The Frame's own
// deliberation already ran (the model called the tool); routing it
// back into Frame.Receive would loop forever.
//
// Out-of-process aspects' chat.send frames flow through the broker's
// handleChatSendFrame → ChatRouter.RouteChat path and DO trigger
// Frame deliberation when ShouldRouteToFrame approves. That path is
// unchanged by this gateway.
type Gateway struct {
	Store    chat.Store
	AspectID string // typically the Frame's name; used as the From on sends
}

// NewGateway wires a Gateway around a Store and aspect identity.
// Both fields are required; nil/empty arguments cause SendChat to
// fail at call time rather than at construction (callers wire
// gateways from startup config — failing loud at the call site
// surfaces misconfiguration in operator-visible logs).
func NewGateway(store chat.Store, aspectID string) *Gateway {
	return &Gateway{Store: store, AspectID: aspectID}
}

// SendChat persists the message and returns the new id. The aspect
// id from the Gateway is used as the From; the funnel doesn't pass
// it because the gateway already knows who's posting (the Frame
// owns one Gateway, and the Gateway carries identity). Bypasses
// the Frame's own RouteChat path on purpose — see Gateway doc.
func (g *Gateway) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
	if g.Store == nil {
		return 0, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	if g.AspectID == "" {
		return 0, fmt.Errorf("framecomms.Gateway: AspectID required to send")
	}
	msg, err := g.Store.Insert(ctx, g.AspectID, content, replyTo, topic)
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

// ReactTo toggles a reaction on the named message. The Gateway's
// AspectID is the reactor; per-reactor independence means anvil
// reacting 👍 doesn't affect wren's separate 👍 on the same message.
// Calling twice with the same emoji removes the reaction (toggle
// semantics).
//
// The model receives a JSON tool result with {ok: true} regardless
// of whether the toggle added or removed — the model already knows
// what it asked for; the gateway's role is to make it durable. Tests
// can observe the toggle direction via the underlying store API.
func (g *Gateway) ReactTo(ctx context.Context, msgID int64, emoji string) error {
	if g.Store == nil {
		return fmt.Errorf("framecomms.Gateway: no store configured")
	}
	if g.AspectID == "" {
		return fmt.Errorf("framecomms.Gateway: AspectID required to react")
	}
	_, err := g.Store.ToggleReaction(ctx, msgID, g.AspectID, emoji)
	return err
}

// ReadThread pulls thread history from the store. Lock 2's pull
// path: aspects use this to read context they weren't pushed.
//
// Limit is set to 200 by default — large enough for normal threads,
// bounded so a model that calls chat.read on a 10k-message root
// doesn't load the entire log into context. Operators can audit
// thread sizes in dashboard if a real thread approaches the cap.
func (g *Gateway) ReadThread(ctx context.Context, threadID, sinceID int64) ([]funnel.ChatMessage, error) {
	if g.Store == nil {
		return nil, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	const defaultReadLimit = 200
	rows, err := g.Store.ListThread(ctx, threadID, sinceID, defaultReadLimit)
	if err != nil {
		return nil, err
	}
	out := make([]funnel.ChatMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, funnel.ChatMessage{
			ID:         r.ID,
			From:       r.From,
			Content:    r.Content,
			ReplyTo:    r.ReplyTo,
			Topic:      r.Topic,
			ReceivedAt: r.FormatRFC3339(),
		})
	}
	return out, nil
}

// AnnounceFile inserts a chat post announcing the file plus a
// shared_files row linking back to it. Returns the chat msg_id (the
// model's reference for follow-up activity) — the share_id stays
// internal for now.
func (g *Gateway) AnnounceFile(ctx context.Context, path, description string) (int64, error) {
	if g.Store == nil {
		return 0, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	if g.AspectID == "" {
		return 0, fmt.Errorf("framecomms.Gateway: AspectID required to announce")
	}
	msgID, _, err := g.Store.AnnounceSharedFile(ctx, g.AspectID, path, description)
	return msgID, err
}

// ShareFile records a direct share with no chat post. Returns the
// share id the model can reference.
func (g *Gateway) ShareFile(ctx context.Context, path string, recipients []string) (int64, error) {
	if g.Store == nil {
		return 0, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	if g.AspectID == "" {
		return 0, fmt.Errorf("framecomms.Gateway: AspectID required to share")
	}
	return g.Store.ShareFile(ctx, g.AspectID, path, recipients)
}
