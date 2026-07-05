# M1 Unit 6 — Event-triggered orchestrator graph-drain (build spec)

**Goal:** the standing orchestrator MECHANISM — a stateless drain pass that reads the work-graph + new results, dispatches ready items to the pool, reaps dead workers, and holds on auth failure. The LLM judgment (decompose/route) rides the drain PROMPT; this unit builds the machinery around it. Ref: PHASE2-DESIGN §2, §2.1, §5, §6.

## Scope boundary (important — keep bounded)
This unit builds the CODE MECHANISM, NOT the LLM decomposition logic. The orchestrator's "judge what to create next" is its runtime drain prompt (a later concern). Deliver: the drain-pass functions + wake hook + auto-reap + auth preflight, wired to the pieces already on this integration line (nexus/workgraph, runtime/dispatch pool SubmitPool, nexus/workerstatus). Everything testable with fakes/unit tests — no live LLM needed.

## Build on the INTEGRATION line
`cairn express builder/m1-unit6-orchestrator --from builder/m1-integration`. This base has: `nexus/workgraph` (CreateWorkItem/ListReady/Transition/RecordResult/Rework/Claim/Cancel), pool dispatch (`runtime/dispatch` SubmitPool + role/skill/policy Brief fields), `nexus/workerstatus` (Store, Status.Stale()).

## Deliverables (a new package, e.g. `nexus/orchestrator`)
1. **Graph-drain pass** `DrainOnce(ctx) (DrainReport, error)`: for each role the pool serves, `workgraph.ListReady(role, "")` → for each ready item, dispatch via `dispatch.SubmitPool` with the item's Role (label) + RolePrompt + SkillAllowlist + PolicyFragment + WorkItemID (carry the handoff into the Brief); `workgraph.Transition(id, dispatched)` on successful submit. Idempotent: an already-dispatched/claimed item is skipped (use `workgraph.Claim` or a dispatched-status guard so two drain passes don't double-dispatch).
2. **Result intake** `RecordJobResult(ctx, workItemID, result)`: on job completion, `workgraph.RecordResult` (verdict→transition: done→Done, reject→Rework follow-up, blocked→escalate), then trigger a re-drain (ready dependents may have unblocked).
3. **Wake hook**: wire `runner.OnJobDone` (exists — fires on completion, currently frees agents + drains the dispatch queue) to ALSO call RecordJobResult + DrainOnce. Keep the existing OnJobDone behavior; ADD the orchestrator poke. The drain is event-triggered (on completion) + a cadence fallback (a ticker) + explicit poke.
4. **Auth preflight** `PreflightAuth(ctx) error`: before a drain dispatches frontier-tier work, probe frontier auth health (reuse whatever the worker-status auth_ok/session health exposes, OR a cheap frontier probe). On failure: HOLD (DrainOnce dispatches nothing, items stay queued) + alert via loki-alert-bridge (the alert sink that exists). Never fail items on auth-down — hold + page.
5. **Auto-reap** `ReapStale(ctx) (reaped []string, error)`: scan `workerstatus.Store` for rows whose `last_heartbeat` is stale (Status.Stale(), threshold configurable, default e.g. 5m) → `workgraph.Cancel(workItemID, requeue=true, "stale heartbeat")` to return the item to queued; alert only on a SECOND strike for the same item (recovery-before-escalation — track strikes in-memory or a small table). Reap runs each drain pass.

## Reuse, don't rebuild
- Study `runtime/cmd/agentfunnel/drain.go` (`runDrain`, `drainGateOpen`) — the existing `-drain` skeleton (Jira-snapshot today). You are providing the GRAPH-drain equivalent as a package the drain mode (or a broker goroutine) can call. Don't rip out the Jira drain; add the graph-drain path.
- `runner.OnJobDone` (nexus dispatch) for the wake.
- `loki-alert-bridge` for alerts (find how existing code posts to it).

## Constraints
- cairn line `builder/m1-unit6-orchestrator` off `builder/m1-integration`. `cairn commit`, no push.
- Stateless: DrainOnce reads all state from the stores (workgraph/workerstatus), acts, returns — no in-process graph cache beyond the strike counter. Crash-safe.
- Additive: don't break the existing dispatch/drain paths.

## Acceptance
1. `go build ./...` + `go vet` clean; existing tests pass.
2. Unit tests (fakes for workgraph/workerstatus/dispatch): DrainOnce dispatches only ready items and skips already-dispatched (no double-dispatch across two passes); RecordJobResult advances the graph (done→Done, reject→Rework created); PreflightAuth-fail holds the drain (nothing dispatched) + emits an alert; ReapStale requeues a stale item and second-strike alerts; the OnJobDone wake calls the intake+drain.
3. README: the drain-pass lifecycle, the wake/cadence/poke triggers, the reap+strike policy, the auth-hold behavior, and the live-verify path (enqueue a work-item → observe dispatch → complete → dependent unblocks → kill a worker's heartbeat → observe requeue).
