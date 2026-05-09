// Knowledge gateway: in-process funnel.KnowledgeGateway implemented
// against *knowledge.Store. Lives alongside the chat Gateway so
// cmd/nexus has a single place to assemble both seams when wiring
// the embedded Frame's CommsRunner.

package framecomms

import (
	"context"
	"fmt"

	"github.com/nexus-cw/nexus/nexus/frame/funnel"
	"github.com/nexus-cw/nexus/nexus/knowledge"
)

// KnowledgeGateway adapts a *knowledge.Store to the
// funnel.KnowledgeGateway interface. The Store is required; nil
// surfaces as a tool-result error at call time (same pattern as
// chat Gateway).
type KnowledgeGateway struct {
	Store *knowledge.Store
}

// NewKnowledgeGateway wires a gateway around a Store handle.
func NewKnowledgeGateway(store *knowledge.Store) *KnowledgeGateway {
	return &KnowledgeGateway{Store: store}
}

func (g *KnowledgeGateway) StoreKnowledge(ctx context.Context, fromAgent, topic, content string, shared bool) (int64, error) {
	if g.Store == nil {
		return 0, fmt.Errorf("framecomms.KnowledgeGateway: no store configured")
	}
	return g.Store.Put(ctx, fromAgent, topic, content, knowledge.PutOptions{Shared: shared})
}

func (g *KnowledgeGateway) SearchKnowledge(ctx context.Context, q funnel.KnowledgeQuery) ([]funnel.KnowledgeHit, error) {
	if g.Store == nil {
		return nil, fmt.Errorf("framecomms.KnowledgeGateway: no store configured")
	}
	hits, err := g.Store.Search(ctx, knowledge.Query{
		Text: q.Text,
		Scope: knowledge.Scope{
			Agent:    q.Agent,
			OwnAgent: q.OwnAgent,
			Shared:   q.Shared,
			Peers:    q.Peers,
		},
		TopK: q.TopK,
	})
	if err != nil {
		return nil, err
	}
	out := make([]funnel.KnowledgeHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, funnel.KnowledgeHit{
			ID:        h.ID,
			FromAgent: h.FromAgent,
			Topic:     h.Topic,
			Content:   h.Content,
			Shared:    h.Shared,
			UpdatedAt: h.UpdatedAt,
			Score:     h.Score,
			Matched:   h.Matched,
		})
	}
	return out, nil
}
