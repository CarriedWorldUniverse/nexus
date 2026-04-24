# Nexus Transport & Dispatch Spec — v0.1

**Date:** 2026-04-25
**Status:** Draft
**Owner:** keel (spec)
**Companion to:** [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) (partially supersedes §4 registration endpoints)
**Companion to:** [`2026-04-24-provider-adapter-spec.md`](2026-04-24-provider-adapter-spec.md)

## 1. Scope

This document defines how Nexus components talk to each other on the wire.

The registration spec (v0.5) defined the *shape* of aspects, provider adapters, session trees, knowledge store, and compaction. What it did not nail down was transport: the HTTP registration endpoints it described were a v1 stopgap. This spec replaces that stopgap with a WS-first protocol that supports the distributed network operator has outlined (aspects anywhere, behind NAT, behind Outposts, across hosts).

The registration spec remains authoritative for everything inside an aspect or inside the Nexus process; this spec takes over at the inter-process boundary.

## 2. Topology

Three kinds of process, plus the harness binary that runs in two modes.

```
┌────────────────────────┐
│        Nexus           │   single primary; no upstream
│  - chat bus            │
│  - knowledge (SQLite)  │
│  - roster              │
│  - embedded dispatcher │
│  - UI (HTTPS)          │
│  - embedded keel frame │
└───────────┬────────────┘
            │ WS
     ┌──────┴───────┐
     │              │
     ▼              ▼
┌─────────┐   ┌──────────────┐
│ aspect  │   │   Outpost    │   per-host presence
│ (direct)│   │  - relay     │
└─────────┘   │  - queue     │
              │  - spawn     │
              └──────┬───────┘
                     │ WS (local)
              ┌──────┴────────┐
              ▼               ▼
         ┌─────────┐     ┌─────────┐
         │ aspect  │     │ aspect  │
         └─────────┘     └─────────┘
```

### 2.1 Components

- **Nexus** — primary process. Owns cross-aspect state (knowledge, chat, tickets, roster). Embeds the Frame (keel). Serves the operator UI over HTTPS. Runs an embedded dispatcher (same shape as an Outpost, no upstream).
- **Outpost** — per-host process. Relays WS frames between local aspects and Nexus. Runs the hand dispatch queue. Spawns aspects at startup (fire-and-forget). Stateless beyond its in-memory aspect table — rebuilds on reconnect.
- **Aspect** — long-running process, one per aspect identity (wren, forge, maren, harrow, etc.). Runs via the harness binary in aspect mode. Connects to its upstream (Nexus directly, or via an Outpost) and stays connected.
- **Hand harness** — fresh, single-turn process spawned by the dispatcher on demand. Same binary as the aspect harness, different invocation. Exits after one hand completes.

### 2.2 Harness binary modes

One binary, two modes:

- `harness <home>` — aspect mode. Persistent. Reads `<home>/aspect.json`, connects via WS, keeps session tree, handles turn frames indefinitely.
- `harness <home> --hand <name> --input <payload>` — hand mode. Stateless. Spawned by dispatcher for a single hand invocation. Writes result to dispatcher via local socket/stdout, exits.

Both modes read the same aspect home (same `.credentials/`, SOUL.md, CLAUDE.md, knowledge access). The difference is lifecycle and state.

## 3. Configuration

Two env vars govern connection:

- **`NEXUS_UPSTREAM`** — URL of the Nexus. Set machine-wide on hosts where direct aspects or Outposts live. Required for Outposts. Required for aspects on direct-to-Nexus hosts.
- **`NEXUS_OUTPOST`** — URL of a local Outpost. Set in aspect launchers (or machine-wide on dispatcher hosts) to route aspects through the local Outpost instead of direct-to-Nexus.

### 3.1 Aspect precedence

An aspect's harness resolves its upstream as:

1. If `NEXUS_OUTPOST` is set → connect there.
2. Else if `NEXUS_UPSTREAM` is set → connect directly to Nexus.
3. Else → error out at startup.

### 3.2 Outpost config

- **`NEXUS_UPSTREAM`** (required) — where to phone home to Nexus.
- **`OUTPOST_LISTEN`** — bind address for incoming aspect WS connections (e.g. `:7950`).
- Inherited `NEXUS_OUTPOST` is ignored even if set (no dispatcher-of-dispatcher).

### 3.3 Nexus config

- No upstream. Root of the network.
- Serves WS on a configured address (aspects + Outposts both connect here).
- Serves HTTPS for UI on the same or a companion port.

### 3.4 Deployment patterns

**Single-host, direct aspects (dev setup):**
```
Machine-wide: NEXUS_UPSTREAM=wss://localhost:7888
Aspects inherit, connect direct.
```

**Dispatcher host (WSL example):**
```
Machine-wide in WSL:
  NEXUS_OUTPOST=wss://localhost:7950

Outpost launcher (narrow override):
  NEXUS_UPSTREAM=wss://windows-host:7888
  OUTPOST_LISTEN=:7950

Aspects on the host:
  Inherit NEXUS_OUTPOST → connect local.
```

### 3.5 Fail-loudly rule

If `NEXUS_OUTPOST` is set but unreachable on initial connect, the aspect exits non-zero. No silent fallback to `NEXUS_UPSTREAM` — the operator explicitly asked for an Outpost route; deviation masks config errors. Transient flaps after initial connect are retried on the same upstream.

## 4. Connection lifecycle

All inter-component traffic goes over a single WebSocket connection, opened by the client (aspect or Outpost) to the server (Outpost or Nexus).

### 4.1 Connect

1. Client dials `wss://<upstream>/connect` (path is fixed; the URL path name isn't part of the protocol but is stable so operators can recognise it in logs).
2. HTTP upgrade request carries `Authorization: Bearer <token>` header.
3. Server accepts or rejects. Reject reasons: bad token, protocol version mismatch.
4. On accept, client sends a `register` or `outpost.register` frame as the first message.
5. Server responds with `register.ack` including runtime parameters (heartbeat interval, stale threshold).

### 4.2 Keepalive

- WS ping/pong handles low-level liveness at the socket layer.
- No app-level heartbeat frame in v1 — the socket being open is authoritative. If the socket closes unexpectedly, the peer is gone.

### 4.3 Disconnect

- Graceful: client sends `deregister` (or `outpost.deregister`), waits for ack, closes the socket.
- Abrupt: socket closes. Server marks the client gone.
- Reconnect: client redials and re-registers. Session id on the re-register determines whether it's "same session reconnecting" or "new session displacing the old." Existing §4.2 displacement logic applies.

## 5. Frame protocol

Every WebSocket message is a JSON object with a `kind` discriminator. Unknown `kind` values are logged and ignored (forward-compat).

### 5.1 Envelope

```json
{
  "kind": "<frame-type>",
  "id": "<ulid>",              // optional; present for request frames that expect a response
  "in_reply_to": "<ulid>",     // optional; present on response frames
  "ts": "<ISO-8601>",
  "payload": { ... }           // frame-specific
}
```

Request/response correlation uses `id` + `in_reply_to`. Responses echo the request's `id` into `in_reply_to`.

### 5.2 Frame catalogue

#### Registration

- **`register`** (aspect → upstream) — first frame after connect. Payload matches the registration spec §4.2 `RegisterRequest` shape.
- **`register.ack`** (upstream → aspect) — response. Carries heartbeat interval, stale threshold.
- **`deregister`** (aspect → upstream) — graceful shutdown.
- **`outpost.register`** (Outpost → Nexus) — same shape but identifies as an Outpost; carries Outpost id, host info, capabilities.
- **`outpost.deregister`** (Outpost → Nexus) — graceful shutdown.

Outposts forward aspect `register` frames up to Nexus with a `via_outpost` field stamped on, so Nexus's roster records the route.

#### Turn dispatch

- **`turn`** (upstream → aspect) — "run a turn with this prompt." Payload: `prompt`, `system_prompt`, `model`, `thinking_level`, `max_tokens`.
- **`turn.result`** (aspect → upstream) — completion. Payload: `output`, `stop_reason`, `tokens`, `cost`, `tool_calls` (if any).

#### Hand dispatch

- **`hand.dispatch`** (any → dispatcher) — enqueue a hand invocation. Payload: `target_aspect`, `hand_name`, `thread_id`, `input`, `invoker`.
- **`hand.result`** (hand harness → dispatcher → upstream) — hand execution result. Payload: `target_aspect`, `hand_name`, `thread_id`, `output`, `cost`, `tokens`.
- **`hand.error`** (dispatcher → requester) — hand could not be dispatched (target offline, queue saturated, unknown hand).

Dispatchers log all `hand.dispatch` and `hand.result` frames up to Nexus for chat-bus audit visibility, even if execution was local (e.g. two aspects on the same Outpost). Preserves the auditability property that motivated the dispatch system.

#### Chat / comms

- **`chat.send`** (aspect → upstream) — post a chat message. Payload: `from`, `content`, `reply_to`, `thread`, `mentions`.
- **`chat.deliver`** (upstream → aspect) — chat arrived for this aspect (mention, reply, thread participation, unrestricted observation).
- **`chat.reaction`** (aspect → upstream, or upstream → aspect) — toggle a reaction on a message.
- **`chat.read`** (aspect → upstream) — request a message or thread by id.

#### Knowledge

- **`knowledge.store`** (aspect → upstream) — write an entry.
- **`knowledge.search`** (aspect → upstream) — query. Response via `knowledge.search.result`.
- **`knowledge.search.result`** (upstream → aspect).

#### Session observability (projection upward)

- **`session.entry.appended`** (aspect → upstream) — aspect just appended an entry to its local session JSONL. Nexus persists to a projection table (read-only view) so the dashboard can render the live session. Source of truth remains the aspect-local file.
- **`session.rewind`** / **`session.fork`** (aspect → upstream) — head-pointer change events, also projected.

#### Shutdown

- **`shutdown`** (upstream → aspect OR Outpost → aspect OR Nexus → Outpost) — please wind down cleanly. Payload: `reason`, `grace_period_s`.

### 5.3 Forward-compatibility

- Unknown `kind` values are logged and dropped, not errors.
- Unknown payload fields are preserved through forwards (Outpost doesn't strip fields it doesn't understand — Nexus may need them).
- Use `json.Decoder` with default behaviour (unknown fields ignored on decode but retained if re-encoded).

## 6. Dispatch

### 6.1 Queue semantics

Each dispatcher (embedded-in-Nexus OR remote Outpost) maintains an in-memory job queue:

- **Queue**: FIFO of pending `hand.dispatch` frames.
- **Max concurrency**: `DISPATCHER_MAX_HANDS` env, default 5. At most this many hand harness processes running at once.
- **Slot release**: when a spawned harness exits (any reason), its slot frees and the next queued job is pulled.
- **Per-aspect concurrency caps**: future refinement. v1 uses a global cap per dispatcher.

### 6.2 Routing

When a `hand.dispatch` frame is received:

1. Dispatcher looks up `target_aspect` in its local aspect table (for Outpost) OR the Nexus roster (for Nexus).
2. If the target is local → enqueue locally. Spawn when a slot opens.
3. If the target is on a remote Outpost → forward the frame up to Nexus (if not already there), Nexus forwards down to the target Outpost.
4. If the target is unknown / offline → respond with `hand.error`.

### 6.3 Spawn mechanics

When a slot opens for a queued job:

1. Dispatcher locates the target aspect's home directory (known from its registration).
2. Spawns `harness <home> --hand <name> --input <payload-json>` with inherited env (`NEXUS_UPSTREAM`, token, etc.).
3. Captures stdout and stderr; forwards to the dispatcher's own log with an `[hand:forge:verify-canon:<corr-id>]` prefix.
4. Waits for process exit.
5. Expects a `hand.result` frame written to a known local socket (or stdout in a structured format). If not received, synthesises `hand.error` with the exit reason.
6. Forwards `hand.result` back up the chain.
7. Frees the slot.

### 6.4 Audit mirroring

All `hand.dispatch` and `hand.result` frames traverse the full chain up to Nexus, even when execution is local. The chat bus logs both. This preserves the "dispatch is visible in chat" property that motivated the original design — no invisible local execution.

## 7. Auto-spawn (fire-and-forget)

Nexus and Outpost can spawn managed aspects at startup.

### 7.1 Discovery

Config env var `NEXUS_ASPECT_DIR` (default `./aspects/`). On startup, Nexus/Outpost scans the directory for subdirectories containing a valid `aspect.json`. Each is a candidate for spawn.

An aspect.json field `auto_spawn: false` opts out — the aspect can still be started manually.

### 7.2 Ordering

1. Parent (Nexus or Outpost) starts.
2. Parent binds its WS listener.
3. Outpost only: connects upstream and registers. Waits for `outpost.register.ack`.
4. Parent scans aspect dir.
5. For each candidate, spawns `harness <home>` with env set (`NEXUS_OUTPOST` pointing at parent's listener for Outposts; `NEXUS_UPSTREAM` pointing at Nexus for direct-Nexus spawns; `NEXUS_TOKEN` inherited).
6. Parent does not monitor. Children dial in, register, run. If they crash, they stay crashed until container orchestration restarts the parent.

### 7.3 What parent does NOT do

- Does not supervise: no respawn, no restart-on-crash, no backoff.
- Does not track pid state: the child is on its own OS process, OS handles reaping.
- Does not expose a lifecycle API: no "start/stop/restart aspect X" WS frame. Operators restart the container if they want that.

Rationale: container orchestration (Docker, k8s, nomad, systemd) already does process management. Duplicating it in Nexus/Outpost adds complexity without benefit.

### 7.4 Logging

Parent captures child stdout/stderr and forwards to its own log stream with a `[<aspect>] ` prefix. Operator sees everything in one place. If child crashes, the logs are the forensic trail.

## 8. Data ownership

| State                           | Where it lives                       | Why                                              |
|---------------------------------|--------------------------------------|--------------------------------------------------|
| Knowledge base                  | Nexus (SQLite + FTS5)                | Cross-aspect shared, needs central authority     |
| Chat history, threads, tickets  | Nexus (SQLite)                       | Cross-aspect shared                              |
| Roster (who's connected)        | Nexus (in-memory + projection table) | Single source of truth for routing               |
| Activity log / telemetry        | Nexus (SQLite)                       | Cross-aspect, used by dashboard                  |
| Session JSONL tree              | Local to aspect home                 | Per-aspect private; fast local append            |
| Session projection              | Nexus (read-only view)               | Observability for dashboard                      |
| Aspect home (aspect.json, creds)| Local to aspect's host               | Per-host filesystem, credentials don't travel    |
| Outpost aspect table            | In-memory on Outpost                 | Rebuildable from aspect reconnects; no durability needed |

Outpost holds **no** SQLite, no durable state. It's a pure relay + queue + spawner. This is deliberate: no distributed-database reconciliation, no cross-node schema migrations, no sync protocol.

### 8.1 Session projection upward

Each time an aspect appends to its local session JSONL, it also emits a `session.entry.appended` frame upward. Nexus stores the entry in a projection table. Dashboard reads from the projection for live-session rendering.

Crucial: the projection is **read-only** at Nexus. If the projection is lossy (dropped frames during partition, lagging catch-up), the local JSONL remains authoritative. An aspect replaying its branch for a provider call reads local disk, not Nexus. Aspects are autonomous at the per-process level.

## 9. Partition behaviour

### 9.1 Outpost ↔ Nexus link drops

- Outpost continues serving its local aspects. Aspects don't notice.
- Outbound frames (chat.send, knowledge.store, session.entry.appended, hand.dispatch targeting remote aspects) queue on the Outpost.
- Inbound frames from Nexus pause.
- Aspects can still invoke hands targeting local aspects (Outpost routes locally).
- When link restores: Outpost re-sends `outpost.register`, then replays `register` frames for all currently-connected aspects, then flushes its queued outbound frames.

### 9.2 Aspect ↔ Outpost link drops

- Outpost emits `roster.aspect.disconnected` upward.
- Aspect's harness reconnects (exponential backoff, 1s → 60s).
- On reconnect, aspect re-registers. Session id determines whether it displaces the previous entry or is the same session resuming.

### 9.3 Nexus restart

- All Outposts and direct aspects lose their connections.
- They reconnect when Nexus is back.
- Nexus reconstructs roster from fresh registrations — no persisted WS state.

### 9.4 Queued frames

- Outpost queues are in-memory. Restart of the Outpost loses the queue. Aspects retry their own requests on reconnect where applicable (knowledge writes, chat sends).
- Not a durable message queue in v1. If that's needed later, a durable outbox in SQLite on Nexus is the add-on.

## 10. HTTP surface

WS-first is the rule. HTTP is reserved for:

- **WS upgrade**: the initial handshake on `/connect`. Protocol-mandated.
- **UI assets**: Nexus serves the dashboard SPA over HTTPS (index.html, JS, CSS). Standard web stack.
- **Health probes**: `/health` (unauth) for external monitoring, load balancers, curl from terminal. Returns JSON.
- **Roster listing for dashboard**: `GET /api/aspects` returning the current roster. Dashboard convenience; authoritative state is the WS-driven roster.
- **File uploads/downloads** (future): binary streams don't fit the WS frame model. Not v1.

Nothing else. Registration, heartbeat, turn dispatch, hand dispatch, chat, knowledge — all WS.

## 11. Migration from §6.3

The §6.3 implementation built:

- `nexus/broker` with HTTP `POST /aspects/register|heartbeat|deregister` + `GET /aspects` + `GET /health`.
- `runtime/agent` with `POST /turn` as the dispatch surface.
- `runtime/cmd/agent/main.go` with HTTP client loops for register/heartbeat/deregister.

Migration plan:

- **`nexus/broker`**: replace HTTP register/heartbeat/deregister with WS `/connect` endpoint. Keep `GET /aspects` + `GET /health` as the HTTP surface. Add WS frame router.
- **`runtime/agent`**: remove `POST /turn` HTTP server. Aspect connects via WS and handles inbound `turn` frames.
- **`runtime/cmd/agent` → `runtime/cmd/harness`**: rename; add hand mode flag handling.
- **New: `nexus/outpost` + `nexus/cmd/outpost`**: standalone Outpost binary. Relay + queue + spawn.
- **Session projection**: aspect emits `session.entry.appended` on each tree append; Nexus has a `session_projection` table.

Session tree storage, knowledge store, provider adapters, compaction — all unchanged. The reshape is purely the inter-process layer.

## 12. Open questions

- **WS library**: Go stdlib doesn't include WebSocket. Use `github.com/coder/websocket` (modern, clean API) or `github.com/gorilla/websocket` (mature, widely used). Lean: coder/websocket. Decide at implementation time.
- **Frame encoding**: JSON is v1. MessagePack / CBOR for efficiency is a future option.
- **Per-aspect auth**: v1 uses one shared bearer token for all clients. Per-aspect tokens are a hardening step later.
- **Multi-tier Outposts**: v1 forbids dispatcher-of-dispatcher. If ever needed (deep federation), revisit.
- **Durable outbox**: v1 in-memory queue on Outposts. If reliability matters for specific frame types, add a per-aspect durable outbox on Nexus.

## 13. Status

v0.1 draft. Locks the WS-first shape, Outpost topology, aspect/dispatcher config model, frame catalogue, data-ownership split, partition behaviour, HTTP surface, and migration plan. Implementation decomposition against this spec is the next step (§6.4 work plan).
