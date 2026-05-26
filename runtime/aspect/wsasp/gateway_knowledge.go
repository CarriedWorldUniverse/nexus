// gateway_knowledge.go — out-of-process knowledge gateway for the
// funnel.
//
// Bug background (operator 2026-05-27): harrow reported "the
// knowledge store isn't connected". Root cause: agentfunnel and
// aspect runtime both constructed funnel.CommsRunner with no
// Knowledge field, so search_knowledge / store_knowledge tool calls
// returned "knowledge gateway not configured" straight from
// frame/funnel/comms.go:runStoreKnowledge / runSearchKnowledge. The
// in-process Frame had a gateway wired (framecomms.KnowledgeGateway
// against *knowledge.Store) but no equivalent existed for remote
// aspects despite the broker already wiring the wire surface
// (knowledge.search / knowledge.store frames; see broker/aspect_
// knowledge.go).
//
// This adapter closes the gap. wsasp.NewKnowledgeGateway(client)
// satisfies funnel.KnowledgeGateway against the existing frames —
// same wire path nexus-comms-mcp uses for its MCP-tool variant.

package wsasp

import (
	"context"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// requestSender is the subset of *Client KnowledgeGateway needs.
// Defined as an interface so tests can inject a stub that records
// envelopes + returns scripted responses without standing up a real
// WS server. Production callers pass a *Client via NewKnowledgeGateway.
type requestSender interface {
	Request(ctx context.Context, env frames.Envelope) (frames.Envelope, error)
}

// KnowledgeGateway adapts a wsasp.Client to funnel.KnowledgeGateway
// over the broker's knowledge.search / knowledge.store WS frames.
type KnowledgeGateway struct {
	sender requestSender
}

// NewKnowledgeGateway wraps the supplied wsasp.Client. The client
// MUST be the same one connected to the broker on behalf of this
// aspect — the broker stamps from_agent from the connection's
// authenticated identity, ignoring any value the caller might pass
// for fromAgent in StoreKnowledge.
func NewKnowledgeGateway(client *Client) *KnowledgeGateway {
	return &KnowledgeGateway{sender: client}
}

// newKnowledgeGatewayWithSender is the test-only constructor: lets
// tests inject a stub requestSender.
func newKnowledgeGatewayWithSender(s requestSender) *KnowledgeGateway {
	return &KnowledgeGateway{sender: s}
}

// StoreKnowledge upserts a knowledge entry under (caller, topic).
// fromAgent is informational only — the broker rebinds it to the
// connection's authenticated identity per the aspect_knowledge.go
// handler contract. Pass the caller's id anyway so logs are
// consistent and a future broker-side audit can cross-check.
func (g *KnowledgeGateway) StoreKnowledge(ctx context.Context, fromAgent, topic, content string, shared bool) (int64, error) {
	env, err := frames.NewRequest(frames.KindKnowledgeStore, frames.KnowledgeStorePayload{
		Topic:   topic,
		Content: content,
		Shared:  shared,
	})
	if err != nil {
		return 0, fmt.Errorf("wsasp: knowledge.store encode: %w", err)
	}
	resp, err := g.sender.Request(ctx, env)
	if err != nil {
		return 0, fmt.Errorf("wsasp: knowledge.store: %w", err)
	}
	var out frames.KnowledgeStoreResultPayload
	if err := frames.PayloadAs(resp, &out); err != nil {
		return 0, fmt.Errorf("wsasp: knowledge.store.result decode: %w", err)
	}
	return out.ID, nil
}

// SearchKnowledge runs the broker's knowledge.search and translates
// the result into the funnel-layer hit shape.
func (g *KnowledgeGateway) SearchKnowledge(ctx context.Context, q funnel.KnowledgeQuery) ([]funnel.KnowledgeHit, error) {
	env, err := frames.NewRequest(frames.KindKnowledgeSearch, frames.KnowledgeSearchPayload{
		Text:     q.Text,
		OwnAgent: q.OwnAgent,
		Shared:   q.Shared,
		Peers:    q.Peers,
		TopK:     q.TopK,
	})
	if err != nil {
		return nil, fmt.Errorf("wsasp: knowledge.search encode: %w", err)
	}
	resp, err := g.sender.Request(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("wsasp: knowledge.search: %w", err)
	}
	var out frames.KnowledgeSearchResultPayload
	if err := frames.PayloadAs(resp, &out); err != nil {
		return nil, fmt.Errorf("wsasp: knowledge.search.result decode: %w", err)
	}
	hits := make([]funnel.KnowledgeHit, len(out.Hits))
	for i, h := range out.Hits {
		hits[i] = funnel.KnowledgeHit{
			ID:        h.ID,
			FromAgent: h.FromAgent,
			Topic:     h.Topic,
			Content:   h.Content,
			Shared:    h.Shared,
			UpdatedAt: h.UpdatedAt,
			Score:     h.Score,
			Matched:   h.Matched,
		}
	}
	return hits, nil
}

// GetKnowledgeShared returns the current shared flag for the entry
// under (fromAgent, topic), used by CommsRunner.runStoreKnowledge to
// preserve operator-curated state when the model omits the shared
// field on a content-only refresh.
//
// No dedicated wire frame today; implemented via knowledge.search
// using the topic as the search text + own-agent scope, then exact
// topic+agent match against the hits. Cost is one extra FTS5 lookup
// per "shared-omitted refresh" call — rare path, acceptable.
//
// Returns ok=false when no matching entry exists; the caller then
// stores with shared=false (the default), matching the in-process
// gateway's behaviour for a brand-new topic.
func (g *KnowledgeGateway) GetKnowledgeShared(ctx context.Context, fromAgent, topic string) (bool, bool, error) {
	hits, err := g.SearchKnowledge(ctx, funnel.KnowledgeQuery{
		Text:     topic,
		OwnAgent: true,
		TopK:     20,
	})
	if err != nil {
		return false, false, err
	}
	for _, h := range hits {
		if h.Topic == topic && h.FromAgent == fromAgent {
			return h.Shared, true, nil
		}
	}
	return false, false, nil
}

// Compile-time interface check.
var _ funnel.KnowledgeGateway = (*KnowledgeGateway)(nil)
