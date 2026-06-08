// Operator dashboard WS frame handlers — request/response shape per
// dashboard-ws-port spec §3.2. Aspect-aware frames already live in
// ws.go (chat.send, chat.read, etc.); these handlers serve the
// dashboard SPA's data plane.
//
// All handlers in this file:
//
//   - Gate on c.auth.Operator: aspects must NOT see operator
//     frames (they have the bridle-tool surface for similar
//     functionality, and operator frames bypass scope rules that
//     aspects shouldn't bypass).
//   - Echo correlation_id via frames.NewResponse(env.ID).
//   - Map storage failures to a `.error`-suffixed kind so the SPA's
//     comms.js can reject the matching Promise without a separate
//     parse path.
//
// Lives separately from ws.go to keep the aspect WS surface (which
// is well-tested and security-critical) untouched by dashboard-only
// growth.

package broker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
)

// dispatchOperatorFrame routes operator-only frame kinds. Returns
// true when the kind was handled, false for caller fallthrough.
//
// Called from wsConn.dispatch BEFORE the aspect frame switch so
// operator frames can override aspect-named frames if any future
// kind collides.
func (c *wsConn) dispatchOperatorFrame(env frames.Envelope) bool {
	if !c.auth.Operator {
		return false
	}
	// Subscription frames first — they're stateful flips with no
	// store dependency and the ack must land regardless of whether
	// stores are configured.
	if c.dispatchOperatorSubFrame(env) {
		return true
	}
	switch env.Kind {
	case frames.KindRosterList:
		c.handleOperatorRosterList(env)
	case frames.KindChatList:
		c.handleOperatorChatList(env)
	case frames.KindChatReplies:
		c.handleOperatorChatReplies(env)
	case frames.KindReactionsFetch:
		c.handleOperatorReactionsFetch(env)
	case frames.KindKnowledgeList:
		c.handleOperatorKnowledgeList(env)
	case frames.KindKnowledgeSearch:
		c.handleOperatorKnowledgeSearch(env)
	case frames.KindKnowledgeStore:
		c.handleOperatorKnowledgeStore(env)
	case frames.KindAspectSay:
		c.handleOperatorAspectSay(env)
	case frames.KindRunsList:
		c.handleOperatorRunsList(env)
	case frames.KindRunGet:
		c.handleOperatorRunGet(env)
	case frames.KindRunCancel:
		c.handleOperatorRunCancel(env)
	case frames.KindActivityHistory:
		c.handleOperatorActivityHistory(env)
	case frames.KindEnvHealth:
		c.handleOperatorEnvHealth(env)
	case frames.KindEscalationDecision:
		// P3c: operator answered a paused tool call. Route the decision
		// back to the originating aspect's connection.
		c.handleEscalationDecisionFrame(env)
	default:
		return false
	}
	return true
}

// opCtx returns the context to use for operator handler queries.
// Falls back to context.Background when the broker hasn't recorded
// its parent (test paths where ListenAndServe didn't run). Bounded
// by a per-call timeout so a stuck DB query can't hold a WS write.
func (c *wsConn) opCtx() (context.Context, context.CancelFunc) {
	parent := c.broker.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 10*time.Second)
}

// errorResponse sends an envelope with kind "<base>.error" carrying
// {error: msg}. Mirrors the dispatch.error pattern existing aspect
// frames use. Wrapped here so each handler is one line for the
// failure path.
func (c *wsConn) operatorError(env frames.Envelope, msg string) {
	errKind := frames.Kind(string(env.Kind) + ".error")
	resp, _ := frames.NewResponse(errKind, env.ID, map[string]string{"error": msg})
	c.send(resp)
}

// --- roster.list ---

func (c *wsConn) handleOperatorRosterList(env frames.Envelope) {
	if c.broker.roster == nil {
		c.operatorError(env, "roster not configured")
		return
	}
	rows := c.broker.roster.List()
	out := make([]frames.RosterAspect, 0, len(rows))
	for _, r := range rows {
		var lastSeen string
		if !r.LastHeartbeat.IsZero() {
			lastSeen = r.LastHeartbeat.UTC().Format(time.RFC3339)
		}
		out = append(out, frames.RosterAspect{
			Name:         r.Name,
			Status:       r.Status,
			LastSeen:     lastSeen,
			Capabilities: r.Capabilities,
			Model:        r.Model,
			Provider:     r.Provider,
			ContextMode:  string(r.ContextMode),
			Role:         "", // schema.AspectState doesn't carry role today
		})
	}
	resp, _ := frames.NewResponse(frames.KindRosterListResult, env.ID, frames.RosterListResultPayload{Aspects: out})
	c.send(resp)
}

// --- chat.list ---

func (c *wsConn) handleOperatorChatList(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		c.operatorError(env, "chat store not configured")
		return
	}
	var p frames.ChatListPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	if p.BeforeID < 0 || p.AfterID < 0 || p.Limit < 0 {
		c.operatorError(env, "before_id, after_id, limit must be non-negative")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	msgs, hasMore, err := store.ListPage(ctx, p.BeforeID, p.AfterID, p.Limit)
	if err != nil {
		c.operatorError(env, "list: "+err.Error())
		return
	}
	deliv := make([]frames.ChatDeliverPayload, 0, len(msgs))
	for _, m := range msgs {
		deliv = append(deliv, frames.ChatDeliverPayload{
			ID:         int(m.ID),
			From:       m.From,
			Content:    m.Content,
			ReplyTo:    int(m.ReplyTo),
			ReceivedAt: m.CreatedAt.UTC().Format(time.RFC3339),
			ReplyCount: m.ReplyCount, // ListPage's recursive subtree count
			ThreadRoot: int(m.ThreadRootMsgID),
		})
	}
	resp, _ := frames.NewResponse(frames.KindChatListResult, env.ID, frames.ChatListResultPayload{
		Messages: deliv,
		HasMore:  hasMore,
	})
	c.send(resp)
}

// --- chat.replies ---

func (c *wsConn) handleOperatorChatReplies(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		c.operatorError(env, "chat store not configured")
		return
	}
	var p frames.ChatRepliesPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	if p.ParentID <= 0 {
		c.operatorError(env, "parent_id required")
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	// ListThread (recursive CTE) returns the parent + every descendant
	// in the subtree, not just direct children. The SPA's ThreadView
	// renders the result flat and doesn't recurse on its own, so using
	// the shallow ListReplies hid depth-2+ replies entirely — keel
	// replies to your reply never showed up in the thread view, even
	// though they were in the DB. Recursive walk + drop the root from
	// the result (the dashboard already renders it in the parent feed).
	const maxThread = 500
	all, err := store.ListThread(ctx, p.ParentID, 0, maxThread)
	if err != nil {
		c.operatorError(env, "list: "+err.Error())
		return
	}
	out := make([]frames.ChatDeliverPayload, 0, len(all))
	for _, m := range all {
		if m.ID == p.ParentID {
			continue // ListThread includes the root; the SPA already has it
		}
		out = append(out, frames.ChatDeliverPayload{
			ID:         int(m.ID),
			From:       m.From,
			Content:    m.Content,
			ReplyTo:    int(m.ReplyTo),
			ReceivedAt: m.CreatedAt.UTC().Format(time.RFC3339),
			ThreadRoot: int(m.ThreadRootMsgID),
		})
	}
	resp, _ := frames.NewResponse(frames.KindChatRepliesResult, env.ID, frames.ChatRepliesResultPayload{
		ParentID: p.ParentID,
		Messages: out,
	})
	c.send(resp)
}

// --- chat.reactions.fetch ---

func (c *wsConn) handleOperatorReactionsFetch(env frames.Envelope) {
	store := c.broker.cfg.ChatStore
	if store == nil {
		c.operatorError(env, "chat store not configured")
		return
	}
	var p frames.ReactionsFetchPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	const maxBatch = 500
	if len(p.MsgIDs) > maxBatch {
		c.operatorError(env, fmt.Sprintf("too many msg_ids (max %d)", maxBatch))
		return
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	rows, err := store.GetReactions(ctx, p.MsgIDs)
	if err != nil {
		c.operatorError(env, "fetch: "+err.Error())
		return
	}
	out := make(map[string][]frames.ReactionRow, len(rows))
	for msgID, list := range rows {
		key := fmt.Sprintf("%d", msgID)
		converted := make([]frames.ReactionRow, 0, len(list))
		for _, r := range list {
			converted = append(converted, frames.ReactionRow{Aspect: r.Aspect, Emoji: r.Emoji})
		}
		out[key] = converted
	}
	resp, _ := frames.NewResponse(frames.KindReactionsFetchResult, env.ID, frames.ReactionsFetchResultPayload{
		Reactions: out,
	})
	c.send(resp)
}

// --- knowledge.list ---

func (c *wsConn) handleOperatorKnowledgeList(env frames.Envelope) {
	kstore := c.broker.cfg.KnowledgeStore
	if kstore == nil {
		c.operatorError(env, "knowledge store not configured")
		return
	}
	var p frames.KnowledgeListPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	agent := strings.TrimSpace(p.Agent)
	ctx, cancel := c.opCtx()
	defer cancel()
	// Empty agent filter → cross-agent listing so the dashboard's default
	// knowledge view surfaces every entry (canon, research, per-aspect
	// notes, operator-authored). Previous behavior defaulted to
	// agent="operator" which hid all migrated rows.
	var entries []knowledge.Entry
	var err error
	if agent == "" {
		entries, err = kstore.ListAll(ctx, p.Limit)
	} else {
		entries, err = kstore.List(ctx, agent, p.Limit)
	}
	if err != nil {
		c.operatorError(env, "list: "+err.Error())
		return
	}
	hits := make([]frames.KnowledgeHit, 0, len(entries))
	for _, e := range entries {
		hits = append(hits, knowledgeEntryToFrame(e))
	}
	resp, _ := frames.NewResponse(frames.KindKnowledgeListResult, env.ID, frames.KnowledgeListResultPayload{Entries: hits})
	c.send(resp)
}

// --- knowledge.search ---

func (c *wsConn) handleOperatorKnowledgeSearch(env frames.Envelope) {
	kstore := c.broker.cfg.KnowledgeStore
	if kstore == nil {
		c.operatorError(env, "knowledge store not configured")
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
	// Operator scope: see everything. We override OwnAgent/Shared/Peers
	// so the operator's view is unrestricted (per spec §2.4 — operator
	// reads all scopes by design).
	q := knowledge.Query{
		Text: p.Text,
		Scope: knowledge.Scope{
			Agent:    "operator",
			OwnAgent: true,
			Shared:   true,
			Peers:    p.Peers,
		},
		TopK:    topK,
		MaxRank: p.MaxRank,
	}
	// If the caller explicitly scoped narrower, honor that. Today the
	// payload doesn't distinguish "default" from "explicit false" for
	// OwnAgent/Shared (no *bool), so we accept the broad-by-default
	// behavior; a follow-up can refine if operator views need
	// per-aspect filtering at the WS layer.
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
	resp, _ := frames.NewResponse(frames.KindKnowledgeSearchResult, env.ID, frames.KnowledgeSearchResultPayload{Hits: out})
	c.send(resp)
}

// --- knowledge.store ---

func (c *wsConn) handleOperatorKnowledgeStore(env frames.Envelope) {
	kstore := c.broker.cfg.KnowledgeStore
	if kstore == nil {
		c.operatorError(env, "knowledge store not configured")
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
	id, err := kstore.Put(ctx, "operator", p.Topic, p.Content, knowledge.PutOptions{Shared: p.Shared})
	if err != nil {
		c.operatorError(env, "store: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindKnowledgeStoreResult, env.ID, frames.KnowledgeStoreResultPayload{ID: id})
	c.send(resp)
}

// --- aspect.say ---

func (c *wsConn) handleOperatorAspectSay(env frames.Envelope) {
	var p frames.AspectSayPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.operatorError(env, "malformed payload: "+err.Error())
		return
	}
	aspect := strings.TrimSpace(p.Aspect)
	content := strings.TrimSpace(p.Content)
	if aspect == "" || content == "" {
		c.operatorError(env, "aspect and content required")
		return
	}
	// Sugar over chat.send: prepend "@<aspect> " so the message is
	// addressed via the existing recipient-policy path. If the
	// content already starts with @aspect-handle, leave it alone.
	mentioned := "@" + aspect
	if !strings.HasPrefix(content, mentioned) {
		content = mentioned + " " + content
	}
	ctx, cancel := c.opCtx()
	defer cancel()
	// reply_to is hardcoded to 0 here — AspectSayPayload doesn't
	// carry a reply_to field today (per dashboard-ws-port spec §3.2,
	// aspect.say is a top-level "talk to this aspect" affordance).
	// If the dashboard ever needs threaded replies into a specific
	// message, add the field to the payload and forward it here.
	msgID, err := c.broker.HandleChatSend(ctx, "operator", content, 0, "")
	if err != nil {
		// Failures here include both transient (context cancellation)
		// and permanent (validation rejections). The SPA's comms.js
		// rejects the matching Promise on any ".error" kind; we don't
		// distinguish further today. If the dashboard grows
		// retry-worthy classification, encode it in the payload.error
		// field rather than splitting the kind.
		c.operatorError(env, "send: "+err.Error())
		return
	}
	resp, _ := frames.NewResponse(frames.KindAspectSayResult, env.ID, frames.AspectSayResultPayload{MsgID: msgID})
	c.send(resp)
}

// knowledgeEntryToFrame translates a knowledge.Entry into the
// frames.KnowledgeHit shape used as a list-row across both list and
// search results. Score/Matched left zero since list isn't a search.
func knowledgeEntryToFrame(e knowledge.Entry) frames.KnowledgeHit {
	return frames.KnowledgeHit{
		ID:        e.ID,
		FromAgent: e.FromAgent,
		Topic:     e.Topic,
		Content:   e.Content,
		Shared:    e.Shared,
		UpdatedAt: e.UpdatedAt,
	}
}
