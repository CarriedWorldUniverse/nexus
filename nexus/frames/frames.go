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
	KindRegister        Kind = "register"
	KindRegisterAck     Kind = "register.ack"
	KindDeregister      Kind = "deregister"
	KindOutpostRegister    Kind = "outpost.register"
	KindOutpostRegisterAck Kind = "outpost.register.ack"
	KindOutpostDeregister  Kind = "outpost.deregister"

	// Turn dispatch — upstream asks an aspect to run a single turn.
	KindTurn       Kind = "turn"
	KindTurnResult Kind = "turn.result"

	// Hand dispatch — stateless single-turn invocation routed through
	// a dispatcher. hand.error when dispatch couldn't happen at all
	// (target offline, queue saturated, unknown hand).
	KindHandDispatch Kind = "hand.dispatch"
	KindHandResult   Kind = "hand.result"
	KindHandError    Kind = "hand.error"

	// Chat — the existing comms surface in frame form.
	KindChatSend     Kind = "chat.send"
	KindChatDeliver  Kind = "chat.deliver"
	KindChatReaction Kind = "chat.reaction"
	KindChatRead     Kind = "chat.read"

	// Knowledge — aspect-to-Nexus store/query.
	KindKnowledgeStore        Kind = "knowledge.store"
	KindKnowledgeSearch       Kind = "knowledge.search"
	KindKnowledgeSearchResult Kind = "knowledge.search.result"

	// Session observability — projection upward for dashboard view.
	// Local aspect JSONL is source of truth; Nexus keeps a read-only
	// mirror for rendering.
	KindSessionEntryAppended Kind = "session.entry.appended"
	KindSessionRewind        Kind = "session.rewind"
	KindSessionFork          Kind = "session.fork"

	// Lifecycle.
	KindShutdown Kind = "shutdown"
)

// Envelope is the shared shape of every frame.
type Envelope struct {
	Kind       Kind            `json:"kind"`
	ID         string          `json:"id,omitempty"`
	InReplyTo  string          `json:"in_reply_to,omitempty"`
	TS         time.Time       `json:"ts"`
	Payload    json.RawMessage `json:"payload,omitempty"`
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
		KindHandDispatch, KindHandResult, KindHandError,
		KindChatSend, KindChatDeliver, KindChatReaction, KindChatRead,
		KindKnowledgeStore, KindKnowledgeSearch, KindKnowledgeSearchResult,
		KindSessionEntryAppended, KindSessionRewind, KindSessionFork,
		KindShutdown:
		return true
	}
	return false
}
