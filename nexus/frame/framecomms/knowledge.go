// Knowledge gateway: in-process funnel.KnowledgeGateway implemented
// against *knowledge.Store. Lives alongside the chat Gateway so
// cmd/nexus has a single place to assemble both seams when wiring
// the embedded Frame's CommsRunner.

package framecomms

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
)

// KnowledgeGateway adapts a *knowledge.Store to the
// funnel.KnowledgeGateway interface. The Store is required; nil
// surfaces as a tool-result error at call time (same pattern as
// chat Gateway).
type KnowledgeGateway struct {
	Store *knowledge.Store
}

// Compile-time check that KnowledgeGateway satisfies the funnel
// interface. Catches method-set drift the moment funnel adds a new
// method without the framecomms adapter being updated to match.
var _ funnel.KnowledgeGateway = (*KnowledgeGateway)(nil)

// NewKnowledgeGateway wires a gateway around a Store handle.
func NewKnowledgeGateway(store *knowledge.Store) *KnowledgeGateway {
	return &KnowledgeGateway{Store: store}
}

func (g *KnowledgeGateway) GetKnowledgeShared(ctx context.Context, fromAgent, topic string) (bool, bool, error) {
	if g.Store == nil {
		return false, false, fmt.Errorf("framecomms.KnowledgeGateway: no store configured")
	}
	e, err := g.Store.Get(ctx, fromAgent, topic)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	return e.Shared, true, nil
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
