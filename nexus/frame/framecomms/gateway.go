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
	"database/sql"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/nexus/chat"
	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
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
// ChatSender is the canonical chat-send seam. The broker's
// HandleChatSend satisfies it; the Gateway uses it for SendChat so
// in-process Frame posts go through the same persist+fan-out path
// as out-of-process aspect WS frames (per docs/2026-05-04 unify spec).
//
// Defined here (not in chat or broker) to avoid the import cycle:
// broker imports chat, framecomms imports chat — so framecomms can't
// import broker. The interface seam keeps the dependency edge
// pointing the right way.
type ChatSender interface {
	HandleChatSend(ctx context.Context, from, content string, replyTo int64, topic string) (int64, error)
}

// ReactBroadcaster is the seam Gateway uses to push chat.reaction.update
// frames to subscribed operators after an in-process toggle. The broker's
// BroadcastChatReactionUpdate satisfies it. Defined here (not in broker)
// for the same import-cycle reason as ChatSender — broker imports chat,
// framecomms imports chat, so framecomms can't import broker. The
// interface keeps the dependency edge pointing the right way.
//
// nil = no broadcast (legacy path / tests). Production wires this from
// cmd/nexus after broker.New the same way Sender gets wired.
type ReactBroadcaster interface {
	BroadcastChatReactionUpdate(payload frames.ChatReactionUpdatePayload)
}

type Gateway struct {
	Store            chat.Store       // still used for ReactTo / ReadThread / AnnounceFile / ShareFile
	Sender           ChatSender       // canonical SendChat path; nil = legacy direct-Insert fallback
	ReactBroadcaster ReactBroadcaster // chat.reaction.update fan-out after in-process toggle; nil = silent
	AspectID         string           // typically the Frame's name; used as the From on sends
}

// NewGateway wires a Gateway around a Store and aspect identity.
// Both fields are required; nil/empty arguments cause SendChat to
// fail at call time rather than at construction (callers wire
// gateways from startup config — failing loud at the call site
// surfaces misconfiguration in operator-visible logs).
//
// Sender is left nil here for back-compat with tests that don't
// have a broker. Production wiring (cmd/nexus) sets Sender to the
// broker after construction so SendChat takes the unified path.
func NewGateway(store chat.Store, aspectID string) *Gateway {
	return &Gateway{Store: store, AspectID: aspectID}
}

// SendChat persists the message and returns the new id. When a
// Sender is configured (production), this delegates to
// broker.HandleChatSend so persistence + recipient fan-out + the
// Frame's own ChatRouter callback all run in one place. Without a
// Sender (legacy tests), falls back to a direct Store.Insert — same
// behavior as before the unify spec, minus fan-out.
func (g *Gateway) SendChat(ctx context.Context, content string, replyTo int64, topic string) (int64, error) {
	if g.AspectID == "" {
		return 0, fmt.Errorf("framecomms.Gateway: AspectID required to send")
	}
	if g.Sender != nil {
		return g.Sender.HandleChatSend(ctx, g.AspectID, content, replyTo, topic)
	}
	if g.Store == nil {
		return 0, fmt.Errorf("framecomms.Gateway: no store or sender configured")
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
	reacted, err := g.Store.ToggleReaction(ctx, msgID, g.AspectID, emoji)
	if err != nil {
		return err
	}

	// Broadcast chat.reaction.update so the dashboard SPA sees the toggle
	// without waiting for the next chat.reactions.fetch on reconnect.
	// Mirrors the post-toggle path in broker/ws.go handleChatReactionFrame.
	// Without this, in-process Frame reactions (👀/👍/🙊 from the funnel's
	// work-signal path) land in the DB but never push to operators — so
	// from the operator's view the Frame looks dead even when it's working.
	if g.ReactBroadcaster != nil {
		all, fetchErr := g.Store.GetReactions(ctx, []int64{msgID})
		if fetchErr != nil {
			// Best-effort: the toggle succeeded, the broadcast didn't.
			// Don't surface to the caller — the model can't act on a
			// post-write fan-out failure. Operator catches it on next
			// page load via chat.reactions.fetch.
			return nil
		}
		current := all[msgID]
		rows := make([]frames.ReactionRow, 0, len(current))
		for _, r := range current {
			rows = append(rows, frames.ReactionRow{Aspect: r.Aspect, Emoji: r.Emoji})
		}
		op := "removed"
		if reacted {
			op = "added"
		}
		g.ReactBroadcaster.BroadcastChatReactionUpdate(frames.ChatReactionUpdatePayload{
			MsgID:     int(msgID),
			Reactor:   g.AspectID,
			Emoji:     emoji,
			Op:        op,
			Reactions: rows,
		})
	}
	return nil
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

// ReadMessage returns a single chat row by id.
func (g *Gateway) ReadMessage(ctx context.Context, msgID int64) (funnel.ChatMessage, error) {
	if g.Store == nil {
		return funnel.ChatMessage{}, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	r, err := g.Store.GetByID(ctx, msgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return funnel.ChatMessage{}, fmt.Errorf("message %d: not found", msgID)
		}
		return funnel.ChatMessage{}, err
	}
	return funnel.ChatMessage{
		ID:         r.ID,
		From:       r.From,
		Content:    r.Content,
		ReplyTo:    r.ReplyTo,
		Topic:      r.Topic,
		ReceivedAt: r.FormatRFC3339(),
	}, nil
}

// ListShared returns recently-shared files (newest-first), capped by limit.
func (g *Gateway) ListShared(ctx context.Context, limit int) ([]funnel.SharedFileRef, error) {
	if g.Store == nil {
		return nil, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	rows, err := g.Store.ListShared(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]funnel.SharedFileRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, sharedFileRefFromRow(r))
	}
	return out, nil
}

// GetShared returns a single shared_files row by id.
func (g *Gateway) GetShared(ctx context.Context, shareID int64) (funnel.SharedFileRef, error) {
	if g.Store == nil {
		return funnel.SharedFileRef{}, fmt.Errorf("framecomms.Gateway: no store configured")
	}
	r, err := g.Store.GetShared(ctx, shareID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return funnel.SharedFileRef{}, fmt.Errorf("shared %d: not found", shareID)
		}
		return funnel.SharedFileRef{}, err
	}
	return sharedFileRefFromRow(r), nil
}

func sharedFileRefFromRow(r chat.SharedFile) funnel.SharedFileRef {
	return funnel.SharedFileRef{
		ID:             r.ID,
		Path:           r.Path,
		Description:    r.Description,
		SharedBy:       r.SharedBy,
		AnnounceMsgID:  r.AnnounceMsgID,
		RecipientsJSON: r.RecipientsJSON,
		CreatedAt:      r.CreatedAt,
	}
}
