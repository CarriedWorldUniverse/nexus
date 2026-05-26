// Aspect-side knowledge frame handlers. Parallel to the operator
// handlers in operator_frames.go but scoped to the conn's
// authenticated aspect identity. Lets shadow-class CLI sessions
// (keel-cli, shadow, future operator-CLI aspects) read/write the
// knowledge store via nexus-comms-mcp without needing the operator's
// dashboard WS or operator JWT.
//
// Funnel-driven remote aspects (anvil, harrow, etc.) reach this
// surface in TWO ways:
//   1. via their agentfunnel CommsRunner using the wsasp.Knowledge-
//      Gateway adapter (PR #174) — that connection IS registered;
//   2. via a co-located nexus-comms-mcp using its own WS connection
//      — that connection may NOT be registered if the agentfunnel
//      already owns the aspect's roster slot (sendRegister fails
//      with ErrAlreadyRegistered). The auth middleware still has
//      proven the connection's identity (aspect JWT), so the
//      handlers fall back to c.auth.AgentID when c.registeredAs is
//      empty. Operator-reported 2026-05-27: "harrow tried to store
//      — broker rejected with 'connection not registered'".

package broker

import (
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
)

// aspectIdentity returns the connection's aspect identity for
// knowledge-frame handlers. Prefers c.registeredAs (the live-roster
// slot) so a registered aspect's identity is unambiguous. Falls
// back to c.auth.AgentID (the JWT-verified identity from the auth
// middleware) for connections that authenticated as an aspect but
// failed to register because the aspect already had a live slot
// (the nexus-comms-mcp-alongside-agentfunnel case).
//
// Returns "" only if both are empty — which the auth middleware
// should make impossible for non-operator connections.
//
// Operator connections never reach here: dispatchOperatorFrame
// intercepts knowledge.* frames upstream.
func (c *wsConn) aspectIdentity() string {
	if c.registeredAs != "" {
		return c.registeredAs
	}
	return c.auth.AgentID
}

// handleAspectKnowledgeSearch answers an aspect-issued knowledge.search.
// Scope is restricted to the caller's own entries + shared (operator-
// curated) — peer entries are not visible cross-aspect via this path.
// Per registration spec §2.8: aspects see own + shared; cross-peer
// reads need an explicit Peers list which the caller passes through.
func (c *wsConn) handleAspectKnowledgeSearch(env frames.Envelope) {
	kstore := c.broker.cfg.KnowledgeStore
	if kstore == nil {
		c.operatorError(env, "knowledge store not configured")
		return
	}
	aspectID := c.aspectIdentity()
	if aspectID == "" {
		c.operatorError(env, "knowledge.search: no aspect identity (connection not authenticated as aspect)")
		return
	}
	var p frames.KnowledgeSearchPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	if strings.TrimSpace(p.Text) == "" {
		c.operatorError(env, "text is required")
		return
	}
	const maxTopK = 50
	topK := p.TopK
	if topK > maxTopK {
		topK = maxTopK
	}
	// Default scope when the caller didn't specify: include the
	// caller's own entries + operator-curated shared. Matches the
	// funnel-side KnowledgeGateway default (frame/funnel/comms.go).
	ownAgent := p.OwnAgent
	shared := p.Shared
	if !ownAgent && !shared && len(p.Peers) == 0 {
		ownAgent = true
		shared = true
	}
	q := knowledge.Query{
		Text: p.Text,
		Scope: knowledge.Scope{
			Agent:    aspectID,
			OwnAgent: ownAgent,
			Shared:   shared,
			Peers:    p.Peers,
		},
		TopK:    topK,
		MaxRank: p.MaxRank,
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	hits, err := kstore.Search(ctx, q)
	if err != nil {
		c.operatorError(env, "search: "+err.Error())
		return
	}
	out := make([]frames.KnowledgeHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, frames.KnowledgeHit{
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
	resp, err := frames.NewResponse(frames.KindKnowledgeSearchResult, env.ID, frames.KnowledgeSearchResultPayload{Hits: out})
	if err != nil {
		c.log.Warn("knowledge.search: build response failed", "err", err)
		return
	}
	c.send(resp)
}

// handleAspectKnowledgeStore answers an aspect-issued knowledge.store.
// The row's from_agent is the conn's authenticated identity — aspects
// cannot impersonate each other via this path. The Shared flag is
// respected as the caller asks.
func (c *wsConn) handleAspectKnowledgeStore(env frames.Envelope) {
	kstore := c.broker.cfg.KnowledgeStore
	if kstore == nil {
		c.operatorError(env, "knowledge store not configured")
		return
	}
	aspectID := c.aspectIdentity()
	if aspectID == "" {
		c.operatorError(env, "knowledge.store: no aspect identity (connection not authenticated as aspect)")
		return
	}
	var p frames.KnowledgeStorePayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	if strings.TrimSpace(p.Topic) == "" || p.Content == "" {
		c.operatorError(env, "topic and content required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	id, err := kstore.Put(ctx, aspectID, p.Topic, p.Content, knowledge.PutOptions{Shared: p.Shared})
	if err != nil {
		c.operatorError(env, "store: "+err.Error())
		return
	}
	resp, err := frames.NewResponse(frames.KindKnowledgeStoreResult, env.ID, frames.KnowledgeStoreResultPayload{ID: id})
	if err != nil {
		c.log.Warn("knowledge.store: build response failed", "err", err)
		return
	}
	c.send(resp)
}
