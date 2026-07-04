# Phase 2 — Pool Mechanics: Work Graph, Role-at-Spawn, Standing Orchestration

*Design for the network rebuild's build phase. v1 — 2026-07-05. Companion to `ROLE-MODEL.md` (the operating rules), `roles/*.yaml`, `handoff.schema.json`.*

## 0. Supersession notice (read first)

This design **supersedes two recorded decisions**:
- `docs/2026-06-08-named-agent-dispatch-model.md` (work runs AS a named member; persona is load-bearing)
- `docs/2026-06-07-dispatch-roles-build-review-verify.md` (no role enum in dispatch infra; roles as briefs only)

**Why:** the named-personality fleet sprawled — every capability became a named aspect to maintain (operator, 2026-07-04: "I had too many roles"). The rebuild unbundles role/identity/personality/model (`ROLE-MODEL.md` §1). Personality is demoted to a thin spawn-time label with zero capability; **role becomes a first-class dispatch concept** — the June docs' one concession ("role touches infrastructure where it gates credential scope") is in fact the general case: roles gate skills, tools, tiers, and write access. Named aspects (plumb/anvil/keel/…) are retired to personality labels.

What survives from the June model: real accountable identities under every worker (the pool slots), the brief as the task carrier, the audit thread. We change *addressing* (role, not name), not accountability.

## 1. Work graph (the orchestrator's state)

Work is a persistent **graph of work-items**, not a pipeline. Streams = sibling subtrees. Extend the runs store (`nexus/runs/`) with a `work_items` table:

```
work_item: {id, parent_id, stream_id, role, status, depends_on[],
            handoff jsonb,          -- the work_item blob per handoff.schema.json
            created_by,             -- operator|scheduled|event|rework(item_id)
            result jsonb}           -- the result blob when done
status: queued | ready | dispatched | running | done | rejected | blocked | cancelled
```

Rules:
- An item is **ready** when all `depends_on` are `done`. Ready items dispatch in parallel (pool cap permitting) — parallel streams fall out for free.
- Every result returns to the orchestrator (hub). `pass/done` → create next-stage item(s). `reject` → create a **rework item** with a back-edge: `created_by: rework(<rejecting_item>)`, `prior_results` carrying the rejecting result. `blocked` → escalate to operator.
- No role-to-role handoffs in v1. Flexibility = graph + machine-routable verdicts + orchestrator judgment. **We do not build a workflow engine.**

Reuse: `runs` table lifecycle, `ParentRunID`/`SpawnParent` lineage, `dispatch.Runner` queue+caps.

## 2. Orchestrator: event-triggered, stateless, over persistent state

Not a CronJob-cadence drain (pays latency at every hop of every stream), not an idle frontier pod (pays tokens to wait). **Each invocation is one-shot**: wake → read graph + new results → judge → create/dispatch next items → exit. Crash-safe because all state is in the store.

- **Wake triggers:** `OnJobDone` completion hook (primary — hop latency becomes seconds), shadow-enqueue (new work), cadence fallback (catches missed events), operator poke.
- **Body:** evolve `-drain` mode (`runtime/cmd/agentfunnel/drain.go`) — the orchestrate procedure exists; retarget it from Jira-snapshot to graph-drain, keep the cheap gate (don't spend frontier tokens on an empty graph).
- **Preflight (fail-loud):** every wake starts with an auth probe (§6). Auth dead → HOLD the queue (items stay queued, nothing fails) + alert operator via chat + loki-alert-bridge. A stalled pipeline pages; it never waits to be noticed.

## 2.1 Terminate-and-requeue (built-in, not an afterthought)

`cancel(work_item, requeue=true)` — kill the k8s Job, release the pool lease, mark the **run** `cancelled`, set the **work_item** back to **`queued`** with the termination reason recorded in `prior_results` (a redispatch knows it was cut short and why). `requeue=false` = abandon: item → `cancelled`, dependents → `blocked` → operator escalation.

Callers:
1. **Operator** — chat command / `nexus work cancel <id>` / admin API (reuse+extend the existing run-cancel RPC).
2. **Orchestrator judgment** — gate evidence says a stream is wrong → cut it early rather than let it finish.
3. **Orchestrator automation** — stale heartbeat (§5) > N min → reap Job, requeue item; alert only on second strike. Recovery before escalation.

Half-done work is preserved: the terminated builder's cairn line keeps its commits; the requeued item redispatches onto the same line so work continues (or the orchestrator explicitly abandons the line). Nothing partial reaches main — only the orchestrator folds, only on all-gates-pass.

## 3. Role-at-spawn

- **`Brief` gains:** `Role`, `WorkItemID`, `SkillAllowlist []string`, `PolicyFragment` (ToolPolicy overlay), `Personality string`. Transport unchanged (brief ConfigMap → `-brief-file`).
- **agentfunnel:** `composeSystemPrompt` prepends the **role prompt** (from `roles/*.yaml`, delivered in the brief) above the (thin) personality; `loadToolPolicy` accepts the spawn-supplied policy fragment instead of only the static `-policy` file (completes the recorded Tier-B TODO, delivered per-spawn rather than per-aspect).
- **Skill gating (new primitive):** filter `.agents/skills` materialisation at spawn to the role's allowlist. Tool gating rides the existing P3b `PermissionHook`.
- **Write scope:** builder gets a cairn line + workdir; tester test-paths only; reviewers read-only — enforced via `WritePathAllow` + credential scope (read-only cairn token for gate roles).

## 4. Pool of 3

Three **generic slots as derived identities** leased per-dispatch via the existing hand machinery (`freeHandNames` / `aspects.DerivedName`) under the orchestrator's parent identity — e.g. `orch.sub-1..3`. Reuses live, tested identity/keyfile/JWT plumbing; no resurrection of the deleted pool-slot keyfile model. New: a **pool-cap dimension** in `canRun` (3 concurrent leases) replacing per-agent-name serialization for pool work. Accountability = slot identity + role + work_item in every commit/result; personality is display-only.

## 5. Worker status contract (uniform, consolidatable)

Every pod (workers AND orchestrator invocations) publishes one shape — **state machine first, events over scraped prose**:

```
worker_status: {agent, role, personality, work_item_id,
                state: spawning|running|blocked|awaiting_gate|done|failed,
                auth_ok, token_expires_at, provider, model,
                cli_version, image_tag,
                last_heartbeat, started_at, turns, tokens_used}
```

- Emitted at boot, each turn boundary, and on a ~60s heartbeat — over the **existing** in-band `dispatch.status` frame path (`nexus/broker/dispatch_status.go`); broker upserts a `worker_status` table.
- **One consolidation endpoint:** `GET /api/admin/workers` → the fleet, one query. The Phase-5 UI, the minimal status view, and `nexus workers` CLI all read this and nothing else.
- Auth preflight results and CLI versions report through the same payload: "is the token dying" / "which pod is on an old CLI" are queries, not log archaeology.

## 6. Frontier auth resilience

- **Credential:** `claude setup-token` (operator, one-time browser flow) → k8s secret → `CLAUDE_CODE_OAUTH_TOKEN` injected into every frontier seat (orchestrator invocations, reviewer/security pods, croft). Kills the idle->8h expiry & the morning re-login.
- **Fail-loud:** orchestrator preflight (§2) + `auth_ok/token_expires_at` in every heartbeat (§5) + alert on failure or near-expiry (loki-alert-bridge → operator). The queue holds; work never silently fails.

## 7. CLI version strategy

- Runner images pin the CLI (`@anthropic-ai/claude-code@X.Y.Z`, auto-updater disabled — pods never self-update mid-job).
- Scheduled CI rebuild on new releases → images tagged `runner:cli-<ver>`.
- **One config knob** (broker setting) selects the tag: default = latest built; override = pin on bug; clear pin when fixed. jobspec reads the knob at dispatch.
- `cli_version`/`image_tag` in every heartbeat closes the loop.

## 8. Context passing (data plane ≠ chat)

**Chat is the audit surface; files + refs are the data plane.** Handoff JSON lives in the work graph; artifacts pass by reference (cairn commit ids, paths); the **cairn line checkout is the shared workspace** (builder commits, tester/reviewer read the same line; evidence files ride alongside). Chat carries only human-readable summaries. No context service, no new store.

## 9. Minimal status view (not the UI)

One server-rendered page over `GET /api/admin/workers` + the work graph (deliberately boring; HTMX-style). The **full UI rethink is Phase 5**, designed against the running network. Requirements already banked: work-graph view, gate verdicts, blocked-items queue, operator-visual judgment queue.

## 10. Build order

1. `work_items` table + graph CRUD in the runs store  *(builder)*
2. `Brief` extension + jobspec threading + funnel role-overlay/policy/skill-gating  *(builder ×2, parallel)*
3. Pool leasing + cap  *(builder)*
4. Worker status frames + `worker_status` table + `/api/admin/workers`  *(builder)*
5. Orchestrator graph-drain + OnJobDone wake + auth preflight/alerts  *(builder, after 1+4)*
6. Auth secret wiring + CLI version knob + CI image rebuild  *(infra)*
7. Minimal status view  *(builder, last)*
Each unit: bounded spec + acceptance criteria, gates per `ROLE-MODEL.md` §4. Built via the existing ticket-pipeline (shadow orchestrates) — the new network builds itself only after it exists.
