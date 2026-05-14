package funnel

import (
	"context"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// TurnTrigger is the typed inbox-item view a ReturnHandler receives.
// Carries just the fields the return-side needs to route the result —
// not the full bridle.InboxItem (which has provider-private fields
// like the funnel's internal session/observability hints).
//
// Trigger fields are zero-valued for non-inbox deliberations
// (operator REST / eval / userMessage-only invocations). A nil-ish
// trigger means "this turn wasn't reacting to a message; do whatever
// makes sense for an unsolicited result on your channel."
type TurnTrigger struct {
	// MsgID is the chat message id that triggered this deliberation,
	// or 0 if the turn wasn't reacting to a specific message.
	MsgID int64

	// From is the sender of the triggering message ("operator",
	// peer-aspect name, etc.). Empty when MsgID is 0.
	From string

	// Content is the triggering message body. Empty when MsgID is 0.
	// Return handlers usually don't need this — the engine already
	// fed it to the model — but it's exposed for clients that want to
	// quote the trigger in their channel (panel rendering, audit
	// trails, etc.).
	Content string

	// ThreadRoot is the canonical thread id (chat_messages.thread_root_msg_id)
	// the triggering message belongs to. Zero when the trigger wasn't
	// a chat message. NexusChatReturnHandler uses this for reply_to
	// resolution; agora-side return handlers can use it to route
	// results into the right thread surface.
	ThreadRoot int64

	// Source identifies which channel the trigger arrived on. For the
	// nexus-chat substrate this is always "chat"; agora-side handlers
	// can branch on Source="chat" vs Source="tty" (or future) to apply
	// different routing rules (e.g. filter-respect for chat, bypass
	// for tty). Empty Source = "chat" by convention (the legacy default).
	Source string
}

// ReturnHandler is the seam between the deliberation engine and the
// channel that surfaces results. The engine (TurnHandler-equivalent
// inside Funnel.Deliberate) calls these two methods at the right
// points in the turn lifecycle; implementations route per their
// channel.
//
// Two methods, not one — see chat #1023:
//   - OnTurnStart fires the "I picked up your message" pulse the
//     moment a turn begins (before any model work). For nexus chat
//     that's the 👀 reaction on the trigger msg; for agora-side
//     that's a panel-state event.
//   - Handle fires after the deliberation result is in hand. For
//     nexus chat that's the 👀→👍/🙊 toggle plus the auto-post of
//     FinalText if Filter.ShouldPost. Agora-side that's source-routed
//     dispatch into chat or tty.
//
// Implementations should NOT block the turn pipeline on slow I/O —
// the engine awaits Handle synchronously, so a hung chat post stalls
// the next inbox-pop. Return promptly; do queuing internally if
// needed.
type ReturnHandler interface {
	// OnTurnStart fires once at the top of Deliberate, after the
	// inbox-pop and before the model call. Use for "I'm working on it"
	// signals. Idempotent — repeated calls with the same trigger
	// should converge (e.g. NexusChatReturnHandler's reaction is
	// itself idempotent via the broker).
	//
	// Returning an error is logged but NEVER aborts the turn — a
	// failed start-pulse shouldn't kill substantive work. Engine
	// drops the error to debug-log.
	OnTurnStart(ctx context.Context, trigger TurnTrigger) error

	// Handle fires once at the end of Deliberate, after the model's
	// FinalText, the filter judge, and observability emission. The
	// implementation decides what to do with the result given the
	// trigger context and the filter decision. For nexus chat that's
	// (1) the resolve-emoji on the trigger and (2) auto-post if
	// ShouldPost. For agora that's source-routed dispatch.
	//
	// Returning an error is logged but doesn't fail the turn —
	// Deliberate's caller has already seen a successful turn result
	// at this point; return-side failures are observability concerns
	// not turn-correctness concerns.
	Handle(ctx context.Context, result DeliberateResult, trigger TurnTrigger) error
}

// NoopReturnHandler is a return handler that does nothing. Used in
// tests and in headless deliberation paths (operator REST eval) where
// the engine should run without any channel-side side-effects.
type NoopReturnHandler struct{}

func (NoopReturnHandler) OnTurnStart(context.Context, TurnTrigger) error { return nil }

func (NoopReturnHandler) Handle(context.Context, DeliberateResult, TurnTrigger) error {
	return nil
}

// Verify NoopReturnHandler satisfies the interface at compile time.
var _ ReturnHandler = NoopReturnHandler{}

// triggerFromInboxItem builds a TurnTrigger from the bridle InboxItem
// the engine just popped. Helper used by Deliberate at the top of
// the turn — keeps the trigger construction in one place so future
// fields land in lock-step with the InboxItem schema.
//
// Returns the zero-value TurnTrigger when the caller passes an
// empty/zero InboxItem (no inbox-driven turn — operator REST eval,
// userMessage-only path). The zero value's MsgID == 0, so return
// handlers can use that as the "no trigger" sentinel.
func triggerFromInboxItem(item bridle.InboxItem) TurnTrigger {
	// Source is hard-coded to "chat" for the funnel.Funnel call path —
	// every InboxItem reaching the funnel today is a chat-substrate
	// message. The TurnTrigger.Source field exists so callers that
	// construct TurnTrigger directly (agora-side handlers, future
	// non-chat sources) can tag a different origin and let
	// ReturnHandler implementations branch on it. bridle.InboxItem has
	// no Source field today — if/when chat-vs-non-chat triggers need
	// to differentiate at the funnel layer, this is the seam to
	// extend (probably by adding the field on bridle.InboxItem or by
	// taking a separate per-source funnel.Receive entry point).
	source := "chat"
	return TurnTrigger{
		MsgID:      item.MsgID,
		From:       item.From,
		Content:    item.Content,
		ThreadRoot: item.ThreadRoot,
		Source:     source,
	}
}
