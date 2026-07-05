# nexus/orchestrator

The M1 Unit 6 standing orchestrator **MECHANISM** (PHASE2-DESIGN §2, §2.1,
§5, §6): a stateless drain pass that reads the work graph + worker
heartbeats, dispatches ready items to the pool, reaps dead workers, and
holds on auth failure.

**Scope boundary:** this package is the CODE MECHANISM, not the LLM
decomposition logic. "What work to create next" is the orchestrator's
runtime drain PROMPT — a later concern, out of scope here. Everything in
this package is deterministic and unit-testable with fakes; no live LLM is
involved.

## Drain-pass lifecycle

`DrainOnce(ctx) (DrainReport, error)` is one pass:

1. **`ReapStale`** — requeue any work item whose worker's heartbeat has
   gone stale. Runs at the top of every pass (see "Reap + strike policy").
2. **`PreflightAuth`** (`Orchestrator.AuthProbe`, if configured) — if it
   fails, HOLD: dispatch nothing this pass, alert, return
   `(DrainReport{Held: true, HoldReason: ...}, nil)`. See "Auth-hold".
3. **Dispatch** — for each role in `Orchestrator.Roles`:
   `workgraph.ListReady(role, Stream)` → for each ready item,
   `workgraph.Claim(id, ClaimAgent)` (the idempotent-dispatch guard — see
   below), resolve the role overlay (`Orchestrator.Resolver`, optional),
   `dispatch.SubmitPoolItem`, then `workgraph.Transition(id, dispatched)`
   on a successful submit.

`DrainOnce` reads all state fresh from `Graph`/`WorkerStatus` on every
call — the only in-process state this package keeps across calls is the
reap strike-counter (see below). Crash-safe: a crashed orchestrator loses
nothing durable; the next `DrainOnce` picks up exactly where the stores
say work stands.

### Idempotent dispatch (no double-dispatch across passes)

Per the workgraph README's own caveat, ledger's `ListReadyIssues` filters
`status IN ('To Do', 'In Progress')` — meaning an item this orchestrator
already transitioned to `dispatched` (`In Progress`) can plausibly still
show up in a later `ListReady` call. `DrainOnce` does NOT rely on
`ListReady` excluding already-handled items; it relies on
**`workgraph.Claim`'s atomicity** instead: every ready item is claimed
(`ClaimAgent`, default `"orchestrator-drain"`) before dispatch, and
`ErrAlreadyClaimed` means an earlier pass (or a concurrent one) already has
it — the item is recorded in `DrainReport.Skipped` and left alone, never
dispatched twice. `TestDrainOnceSkipsAlreadyDispatchedAcrossTwoPasses`
exercises exactly this path with a fake that intentionally re-surfaces the
already-claimed item.

## Result intake: `RecordJobResult`

`RecordJobResult(ctx, workItemID, result) (DrainReport, error)`:

- `workgraph.RecordResult` always runs first (records the result comment;
  `done` transitions the item to `Done`, `blocked` to `Blocked` — both
  handled inside `workgraph.RecordResult` itself).
- `VerdictReject`: additionally calls `workgraph.Rework` to create the
  follow-up work item (task_spec relays the rejection reasons — mechanical
  relaying, not a judgment about what new work to create; acceptance
  criteria and cairn line are carried forward from the rejected item).
- `VerdictBlocked`: additionally alerts (operator escalation, per
  PHASE2-DESIGN §1 "blocked -> escalate to operator").
- `VerdictPass`: no extra action — passing a gate doesn't terminate the
  item; a later hop marks it done.
- Every call ends with a **re-drain** (`DrainOnce`) — a done result may
  have unblocked dependents; a fresh rework item is itself now ready to
  drain.

## Wake / cadence / poke triggers

Per PHASE2-DESIGN §2 "Wake triggers": `OnJobDone` completion hook
(primary), cadence fallback (a ticker, catches missed events), and an
explicit poke (operator/admin calling `DrainOnce` directly). This package
only builds the first two triggers' plumbing seam plus the callable
primitives — the process wiring (a goroutine ticker, an admin endpoint) is
left to the caller (broker/cmd), since it's deployment topology, not
mechanism.

### `OnJobDone` wake wiring

`runtime/dispatch.Runner` gained an additive `OnJobDoneHook func(JobDone)`
field (nil by default, reproducing `OnJobDone`'s exact prior behavior).
Wire this package's `Orchestrator.OnJobDoneHook()` into it:

```go
runner.OnJobDoneHook = orch.OnJobDoneHook()
```

`OnJobDone` (unchanged) still frees the completed run's agent, drains the
dispatch queue, and posts the completion summary exactly as before this
unit — the hook fires strictly AFTER all of that, as an addition, never a
replacement.

**`dispatch.JobDone` carries only `Ticket` (== the pool work-item id) and
an `OK` bool, not a full `workgraph.Result`.** There is no richer
worker-reported verdict channel (pass/reject/blocked + reasons/artifacts)
in this codebase yet — the worker's actual gate verdict has no wired path
back to the orchestrator today. `OnJobDoneHook()` is therefore a
**fallback translation**: `OK=true -> VerdictDone`, `OK=false ->
VerdictBlocked` (with a generic reason). This is flagged as a genuine gap,
not silently papered over: once a real result-reporting channel exists (a
worker posting its structured `Result` back, e.g. over a frame or an admin
RPC), that channel should call `RecordJobResult` directly with the
worker's real verdict — `RecordJobResult` is exported for exactly that, and
is tested independently of the `OK`-bool fallback (see
`TestRecordJobResultRejectCreatesRework`, which a `JobDone`-only wake path
can never trigger).

### Cadence fallback

Not wired to a concrete ticker/CronJob by this package — `DrainOnce` is a
plain function; a caller wanting a cadence fallback runs
`time.NewTicker(period)` and calls `orch.DrainOnce(ctx)` each tick (or
reuses `runtime/cmd/agentfunnel/drain.go`'s existing `-drain` CronJob
skeleton as the timer, pointed at this package's `DrainOnce` instead of
the Jira-snapshot path it drains today — see "Reuse, don't rebuild" in the
build spec; that rewiring is left to the operator/a later ticket, not done
by this unit, which only had to ship `DrainOnce` as a callable package).

## Reap + strike policy

`ReapStale(ctx) ([]string, error)` scans `WorkerStatus.List` for rows
whose heartbeat has gone stale (`workerstatus.Status.Stale(now,
StaleAfter)`, default 5 minutes) and requeues the bound work item
(`workgraph.Cancel(id, requeue=true, "stale heartbeat: ...")`) — this is
PHASE2-DESIGN §2.1's "orchestrator automation" cancel/requeue path, not
the operator's or orchestrator-judgment's cancel paths.

**Recovery-before-escalation**: a work item alerts only on its SECOND
consecutive stale-and-reaped strike. `Orchestrator.strikes` (guarded by
`Orchestrator.mu`) is the one piece of in-process state this package
keeps beyond the durable stores — a work item whose worker row comes back
NOT stale on a later pass has its strike count cleared, so a genuine
recovery (redispatch lands on a healthy slot) never escalates; only a work
item that's STILL stale strike-over-strike pages. `Orchestrator.strikes`
is lost on a process restart — acceptable, since losing strike history
only means one additional silent reap before the next escalation, not a
durability or correctness issue (see package doc "Stateless": everything
that must survive a crash is already in the stores).

`ReapStale` runs at the top of every `DrainOnce`, and is independently
callable (e.g. a tighter reap-only cadence than the full drain).

## Auth-hold behavior

`Orchestrator.AuthProbe func(ctx) error` is the pluggable frontier-auth
preflight (PHASE2-DESIGN §6 "Fail-loud"). When set and it returns an
error, `DrainOnce` HOLDS: it dispatches nothing this pass (every ready item
stays queued/claimed exactly as it was), alerts, and returns
`DrainReport{Held: true, HoldReason: err.Error()}` — **never a Go `error`**
(an auth-down preflight is an expected, handled state, not a crash). Items
are never transitioned to failed on auth-down; they simply wait for the
next pass where the probe succeeds. A nil `AuthProbe` disables the gate
entirely (no preflight check) — the default, since a live probe
implementation (reusing worker-status `auth_ok`/`token_expires_at`, or a
cheap frontier ping) is deployment-specific and left to the caller to
wire, per this unit's scope (mechanism, not the probe's own implementation
choice).

## Alerting: no push API into `loki-alert-bridge`

The build spec asked this unit to "find how existing code posts to
loki-alert-bridge." Confirmed by reading
`runtime/cmd/loki-alert-bridge/{main,bridge,loki}.go`: **there is no such
API.** `loki-alert-bridge` is a pull-and-forward service — an Alertmanager
webhook receiver that, on receipt, queries Loki for surrounding log
context and posts a chat summary. Nothing in this codebase calls INTO it
programmatically; it is driven by Alertmanager alert rules (themselves
driven by Prometheus/Loki queries), not by application code pushing
alerts.

Given that, `Alerter` is a small pluggable interface
(`Alert(ctx, subject, detail) error`) rather than a loki-alert-bridge
client:

- **`LogAlerter`** (the package default) emits a structured
  `slog.Error("orchestrator: ALERT", "subject", ..., "detail", ...)` line —
  an Alertmanager rule matching this codebase's log-alerting convention
  (once one exists) can key on it and forward through
  `loki-alert-bridge`/Alertmanager exactly like any other paged condition.
- A caller wanting immediate chat visibility (not waiting on an
  Alertmanager rule's scrape interval) can supply its own `Alerter` backed
  by `dispatch.Poster` (`Post(thread, text)`, already used elsewhere in
  this codebase for status lines) for a direct post.

This is flagged as a genuine gap versus the spec's assumption, not
invented: there is no alert-push primitive to build against, so the seam
is pluggable rather than hard-wired to a nonexistent client.

## Role resolution (out of scope, by design)

`RoleResolver.Resolve(role) (rolePrompt string, skillAllowlist []string,
policy *funnel.ToolPolicy)` is an optional seam: `DrainOnce` calls it (if
`Orchestrator.Resolver` is set) before every dispatch to fill
`dispatch.PoolItem`'s role-at-spawn overlay fields. A nil `Resolver`
dispatches with the role LABEL alone (`RolePrompt=""`,
`SkillAllowlist=nil`, `PolicyFragment=nil`) — reproducing `SubmitPool`'s
original behavior exactly.

This unit does NOT ship a `docs/network/roles/*.yaml`-backed
implementation of `RoleResolver`. The build spec's deliverable 1 assumes
"the item's Role (label) + RolePrompt + SkillAllowlist + PolicyFragment"
are readily available to dispatch, but no code anywhere in this tree
resolves a role label to that overlay — `docs/network/roles/*.yaml` are
structured metadata (tier, skills, constraints, handoff shape), not
ready-to-use prompt text, and designing the YAML -> prompt-text transform
is a decision this unit's scope boundary ("code mechanism, not decomposition
judgment") argues belongs elsewhere. `RoleResolver` ships the seam;
wiring a concrete resolver is a follow-up.

## Deviations from the build spec's assumptions

1. **`nexus/workerstatus` was not yet on `builder/m1-integration`.** The
   spec says the integration base "already has ... `nexus/workerstatus`",
   but at express time `builder/m1-integration`'s tip (`96d81c40`) predates
   M1 Unit 5's fold — Unit 5 exists only on the sibling
   `builder/m1-unit5-worker-status` line (tip `b7d33b8c`), not yet reconciled
   into integration (build order step 6 says "after 1+5", so this is a
   sequencing gap in the base, not a spec error). This unit's line pulled
   Unit 5's changes directly (the diff is purely additive — new
   `nexus/workerstatus` package, `GET /api/admin/workers`, `worker.status`
   frame handling — verified via `cairn diff 96d81c40 b7d33b8c` before
   copying) so `Orchestrator.WorkerStatus`/`ReapStale` have a real
   `workerstatus.Store` to build against. Building on a phantom package
   would have been unbuildable and unverifiable.
2. **`dispatch.SubmitPool`'s actual signature has no room for
   `RolePrompt`/`SkillAllowlist`/`PolicyFragment`.** The spec's deliverable
   1 says to dispatch "via `dispatch.SubmitPool` with the item's Role
   (label) + RolePrompt + SkillAllowlist + PolicyFragment + WorkItemID" —
   but `SubmitPool(ctx, role, task, workItemID, thread string) (string,
   error)` (M1 Unit 4) only takes 4 strings, nowhere to carry the M1 Unit 3
   overlay fields through. Added `dispatch.PoolItem` (a struct superset)
   and `Runner.SubmitPoolItem(ctx, PoolItem) (string, error)` — `SubmitPool`
   is now one line of sugar calling `SubmitPoolItem` with an empty overlay,
   byte-for-byte the same behavior as before (all of `pool_test.go`'s
   existing `SubmitPool`-based tests pass unmodified). This is additive to
   `runtime/dispatch`, not a redesign — no existing signature changed.
3. **No push API into `loki-alert-bridge` exists** (see "Alerting" above)
   — the spec assumed one to find; there isn't one, so `Alerter` is a
   pluggable seam instead of a client for a nonexistent API.
4. **`dispatch.JobDone` carries no verdict** (see "OnJobDone wake wiring"
   above) — the spec's "wire OnJobDone to call RecordJobResult" implicitly
   assumes JobDone (or something near it) carries the worker's actual gate
   verdict. It only carries an OK bool. `OnJobDoneHook()` is documented as
   a fallback translation, not the final result-reporting channel.

## Live-verify path

1. **Enqueue → observe dispatch.** File a work item via
   `workgraph.CreateWorkItem` (role `builder`, no `depends_on`) against a
   live ledger + broker with a real `dispatch.Runner` (pool row + minter
   configured per `runtime/dispatch/README.md`'s pool live-verify
   prerequisites). Call `Orchestrator.DrainOnce` (or wait for the cadence
   ticker / `OnJobDoneHook` poke once wired). Confirm: the item's ledger
   status moves `To Do -> In Progress`, a `pool.sub-N` Job appears
   (`GET /api/admin/workers` or `kubectl get jobs -l
   nexus.dispatch/lineage=pool`), and `DrainReport.Dispatched` contains the
   item id.
2. **Complete → dependent unblocks.** File a second item with
   `depends_on: [<first item id>]`. Confirm it does NOT appear in
   `DrainOnce`'s dispatched/ready set while the first is running. Once the
   first Job completes and the wake fires (`OnJobDoneHook` → `RecordResult`
   done → re-drain), confirm the dependent now dispatches on the next
   pass without any operator action.
3. **Kill a worker's heartbeat → observe requeue.** While a pool Job is
   running, stop its heartbeat (kill the pod, or let `dispatch.status`
   frames stop) past `Orchestrator.StaleAfter` (default 5m). Call
   `ReapStale` (or wait for the next `DrainOnce`). Confirm: the work item's
   ledger status returns to `To Do` (`queued`), the stale `worker_status`
   row is still visible via `GET /api/admin/workers` (unchanged by
   `ReapStale` — it only acts on the work graph, not the status row) until
   a fresh heartbeat or explicit `Delete`, and — if the SAME item goes
   stale again on a later pass without recovering — an alert fires on
   exactly the second strike (verify via whatever `Alerter` is wired: the
   default `LogAlerter`'s `slog.Error("orchestrator: ALERT", ...)` line, or
   a chat post if a `Poster`-backed `Alerter` is configured).
