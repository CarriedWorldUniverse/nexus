# Nexus Transport & Dispatch Spec — v0.2

**Date:** 2026-04-25 (v0.1) · 2026-05-14 (v0.2 amendment)
**Status:** Draft — v0.2 adds the Outpost deploy plane (§13), local mailbox property (§8.2), and resolved per-aspect auth (§12).
**Owner:** keel (v0.1 spec), shadow (v0.2 amendment)
**Companion to:** [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) (partially supersedes §4 registration endpoints)
**Companion to:** [`2026-04-24-provider-adapter-spec.md`](2026-04-24-provider-adapter-spec.md)

## v0.2 amendment summary

Added to v0.1:
- **§8.2 Outpost mailbox.** Outpost gains a per-local-aspect mailbox so messages survive aspect restarts and reach claude-code-session-style clients that don't hold a persistent WS. Bounded local durability; Nexus remains the source of truth.
- **§13 Deploy plane.** Binary update delivery (`binary.advertise` / `binary.fetch`), restart signalling (`aspect.restart`), lifecycle reporting (`aspect.lifecycle`), push-event delivery to non-WS consumers. Replaces today's ad-hoc out-of-band agentfunnel-linux shipping + manual restarts.
- **§7.3 narrowed.** "Outpost doesn't supervise" rule now applies only to crash-respawn; binary-update + operator-driven restart are first-class.
- **§12 resolved.** Per-aspect auth is no longer "future" — `AspectTokens` map on Outpost is the v1 shape (`#33`), with keyfile-derived JWTs the production path.

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

### 7.3 What parent does NOT do (narrowed in v0.2)

- Does not supervise crashes: no respawn-on-crash, no backoff, no health-driven restart.
- Does not track pid state for crash detection: the child is on its own OS process, OS handles reaping.

What it now DOES do (v0.2 §13):
- Performs operator-driven restart (`aspect.restart` frame) for binary updates and deploys.
- Captures lifecycle transitions for reporting upward (`aspect.lifecycle` frames).

Rationale (unchanged for crash supervision): container orchestration (Docker, k8s, nomad, systemd) already does crash recovery. Duplicating that in Nexus/Outpost adds complexity without benefit. But the deploy plane — "operator pushed a new build, restart aspects to pick it up" — is not crash recovery; it's a coordinated action that nothing else in the stack does for us, so the Outpost owns it.

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
| Outpost mailbox cache (v0.2)    | Local SQLite on Outpost (bounded)    | Smooths aspect-restart re-attach; serves non-WS consumers like shadow's claude-code session at local-host latency |
| Outpost installed binaries (v0.2)| Local filesystem on Outpost          | Per-host binary cache; checksums verified before swap; old generation retained for rollback |

Outpost holds **no** authoritative state — Nexus is the source of truth. The v0.2 mailbox cache and binary cache are *both* derived state: the mailbox can be rebuilt from Nexus's chat store, and the binaries are fetched from Nexus on demand. Losing either is recoverable (re-sync from Nexus); neither breaks the network. This preserves the v0.1 "no distributed-database reconciliation, no cross-node schema migrations, no sync protocol" property while still letting the Outpost do useful local work.

### 8.2 Outpost mailbox (v0.2)

The Outpost now holds a **per-local-aspect mailbox** — a bounded local store of inbound `chat.deliver` frames addressed to aspects whose home host is this Outpost.

**Why it's needed:**
- Aspects restart for binary updates (§13). Without a local buffer, every restart races chat traffic — frames in flight during the restart window are dropped at the WS layer and must be replayed from Nexus on reconnect, paying full round-trip latency.
- Non-WS consumers like shadow's claude-code session don't hold a persistent WS connection. They poll an MCP server (`nexus-comms-mcp`) which today connects directly to Nexus over the tailnet. A local mailbox lets that MCP poll the Outpost over loopback, dropping per-poll latency from network round-trip to IPC.
- The Outpost can push wake events to non-WS consumers when new mail arrives (filesystem touch, named-pipe write, SIGUSR1) — removes polling latency entirely for clients that opt in.

**Mechanism:**
- Outpost subscribes to `chat.deliver` frames for every locally-registered aspect.
- Frames are persisted to a small SQLite at `<outpost-dir>/mailbox.db` with `(aspect_id, msg_id, payload, received_at)`. Bounded by `OUTPOST_MAILBOX_MAX_PER_ASPECT` (default 500) on a FIFO drop policy.
- When a local aspect reconnects (post-restart, or first connect), the Outpost replays the relevant mailbox rows before flowing live frames.
- When an MCP / IPC client polls (`outpost.mailbox.read?aspect=<id>&since=<msg_id>`), the Outpost serves from the local SQLite — no network round-trip.
- When new mail arrives, the Outpost optionally signals registered local consumers (`OUTPOST_PUSH_<aspect>` config: filesystem path to touch, named pipe to write to, signal to send) so polling cadence can drop to "wake on push."

**Consistency:**
- Nexus still owns the canonical chat store. Mailbox at Outpost is a write-through cache of `chat.deliver`s the Outpost saw; if it's missing, the consumer can fall back to Nexus over tailnet (the `nexus-comms-mcp` route works today, would remain the fallback).
- Outpost prunes its local mailbox per the FIFO bound + on graceful `aspect.deregister`. Rebuilding from Nexus's `since_msg_id` replay on reconnect is the recovery path.

**Not in scope here:**
- Outpost storing `chat.send` outbound — that path already queues on the Outpost during Nexus-link partition (§9.1). The mailbox property is purely inbound.
- Mailbox for non-chat frames (knowledge.search.result, hand.result, etc.) — these are request/response, not unsolicited push, so the persistent-WS reconnect-and-retry model is sufficient. Mailbox is specifically for *unsolicited inbound to long-lived aspect identities*.

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

- **WS library**: Go stdlib doesn't include WebSocket. Use `github.com/coder/websocket` (modern, clean API) or `github.com/gorilla/websocket` (mature, widely used). Lean: coder/websocket. **Resolved:** `coder/websocket` shipped.
- **Frame encoding**: JSON is v1. MessagePack / CBOR for efficiency is a future option.
- **Per-aspect auth**: ~~v1 uses one shared bearer token for all clients. Per-aspect tokens are a hardening step later.~~ **Resolved in v0.2:** `Outpost.Config.AspectTokens` maps inbound aspect-name → per-aspect bearer (`#33`). Production wiring uses keyfile-derived JWTs minted by Nexus on validation; legacy shared-token mode persists as a back-compat path for in-development setups but is not production.
- **Multi-tier Outposts**: v1 forbids dispatcher-of-dispatcher. If ever needed (deep federation), revisit.
- **Durable outbox**: v1 in-memory queue on Outposts. v0.2 adds a bounded mailbox cache (§8.2) for inbound `chat.deliver`. If wider reliability matters for additional frame types, a per-aspect durable outbox on Nexus is the add-on.

## 13. Deploy plane (v0.2)

The Outpost owns three coordinated lifecycle actions that nothing else in the stack does for us: distributing new binaries to per-host caches, restarting aspects to pick them up, and reporting the resulting lifecycle transitions back to Nexus so the operator can see what happened.

### 13.1 Why the Outpost (not Nexus, not the OS)

- **Container orchestration doesn't help.** Docker/systemd restart on crash, but they don't know about "Nexus pushed a new agentfunnel-linux build; restart the linux-shaped aspects on this host to pick it up." That's a coordinated network-driven action.
- **Nexus can't reach into hosts directly.** Aspects live wherever their operators put them — little-blue (macOS), dMon (Windows), nexus-cw-ec2 (Linux), forge's WSL/Ubuntu. Nexus has no inventory of those hosts beyond "an Outpost identifies as being on host X."
- **The Outpost is the natural agent.** It's the per-host process that already (a) holds an authenticated WS to Nexus, (b) knows what aspects run on its host, (c) can spawn them. Adding "fetch a binary, swap it, restart the affected aspects" is the next natural responsibility.

### 13.2 Binary update delivery

**Catalogue:**

- **`binary.advertise`** (Nexus → Outpost) — Nexus has a new build available. Payload: `{ name: "agentfunnel-linux" | "agentfunnel.exe" | "nexus.exe", version: <semver-or-sha>, sha256: <hex>, size_bytes, download_url, applies_to: {os, arch}, signed_by: <nexus-key-id>, signature: <ed25519> }`. Outposts subscribe at register-time to advertisements matching their host's `{os, arch}`.
- **`binary.fetch`** (Outpost → Nexus) — request the bytes. Nexus responds with the binary over the WS in chunked frames, OR with a redirect to an HTTPS download URL signed for short expiry. Default: HTTPS redirect (WS frames don't multiplex well for multi-MB transfers).
- **`binary.fetched`** (Outpost → Nexus) — Outpost confirms it received and verified the binary. Payload: `{ name, version, sha256, fetched_at }`. Verification = sha256 match + signature check against Nexus's published key.

**Local cache shape:**

```
<outpost-dir>/
  bin/
    agentfunnel-linux@<sha>
    agentfunnel-linux@<sha>.prev   # last generation, retained for rollback
    agentfunnel-linux              # symlink → current generation
```

The Outpost holds the **current** + **previous** generation only. Older generations are gc'd. Rollback = relink to `.prev`.

**The Outpost does NOT auto-swap.** A new binary lands in the cache as a candidate; switching the symlink + restarting the aspect requires an explicit `aspect.restart` (§13.3). This is the operator's safety valve: a bad binary push doesn't take down the network.

**Signature verification:**

The Nexus instance key (already used for `PublicProjectionAdvance` records in the Cairn delay-policy spec) signs binary advertisements. The Outpost validates against the published key at boot. A binary that fails sig-check is logged loudly and discarded — never installed, never advertised as available.

### 13.3 Restart signalling

**Catalogue:**

- **`aspect.restart`** (operator → Nexus → Outpost) — please restart aspect X on this host. Payload: `{ aspect_id, target_binary: <name@version>, grace_period_s: 30, reason: <human-string> }`. `target_binary` lets the operator pin which generation to swap to (defaults to the most-recently-fetched).
- **`aspect.restart.ack`** (Outpost → Nexus) — accepted; restart in progress. Carries `{ aspect_id, old_pid, expected_new_pid_known: false }`.
- **`aspect.restart.done`** (Outpost → Nexus) — aspect is back up. Payload: `{ aspect_id, new_pid, new_binary_version, restart_duration_ms, mailbox_drained_count }`.

**Restart sequence (Outpost-side):**

1. Receive `aspect.restart`, ack immediately.
2. Send `shutdown` frame to the local aspect with the requested `grace_period_s`. Aspect's funnel uses the grace window to finish its current turn + flush outbound frames.
3. Wait up to `grace_period_s`. If the aspect's WS doesn't close in that window, SIGTERM the process; another `grace_period_s/2` later, SIGKILL.
4. Swap the binary symlink to `target_binary`'s generation.
5. Spawn the aspect via the existing §7 auto-spawn machinery (same env, same home, same WS-back-to-this-outpost route).
6. Wait for the new aspect's `register` frame to arrive.
7. On register, drain mailbox (§8.2) to the new aspect: replay any `chat.deliver` frames that landed during the restart window.
8. Send `aspect.restart.done` upward with the lifecycle summary.

**Failure modes:**

- Aspect refuses to shut down within `grace_period_s` → escalate to SIGTERM/SIGKILL. Logged. Restart-done frame carries `forced: true`.
- New binary fails to start → relink symlink to `.prev`, spawn previous generation, send `aspect.restart.done` with `rolled_back: true` and an error string. Operator sees the rollback in the activity feed.
- Aspect re-registers under a different identity than expected → reject the connection; flag the Outpost as compromised (the new spawn should have produced exactly `<aspect_id>`).

### 13.4 Lifecycle reporting

**Catalogue:**

- **`aspect.lifecycle`** (Outpost → Nexus) — proactive notification of any state transition for a locally-managed aspect. Payload: `{ aspect_id, transition: "spawned" | "shutdown_signalled" | "exited" | "registered" | "deregistered" | "rolled_back", at, details: { pid, exit_code?, binary_version?, signal? } }`.

Sent for both operator-driven restarts (so Nexus can correlate with the `aspect.restart` it issued) AND unplanned exits (crash, OS kill). For the unplanned case the Outpost still doesn't *respawn* (§7.3) — it just reports the exit. The operator decides whether to restart.

Lifecycle frames feed the operator dashboard's activity stream the same way chat does. If forge crashes on its WSL host, the operator sees it in chat-bus visibility within milliseconds.

### 13.5 Push-event delivery to non-WS consumers

Some clients don't hold a persistent WS to the Outpost. Shadow's claude-code session connects via the `nexus-comms-mcp` stdio MCP — that MCP doesn't run a daemon; it polls.

**Per-aspect push config** (in `<aspect-home>/aspect.json` or via env):

```
push_targets:
  - kind: file_touch
    path: "/tmp/nexus-comms.wake"
  - kind: named_pipe
    path: "/tmp/nexus-comms.pipe"
  - kind: signal
    pid_file: "/var/run/nexus-comms-mcp.pid"
    signal: "SIGUSR1"
```

On every new `chat.deliver` landing in the local mailbox for that aspect, the Outpost fires every registered push target. Consumers (the MCP, a dashboard tail, a CLI watcher) handle them as wake events and poll the mailbox immediately, dropping latency from the polling cadence to ~IPC-roundtrip.

This is **optional**. Aspects that don't declare push targets just have their messages buffered in the mailbox and consumers fall back to polling cadence.

### 13.6 Connection topology with the deploy plane

The pre-v0.2 topology (§2) is unchanged: aspect ↔ Outpost ↔ Nexus. The deploy-plane additions are all carried over the same WS, just new frame kinds.

The shadow / claude-code shape is new:

```
              ┌──────────────┐
              │    Nexus     │
              └──────┬───────┘
                     │ WS (single)
              ┌──────┴───────┐
              │   Outpost    │
              │ - mailbox    │
              │ - binary cache│
              └──┬──────┬────┘
                 │      │
       WS (local)│      │ stdio MCP + mailbox poll over loopback
                 ▼      ▼
            ┌────────┐  ┌─────────────────────────┐
            │ aspect │  │ claude-code session     │
            │ (push) │  │ (shadow, plumb-cli, etc)│
            └────────┘  └─────────────────────────┘
```

Persistent-WS aspects connect to the Outpost as before. Non-WS clients (CC sessions) talk to a local MCP server (`nexus-comms-mcp`) that opens its own short-lived WS or — in the post-v0.2 shape — uses an Outpost-local API instead. Either way the mailbox state lives next to them; no tailnet round-trip per poll.

## 14. Status

v0.2 draft. v0.1 locks the WS-first shape, Outpost topology, aspect/dispatcher config model, frame catalogue, data-ownership split, partition behaviour, HTTP surface, and migration plan. v0.2 adds the Outpost deploy plane (binary delivery, restart signalling, lifecycle reporting, push delivery to non-WS consumers) and the local mailbox property that makes restarts and CC-session-style consumers cheap. Implementation against v0.2 is filed as NEX-20 / NEX-25 children in Jira.
