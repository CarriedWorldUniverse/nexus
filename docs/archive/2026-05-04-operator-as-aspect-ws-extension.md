# Operator-as-Aspect WS Extension — v0.1

**Date:** 2026-05-04
**Status:** Draft
**Owner:** keel
**Companion to:**
- [`2026-05-02-aspect-funnel-architecture.md`](2026-05-02-aspect-funnel-architecture.md)
- [`2026-04-28-frame-role-spec.md`](2026-04-28-frame-role-spec.md)
- [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) §2.2

## 1. Scope

Operator framing (#9677): **the dashboard is an aspect.** Its turn driver is human interaction — operator reads what landed in their inbox, takes an action, the action becomes an outbound frame. Same WS protocol as every other aspect.

This collapses three previously-separate surfaces (dashboard WS push, dashboard REST writes, aspect WS) into one:

- **Connection:** SPA dials `/connect` with role `operator`. Same WebSocket, same handshake, same envelope.
- **Reads:** SPA receives `chat.deliver`, `aspect.activity`, etc. via the existing recipient-routing logic (Lock 2).
- **Writes:** SPA emits `chat.send`, `react_to`, `ticket.*`, etc. — the same frames any aspect sends, plus admin-gated frames the operator role grants.

No `/ws/dashboard` mirror. No dashboard-specific frame catalogue. The vocabulary already exists; this spec adds the operator role and the frame extensions tickets / files / docs / usage / admin require regardless of who's calling them (aspects need most of these too).

## 2. Connection role declaration

Extend `shared/schemas` `Role` enum:

```go
const (
    RoleAspect   Role = "aspect"
    RoleFrame    Role = "frame"
    RoleOperator Role = "operator"   // NEW
    RoleOutpost  Role = "outpost"    // NEW (formalising the existing OutpostRegister path)
    RoleExternal Role = "external"   // NEW (reserved for cross-Nexus federation)
)
```

The role is declared on the WebSocket handshake via the `register` frame (`shared/schemas.RegisterRequest`):

```json
{
  "kind": "register",
  "payload": {
    "aspect_id": "operator",
    "role": "operator",
    "auth_token": "<bearer>",
    "since_msg_id": 12345
  }
}
```

Server validates:
1. Role is one of the known values.
2. `auth_token` matches a token issued by the auth surface (§5).
3. The token's role claim matches the declared role. Token says aspect, frame says operator → reject.
4. For role=operator, `aspect_id` MUST be `"operator"` — single canonical operator aspect per Nexus. Multi-operator support is post-cutover (§9).

## 3. Frame access control

Each frame has a role-access list. The server validates inbound frames against the connection's role:

| Frame | aspect | frame | operator | outpost |
|---|---|---|---|---|
| `register`, `deregister` | ✓ | ✓ | ✓ | ✓ |
| `chat.send`, `chat.read`, `chat.read.result` | ✓ | ✓ | ✓ | passthrough |
| `chat.deliver` (server→client) | ✓ | ✓ | ✓ | passthrough |
| `react_to` | ✓ | ✓ | ✓ | passthrough |
| `announce_file`, `share_file` | ✓ | ✓ | ✓ | passthrough |
| `file.result` (server→client) | ✓ | ✓ | ✓ | passthrough |
| `aspect.activity` (server→client) | — | — | ✓ | passthrough |
| `knowledge.store`, `knowledge.search`, `knowledge.search.result` | ✓ | ✓ | ✓ | passthrough |
| `dispatch`, `dispatch.result`, `dispatch.error` | ✓ | ✓ | — | passthrough |
| `turn`, `turn.result` (server↔aspect) | ✓ | ✓ | — | passthrough |
| `session.entry.appended`, `session.rewind`, `session.fork` | ✓ | ✓ | — | passthrough |
| `ticket.*` (§4.1) | ✓ | ✓ | ✓ | passthrough |
| `file.list`, `file.get` (§4.2) | ✓ | ✓ | ✓ | passthrough |
| `docs.list`, `docs.get` (§4.3) | ✓ | ✓ | ✓ | passthrough |
| `usage.query`, `usage.query.result` (§4.4) | — | ✓ | ✓ | passthrough |
| `network.*`, `agents.*` (§4.5) | — | ✓ | ✓ | passthrough |

`passthrough` for outpost = the outpost forwards on behalf of a registered downstream aspect/operator. The frame's allowed-or-not check happens against the downstream connection's role, not the outpost's.

Operator gets `aspect.activity` so the dashboard can render "agent X is thinking…" indicators per #119. Aspects don't subscribe to other aspects' activity by default (different concern; not blocking cutover).

Frames the operator role can send that aspects can't: usage queries, network/admin operations, agents.start/stop. Frames operator can't send: turn-loop frames (no LLM behind operator), dispatch (operator doesn't dispatch Hands).

## 4. New frames

### 4.1 Tickets

```go
type TicketCreatePayload struct {
    Title       string `json:"title"`
    Description string `json:"description,omitempty"`
    Assignee    string `json:"assignee,omitempty"`
    Priority    string `json:"priority,omitempty"`   // low | normal | high | urgent
    Domain      string `json:"domain,omitempty"`
    SourceMsgID int64  `json:"source_msg_id,omitempty"`
}

type TicketUpdatePayload struct {
    ID          int64   `json:"id"`
    Status      *string `json:"status,omitempty"`     // open | in-progress | blocked | review | done
    Assignee    *string `json:"assignee,omitempty"`   // pointer so empty-string clears to NULL
    Priority    *string `json:"priority,omitempty"`
    Title       *string `json:"title,omitempty"`
    Description *string `json:"description,omitempty"`
    Domain      *string `json:"domain,omitempty"`
}

type TicketListPayload struct {
    Assignee string `json:"assignee,omitempty"`
    Status   string `json:"status,omitempty"`
    Creator  string `json:"creator,omitempty"`
    Domain   string `json:"domain,omitempty"`
    Limit    int    `json:"limit,omitempty"`          // default 50, cap 200
}

type TicketListResultPayload struct {
    Tickets []TicketSummary `json:"tickets"`
}

type TicketSummary struct {
    ID         int64  `json:"id"`
    Title      string `json:"title"`
    Status     string `json:"status"`
    Priority   string `json:"priority"`
    Domain     string `json:"domain,omitempty"`
    Assignee   string `json:"assignee,omitempty"`
    Creator    string `json:"creator"`
    CreatedAt  string `json:"created_at"`            // RFC 3339 UTC
}

type TicketGetPayload struct {
    ID int64 `json:"id"`
}

type TicketGetResultPayload struct {
    Ticket TicketDetail `json:"ticket"`
    Notes  []TicketNote `json:"notes"`
}

type TicketDetail struct {
    TicketSummary
    Description string `json:"description,omitempty"`
    SourceMsgID int64  `json:"source_msg_id,omitempty"`
    UpdatedAt   string `json:"updated_at"`
    ClosedAt    string `json:"closed_at,omitempty"`
}

type TicketNote struct {
    ID        int64  `json:"id"`
    Author    string `json:"author"`
    Content   string `json:"content"`
    CreatedAt string `json:"created_at"`
}

type TicketNoteAddPayload struct {
    TicketID int64  `json:"ticket_id"`
    Content  string `json:"content"`
}
```

`next_ticket` is deferred to the `/` routing + pull-queue work (`2026-05-03-ticket-system-upgrade-spec.md` Part 4). Not in v0.1 of this WS extension.

### 4.2 Files

**Superseded by [`2026-05-04-files-subsystem-spec.md`](../2026-05-04-files-subsystem-spec.md).** This earlier draft assumed Nexus stored bytes and used a signed-URL download pattern; harrow's files spec replaces that with a broker model — Nexus holds references only, files stay on the announcing aspect's filesystem (or a public URL like Google Drive), and `ws://aspect/file/path` URIs are resolved via a `file.fetch` frame routed to the owning aspect's funnel.

That model is strictly better:
- Single source of truth (no stale copies in Nexus)
- File deletes propagate naturally (404 on next fetch)
- Author controls visibility/lifecycle — Nexus is just routing
- New pattern emerges: "funnel service frames" — non-turn frames the funnel handles directly via a dispatch table. Future cases (health checks, capability queries) follow the same shape.

Frames defined in the files spec:
- `file.announce` / `file.announce.result` — aspect or operator announces a file by reference
- `file.list` / `file.list.result` — list files (no URL in summaries; routing is internal)
- `file.get` — by id; Nexus inspects the URL scheme. Public `https://` returns URL directly; `ws://aspect/file/path` triggers a `file.fetch` to the owning funnel and forwards the `file.deliver` response with bytes inline (base64) to the requester
- `file.fetch` / `file.deliver` — internal funnel-handled exchange; bytes go base64-in-JSON for v0.1 (binary WS frames are the post-cutover upgrade path for large assets)

Path traversal hardening on the funnel-side handler. Offline aspect → `file unavailable` error to requester (no queue, no retry; caching is post-cutover).

### 4.3 Docs

```go
type DocsListPayload struct {
    Path string `json:"path,omitempty"` // optional subdir filter
}

type DocsListResultPayload struct {
    Docs []DocEntry `json:"docs"`
}

type DocEntry struct {
    Path     string `json:"path"`     // relative to docs root
    Size     int64  `json:"size"`
    Modified string `json:"modified"` // RFC 3339 UTC
}

type DocsGetPayload struct {
    Path string `json:"path"` // sandbox-validated server-side
}

type DocsGetResultPayload struct {
    Path     string `json:"path"`
    Content  string `json:"content"`     // UTF-8; non-text files rejected
    Modified string `json:"modified"`
}
```

Path traversal: server rejects any `..` segment, absolute paths, or paths escaping the configured docs root. Same hardening as the existing agent-network endpoint.

### 4.4 Usage

```go
type UsageQueryPayload struct {
    Period   string `json:"period,omitempty"`   // "1h" | "24h" | "7d" | "30d"; default 7d
    Aspect   string `json:"aspect,omitempty"`   // filter to one aspect
    GroupBy  string `json:"group_by,omitempty"` // "aspect" | "msg_id" | "day"; default "aspect"
}

type UsageQueryResultPayload struct {
    Period   string        `json:"period"`
    Rows     []UsageRow    `json:"rows"`
}

type UsageRow struct {
    Key          string `json:"key"`           // aspect-id, msg-id, or YYYY-MM-DD per group_by
    InputTokens  int64  `json:"input_tokens"`
    OutputTokens int64  `json:"output_tokens"`
    TotalTokens  int64  `json:"total_tokens"`
}
```

Backed by `nexus/usage` Store (F3.1). This frame replaces F3.3 (the planned REST endpoint).

### 4.5 Network and agents (operator/frame only)

```go
type NetworkRestartPayload struct {
    Target string `json:"target,omitempty"` // empty = whole network; or specific aspect-id
}

type NetworkShutdownPayload struct {
    GracePeriodS int `json:"grace_period_s,omitempty"`
}

type NetworkMaintenancePayload struct {
    Enabled bool   `json:"enabled"`
    Reason  string `json:"reason,omitempty"`
}

type AgentStartPayload struct {
    AspectID string `json:"aspect_id"` // empty = "all"
}

type AgentSayPayload struct {
    AspectID string `json:"aspect_id"`
    Content  string `json:"content"` // direct prompt to the aspect, bypassing chat
}
```

These map 1:1 to the existing agent-network REST endpoints (`/api/network/restart`, `/api/agents/:id/say`) but flow over WS. Server-side handlers reuse the same role-gating + supervisor-call code paths.

## 5. Auth handshake

Operator's role declaration MUST be backed by a token. Two paths:

**5.1 First-time setup.** Operator registers via the bootstrap REST flow (existing — `/bootstrap/setup` from §6.5 P2, with WebAuthn or invite-code as configured). Server issues an `auth_token` with role claim `operator`. Token is stored in the SPA's local state (same place agent-network's SPA stores it today).

**5.2 Connect.** SPA opens WS to `/connect`, sends `register` frame with `auth_token`. Server validates token, accepts the connection with role=operator. Token rotation is post-cutover; v1 tokens are long-lived.

**5.3 Token issue surface.** Stays REST: `/api/auth/login`, `/api/auth/login-options`, `/api/auth/register`, `/api/auth/register-options`, `/api/auth/check`. WebAuthn challenge/response is HTTP-shaped and the relay/Outpost path proxies HTTP for these specific paths even when the rest of the surface is WS-only. ~5 endpoints; thin shim.

This is the only REST surface left in the operator-aspect dataflow. Everything else is WS.

## 6. Recipient routing for operator

Lock 2 recipient-routing applies unchanged. Operator gets `chat.deliver` for:

1. Messages with `topic IS NULL OR topic = 'general'` (the operator's main feed)
2. Messages with `@operator` mention or `@all` mention
3. Messages in topics the operator is "subscribed to" (initially: any topic the operator has posted in)
4. Direct replies to operator's own posts
5. Messages in `topic='dm:operator'` (operator-DMs)

Aspects don't subscribe to operator activity by default. The current Lock 2 rules already produce the right behaviour with operator just being another aspect-id in the routing computation.

## 6.5 Envelope: request_id correlation

Operator's flows are request/response shaped (`ticket.list` → `ticket.list.result`, `chat.send` → success ack with assigned `msg_id`, `usage.query` → `usage.query.result`). Without correlation, a SPA that fires three `ticket.list` requests can't tell which result belongs to which call.

Every client→server frame carries an optional `request_id` (string, client-assigned, opaque to the server). Server's response frame echoes the same `request_id`. Recommended client format: monotonic integer or UUIDv4; the server doesn't care.

Wire shape:

```json
// client → server
{"kind": "ticket.list", "request_id": "17", "payload": {"domain": "wren"}}

// server → client (success)
{"kind": "ticket.list.result", "request_id": "17", "payload": {"tickets": [...]}}

// server → client (error)
{"kind": "result", "request_id": "17", "status": 403, "error": "frame_not_allowed_for_role"}
```

Three rules:

1. **Server-pushed frames have no request_id.** `chat.deliver`, `aspect.activity`, and `file.result` (when emitted as a notification, not a reply) flow without correlation. Clients distinguish push frames from RPC results by `request_id` presence.
2. **Errors use a generic `result` kind with status + error string.** Per-frame error result types (`ticket.list.error`, etc.) are noise; one shape covers all rejections. Status codes mirror HTTP semantics (`400` invalid payload, `403` frame-not-allowed-for-role, `404` resource-not-found, `409` conflict, `429` rate-limited, `500` server error).
3. **request_id is optional, never required.** A client that omits it gets responses without it; the response shape stays parseable. This keeps fire-and-forget patterns (e.g. operator emitting `react_to` and not caring about the ack) simple.

This envelope shape applies to all WS frames in the protocol, not just operator-aspect ones — the same correlation pattern benefits the aspect runtime too. The transport spec (`2026-04-25-nexus-transport-spec.md`) should pull this section in as the canonical envelope; this spec carries it for now so the SPA port has a stable reference.



The SPA's `api.js` becomes a `wsclient.js` with:

- One persistent WebSocket to `/connect`
- A request-id correlator: `send_chat()` returns a Promise that resolves when the matching `result` frame comes back
- An event router: `chat.deliver` → message-list signal, `aspect.activity` → activity-strip signal, etc.
- Binary uploads: still POST to `/api/images`; afterward emit `announce_file` over WS
- Binary downloads: WS frame returns a signed URL; SPA fetches the bytes via REST GET

Auth flow stays REST (login + WebAuthn). Once token issued, all data flow is WS.

## 8. Compatibility / migration story

Agent-network's REST API stays in place during cutover. The nexus rebuild's WS-only surface ships alongside (different host:port). Operator picks which one to connect to. Once parity is verified the agent-network broker can be retired.

The funnel architecture spec's Lock 2 already covers what the operator-aspect needs — recipient routing produces the right `chat.deliver` set. No changes to existing locks.

## 9. Open questions

- **Multi-operator?** v1 = single operator-aspect. Two operators on one Nexus (one phone + one desktop) means either two registrations sharing aspect-id (rejected by ticket #124's defence) or distinct aspect-ids (`operator-mobile`, `operator-desktop`). Defer to a follow-up — single operator suffices for cutover.
- **Operator typing indicator** (per #119): aspects emit `aspect.activity` for `turn.start` etc. Operator could symmetrically emit `operator.typing` so aspects waiting on the human see "operator is typing." Not blocking cutover; folded into a future activity-strip extension.
- **Outpost passthrough rules.** The "passthrough" cells in §3 assume the outpost trusts and forwards the downstream's frames; the actual gating happens at the downstream WS endpoint. This needs a concrete implementation pass when the outpost path lands.
- **REST shim for non-SPA tooling.** Operators using curl, scripts, future relay-cli equivalents need *some* way to call into the system without a WS client. Defer: the WS surface is canonical; REST tooling can either implement a small WS-RPC client or live with the auth-only REST surface during cutover. Revisit post-cutover.

## 10. Acceptance criteria

- [ ] Role enum extended (`operator`, `outpost`, `external`)
- [ ] Server-side handshake validates role + token
- [ ] Frame access control table enforced server-side; rejected frames return a `result` with status=403
- [ ] All ticket / file / docs / usage / network / agents frames defined in `frames/payloads.go`
- [ ] SPA opens WS to `/connect`, registers as operator, receives `chat.deliver` for general feed + mentions
- [ ] SPA's `chat.send` lands as a chat row; recipient routing produces correct fan-out to other aspects
- [ ] Auth REST endpoints (`/api/auth/*`) issue tokens that the WS handshake accepts
- [ ] Operator-only frames (`network.*`, `agents.*`, `usage.query`) reject when sent by aspect-role connections
- [ ] Aspect-only frames (`turn`, `dispatch`) reject when sent by operator-role connections

## 11. Status

v0.1 draft. Role declaration, frame access matrix, new ticket/file/docs/usage/admin frame catalogue, auth handshake, recipient-routing reuse, and open questions defined. Implementation sequencing follows F2.5 (out-of-process aspect binary) — once the WS protocol has a real out-of-process consumer, the SPA port becomes the second consumer of the same surface.
