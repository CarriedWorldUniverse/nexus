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

- **Credential:** `claude setup-token` (operator, one-time browser flow) → **almanac `SecureParameter`** (source of truth — AUDIT correction: the frontier token is *internal platform config*, almanac's domain; custodian is external-creds only) → k8s secret as delivery → `CLAUDE_CODE_OAUTH_TOKEN` in every frontier seat (orchestrator invocations, reviewer/security pods, croft). Kills the idle->8h expiry & the morning re-login.
- **Fail-loud:** orchestrator preflight (§2) + `auth_ok/token_expires_at` in every heartbeat (§5) + alert on failure or near-expiry (loki-alert-bridge → operator). The queue holds; work never silently fails.

## 7. CLI version strategy

- Runner images pin the CLI (`@anthropic-ai/claude-code@X.Y.Z`, auto-updater disabled — pods never self-update mid-job).
- Scheduled CI rebuild on new releases → images tagged `runner:cli-<ver>`.
- **One config knob** (broker setting) selects the tag: default = latest built; override = pin on bug; clear pin when fixed. jobspec reads the knob at dispatch.
- `cli_version`/`image_tag` in every heartbeat closes the loop.

## 8. Context passing (data plane ≠ chat)

**Chat is the audit surface; files + refs are the data plane.** Handoff JSON lives in the work graph; artifacts pass by reference (cairn commit ids, paths); the **cairn line checkout is the shared workspace** (builder commits, tester/reviewer read the same line; evidence files ride alongside). Chat carries only human-readable summaries. No context service, no new store.

## 9. Document register + operator approval gate

Specs/plans/designs are first-class, structured, lifecycle-managed — **never a pile of files in a folder**. A doc that isn't attached to a work-item with a status doesn't exist.

```
document: {id, kind: spec|plan|design|report, title, version,
           status: draft|awaiting_approval|approved|approved_with_changes|rejected|superseded,
           work_item_id,      -- every doc BELONGS to a job; orphans impossible
           cairn_ref,         -- MD content lives in cairn (versioned, diffable)
           approvals: [{by, verdict, comments, at}]}
```

- **Storage/structure split:** cairn stores the markdown (history + diffs, already there); the register is the queryable index with lifecycle. Finding = query by kind/status/stream, never folder-browsing.
- **Approval is a work-item** (`role: operator-approval`) in the operator's queue — same graph, same verdict machinery: approve → dependents go ready (the doc rides into builder briefs as context, per §8); approve-with-changes → operator's inline edits commit to cairn as a new version, then proceed; reject → reasons become a rework item to the authoring role.
- **Dispatch context lives here too:** builder briefs reference register doc ids; the funnel materialises them as files at spawn.
- **Whose space it is:** the register + approval queue is the **operator + interface-AI (shadow) shared workbench** — this is where the two of them work on specs/plans together, *including* documents that originated in the orchestrator layer (a decomposition plan the orchestrator files surfaces here for the pair to review and refine before verdict). shadow has first-class read/draft/revise access from planning sessions; **verdicts are operator-only**. shadow refines, operator decides. The register API must therefore be reachable from croft, not internal to the orchestrator.

## 9a. Operator console v0 (upgraded from "minimal status view")

One boring server-rendered (HTMX-style) surface, two panes:
1. **Approval queue** — rendered markdown, inline-editable, three verdict buttons.
2. **Fleet + graph status** — `GET /api/admin/workers` + work-graph view (streams, gates, blocked items).

The **full UI rethink remains Phase 5**, designed against the running network. Banked requirements: work-graph view, gate verdicts, blocked queue, operator-visual judgment queue (art/renders).

## 10. Nexus as a CWB consumer (dogfood mapping)

**Context (operator, 2026-07-05):** nexus came first; CWB grew from its expanding needs. The rework makes nexus a proper CONSUMER of CWB products, with this architecture as the core of the adjustment — use our own products, don't just have them.

| Component | CWB product |
|---|---|
| Work graph / work_items | **ledger** (the tracker — cairn already files PRs as ledger issues); a work-item is a structured ledger issue with deps/status/handoff |
| Document register | **ledger** (lifecycle/approvals) + **cairn** (MD content) + **commonplace** (approved-knowledge distillate) |
| Artifacts / shared workspace | **cairn** |
| Pool slot identities | **herald** (IdP) + **casket** (keys) — herald is DORMANT; see sequencing note |
| Internal secrets + platform config (incl. frontier token) | **almanac** (`SecureParameter`) as source of truth; k8s secret as delivery — AUDIT-corrected from custodian |
| External creds (git tokens, Drive OAuth) | **custodian** — its real domain (kind=git today; porter's Drive OAuth is its consumer-of-record) |
| Scheduled triggers (hermes) | **k8s CronJob only** — AUDIT: no scheduling pillar exists anywhere in CWB; almanac is config/secrets, not a scheduler |
| Base/fleet knowledge | **commonplace** (live) |
| Backup of all of it | **porter** (backup half is built + custodian-integrated; verify scope covers work-graph + register data) |
| Agent web access (web_fetch/extract) | **lynxai → core CWB** (cwb-core supporting service; operator-confirmed a service, never an aspect; moves before nexus-ns cleanup) |

Rules:
1. **Consume where the product genuinely is the thing; never force-fit.** Ephemeral runtime state (heartbeats, pool leases) stays broker-local.
2. **Dogfood-or-delete is now the pillar test.** The network is each pillar's consumer-of-record; a pillar with no consumer after this mapping is a delete candidate, not a freeze candidate.

Sequencing guard: **herald revival must not block the pipeline** — v1 leases broker keyfiles (existing machinery), v2 swaps the identity source to herald behind the same seam.

AUDIT finding that reshapes this section: **nexus's CWB-client integration already exists and is DARK** — custodian/almanac/cfgreconcile code is written and tested but disabled by default (gated on unset `*_GRPC_ADDR`, silent local fallback). The dogfood cutover is mostly *activate-and-cut-over*, not greenfield. nexus's internal reimplementations (embedded ledger.Service, the local FTS5 "knowledge" store, the AES-GCM credentials table, broker-minted identities) each retire as its pillar consumption goes live.

## 10a. Build order

1. Work graph CRUD (incl. cancel/requeue §2.1) on a **ledger adapter** (work-items as structured ledger issues); runs store keeps runtime-only state  *(builder)*
2. Document register lifecycle on **ledger + cairn** (§9)  *(builder, parallel with 1)*
3. `Brief` extension + jobspec threading + funnel role-overlay/policy/skill-gating + doc-context materialisation  *(builder ×2, parallel)*
4. Pool leasing + cap  *(builder)*
5. Worker status frames + `worker_status` table + `/api/admin/workers`  *(builder)*
6. Orchestrator graph-drain + OnJobDone wake + auth preflight/alerts + heartbeat auto-reap  *(builder, after 1+5)*
7. Auth wiring (**almanac**-sourced secret) + CLI version knob + CI image rebuild (note: worker image is `PullNever` — CI must distribute images to nodes, not just push a registry)  *(infra)*
8. Operator console v0: approval queue + fleet/graph status (§9a)  *(builder, after 2+5)*
Each unit: bounded spec + acceptance criteria, gates per `ROLE-MODEL.md` §4. Built via the existing ticket-pipeline (shadow orchestrates) — the new network builds itself only after it exists.

## M1 CORE RUNTIME BUILT 2026-07-05 — units 1,3,4,5,6 (PRs #397, #398, #399)
The event-triggered orchestrator loop physically exists in code, fully unit-tested:
- Unit 1 work-graph adapter on the sovereign ledger (#397) — verified live e2e.
- Units 3+4 role-at-spawn + pool leasing (#398) — reconciled (Brief.Role→label + RolePrompt→text).
- Unit 5 worker-status contract (heartbeat/table/`/api/admin/workers` requireAdmin).
- Unit 6 orchestrator graph-drain (DrainOnce/RecordJobResult/OnJobDone-wake/PreflightAuth-hold/ReapStale-2nd-strike).
- Integration PR #399 = the full stack composed + green (62 pkgs pass).

**Verify-gate lesson held all the way through:** unit 1 caught ~9 real-ledger mismatches fakes missed; every unit built via builder→independent-verify→review→security→PR. The orchestrator layer caught the cross-unit Brief.Role semantic conflict neither builder could see.

**LIVE-INTEGRATION FOLLOW-UPS before this runs against a real broker (documented, mechanism is complete + tested):**
1. **Result channel** — dispatch.JobDone carries only Ticket+OK; a worker's rich verdict (reject reasons→rework) has no wired path to the orchestrator. RecordJobResult (full path) is exported+tested; needs a real result channel worker→orchestrator.
2. **Alert delivery** — loki-alert-bridge is pull-only; Alerter is a pluggable seam (LogAlerter default). Needs a real sink.
3. **Frontier auth source** — worker-status auth_ok reports session-JWT health; the almanac-sourced CLAUDE token (§6/§7) is a separate unit (this is build-order unit 7).
4. **Skill-gating activation** — the gating primitive is built; needs per-worker MCP-client wiring (none exists for any provider yet).
5. **Pool aspect row** — MintDerivedCredential needs a `pool` aspect provisioned in the roster for live leasing.
6. **RoleResolver** — the docs/network/roles/*.yaml → resolved-prompt transform is a seam (interface only); needs an impl.

**REMAINING M1 units:** 2 (document register on ledger+cairn), 7 (auth wiring almanac-sourced + CLI version knob + CI image rebuild — covers follow-up #3), 8 (operator console v0 — reads #397's register + #399's /api/admin/workers). The core loop is done; these are the surfaces + the live-wiring around it.
