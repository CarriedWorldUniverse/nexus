# Dashboard SPA — WebSocket Port Spec

**Status:** Draft, awaiting operator review
**Date:** 2026-05-09
**Supersedes Crossing Part 5 of 2026-05-07-crossing-migration-spec.md (was: REST port-as-is)
**Driving framing:** Operator (chat 2026-05-09) — "we need to move the SPA to the websocket comms." No new REST endpoints land in nexus broker for the dashboard. The SPA becomes a comms-client peer over the existing `/connect` WS surface, sending/receiving the same envelope frames aspects use plus a small set of dashboard-specific frames. Operator authenticates via passkey-unlocked keyfile generated at login.

## 1. Why this changes shape

Original Crossing Part 5 said "copy SPA, adapt endpoints." That implicitly assumed the agent-network REST surface (~20 endpoints — `/api/chat`, `/api/topics`, `/api/agents`, `/api/knowledge`, `/api/tickets`, `/api/files`, `/api/usage`, `/api/chat/:id/react`, etc.) would be ported to nexus broker.

Operator decision: don't add that REST surface. The dashboard goes WS-native. Two reasons this is the right call:

1. **One transport, one auth model.** Aspects already speak `/connect` with envelope frames; adding REST means two parallel surfaces with separate auth, separate session lifecycle, separate testing. Collapsing onto WS keeps the broker's contract narrow.
2. **Live-by-default.** Chat, status, and roster views all want push. Building them on REST means polling or layering a parallel WS for push and HTTP for queries; one WS surface gives push and request/response both via correlated frames.

The cost: define a request/response correlation pattern over WS (today the envelope is mostly fire-and-forget chat fan-out — request/response with `correlation_id` is a small extension, not new shape). And: the SPA's `js/api.js` becomes `js/comms.js`, a thin RPC layer over the WS connection.

## 2. Identity & auth

### 2.1 Operator identity

The operator is a first-class identity on the network — not an aspect, not Frame, but a registered principal. Internally treat as `Role = RoleOperator` (extends `shared/schemas.aspect.Role`). The roster row:

```
name: "operator"
role: "operator"
context_mode: "stateless"  // operator browser is per-session, no persistent thread/global context
provider: ""               // no AI; human-driven
capabilities: ["chat-send", "chat-read", "knowledge-rw", "admin"]
```

(Capabilities are advisory metadata; auth gates are enforced server-side per §2.4.)

A separate operator identity (vs. reusing `frame` or `admin`) means:
- `from: "operator"` posts in chat are properly attributed (today this is implicit; with a real identity it's explicit and verifiable).
- Operator can hold its own knowledge entries (`from_agent="operator"`) — the operator's curated facts are first-class.
- Future: multiple operators on the same Nexus get distinct identities (`operator:jacinta`, `operator:colleague`) without protocol changes.

### 2.2 Passkey → keyfile flow

The operator owns a WebAuthn passkey registered against this Nexus. At login the SPA:

1. Browser presents the WebAuthn assertion (existing platform API, no library needed beyond the browser primitives).
2. SPA POSTs `{credential_id, signed_challenge}` to `POST /api/operator/login` (the *only* new HTTP endpoint this spec adds — everything else is WS).
3. Broker verifies the assertion against the registered passkey, and on success **mints a fresh keyfile in-memory** and returns `{keyfile_envelope, encrypted_payload}` — same shape aspects load from disk, but never persisted to the operator's filesystem.
4. SPA holds the keyfile + payload in memory for the session. Page refresh = re-authenticate.
5. SPA runs the standard keyfile validation handshake (`GET /api/nexus_id` → compare → `POST /api/aspect/validate`) — the existing flow, unchanged. **No new endpoint.** This issues a session JWT.
6. SPA opens `/connect?token=<jwt>` for WS comms.

The "in-memory keyfile" pattern matters: the dashboard reuses 100% of the existing aspect validation endpoint and JWT issuance — broker doesn't grow a parallel session-token path. The dashboard is, mechanically, a short-lived aspect with `Role: RoleOperator`.

**Why not a long-lived dashboard token?** Forces re-auth on every session. Browser passkey is one tap; long-lived bearer in localStorage is the larger risk. Also: rotating keyfiles per session means a stolen JWT has at most that session's TTL of damage.

### 2.3 Passkey registration

First-time registration is operator-driven via a one-shot CLI: `nexus operator register-passkey`. Prompts the operator to complete the WebAuthn ceremony in a browser tab pointed at `https://<nexus-host>/operator/register?token=<one-time-pin>`. Persists the passkey credential id + public key + sign-counter in a new `operator_passkeys` table (schema in §6.1).

Registration is rare (once per device the operator wants to log in from). v1 supports multiple registered passkeys — re-register on each device. No recovery flow in v1; if the operator loses all devices, `nexus operator reset-passkey` from a privileged shell on the Nexus host clears the table and re-runs registration.

### 2.4 Operator's permission set

Operator JWT carries `sub: "operator"`. Server-side gates:

- `chat.send` from operator: always allowed.
- `chat.read` / `chat.deliver` subscriptions: operator subscribes to **all topics** by default (the dashboard is the operator's view of everything; aspect-level filtering doesn't apply).
- `knowledge.store` / `knowledge.search`: operator can read all entries (override Scope.Agent + Scope.Shared rules — dashboard sees everything by design); operator can write to own scope only (`from_agent="operator"`).
- `admin.*` frames (per §3.4): allowed only when token has `admin: true` claim. Operator JWTs are admin by default — operator IS the admin in this Nexus. Future: tiered operators (read-only viewer roles) get `admin: false`.

## 3. WS frame inventory

This is the contract between SPA and broker. Three categories.

### 3.1 Existing frames the dashboard reuses unchanged

- `chat.send` — operator posts a message. (Already implemented; broker's HandleChatSend.)
- `chat.deliver` — broker pushes new messages to the operator. (Already pushed to aspects on recipient match; operator subscribes to all.)
- `chat.read` — fetch a thread. (Already implemented for aspects; same handler.)
- `chat.read.result` — response shape. Already exists.
- `react` — toggle a reaction. (Already implemented.)
- `register` — connect handshake; the operator's connection registers as `name: "operator", role: "operator"`. No code change beyond accepting the role.

### 3.2 New frames for dashboard-side request/response

These are query frames the SPA needs to render views. All correlated via `correlation_id` (UUID set by client; broker echoes in the response).

**Frame: `roster.list` → `roster.list.result`**
```json
// request
{ "type": "roster.list", "correlation_id": "..." }
// response
{ "type": "roster.list.result", "correlation_id": "...",
  "aspects": [{ "name": "anvil", "status": "online", "last_seen": "...", "capabilities": [...], "model": "..." }, ...] }
```
Replaces `/api/aspects` + `/api/status/all`.

**Frame: `topics.list` → `topics.list.result`**
```json
{ "type": "topics.list", "correlation_id": "...", "limit": 50 }
{ "type": "topics.list.result", "correlation_id": "...",
  "topics": [{ "name": "...", "last_msg_id": ..., "msg_count": ..., "last_activity": "..." }, ...] }
```
Replaces `/api/topics`.

**Frame: `topic.messages` → `topic.messages.result`**
```json
{ "type": "topic.messages", "correlation_id": "...",
  "topic": "...", "before_id": 0, "after_id": 0, "limit": 100 }
{ "type": "topic.messages.result", "correlation_id": "...",
  "topic": "...", "messages": [...], "has_more": false }
```
Replaces `/api/topics/:t/messages`.

**Frame: `chat.replies` → `chat.replies.result`**
```json
{ "type": "chat.replies", "correlation_id": "...", "parent_id": 1234 }
{ "type": "chat.replies.result", "correlation_id": "...",
  "parent_id": 1234, "messages": [...] }
```
Replaces `/api/chat/:parentId/replies`.

**Frame: `chat.reactions.fetch` → `chat.reactions.fetch.result`**
```json
{ "type": "chat.reactions.fetch", "correlation_id": "...", "msg_ids": [1,2,3] }
{ "type": "chat.reactions.fetch.result", "correlation_id": "...",
  "reactions": { "1": [{"emoji":"👀","aspect":"keel"}, ...], "2": [...] } }
```
Replaces `/api/chat/reactions?ids=...`.

**Frame: `knowledge.search` → `knowledge.search.result`**
```json
{ "type": "knowledge.search", "correlation_id": "...",
  "text": "...", "own_agent": true, "shared": true, "peers": [], "top_k": 20 }
{ "type": "knowledge.search.result", "correlation_id": "...", "hits": [...] }
```
Operator-issued, so server treats `own_agent` as "operator's own" and ignores normal scope restrictions per §2.4. Replaces `/api/knowledge/search`.

**Frame: `knowledge.list` → `knowledge.list.result`**
```json
{ "type": "knowledge.list", "correlation_id": "...", "agent": "anvil", "limit": 50 }
{ "type": "knowledge.list.result", "correlation_id": "...", "entries": [...] }
```
Replaces `/api/knowledge?agent=...`.

**Frame: `knowledge.store` → `knowledge.store.result`**
```json
{ "type": "knowledge.store", "correlation_id": "...",
  "topic": "...", "content": "...", "shared": null /*|true|false*/ }
{ "type": "knowledge.store.result", "correlation_id": "...", "id": 42 }
```
Same preserve-shared semantics as the bridle tool (Crossing Part 4): `shared: null` (omit field) inherits prior flag.

**Frame: `aspect.say` → `aspect.say.result`**
```json
{ "type": "aspect.say", "correlation_id": "...", "aspect": "anvil", "content": "..." }
{ "type": "aspect.say.result", "correlation_id": "...", "msg_id": ... }
```
Posts a chat message addressed to the named aspect (`@<aspect>`). Convenience over `chat.send` so the SPA's "talk to this aspect" affordance has clean shape. Replaces `/api/agents/:id/say`.

### 3.3 New frames for live push (subscriptions)

The dashboard wants live updates without polling. Subscriptions are stateful per-connection — broker remembers what each operator connection has subscribed to, pushes matching events.

**Frame: `subscribe.roster`** — operator now receives `roster.update` push frames whenever an aspect connects, disconnects, or status-changes.
```json
{ "type": "subscribe.roster", "correlation_id": "..." }
// then, server-pushed:
{ "type": "roster.update",
  "aspect": "anvil", "status": "online", "last_seen": "...", "capabilities": [...] }
```
Idempotent — re-subscribing is a no-op.

**Frame: `subscribe.chat`** — operator receives `chat.deliver` for **all** chat messages (default), or scoped to a topic. Operator's default subscription on connect is "everything"; this frame just makes it explicit and lets the dashboard narrow.
```json
{ "type": "subscribe.chat", "correlation_id": "...", "topics": ["*"] }
// server pushes existing chat.deliver frames matching scope
```

**Frame: `subscribe.aspect_status`** — operator receives `aspect.status_pulse` push frames (per #118 — currently aspirational but the frame shape lands here).
```json
{ "type": "subscribe.aspect_status", "correlation_id": "..." }
// server-pushed when an aspect emits a pulse:
{ "type": "aspect.status_pulse",
  "aspect": "harrow", "phase": "thinking", "detail": "drafting reply", "ts": "..." }
```

**Frame: `unsubscribe.<topic>`** — symmetric off switch. Optional; closing the WS clears all subs.

### 3.4 Admin frames (operator-only, gated by `admin: true` JWT claim)

The existing REST admin surface (`POST /api/admin/shutdown`, `POST /api/admin/compact`, `POST /api/admin/rewind`, `GET /api/admin/dispatch-status`, `GET /api/admin/roster`, `PUT /api/admin/aspect/:name/personality`, `PUT /api/admin/nexus-md`) does NOT migrate to WS in this spec. Reason: those are infrequent, idempotent, well-tested REST handlers; replacing them gains nothing. They stay reachable via the existing REST endpoints, called with the operator JWT in the Authorization header. The dashboard's admin views (Settings panel, personality editor) issue HTTP fetches against those, not WS frames.

Trade-off accepted: the dashboard isn't *purely* WS-driven — admin actions stay on REST. The clean rule: **read/write data flows over WS; control plane (admin ops, login) stays on HTTP.**

### 3.5 Files / Tickets / Terminal — deferred

- **Files view**: needs `/api/files` shape (file announce/share queries). Defer behind §139's comms-client doc; for v1 of the WS port, the Files view shows "not yet wired" until shared-files frames are specced. (`announce_file` / `share_file` / `list_shared` / `get_shared` already exist as bridle tools — the WS frames are easy adds in a follow-up; deferring keeps this part shippable.)
- **Tickets view**: schema deferred per Crossing Part 2. View shows "not yet wired."
- **Terminal view**: terminal-proxy itself is deferred (Crossing §2.3). View shows "not yet wired."
- **Usage view**: `/api/usage` needs a new frame; defer with Files for the same reason.

These four views ship as placeholders in v1. Operator reaction: chat + status + agents + knowledge + docs work; the rest say "deferred" with a link to follow-up tickets.

## 4. SPA changes

### 4.1 `js/api.js` → `js/comms.js`

Today every view imports `request` from `js/api.js`. The shape:

```js
export async function listAgents() { return request('/api/agents'); }
```

Becomes:

```js
import { rpc, subscribe } from './comms.js';
export async function listAgents() { return rpc('roster.list'); }
```

`comms.js` exposes:
- `rpc(type, params) → Promise<result>` — sends a frame with a fresh `correlation_id`, awaits the matching `*.result`, resolves/rejects.
- `subscribe(type, params, handler) → unsubscribeFn` — sends a `subscribe.*` frame; routes matching push frames to `handler`. Returns a cleanup function.
- Internally manages: WS reconnect with exponential backoff; correlation_id → pending-Promise map; reconnect resubscribes all live subscriptions; queues outbound frames during reconnect (10s buffer, then drops with error).

This is the only fundamental rewrite. Every view's data layer touches `comms.js` not `fetch`. The rendering, layout, components — all unchanged.

### 4.2 Login flow

New view: `Login.js`. Renders before any other view. WebAuthn dance, then `/api/operator/login`, then keyfile validation, then JWT in memory, then `comms.js` opens WS. Stores nothing in localStorage. Page refresh = back to login.

### 4.3 View-by-view impact

- **Chat.js**: rewrite api.js calls to `rpc('topic.messages', ...)`, `rpc('chat.replies', ...)`, `rpc('chat.reactions.fetch', ...)`. Subscribe to `chat` and `roster` for live updates.
- **AgentsView.js / Status.js**: `rpc('roster.list')` + `subscribe('roster')`.
- **Knowledge.js**: `rpc('knowledge.search', ...)`, `rpc('knowledge.list', ...)`, `rpc('knowledge.store', ...)`.
- **DocsView.js**: docs are static markdown today (`/docs/*.md`); leave as HTTP fetch from `/docs/` static handler. No WS changes.
- **FeedView.js**: subscribes to `chat` + `roster` + `aspect_status`.
- **Files.js / FilesView.js**: placeholder until shared-files frames spec lands.
- **Tickets.js**: placeholder.
- **Terminal.js**: placeholder.
- **SplitView.js**: composes Chat + another view; same WS subs, shared connection.
- **Placeholder.js**: already a placeholder; no change.

### 4.4 What dashboard files NOT to touch

Vendor (xterm.js, preact, dompurify, marked, qrcode, dijkstrajs, htm, webauthn) — copy verbatim. CSS — copy verbatim. Components (Auth.js, BottomBar.js, ChatInput.js, MessageBubble.js, Shell.js, ThreadView.js, HarnessActivity.js) — copy verbatim; they consume props from views, not API directly. Icons, fonts — verbatim.

Auth.js exists in agent-network for operator-side aspect authentication; review whether its current shape fits the passkey flow or whether it needs replacement. Likely replacement (today's Auth.js handles aspect tokens, not operator passkeys) — flag during the actual port.

## 5. Broker-side work

### 5.1 Operator identity in roster + auth

- New `Role` constant: `RoleOperator = "operator"` in `shared/schemas/aspect.go`.
- Token resolution accepts operator JWTs (already does — JWT shape is uniform; the role is in the sub claim).
- WS upgrade allows a connection to register as `name: "operator", role: "operator"`. Validate that the JWT's sub matches the registered name to prevent identity-spoof.
- Roster doesn't auto-spawn operators — operators are ephemeral connections, born on login, die on disconnect. Roster row exists only while a connection is active.

### 5.2 Frame handlers

Each new frame in §3.2/§3.3 lands as a method on `*Broker` (or on a new `OperatorHandlers` type to keep aspect handlers separate). Most are thin: parse the frame, call existing `chat.Store` / `knowledge.Store` / roster methods, marshal response. The two non-trivial ones:

- **Subscriptions**: per-connection state. `wsConn` grows a `subs` map of `subscriptionType → predicate`. Existing chat.deliver fan-out checks the operator connection's chat subscription scope. New: roster fan-out (already exists for aspects via "online" event broadcast?) extends to push `roster.update` to subscribed operator conns.
- **Correlation IDs**: response frames echo the request's `correlation_id`. Pure mechanical; the envelope spec (§2 of nexus-ws) already permits arbitrary fields.

### 5.3 Passkey storage + login endpoint

New `nexus/broker/passkey.go`:
- `operator_passkeys` table (schema in §6.1).
- `POST /api/operator/login` — verify WebAuthn assertion against stored credential, mint in-memory keyfile + payload, return them.
- Reuses existing `/api/aspect/validate` for the next step — no parallel JWT path.
- Sign-counter check (WebAuthn replay protection) on every login.

`nexus operator register-passkey` CLI subcommand:
- Generates one-time PIN, prints registration URL.
- Browser flow: WebAuthn registration ceremony → POST result back to broker → broker stores credential.

### 5.4 Static asset embedding

Dashboard files embedded into the nexus binary via `//go:embed`. Same pattern as `chat.html` today. Tree:
```
nexus/broker/static/dashboard/
  index.html
  css/...
  js/...
  fonts/...
```
Served at `/dashboard/*` via `http.FileServer(http.FS(embedded))`. Login page at `/dashboard/` redirects to `/dashboard/index.html` once authenticated.

## 6. Storage additions

### 6.1 `operator_passkeys` table

```sql
CREATE TABLE IF NOT EXISTS operator_passkeys (
  id              INTEGER PRIMARY KEY,
  credential_id   BLOB NOT NULL UNIQUE,         -- WebAuthn credential id
  public_key      BLOB NOT NULL,                -- COSE-encoded public key
  sign_count      INTEGER NOT NULL DEFAULT 0,   -- replay-counter
  label           TEXT NOT NULL,                -- operator-given name ("little-blue", "dMon")
  registered_at   TEXT NOT NULL DEFAULT (datetime('now')),
  last_used_at    TEXT
);
```

No multi-operator support in v1 (single operator identity in the system); the table just holds 1+ devices for that operator.

### 6.2 Subscription state — in-memory only

Subscriptions are per-connection state. No DB persistence. WS close = subs gone. Reconnect = SPA replays its subscribes. Keeps the broker simple.

## 7. Build sequence

Five sub-parts. Each independently shippable.

| Sub-part | Scope | Depends on |
|---|---|---|
| 5a | **Operator identity + passkey storage** — RoleOperator constant, operator_passkeys table + migration, sign-counter logic. No login flow yet. ~half day. | — |
| 5b | **`/api/operator/login` + register-passkey CLI** — WebAuthn server-side verification, in-memory keyfile mint, register CLI subcommand. End-to-end testable: register a passkey, log in, get a JWT. ~1 day. | 5a |
| 5c | **WS frame handlers — request/response set** — roster.list, topics.list, topic.messages, chat.replies, chat.reactions.fetch, knowledge.search, knowledge.list, knowledge.store, aspect.say. Each lands with a small handler + test. ~1 day. | 5a, doesn't strictly need 5b for unit tests but does for end-to-end. |
| 5d | **WS subscription handlers** — subscribe.roster, subscribe.chat (default-all → narrow), subscribe.aspect_status. Per-conn sub map + fan-out. ~1 day. | 5c. |
| 5e | **SPA port** — copy dashboard tree, write js/comms.js, rewrite views to use rpc/subscribe, write Login.js, embed via go:embed, hook static handler. Confirm view-by-view: Chat works, Agents works, Status works, Knowledge works, Docs works, Feed works. Files/Tickets/Terminal/Usage stub as placeholders. ~3-4 days. | 5b, 5c, 5d. |

Parts 5a–5d are pure backend; 5e is pure frontend. 5e depends on all backend parts.

## 8. Out of scope

- Files view live wiring (deferred — needs shared-files frame spec).
- Tickets view (deferred to cairn).
- Terminal view (deferred to terminal-proxy port).
- Usage view live wiring (deferred — needs usage-query frame spec).
- Mobile layout (#104–#108, separate track).
- Collaboration-first dashboard rebuild (#173, post-Crossing).
- Multi-operator identities (single `operator` in v1).
- Passkey recovery flow (operator runs `nexus operator reset-passkey` on host shell).

## 9. Open questions for operator

1. **Operator handle.** Memory says operator handle is "operator" (`from: "operator"` in chat). Confirm this stays as the WS-side identity name, vs. adopting "jacinta" or similar. Spec defaults to "operator".
2. **Sign-out semantics.** Page refresh forces re-login. Acceptable, or should the dashboard offer "stay signed in for X hours" with a longer JWT TTL?
3. **Tailnet binding.** Should the `/api/operator/login` endpoint refuse non-tailnet origins? Tailnet binding is already enforced for some endpoints; broadening to login adds defense-in-depth at the cost of locking out non-tailnet browsers (not currently a use case).
4. **Admin views in dashboard.** The current SPA has admin affordances (personality edit, etc.). Confirm those stay backed by REST `/api/admin/*` (per §3.4), with operator JWT as the bearer.
5. **Files / Usage / Tickets / Terminal placeholders.** Confirm these views as "deferred" placeholders is the right v1 scope, vs. blocking the dashboard cutover until they all work.
6. **Order of operator vs. cutover.** Crossing §6 cutover can happen with a chat-and-status-only dashboard (operator can monitor + intervene). Does cutover need the full dashboard, or is "chat works, the rest comes after" acceptable?

## 10. Files referenced

- `agent-network/code/dashboard/` — SPA source.
- `agent-network/docs/2026-05-07-crossing-migration-spec.md` — parent Crossing spec; this doc supersedes Part 5.
- `agent-network/docs/2026-05-06-broker-comms-client-role-spec.md` — task #139's existing spec for comms-client/subscriber role; this doc is its first concrete consumer.
- `nexus-cw/nexus/runtime/keyfile/keyfile.go` — keyfile shape + validation flow this spec reuses.
- `nexus-cw/nexus/nexus/broker/ws.go` — existing WS upgrade + auth path.
- `nexus-cw/nexus/nexus/broker/server.go` — REST surface this doc avoids extending.
- `nexus-cw/nexus/shared/schemas/aspect.go` — Role + ContextMode enums extended.
- Memory: `project_external_gateway_future.md`, `feedback_operator_handle.md`, `project_distributed_nexus_endgame.md`.
