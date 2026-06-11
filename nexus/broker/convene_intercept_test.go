package broker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/convene"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// fakeConveneStore records Insert/Close calls for assertion.
type fakeConveneStore struct {
	mu       sync.Mutex
	inserted []convene.Convene
	closed   []closeCall
	getFn    func(id string) (convene.Convene, error)
}

type closeCall struct {
	id      string
	status  convene.Status
	summary int64
}

func (s *fakeConveneStore) Migrate(context.Context) error { return nil }
func (s *fakeConveneStore) Insert(_ context.Context, c convene.Convene) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserted = append(s.inserted, c)
	return nil
}
func (s *fakeConveneStore) Close(_ context.Context, id string, st convene.Status, _ time.Time, summary int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, closeCall{id, st, summary})
	return nil
}
func (s *fakeConveneStore) Get(_ context.Context, id string) (convene.Convene, error) {
	if s.getFn != nil {
		return s.getFn(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.inserted {
		if c.ConveneID == id {
			return c, nil
		}
	}
	return convene.Convene{}, nil
}
func (s *fakeConveneStore) List(_ context.Context, limit int) ([]convene.Convene, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]convene.Convene, len(s.inserted))
	copy(out, s.inserted)
	return out, nil
}

func (s *fakeConveneStore) firstInsert(t *testing.T) convene.Convene {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inserted) == 0 {
		t.Fatal("no convene inserted")
	}
	return s.inserted[0]
}

// newConveneBroker wires a broker with a retaining chat store (so brief
// posts are visible), a wake controller over a fake scaler, and a fake
// convene store. Participants are registered in the wake-policy map as
// napping wake-on-mention so brief @mentions exercise the wake path.
func newConveneBroker(t *testing.T, scaler *fakeScaler, policies map[string]string) (*Broker, *retainingChatStore, *fakeConveneStore) {
	t.Helper()
	store := &retainingChatStore{}
	policy := &RecipientPolicy{}
	cv := &fakeConveneStore{}
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		ChatStore:          store,
		RecipientPolicy:    policy,
		Replayer:           NewReplayer(store, *policy),
		AspectWakePolicy:   policies,
		ConveneStore:       cv,
	}, roster.New())
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	b.wake = newWakeController(scaler, policies, nil, b.log)
	return b, store, cv
}

// TestConveneInterceptedRecordsAndBriefs verifies the full !convene path:
// the command post is stored as the thread root, a convene record is
// inserted (status open, facilitator defaulted), and one participant brief
// per aspect plus a facilitator brief are posted into the thread.
func TestConveneInterceptedRecordsAndBriefs(t *testing.T) {
	scaler := newFakeScaler()
	b, store, cv := newConveneBroker(t, scaler, map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"anvil": WakePolicyWakeOnMention,
	})

	rootID, err := b.HandleChatSend(context.Background(), "shadow",
		"!convene plumb anvil — should bridle adopt a registry?", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}

	rec := cv.firstInsert(t)
	if rec.Facilitator != "shadow" {
		t.Errorf("facilitator = %q, want shadow (convener default)", rec.Facilitator)
	}
	if rec.Status != convene.StatusOpen {
		t.Errorf("status = %q, want open", rec.Status)
	}
	if rec.RootMsgID != rootID {
		t.Errorf("root_msg_id = %d, want %d", rec.RootMsgID, rootID)
	}
	if len(rec.Participants) != 2 {
		t.Errorf("participants = %v, want 2", rec.Participants)
	}

	// Messages stored: the !convene root + 2 participant briefs + 1
	// facilitator brief = 4.
	store.mu.Lock()
	type briefSnap struct {
		content string
		replyTo int64
	}
	var msgs []briefSnap
	var joinedParts []string
	for _, m := range store.msgs {
		msgs = append(msgs, briefSnap{m.Content, m.ReplyTo})
		joinedParts = append(joinedParts, m.Content)
	}
	store.mu.Unlock()
	if len(msgs) != 4 {
		t.Fatalf("stored %d messages, want 4 (root + 2 participant briefs + facilitator)", len(msgs))
	}
	// Briefs thread under the root.
	for _, m := range msgs[1:] {
		if m.replyTo != rootID {
			t.Errorf("brief reply_to = %d, want root %d", m.replyTo, rootID)
		}
	}
	// Each participant brief @-mentions exactly that participant.
	joined := strings.Join(joinedParts, "\n")
	if !strings.Contains(joined, "@plumb") || !strings.Contains(joined, "@anvil") {
		t.Errorf("participant briefs missing @mentions: %q", joined)
	}
	// Facilitator brief @-mentions the facilitator and carries CONSENSUS:.
	if !strings.Contains(joined, "@shadow") || !strings.Contains(joined, "CONSENSUS:") {
		t.Errorf("facilitator brief missing @shadow / CONSENSUS contract: %q", joined)
	}
}

// TestConveneWakesNappingParticipants verifies the reuse-of-wake claim:
// the participant briefs' @mentions scale up napping participants with no
// special wake code in convene.
func TestConveneWakesNappingParticipants(t *testing.T) {
	scaler := newFakeScaler()
	b, _, _ := newConveneBroker(t, scaler, map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"anvil": WakePolicyWakeOnMention,
	})

	if _, err := b.HandleChatSend(context.Background(), "shadow",
		"!convene plumb anvil — design X", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	// Two briefs → two napping participants → two scale-ups.
	waitScale(t, scaler)
	waitScale(t, scaler)

	scaled := map[string]bool{}
	for _, c := range scaler.scaleCalls() {
		if c.replicas == 1 {
			scaled[c.name] = true
		}
	}
	if !scaled["plumb"] || !scaled["anvil"] {
		t.Errorf("expected both plumb and anvil scaled up, got %v", scaled)
	}
}

// TestConveneOperatorFacilitatorDefaultsShadow verifies an operator-sent
// convene defaults its facilitator to shadow.
func TestConveneOperatorFacilitatorDefaultsShadow(t *testing.T) {
	scaler := newFakeScaler()
	b, _, cv := newConveneBroker(t, scaler, map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"anvil": WakePolicyWakeOnMention,
	})
	if _, err := b.HandleChatSend(context.Background(), "operator",
		"!convene plumb anvil — design X", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	if got := cv.firstInsert(t).Facilitator; got != "shadow" {
		t.Errorf("facilitator = %q, want shadow (operator default)", got)
	}
}

// TestConveneRejectsEmptyFrom verifies the !convene intercept guards an
// empty from the same way the normal chat path does: rejected with the
// "from required" error and no convene root inserted into the chat store.
// (Symmetry with the normal-path guard; without it a convene thread root
// would be attributed to nobody and the facilitator would resolve empty.)
func TestConveneRejectsEmptyFrom(t *testing.T) {
	scaler := newFakeScaler()
	b, store, cv := newConveneBroker(t, scaler, map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"anvil": WakePolicyWakeOnMention,
	})
	_, err := b.HandleChatSend(context.Background(), "",
		"!convene plumb anvil — design X", 0, "")
	if err == nil {
		t.Fatal("expected error for empty from on !convene")
	}
	if !strings.Contains(err.Error(), "from required") {
		t.Errorf("err = %v, want 'from required (convene)'", err)
	}
	store.mu.Lock()
	n := len(store.msgs)
	store.mu.Unlock()
	if n != 0 {
		t.Errorf("stored %d messages, want 0 (rejected before insert)", n)
	}
	cv.mu.Lock()
	ncv := len(cv.inserted)
	cv.mu.Unlock()
	if ncv != 0 {
		t.Errorf("inserted %d convene records, want 0", ncv)
	}
}

// TestConveneRejectsDerivedSender verifies a !convene from a derived (hand)
// identity is rejected before anything happens — mirroring the !dispatch
// derived-rejection: convening a roundtable is a parent-tier capability, a
// hand must not. Nothing reaches the convene store; the hand's !convene post
// itself is not stored; a rejection note lands in the thread.
func TestConveneRejectsDerivedSender(t *testing.T) {
	scaler := newFakeScaler()
	b, store, cv := newConveneBroker(t, scaler, map[string]string{
		"plumb": WakePolicyWakeOnMention,
		"anvil": WakePolicyWakeOnMention,
	})
	if _, err := b.HandleChatSend(context.Background(), "shadow.umbra",
		"!convene plumb anvil — design X", 0, "spawn-7"); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	cv.mu.Lock()
	ncv := len(cv.inserted)
	cv.mu.Unlock()
	if ncv != 0 {
		t.Errorf("inserted %d convene records, want 0 (hands cannot convene)", ncv)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var rejection bool
	for _, m := range store.msgs {
		if strings.HasPrefix(strings.TrimSpace(m.Content), "!convene") {
			t.Errorf("the hand's !convene post must not be stored, got %q", m.Content)
		}
		if strings.Contains(m.Content, "hands cannot convene; ask your parent") {
			rejection = true
		}
	}
	if !rejection {
		t.Errorf("rejection note missing from thread; stored = %v", store.msgs)
	}
}

// TestConveneUnknownAspectNoRecord verifies a bad command does NOT insert a
// convene record (the post is still stored as ordinary text-root).
func TestConveneUnknownAspectNoRecord(t *testing.T) {
	scaler := newFakeScaler()
	b, _, cv := newConveneBroker(t, scaler, map[string]string{
		"plumb": WakePolicyWakeOnMention,
	})
	if _, err := b.HandleChatSend(context.Background(), "shadow",
		"!convene plumb ghost — design X", 0, ""); err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	cv.mu.Lock()
	n := len(cv.inserted)
	cv.mu.Unlock()
	if n != 0 {
		t.Errorf("inserted %d convene records, want 0 (parse rejected)", n)
	}
}
