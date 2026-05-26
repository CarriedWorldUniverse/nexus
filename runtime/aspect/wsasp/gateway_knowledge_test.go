package wsasp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// stubSender records every Request envelope and replies with the
// next scripted response. Tests use it to assert KnowledgeGateway's
// wire-encoding + result-decoding without a real WS server.
type stubSender struct {
	sent      []frames.Envelope
	responses []frames.Envelope
	err       error
}

func (s *stubSender) Request(_ context.Context, env frames.Envelope) (frames.Envelope, error) {
	s.sent = append(s.sent, env)
	if s.err != nil {
		return frames.Envelope{}, s.err
	}
	if len(s.responses) == 0 {
		return frames.Envelope{}, errors.New("stubSender: no scripted response")
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

// NEX-knowledge-fix: StoreKnowledge wraps knowledge.store frame +
// decodes the result envelope's id. Closes the operator-reported
// "knowledge store isnt connected" gap on remote agents.
func TestKnowledgeGateway_StoreKnowledge(t *testing.T) {
	resp, err := frames.NewResponse(frames.KindKnowledgeStoreResult, "", frames.KnowledgeStoreResultPayload{ID: 42})
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSender{responses: []frames.Envelope{resp}}
	g := newKnowledgeGatewayWithSender(stub)

	id, err := g.StoreKnowledge(context.Background(), "harrow", "topic-a", "content-a", true)
	if err != nil {
		t.Fatalf("StoreKnowledge: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}
	if len(stub.sent) != 1 {
		t.Fatalf("expected 1 sent envelope, got %d", len(stub.sent))
	}
	if stub.sent[0].Kind != frames.KindKnowledgeStore {
		t.Errorf("Kind = %q, want %q", stub.sent[0].Kind, frames.KindKnowledgeStore)
	}
	var p frames.KnowledgeStorePayload
	if err := json.Unmarshal(stub.sent[0].Payload, &p); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if p.Topic != "topic-a" || p.Content != "content-a" || !p.Shared {
		t.Errorf("payload = %+v", p)
	}
}

// SearchKnowledge wraps knowledge.search + translates the result
// into funnel.KnowledgeHit shape — caller's KnowledgeQuery fields
// must round-trip through the wire correctly.
func TestKnowledgeGateway_SearchKnowledge(t *testing.T) {
	resp, err := frames.NewResponse(frames.KindKnowledgeSearchResult, "", frames.KnowledgeSearchResultPayload{
		Hits: []frames.KnowledgeHit{
			{ID: 1, FromAgent: "harrow", Topic: "topic-a", Content: "a", Score: 0.9},
			{ID: 2, FromAgent: "operator", Topic: "topic-b", Content: "b", Shared: true, Score: 0.5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSender{responses: []frames.Envelope{resp}}
	g := newKnowledgeGatewayWithSender(stub)

	hits, err := g.SearchKnowledge(context.Background(), funnel.KnowledgeQuery{
		Text: "alpha", OwnAgent: true, Shared: true, TopK: 5,
	})
	if err != nil {
		t.Fatalf("SearchKnowledge: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].FromAgent != "harrow" || hits[1].Shared != true {
		t.Errorf("hits did not round-trip cleanly: %+v", hits)
	}

	// Wire payload must carry the query fields the broker filters on.
	var p frames.KnowledgeSearchPayload
	if err := json.Unmarshal(stub.sent[0].Payload, &p); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if p.Text != "alpha" || !p.OwnAgent || !p.Shared || p.TopK != 5 {
		t.Errorf("wire payload = %+v", p)
	}
}

// GetKnowledgeShared has no dedicated wire frame — it issues a
// knowledge.search and returns the shared flag of the exact-topic
// hit for the calling agent.
func TestKnowledgeGateway_GetKnowledgeShared_Found(t *testing.T) {
	resp, _ := frames.NewResponse(frames.KindKnowledgeSearchResult, "", frames.KnowledgeSearchResultPayload{
		Hits: []frames.KnowledgeHit{
			{ID: 1, FromAgent: "harrow", Topic: "topic-a", Shared: true},
			{ID: 2, FromAgent: "anvil", Topic: "topic-a", Shared: false}, // peer match — skipped
		},
	})
	stub := &stubSender{responses: []frames.Envelope{resp}}
	g := newKnowledgeGatewayWithSender(stub)

	shared, ok, err := g.GetKnowledgeShared(context.Background(), "harrow", "topic-a")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !shared {
		t.Errorf("shared=%v ok=%v; want shared=true ok=true", shared, ok)
	}
}

// GetKnowledgeShared returns ok=false on miss so callers can store
// with shared=false (matches the in-process gateway behaviour for a
// brand-new topic).
func TestKnowledgeGateway_GetKnowledgeShared_NotFound(t *testing.T) {
	resp, _ := frames.NewResponse(frames.KindKnowledgeSearchResult, "", frames.KnowledgeSearchResultPayload{
		Hits: []frames.KnowledgeHit{
			{ID: 99, FromAgent: "harrow", Topic: "other-topic", Shared: true},
		},
	})
	stub := &stubSender{responses: []frames.Envelope{resp}}
	g := newKnowledgeGatewayWithSender(stub)

	shared, ok, err := g.GetKnowledgeShared(context.Background(), "harrow", "missing-topic")
	if err != nil {
		t.Fatal(err)
	}
	if ok || shared {
		t.Errorf("shared=%v ok=%v; want shared=false ok=false (miss)", shared, ok)
	}
}

// Surface-level errors from the underlying Request propagate to the
// caller so the funnel's runStoreKnowledge / runSearchKnowledge can
// emit a meaningful tool-result error to the model (instead of
// silently corrupting state).
func TestKnowledgeGateway_PropagatesRequestError(t *testing.T) {
	stub := &stubSender{err: errors.New("ws disconnected")}
	g := newKnowledgeGatewayWithSender(stub)

	if _, err := g.StoreKnowledge(context.Background(), "x", "t", "c", false); err == nil {
		t.Error("StoreKnowledge must propagate sender error")
	}
	if _, err := g.SearchKnowledge(context.Background(), funnel.KnowledgeQuery{Text: "x"}); err == nil {
		t.Error("SearchKnowledge must propagate sender error")
	}
}
