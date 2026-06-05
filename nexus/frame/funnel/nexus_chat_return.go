package funnel

import (
	"context"
	"log/slog"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// NexusChatReturnHandler is the default ReturnHandler for the nexus
// chat substrate. Implements the three-call-site behavior that lived
// inline in Funnel.Deliberate before the NEX-82 split:
//
//  1. OnTurnStart: 👀 reaction on the trigger msg ("I saw it, I'm
//     working on it").
//  2. Handle resolve-emoji: 👀→👀(toggle-off) when filter approves and
//     a reply will post, 🙊 when filter ate substantive text, 👍 when
//     the model genuinely had nothing to add.
//  3. Handle auto-post: when filter ShouldPost, SendChat the model's
//     FinalText with reply_to set to the trigger MsgID.
//
// Wired by funnel.New as the default when Config.Return is nil and
// Config.ChatGateway is set — preserves the pre-NEX-82 behavior
// without callers needing to construct anything. Callers can pass an
// explicit Return handler (or NoopReturnHandler) to override.
type NexusChatReturnHandler struct {
	// Gateway is the chat-posting + reaction-toggling seam. Required
	// for non-noop behavior — a nil gateway makes all methods no-ops
	// (matching the pre-split "skip if cfg.ChatGateway == nil" guard).
	Gateway ChatGateway

	// AspectID is the calling aspect's name, used for log lines so the
	// operator can correlate return-side warnings to the producing
	// aspect. Required if Logger is set; safe-to-empty otherwise.
	AspectID string

	// Logger is the slog handler for return-side warnings. Nil is
	// fine — the handler degrades to discarding diagnostics, matching
	// the inline-Deliberate behavior which always had a logger
	// available.
	Logger *slog.Logger

	// SuppressAutoPost skips the final auto-post in Handle (text was
	// already streamed to chat during the turn via streamingChatSink).
	// Emoji resolution still fires — the turn lifecycle signals remain
	// intact. Set by the funnel when Config.StreamTextToChat is true.
	SuppressAutoPost bool

	// ReplyTopic is attached to natural auto-post replies. Empty
	// preserves the historical un-topic'd reply behavior.
	ReplyTopic string
}

// Verify NexusChatReturnHandler satisfies the interface at compile time.
var _ ReturnHandler = (*NexusChatReturnHandler)(nil)

// OnTurnStart fires the 👀 work-signal on the trigger message. Gated
// on having a real trigger (MsgID != 0) and a gateway — non-chat
// triggers (operator REST, eval) have no msg to react to and are
// silent no-ops.
//
// Per #189 — single-emoji-per-reactor makes this an ambient
// observability layer the operator scans across aspects: aspects
// reacting with 👀 are the ones actively turning on incoming messages.
func (h *NexusChatReturnHandler) OnTurnStart(ctx context.Context, trigger TurnTrigger) error {
	if h == nil || h.Gateway == nil || trigger.MsgID == 0 {
		return nil
	}
	if err := h.Gateway.ReactTo(ctx, trigger.MsgID, "👀"); err != nil {
		h.debug("funnel: 👀 work-signal failed",
			"trigger_msg_id", trigger.MsgID, "err", err)
		return err
	}
	return nil
}

// Handle resolves the work-signal and auto-posts the model's reply if
// the filter approves. Three branches under single-emoji-per-reactor
// semantics (preserved verbatim from the pre-split Deliberate body):
//
//   - ShouldPost: model's reply will post → toggle 👀 off (re-react
//     with the same emoji removes the reactor's row). The posted reply
//     is itself the signal that the aspect engaged.
//   - !ShouldPost AND non-empty text: filter killed a substantive
//     reply → 🙊 (see-no-evil). Audit signal: aspect had something to
//     say, judge labeled it scratch.
//   - !ShouldPost AND empty text: model genuinely had nothing →
//     👍 ("saw it, nothing to add"). The honest acknowledgement.
//
// Auto-post fires when filter ShouldPost and FinalText is non-empty.
// ReplyTo threads the post under the trigger msg when one exists;
// non-triggered turns post top-level (MsgID == 0).
func (h *NexusChatReturnHandler) Handle(ctx context.Context, result DeliberateResult, trigger TurnTrigger) error {
	if h == nil || h.Gateway == nil {
		return nil
	}

	// Resolve emoji on the trigger msg.
	if trigger.MsgID != 0 {
		var resolveEmoji string
		switch {
		case result.Filter.ShouldPost:
			resolveEmoji = "👀" // toggle off
		case strings.TrimSpace(result.TurnResult.FinalText) != "":
			resolveEmoji = "🙊" // filter ate a substantive reply
		default:
			resolveEmoji = "👍" // genuinely nothing to add
		}
		if err := h.Gateway.ReactTo(ctx, trigger.MsgID, resolveEmoji); err != nil {
			h.debug("funnel: resolve work-signal failed",
				"trigger_msg_id", trigger.MsgID,
				"emoji", resolveEmoji,
				"err", err)
			// Don't return — resolve-emoji failure shouldn't block
			// the auto-post path below.
		}
	}

	// NEX-292: filter SystemNotice posts an in-band message ahead of
	// any reply. CheapModelFilter sets this on judge-degradation entry
	// ("aspect: judge unavailable — replies rate-limited …") and exit
	// ("aspect: judge recovered") so failure / recovery isn't silent.
	// Posted as the aspect itself (no distinct system-author yet);
	// the message reads as the aspect announcing the condition.
	// Posted before the auto-post so the notice appears above the
	// reply it qualifies.
	if notice := strings.TrimSpace(result.Filter.SystemNotice); notice != "" {
		if _, err := h.Gateway.SendChat(ctx, notice, trigger.MsgID, h.ReplyTopic); err != nil {
			h.warn("funnel: system notice post failed",
				"trigger_msg_id", trigger.MsgID,
				"err", err)
			// Don't return — notice failure shouldn't block the reply.
		}
	}

	// Auto-post the model's natural reply when filter approves.
	// Skip when text was already streamed to chat during the turn
	// (SuppressAutoPost), OR when the model posted a chat message via a
	// comms tool this turn (NEX-370): send_chat IS the reply, so
	// auto-posting FinalText too would duplicate it (the multi-post bug).
	// FinalText auto-post stays the fallback for turns where the model
	// did NOT explicitly post.
	postedViaTool := postedChatViaTool(result.TurnResult.ToolCalls)
	if result.Filter.ShouldPost && !h.SuppressAutoPost && !postedViaTool {
		text := strings.TrimSpace(result.TurnResult.FinalText)
		if text != "" {
			if msgID, err := h.Gateway.SendChat(ctx, text, trigger.MsgID, h.ReplyTopic); err != nil {
				h.warn("funnel: auto-post failed",
					"trigger_msg_id", trigger.MsgID,
					"err", err)
				return err
			} else {
				h.info("funnel: auto-posted",
					"msg_id", msgID,
					"reply_to", trigger.MsgID,
					"chars", len(text))
			}
		}
	} else if result.Filter.ShouldPost && postedViaTool && strings.TrimSpace(result.TurnResult.FinalText) != "" {
		h.debug("funnel: auto-post skipped — model already posted via a comms tool this turn (NEX-370)",
			"trigger_msg_id", trigger.MsgID)
	}
	return nil
}

// postedChatViaTool reports whether the model posted a chat MESSAGE via a
// comms tool during the turn. When it did, the reply is already on the bus,
// so auto-posting FinalText would duplicate it (NEX-370). react_to /
// chat_read / knowledge tools don't count — they don't post a message that
// FinalText would duplicate.
func postedChatViaTool(invs []bridle.ToolInvocation) bool {
	for _, name := range toolNamesFromInvocations(invs) {
		switch name {
		case ToolNameSendChat, ToolNameAnnounceFile, ToolNameShareFile:
			return true
		}
	}
	return false
}

// debug / info / warn shorthand for handlers — guards against nil
// logger so callers can construct minimal NexusChatReturnHandlers in
// tests without wiring a logger.
func (h *NexusChatReturnHandler) debug(msg string, args ...any) {
	if h.Logger == nil {
		return
	}
	h.Logger.Debug(msg, append([]any{"aspect", h.AspectID}, args...)...)
}

func (h *NexusChatReturnHandler) info(msg string, args ...any) {
	if h.Logger == nil {
		return
	}
	h.Logger.Info(msg, append([]any{"aspect", h.AspectID}, args...)...)
}

func (h *NexusChatReturnHandler) warn(msg string, args ...any) {
	if h.Logger == nil {
		return
	}
	h.Logger.Warn(msg, append([]any{"aspect", h.AspectID}, args...)...)
}
