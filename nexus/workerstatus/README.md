# nexus/workerstatus

M1 Unit 5 — the worker-status contract (`PHASE2-DESIGN.md` §5): every
worker (and future orchestrator invocation) publishes ONE machine-readable
status shape on a heartbeat; the broker upserts it into a `worker_status`
table; `GET /api/admin/workers` serves the consolidated fleet from one
query. State machine first, events over scraped prose.

## The status shape

```
worker_status: {agent, role, personality, work_item_id,
                state: spawning|running|blocked|awaiting_gate|done|failed,
                auth_ok, token_expires_at, provider, model,
                cli_version, image_tag,
                last_heartbeat, started_at, turns, tokens_used}
```

Three representations of this shape exist, deliberately kept distinct:

| Layer | Type | Where |
|---|---|---|
| Wire | `frames.WorkerStatusPayload` | `nexus/frames/payloads.go` |
| Store | `workerstatus.Status` | this package |
| API response | `workerstatus.Status` (JSON, via `{"workers": [...]}`) | `GET /api/admin/workers` |

`nexus/broker/dispatch_status.go`'s `handleWorkerStatusFrame` is the only
translator between the wire payload and the store row — it also
re-attributes `agent` to the connection's authenticated identity
(`wsConn.registeredAs`), never trusting `payload.Agent` (a worker cannot
spoof another worker's row; same posture as the `observe.*` frames per
keel-cli's caveat #236).

## Why a NEW frame kind, not an extension of `dispatch.status`

`dispatch.status` (`DispatchStatusPayload`) is the narrower
accepted/failed run-lifecycle signal `nexus/runs` already consumes —
its `handleDispatchStatusFrame` logic (accepted → `MarkAccepted`,
failed → `MarkDone` + escalation logging) is unrelated to fleet-status
consolidation and already has test coverage (`runs_adapter_test.go`,
predating this unit — the build spec's claim that this path is
"currently untested" no longer matches the code on the integration
line).

`worker.status` (`KindWorkerStatus` / `WorkerStatusPayload`) is a new,
higher-frequency, broader shape. Frames are UNVERSIONED — field-additive
and kind-additive only, never repurpose an existing field or kind — so
adding a new kind means an old receiver that only understands
`dispatch.status` is completely unaffected by this addition, and this
unit never has to touch the existing dispatch.status contract.

## Heartbeat cadence + seams

Emitted over the worker's own aspect WebSocket (no new connection) at:

1. **Boot** — once `agentfunnel` has finished validation/funnel/wsasp
   setup and is about to enter the deliberation loop. State: `running`
   (by the time a worker can emit at all, "spawning" has already
   resolved — see `runtime/cmd/agentfunnel/main.go`, the
   `statusEmitter.Emit(ctx, "running")` call right before "starting
   deliberation loop").
2. **Each main-turn boundary** — `runtime/cmd/agentfunnel/workerstatus.go`'s
   `turnMetricsTracker` wraps the funnel's `ObservabilityHook` (a
   transparent decorator — every `BeginTurn`/`OnBridleEvent`/`EndTurn`
   call still reaches `obsforward`/builder-progress reporting
   unchanged) and additionally counts completed "main"-label turns and
   accumulates token usage from `bridle.TurnDone.Result.Usage`. On each
   `EndTurn` for a "main" turn, it fires an `onMainTurnEnd` callback
   that re-emits the heartbeat with the freshest `turns`/`tokens_used`.
   Judge/compact sub-turns accumulate tokens (real spend) but don't
   increment `turns` (not deliberation progress).
3. **~60s wall-clock ticker** — `workerStatusEmitter.StartHeartbeat`
   runs a **funnel-owned `time.Ticker`**, started once at boot alongside
   the boot emit. This exists because bridle has no mid-turn heartbeat
   hook (confirmed by the wave/audit that scoped this unit) — a worker
   that's mid-turn for minutes at a time (a long tool call, a slow
   provider) still needs *something* refreshing `last_heartbeat` so
   unit 6's stale-detection doesn't reap live work.

All three paths funnel through the same `workerStatusEmitter.Emit`,
which is **best-effort by construction**: `SendWorkerStatus` uses
`wsasp.Client.SendBestEffort` (no durable outbound queue, unlike
`SendDispatchStatus`) — a dropped heartbeat is superseded by the next
tick or turn boundary, so queuing it behind a reconnect would only
deliver a burst of stale snapshots later. A send failure is logged at
debug and otherwise swallowed; `Emit` has no return value on purpose —
nothing in the heartbeat path can block or fail the worker's real turn.

### Field sourcing

| Field | Source |
|---|---|
| `agent` | connection identity (`wsConn.registeredAs`), not the payload |
| `role`, `personality`, `work_item_id` | `dispatch.Brief.Role/Personality/WorkItemID` → `CW_ROLE`/`CW_PERSONALITY`/`CW_WORK_ITEM_ID` env (jobspec.go; `CW_ROLE` is new in this unit — the other two already existed from M1 Unit 3/4) |
| `provider`, `model` | the funnel's own atomic binding cache (`*bindingCache.Load()`) — reflects NEX-335 admin provider-binding edits within one heartbeat |
| `auth_ok`, `token_expires_at` | the aspect's session-JWT state (`sessionState.Snapshot`) — **not** yet the `CLAUDE_CODE_OAUTH_TOKEN` almanac-sourced secret from PHASE2-DESIGN §6/§7 (that's a later build unit); this reports the equivalent "is my auth about to die" signal with what agentfunnel already tracks today. Swap the `authState` closure in `main.go` when §7 lands. |
| `cli_version` | best-effort `<claude binary> --version` at boot (`detectCLIVersion`) — empty for non-claude-code providers, which is expected, not an error |
| `image_tag` | `CW_IMAGE_TAG` env, new in this unit, set from `JobConfig.Image` in `jobspec.go` |
| `turns`, `tokens_used` | `turnMetricsTracker.Snapshot()` |
| `last_heartbeat` | stamped fresh on every `Emit` |
| `started_at` | stamped once at process start; the store's `Upsert` never lets a later zero-valued `started_at` clobber an already-recorded one |

## Endpoint contract

`GET /api/admin/workers` → `{"workers": [workerstatus.Status, ...]}`,
most-recently-heartbeated first.

**`requireAdmin`, not `b.auth`** — this is the separation-of-duties
lesson from the build spec: worker status (`auth_ok`/`token_expires_at`,
`cli_version`, the live provider/model binding) is operator/admin fleet-
management data, not something any authenticated aspect should be able
to read about its peers. `registerAdmin` (`nexus/broker/admin.go`) only
wires the route when `Config.WorkerStatusStore` is non-nil — same
"config gates the surface" convention as the other `KeyfileValidator`/
`Credentials`-gated admin routes. Without a configured store the route
isn't registered at all (404, not 501).

## How unit 6's orchestrator will consume this for auto-reap

PHASE2-DESIGN §2.1 ("terminate-and-requeue") names "stale heartbeat > N
min" as one of the orchestrator's own automation triggers for
`cancel(work_item, requeue=true)`. `Status.Stale(now, maxAge time.Duration) bool`
is the helper this unit ships for that: a zero `LastHeartbeat` (never
reported) is always stale; otherwise `now.Sub(LastHeartbeat) > maxAge`.

The intended unit 6 wiring (not built here — out of this unit's scope):

1. On each orchestrator wake (or a dedicated reap sweep), call
   `WorkerStatusStore.List(ctx)` and filter with `Stale(time.Now(), N)`
   for whatever `N` the orchestrator config picks (PHASE2-DESIGN §2.1
   suggests minutes, with "recovery before escalation" — first strike:
   reap the k8s Job + requeue the work_item (`status` back to
   `queued`, termination reason recorded in `prior_results`); only
   alert the operator on a **second** strike for the same work item.
2. Cross-reference `work_item_id` on the stale row against the
   work-graph (`nexus/workgraph`) to find the item to requeue — this is
   why `work_item_id` rides on every heartbeat, not just at dispatch
   time.
3. `Delete` the stale row once its Job is confirmed reaped (or leave it
   — a subsequent `Upsert` from a redispatched worker on the same agent
   name naturally supersedes it; `Delete` exists mainly for explicit
   cleanup after an `requeue=false` abandon).

## Live-verify path (documented, not run in this environment)

1. Bring up a broker with `Config.WorkerStatusStore` set (production
   wiring: `nexus/cmd/nexus/main.go` now constructs
   `workerstatus.NewSQLStore(db)` and passes it in, alongside the
   existing `RunsStore`/`ConveneStore`).
2. Dispatch a builder (`!dispatch <agent> ticket=... <task>`, or the
   pool-lease path from M1 Unit 4) — `agentfunnel` boots in `-builder`
   mode, validates, and calls `statusEmitter.Emit(ctx, "running")`
   right before entering its deliberation loop.
3. `curl -H "Authorization: Bearer <admin token>" https://<broker>/api/admin/workers`
   — the dispatched agent's row should appear with `state: "running"`,
   `role`/`work_item_id`/`personality` matching the brief, `provider`/
   `model` matching the resolved binding, and a `last_heartbeat` within
   the last minute.
4. Watch the row across repeated `GET` calls roughly a minute apart —
   `last_heartbeat` should advance on the ~60s ticker even during a long
   single turn (no turn boundary in between), and `turns`/`tokens_used`
   should step up each time the builder completes a main turn.
5. Confirm the same request with a non-admin (peer-agent) bearer token
   returns 403 `admin_required` (the `requireAdmin` gate) — this is the
   negative case the build spec calls out explicitly as a
   separation-of-duties requirement, not just an implementation detail.
6. When the builder finishes (task_done / PR verified) and the Job
   exits, its `worker_status` row is left at its last-reported state
   (this unit does not wire a `done`/`failed` terminal emit — a natural
   follow-up, not required by this unit's acceptance criteria, which
   scope emission to boot/turn-boundary/ticker). A future pass wiring
   `builderOnTaskDone`/the idle-monitor failure callback to also call
   `statusEmitter.Emit(ctx, "done"|"failed")` would close that gap.
