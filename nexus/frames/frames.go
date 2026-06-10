// Package frames defines the on-the-wire frame format used by every
// Nexus-component WebSocket connection (aspect↔Outpost, aspect↔Nexus,
// Outpost↔Nexus). Per transport spec v0.1 §5.
//
// Every frame is a JSON object with a `kind` discriminator and an
// opaque payload the handler interprets per-kind. Unknown kinds are
// forward-compat: receivers log and drop rather than error out, so
// new frame types roll out without hard-synchronised upgrades.
package frames

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// ErrNoPayload is returned by PayloadAs when the envelope carries no
// payload. Callers who legitimately expect an empty payload (e.g.
// Shutdown with defaults) should use errors.Is to branch on it;
// everyone else should treat it as a bug and fail loud.
var ErrNoPayload = errors.New("frames: no payload")

// ulidEntropy is the monotonic source for frame id generation. Under
// concurrent NewRequest calls, ulid.Monotonic guarantees ordering
// within the same millisecond window without collisions.
var (
	ulidEntropyMu sync.Mutex
	ulidEntropy   = ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0)
)

// newID returns a fresh ULID suitable as a frame correlation id.
// Thread-safe: callers don't need to lock around NewRequest.
func newID() string {
	ulidEntropyMu.Lock()
	defer ulidEntropyMu.Unlock()
	return ulid.MustNew(ulid.Now(), ulidEntropy).String()
}

// Kind identifies the frame type. See transport spec §5.2 for the
// canonical catalogue.
type Kind string

const (
	// Registration — the first frame on a new connection identifies
	// the speaker. Aspects send `register`; Outposts send
	// `outpost.register`. Server acks with `register.ack` or
	// `outpost.register.ack`.
	KindRegister           Kind = "register"
	KindRegisterAck        Kind = "register.ack"
	KindDeregister         Kind = "deregister"
	KindOutpostRegister    Kind = "outpost.register"
	KindOutpostRegisterAck Kind = "outpost.register.ack"
	KindOutpostDeregister  Kind = "outpost.deregister"

	// Turn dispatch — upstream asks an aspect to run a single turn.
	KindTurn       Kind = "turn"
	KindTurnResult Kind = "turn.result"

	// Dispatch — fresh-context turn run in an interchangeable worker
	// slot loaded with the dispatching aspect's identity. Per
	// hand-dispatch v0.1 §5.1: protocol vocabulary is generic.
	// `dispatch.error` when dispatch can't happen at all (queue
	// saturated, hard ceiling, identity mismatch).
	KindDispatch       Kind = "dispatch"
	KindDispatchResult Kind = "dispatch.result"
	KindDispatchError  Kind = "dispatch.error"

	// CWB data-plane relay (aspect REST/gRPC calls bridged WS<->HTTP by the broker).
	KindCWBRequest  Kind = "cwb.request"
	KindCWBResponse Kind = "cwb.response"

	// Chat — the existing comms surface in frame form.
	KindChatSend       Kind = "chat.send"
	KindChatDeliver    Kind = "chat.deliver"
	KindChatReaction   Kind = "chat.reaction"
	KindChatRead       Kind = "chat.read"
	KindChatReadResult Kind = "chat.read.result"
	KindAnnounceFile   Kind = "announce_file"
	KindShareFile      Kind = "share_file"
	KindFileResult     Kind = "file.result"
	KindAspectActivity Kind = "aspect.activity"

	// Knowledge — aspect-to-Nexus store/query.
	KindKnowledgeStore        Kind = "knowledge.store"
	KindKnowledgeSearch       Kind = "knowledge.search"
	KindKnowledgeSearchResult Kind = "knowledge.search.result"

	// Credentials — aspect-to-Nexus credential fetch. NEX-77.
	// Aspects fetch kind-typed credentials (jira/imap/provider) from
	// the broker's credential store via WS. Authentication is JWT
	// (the conn's authenticated registeredAs); aspects can't fetch
	// credentials they're not on the allowed_aspects list for.
	// Provider creds are usually consumed via ProviderEnv resolution
	// in the funnel — this frame exists for non-provider kinds (jira/
	// imap) where the MCP needs the bundle directly, and for provider-
	// kind plaintext-fetch (mode=fetch|both) when the aspect needs the
	// raw API key for a non-proxy code path.
	KindCredentialFetch       Kind = "credential.fetch"
	KindCredentialFetchResult Kind = "credential.fetch.result"

	// Per-aspect model + credential override fetch. NEX-293.
	// agentfunnel issues this at startup to retrieve the admin-managed
	// AspectModelConfig overrides (NEX-263) so out-of-process aspects
	// see the same per-aspect judge model + credential routing the
	// in-process Frame already sees via applyAspectModelOverrides.
	// Authentication: same JWT/registered identity gate as
	// credential.fetch; the conn's identity is the aspect whose row
	// is read (aspects can't read another aspect's overrides).
	KindAspectModelConfigGet       Kind = "aspect.model_config.get"
	KindAspectModelConfigGetResult Kind = "aspect.model_config.get.result"

	// Session observability — projection upward for dashboard view.
	// Local aspect JSONL is source of truth; Nexus keeps a read-only
	// mirror for rendering.
	KindSessionEntryAppended Kind = "session.entry.appended"
	KindSessionRewind        Kind = "session.rewind"
	KindSessionFork          Kind = "session.fork"

	// Session lifecycle — aspect rotates its session JWT in-place
	// over the existing authenticated WebSocket. Broker identifies
	// the aspect from the connection's bound session; no keyfile
	// material crosses the wire.
	KindSessionRefresh       Kind = "session.refresh"
	KindSessionRefreshResult Kind = "session.refresh.result"

	// Lifecycle.
	KindShutdown Kind = "shutdown"

	// switch.surface — aspect requests a live surface flip
	// (funnel ↔ agora). Broker validates, updates the DB, closes
	// the WS connection. Aspect exits; supervisor restarts with
	// the new binary.
	KindSwitchSurface       Kind = "switch.surface"
	KindSwitchSurfaceResult Kind = "switch.surface.result"

	// Operator dashboard (dashboard-ws-port spec §3.2). Request/response
	// frames the SPA sends from the browser's WS connection. All carry
	// a correlation_id (the envelope's ID) and the broker echoes it on
	// the result. Authoritative consumers: the dashboard SPA today;
	// future operator-tooling clients can reuse the same surface.
	KindRosterList       Kind = "roster.list"
	KindRosterListResult Kind = "roster.list.result"
	// chat.list is operator-only: all chat messages, paginated by id.
	// Distinct from chat.read (which is thread-scoped and aspect-
	// available). Used by the dashboard's main chat feed; topics
	// view + topic-scoped reads are a follow-up part — chat_messages
	// today has no persisted topic column, so topics work needs a
	// schema migration that's out of 5c scope.
	KindChatList             Kind = "chat.list"
	KindChatListResult       Kind = "chat.list.result"
	KindChatReplies          Kind = "chat.replies"
	KindChatRepliesResult    Kind = "chat.replies.result"
	KindReactionsFetch       Kind = "chat.reactions.fetch"
	KindReactionsFetchResult Kind = "chat.reactions.fetch.result"

	// KindChatReactionUpdate is the push frame broadcast to subscribed
	// operators when a chat reaction toggles. Carries the full reactions
	// list for the affected msg so the SPA can replace in-place — same
	// per-id shape as chat.reactions.fetch.result so the existing
	// rendering path works for both load and live-update.
	KindChatReactionUpdate    Kind = "chat.reaction.update"
	KindKnowledgeList         Kind = "knowledge.list"
	KindKnowledgeListResult   Kind = "knowledge.list.result"
	KindKnowledgeStoreResult  Kind = "knowledge.store.result"
	KindAspectSay             Kind = "aspect.say"
	KindAspectSayResult       Kind = "aspect.say.result"
	KindRunsList              Kind = "runs.list"
	KindRunsListResult        Kind = "runs.list.result"
	KindRunGet                Kind = "run.get"
	KindRunGetResult          Kind = "run.get.result"
	KindRunCancel             Kind = "run.cancel"
	KindRunCancelResult       Kind = "run.cancel.result"
	KindActivityHistory       Kind = "activity.history"
	KindActivityHistoryResult Kind = "activity.history.result"
	KindEnvHealth             Kind = "env.health"
	KindEnvHealthResult       Kind = "env.health.result"
	KindPing                  Kind = "ping"
	KindPong                  Kind = "pong"

	// Subscription frames (5d). Each "subscribe.X" enrolls the
	// operator's connection in the corresponding push stream; the
	// matching "unsubscribe.X" turns it off. Subscriptions are
	// per-connection state, not persisted — WS close drops them.
	// Idempotent: re-subscribing is a no-op.
	KindSubscribeRoster         Kind = "subscribe.roster"
	KindSubscribeChat           Kind = "subscribe.chat"
	KindSubscribeAspectStatus   Kind = "subscribe.aspect_status"
	KindSubscribeObserve        Kind = "subscribe.observe"
	KindUnsubscribeRoster       Kind = "unsubscribe.roster"
	KindUnsubscribeChat         Kind = "unsubscribe.chat"
	KindUnsubscribeAspectStatus Kind = "unsubscribe.aspect_status"
	KindUnsubscribeObserve      Kind = "unsubscribe.observe"
	KindSubscribeAck            Kind = "subscribe.ack"
	// Push frames the broker emits to subscribed operators.
	KindRosterUpdate      Kind = "roster.update"
	KindAspectStatusPulse Kind = "aspect.status_pulse"
	KindObserveFrame      Kind = "observe.frame"
	KindRunsUpdate        Kind = "runs.update"

	// Upstream observability frames — sent from aspect/agentfunnel TO
	// broker so remote funnels (running in a different process from the
	// broker's observability.Hub) can stream bridle events to operator
	// dashboards via the existing observe.frame fanout.
	//
	// Attribution: the broker tags incoming events with the aspect
	// identity from the wsConn's authenticated registration
	// (wsConn.registeredAs), NOT from the payload. Per keel-cli's
	// caveat at chat #236, a mismatch between payload.Aspect and
	// registeredAs is treated as advisory and the connection's
	// authenticated identity wins.
	KindObserveBegin Kind = "observe.begin"
	KindObserveEvent Kind = "observe.event"
	KindObserveEnd   Kind = "observe.end"

	// Operator escalation (ToolRunner P3c). A native-API aspect's
	// permission policy can mark a tool call "ask a human"; the funnel's
	// BeforeToolCall hook then pauses mid-turn and emits an
	// escalation.request (correlated via the envelope ID). The broker
	// fans the request out to subscribed operators; agora surfaces it.
	// The operator answers with an escalation.decision carrying
	// InReplyTo=request.ID, which the broker routes back to the
	// originating aspect's connection so the blocked Request resolves.
	KindEscalationRequest  Kind = "escalation.request"
	KindEscalationDecision Kind = "escalation.decision"
)

// Envelope is the shared shape of every frame.
type Envelope struct {
	Kind      Kind            `json:"kind"`
	ID        string          `json:"id,omitempty"`
	InReplyTo string          `json:"in_reply_to,omitempty"`
	TS        time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload,omitempty"`

	// TargetAspect names the aspect this frame is destined for, when
	// the immediate WS connection isn't the aspect's own. Used by the
	// broker → outpost path for unsolicited downstream frames (turn,
	// shutdown) so the outpost can route to the right local aspect
	// (#20). Empty when the frame's destination is the connection
	// itself (the common case for direct aspects).
	TargetAspect string `json:"target_aspect,omitempty"`
}

// New stamps a frame with the current time and serialises the payload.
// The envelope is returned ready to send via the wire.
func New(kind Kind, payload any) (Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, fmt.Errorf("frames.New: marshal payload: %w", err)
		}
		raw = b
	}
	return Envelope{
		Kind:    kind,
		TS:      time.Now().UTC(),
		Payload: raw,
	}, nil
}

// NewRequest constructs a frame with a freshly-generated ULID as its
// correlation id. Callers don't pass ids in — the package owns
// generation so collisions are impossible across concurrent goroutines
// and the id is monotonic within a millisecond window. Use the
// returned envelope's ID to track the pending response.
func NewRequest(kind Kind, payload any) (Envelope, error) {
	env, err := New(kind, payload)
	if err != nil {
		return Envelope{}, err
	}
	env.ID = newID()
	return env, nil
}

// NewResponse constructs a response frame echoing the request's id
// into InReplyTo.
func NewResponse(kind Kind, inReplyTo string, payload any) (Envelope, error) {
	env, err := New(kind, payload)
	if err != nil {
		return Envelope{}, err
	}
	env.InReplyTo = inReplyTo
	return env, nil
}

// Encode serialises an envelope to JSON.
func Encode(env Envelope) ([]byte, error) {
	if env.Kind == "" {
		return nil, errors.New("frames.Encode: missing kind")
	}
	return json.Marshal(env)
}

// Decode parses a wire byte slice into an envelope. Does NOT decode
// the payload — callers use PayloadAs to unmarshal into a concrete
// type once they've inspected Kind.
func Decode(raw []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Envelope{}, fmt.Errorf("frames.Decode: %w", err)
	}
	if env.Kind == "" {
		return Envelope{}, errors.New("frames.Decode: missing kind")
	}
	return env, nil
}

// PayloadAs unmarshals the envelope's payload into dst. Returns
// ErrNoPayload when the envelope has no payload at all — callers who
// legitimately expect an empty payload (Shutdown with defaults, etc.)
// should branch on errors.Is(err, ErrNoPayload); everyone else should
// treat it as a bug. Silent no-op would hide routing mistakes.
func PayloadAs(env Envelope, dst any) error {
	if len(env.Payload) == 0 {
		return ErrNoPayload
	}
	return json.Unmarshal(env.Payload, dst)
}

// IsKnown returns true if the kind is one the current build
// recognises. Callers that receive an unknown kind should log and
// drop the frame — do not error out (forward-compat: new kinds roll
// out without synchronised upgrades).
func IsKnown(k Kind) bool {
	switch k {
	case KindRegister, KindRegisterAck, KindDeregister,
		KindOutpostRegister, KindOutpostRegisterAck, KindOutpostDeregister,
		KindTurn, KindTurnResult,
		KindDispatch, KindDispatchResult, KindDispatchError,
		KindCWBRequest, KindCWBResponse,
		KindChatSend, KindChatDeliver, KindChatReaction, KindChatRead,
		KindChatReadResult, KindAnnounceFile, KindShareFile, KindFileResult,
		KindAspectActivity,
		KindKnowledgeStore, KindKnowledgeSearch, KindKnowledgeSearchResult,
		KindCredentialFetch, KindCredentialFetchResult,
		KindAspectModelConfigGet, KindAspectModelConfigGetResult,
		KindSessionEntryAppended, KindSessionRewind, KindSessionFork,
		KindSessionRefresh, KindSessionRefreshResult,
		KindShutdown,
		// Operator dashboard (dashboard-ws-port 5c)
		KindRosterList, KindRosterListResult,
		KindChatList, KindChatListResult,
		KindChatReplies, KindChatRepliesResult,
		KindReactionsFetch, KindReactionsFetchResult,
		KindChatReactionUpdate,
		KindKnowledgeList, KindKnowledgeListResult,
		KindKnowledgeStoreResult,
		KindAspectSay, KindAspectSayResult,
		KindRunsList, KindRunsListResult,
		KindRunGet, KindRunGetResult,
		KindActivityHistory, KindActivityHistoryResult,
		KindEnvHealth, KindEnvHealthResult,
		// Subscription frames (5d)
		KindSubscribeRoster, KindSubscribeChat, KindSubscribeAspectStatus,
		KindSubscribeObserve,
		KindUnsubscribeRoster, KindUnsubscribeChat, KindUnsubscribeAspectStatus,
		KindUnsubscribeObserve,
		KindSubscribeAck,
		KindRosterUpdate, KindAspectStatusPulse, KindObserveFrame,
		KindObserveBegin, KindObserveEvent, KindObserveEnd,
		// Operator escalation (P3c)
		KindEscalationRequest, KindEscalationDecision,
		// App-level liveness
		KindPing, KindPong:
		return true
	}
	return false
}
