// api.js — compatibility shim mapping the agent-network REST API
// surface onto the nexus WS RPC surface (comms.js). Every exported
// function below preserves the agent-network signature so views
// don't need bulk changes; they import the same names and get
// WS-backed implementations.
//
// Two non-trivial differences from the agent-network original:
//
//   1. AUTH STORAGE. agent-network kept the bearer token in
//      localStorage and reused it across page loads. Nexus uses an
//      operator JWT that is mint-on-login from a passkey
//      (dashboard-ws-port spec §2.2). For v1 we keep the JWT
//      in-memory only (refresh = re-login); the get/set/clear shims
//      below operate on a module-level holder so existing call sites
//      keep working without touching localStorage.
//
//   2. DEFERRED ENDPOINTS. /api/usage, /api/files, /api/tickets,
//      /api/topics aren't yet wired (chat_messages has no topic
//      column; tickets schema deferred per Crossing Part 2; usage
//      and files frames are spec-deferred per §3.5). The shims
//      reject with a "not yet wired" error so views render a clean
//      placeholder rather than hanging on a Promise.

import { rpc, send, onPushKind } from './comms.js';

export const BASE = window.location.origin;

// Page-size constants — kept at the agent-network values so views
// that import INITIAL_PAGE / etc continue to render the same
// initial slice.
export const INITIAL_PAGE = 50;

// Token persistence: in-memory holder + localStorage mirror.
//
// History: the spec originally called for memory-only auth ("refresh =
// re-auth"), but ten read sites grew up reading localStorage.auth_token
// directly (Terminal, AgentsView, Status, DocsView, harness-stream-store,
// Auth.useAuthGate, etc), and no write site ever populated it. Login.js
// stashed JWTs in-memory only, so on every page reload those readers
// got null OR a stale legacy hex token someone set externally — which
// then 401'd against the WS upgrade endpoint, leaving the operator
// looking at half-loaded REST chat history with no live delivery.
//
// Fix: setAuthToken writes through to localStorage so the contract every
// view assumed actually holds. Combined with the 24h JWT TTL bump
// (cmd/nexus/main.go) + /api/auth/check (broker/server.go), reload now
// detects a still-valid session and skips the WebAuthn modal.

const TOKEN_KEY = 'auth_token';

let authToken = (typeof localStorage !== 'undefined') ? localStorage.getItem(TOKEN_KEY) : null;

export function getAuthToken() {
  return authToken;
}

export function setAuthToken(t) {
  authToken = t;
  if (typeof localStorage === 'undefined') return;
  if (t) localStorage.setItem(TOKEN_KEY, t);
  else localStorage.removeItem(TOKEN_KEY);
}

export function clearAuthToken() {
  authToken = null;
  if (typeof localStorage !== 'undefined') localStorage.removeItem(TOKEN_KEY);
}

// fetchAgents → roster.list. nexus's RosterAspect uses `name` as the
// identifier; agent-network used `id`. SPA components (ChatInput,
// AgentsView, mention-autocomplete) read `.id`. Without this mapping
// every roster entry has `id === undefined` and the mention validator
// silently drops every aspect, leaving only the synthetic @all/@operator
// targets — which is the "test-keel wasn't a valid @ target" symptom.
export function fetchAgents() {
  return rpc('roster.list', {}).then((p) => (p.aspects || []).map((a) => ({
    ...a,
    id: a.id || a.name || '',
  })));
}

// fetchMessages — agent-network paginated by ?after=lastId; nexus
// chat.list takes the same shape. Channel routing collapses for v1
// since topics aren't persisted: 'general' is the global feed and
// 'topic:X' would need topic.messages which is deferred — same
// global feed for now (the SPA will see the merged stream until
// topics land).
// normalizeChatMessage maps nexus's ChatDeliverPayload shape onto the
// {created_at, ...} fields the SPA's MessageBubble + ThreadView render
// from. Without this, every message's timestamp shows "Invalid Date"
// because views read `m.created_at` and nexus only sends `received_at`.
// Mirrors the chat-ws.js shim's WS-path translation, applied here for
// the chat.list / chat.replies REST-via-WS pull paths.
function normalizeChatMessage(m) {
  if (!m) return m;
  return {
    ...m,
    created_at: m.created_at || m.received_at || '',
  };
}

export function fetchMessages(channel, afterId = 0) {
  const limit = afterId > 0 ? 100 : INITIAL_PAGE;
  return rpc('chat.list', {
    after_id: afterId,
    limit,
  }).then((p) => ({
    messages: (p.messages || []).map(normalizeChatMessage),
    has_more: p.has_more || false,
  }));
}

export function runsList(limit = 100) {
  return rpc('runs.list', { limit }).then((p) => p.runs || []);
}

export function runGet(runId) {
  return rpc('run.get', { run_id: runId }).then((p) => ({
    run: p.run || {},
    timeline: p.timeline || [],
    partial: !!p.partial,
  }));
}

export function activityHistory(runId, limit = 1000) {
  return rpc('activity.history', { run_id: runId, limit }).then((p) => ({
    items: p.items || [],
    partial: !!p.partial,
  }));
}

export function envHealth() {
  return rpc('env.health', {}).then((p) => p || {});
}

export function onRunsUpdate(handler) {
  return onPushKind('runs.update', handler);
}

// fetchOlderMessages — paginate backward via before_id.
export function fetchOlderMessages(channel, beforeId, limit = INITIAL_PAGE) {
  return rpc('chat.list', {
    before_id: beforeId,
    limit,
  }).then((p) => ({
    messages: (p.messages || []).map(normalizeChatMessage),
    has_more: p.has_more || false,
  }));
}

// sendMessage routes through aspect.say when an explicit @aspect
// recipient is implied, otherwise straight chat.send. The agent-
// network `to` parameter is gone — the SPA uses an explicit @-mention
// in the content. For v1 we delegate to chat.send always; the
// recipient policy on the broker side handles @-mention routing.
//
// imageUrl is dropped: image upload (POST /api/images) is deferred
// (no nexus equivalent in 5e). Views that pass imageUrl will see
// it ignored.
export function sendMessage({ from, content, replyTo = 0, topic = '', imageUrl = '' }) {
  // imageUrl is not yet supported on the WS path; surface it as a
  // text suffix so the operator can see what they tried to send.
  // Drops cleanly when imageUrl is empty.
  let body = content;
  if (imageUrl) body = body ? `${body}\n${imageUrl}` : imageUrl;
  // Fire-and-forget: the broker's chat.send handler doesn't emit a
  // .result frame; the message round-trips via the chat.deliver
  // subscription instead. Calling rpc() here wedged for 30s and
  // threw "rpc chat.send timed out" on every send. Returns a
  // resolved promise so call sites that `await sendMessage(...)`
  // continue to work.
  send('chat.send', {
    from: from || 'operator',
    content: body,
    reply_to: replyTo || 0,
    topic: topic || '',
  });
  return Promise.resolve();
}

// sendToAgent → aspect.say (the broker prepends @<aspect>).
export function sendToAgent(agentId, text) {
  return rpc('aspect.say', {
    aspect: agentId,
    content: text,
  });
}

// fetchTopics: deferred. chat_messages has no topic column today;
// topics.list lands in a follow-up part. Returns an empty array so
// the dashboard renders "no topics" without breaking.
export function fetchTopics(_limit = 15) {
  return Promise.resolve([]);
}

// fetchStatusAll → roster.list (same data, different agent-network
// route). Maps to the same payload as fetchAgents.
export function fetchStatusAll() {
  return fetchAgents();
}

// uploadImage: deferred. No image-upload frame today.
export function uploadImage(_file) {
  return Promise.reject(new Error('image upload not yet wired in nexus dashboard'));
}

// fetchKnowledge — Crossing Part 4 wired knowledge.list /
// knowledge.search at the funnel layer; 5c surfaced them on the WS.
export function fetchKnowledge({ agent, search, limit = 100 } = {}) {
  if (search) {
    return rpc('knowledge.search', {
      text: search,
      top_k: limit,
    }).then((p) => p.hits || []);
  }
  return rpc('knowledge.list', {
    agent: agent || '',
    limit,
  }).then((p) => p.entries || []);
}

// fetchFiles: deferred. shared-files frames not wired for operators
// yet (only available via the bridle tool surface for aspects, per
// Crossing Part 3).
export function fetchFiles(_owner) {
  return Promise.resolve([]);
}

// fetchUsage: deferred. No usage frame today.
export function fetchUsage(_period = '7d') {
  return Promise.resolve({ totals: {}, periods: [] });
}

// fetchTickets / fetchTicket: tickets schema deferred (Crossing
// Part 2). Empty list keeps the view rendering.
export function fetchTickets() {
  return Promise.resolve([]);
}

export function fetchTicket(_id) {
  return Promise.reject(new Error('tickets not yet wired in nexus dashboard'));
}

// fetchReplies → chat.replies
export function fetchReplies(parentId) {
  return rpc('chat.replies', { parent_id: Number(parentId) }).then((p) => p.messages || []);
}

// fetchReactionsForIds → chat.reactions.fetch.
// Result shape matches agent-network: { msgIdString: [{aspect, emoji}] }
export function fetchReactionsForIds(ids) {
  if (!ids || ids.length === 0) return Promise.resolve({});
  return rpc('chat.reactions.fetch', {
    msg_ids: ids.map((n) => Number(n)),
  }).then((p) => p.reactions || {});
}

// toggleReaction → existing react_to frame (Crossing Part 3 chat
// substrate). Available on the operator WS by virtue of being an
// aspect-side frame the broker accepts; the operator path uses the
// same kind.
export function toggleReaction(msgId, from, emoji) {
  return rpc('react_to', {
    msg_id: Number(msgId),
    emoji,
    // from is dropped — the broker stamps reactions with the
    // connecting identity (operator).
  });
}
