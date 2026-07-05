# runtime/dispatch — the broker-embedded dispatch engine

Three coexisting dispatch modes, all sharing one `Runner`, one Job/queue/drain
machinery, and one `OnJobDone` completion path:

1. **Named-agent dispatch** (`Submit`) — `!dispatch <named-agent> …`. Runs AS
   the named agent (mounts `aspect-keyfile-<agent>`). One run per agent name
   at a time (NEX-464); a second task for a busy agent queues and drains when
   that agent frees.
2. **Aspect-owned hand fan-out** (`SubmitSpawn`, `spawn.go`) — an aspect
   spawns fresh-context "hands" of itself (`<parent>.sub-N` / kindred-word
   names) for background work, capped per-parent by `SpawnMaxConcurrent`
   (default 4, NEX-571).
3. **Pool leasing** (`SubmitPool`, `pool.go`, **M1 Unit 4**) — a role-based
   work item leases a slot from a fixed pool of N interchangeable derived
   identities, capped by `PoolSize` (default 3) — described below.

Every spawned worker, across all three modes, can additionally be stamped
with role-at-spawn metadata (**M1 Unit 3**) — a role overlay, scoped skills,
and a tool-policy fragment applied at boot. See "Role-at-spawn" below.

## The pool model

- The pool is a synthetic parent identity, `pool` (`poolParentName` in
  `pool.go`), with a fixed, numbered slot vocabulary: `pool.sub-1 .. pool.sub-N`
  (`N` = `Runner.PoolSize`, default `defaultPoolSize` = 3). Unlike a hand's
  kindred-word lineage (`plumb.bob`, `shadow.umbra`, …), pool slots carry no
  persona lineage — they are interchangeable workers; accountability comes
  from the **Role** (label) and **WorkItemID** stamped on the Brief, not the
  slot name.
- A pool work item is dispatched via `Runner.SubmitPool(ctx, role, task,
  workItemID, thread)`. `workItemID` doubles as `Brief.Ticket`, the
  idempotency key — resubmitting the same work item while it is active or
  queued is a no-op / returns the existing run id, exactly like `Submit`'s
  ticket dedupe. `role` here is the short role LABEL (e.g. `"builder"`), not
  prompt text — see "Role-at-spawn" below for the distinction from
  `Brief.RolePrompt`.
- Slots are derived identities minted through the same
  `MintHandCredential` seam a hand spawn uses (`aspects.DerivedName`,
  `IsDerivedName`, broker-signed session JWT) — no keyfile, no new crypto.
  **Production prerequisite:** the credential mint
  (`broker.KeyfileValidator.MintDerivedCredential`) looks the parent up as a
  real, non-retired row in the aspects store, so a `pool` aspect row must
  exist for pool leases to mint in a live broker (a one-time roster addition,
  not built by this unit — see the live-verify note below).

## Lease/release lifecycle

1. **Acquire.** `SubmitPool` tries `tryLeasePoolSlot()`: first-free slot name
   in `pool.sub-1..N` order, gated by the pool cap (`liveHands("pool") <
   PoolSize`) and the global `MaxConc` (same as every other dispatch path). A
   free slot → the Brief is stamped `Agent=<slot>`, reserved, and launched
   immediately (same `reserve`/`launch` as `Submit`/`SubmitSpawn`).
2. **Queue.** No free slot → the Brief (with `Agent` still empty — no slot
   name assigned yet, since any of the N could free next) is appended to the
   Runner's ordinary `queue`. A "pool dispatch queued" status posts to the
   thread.
3. **Release + drain.** `OnJobDone` (unchanged — this unit only adds a new
   caller shape, not a new completion path) frees the completed run's
   `agentBusy[slot]` entry exactly like it frees a named agent or a hand, then
   calls `reserveQueued()`. `reserveQueued` recognizes a queued pool item
   (`SpawnParent == "pool"` and `Agent == ""`) and leases whichever slot is
   free *now* (`tryLeasePoolSlot`) rather than checking a fixed identity —
   this is the one place pool draining differs from `Submit`'s per-name
   drain, because a pool item isn't bound to one specific slot ahead of time.
4. **Reuse.** A released slot returns to the free set the instant
   `agentBusy` drops its key — the very next `SubmitPool` (or a queued item
   draining) can lease it again. There is no persistent "cooldown" or
   retirement; slots are purely interchangeable capacity.

## The pool-cap dimension (distinct from per-agent-name serialization)

`canRun`'s per-parent hand-cap check (`liveHands(base) >= cap`) now resolves
its cap via `capForBase(base)`:

- `base == "pool"` → `PoolSize` (default 3) — the new, independent cap this
  unit adds.
- any other base (a real aspect running hands) → `SpawnMaxConcurrent`
  (default 4) — unchanged NEX-571 behavior.

This keeps the two dimensions from bleeding into each other: raising
`SpawnMaxConcurrent` for aspect hand fan-out never loosens the pool cap, and
vice versa. Per-agent-name serialization (`agentBusy`, one live run per exact
name) is untouched — it still governs `!dispatch <named-agent>` exactly as
before this unit, and pool slot names (`pool.sub-N`) simply occupy their own
entries in the same map, alongside named agents and hands, with zero
cross-talk.

## Coexistence with named dispatch

Named dispatch, hand fan-out, and pool leasing all read/write the same
`Runner.agentBusy`/`active`/`queue` state under the same mutex, but on
disjoint keys (a real aspect name, `<aspect>.<word>`, or `pool.sub-N`), so:

- A full pool never blocks or delays a named-agent dispatch, and vice versa.
- `!dispatch anvil …` still serializes strictly per name (a second `anvil`
  task queues regardless of pool state).
- `OnJobDone` for one dispatch mode only ever drains work belonging to the
  *same* identity/base that just freed — completing an `anvil` ticket never
  reaches into the pool queue, and completing a pool lease never touches
  `anvil`'s queue.

See `pool_test.go`'s `TestNamedDispatchCoexistsWithPoolLeasing` for the test
that exercises this directly.

## Pool live-verify path (not run by this unit — documented for the operator)

1. Ensure a `pool` aspect row exists in the roster (non-retired) so
   `MintDerivedCredential(ctx, "pool", "pool.sub-N")` can mint — a one-time
   setup step, analogous to any named aspect needing a keyfile row.
2. Dispatch `N+1` pool work items (`PoolSize` + 1) via `Runner.SubmitPool`
   (or whatever caller wires role-based dispatch to it — the orchestrator's
   graph-drain, per PHASE2-DESIGN §2, is the intended production caller).
3. Observe: `N` Jobs running as `pool.sub-1..N` (`GET /api/admin/workers` or
   `kubectl get jobs -l nexus.dispatch/lineage=pool`), and the `(N+1)`th item
   sitting queued (a "pool dispatch queued" post in its thread, no Job yet).
4. Complete one of the `N` running Jobs (let it finish, or force-complete in
   a test cluster). Observe the queued `(N+1)`th item transition to running
   on the freed slot — a new Job appears as the same `pool.sub-<k>` name the
   just-completed lease held, and its completion summary in chat stamps
   `slot=pool.sub-<k> role=<role> work_item=<id>`.
5. Confirm named dispatch (`!dispatch <agent> …`) issued at any point during
   steps 2–4 lands and runs unaffected — the pool being at cap never blocks
   it, and it never blocks the pool queue from draining.

## Accountability

Every pool run's completion summary (`Runner.completionSummary`) stamps
`slot=<pool.sub-N> role=<role> work_item=<id>` instead of the builder
branch/PR block (ticket dispatch) or the `hand of <parent>` lineage line
(aspect hand fan-out) — mirroring how each dispatch mode records identity in
its own accountable shape.

## Role-at-spawn (M1 Unit 3)

This section documents the dispatch-side half of role-at-spawn: `Brief`
gains `{Role, RolePrompt, WorkItemID, SkillAllowlist, PolicyFragment,
Personality}` and threads them into the builder Job so agentfunnel can stamp
a spawned worker with its role, scoped skills, and tool policy at boot. See
`docs/network/build-specs/unit3-role-at-spawn.md`,
`docs/network/PHASE2-DESIGN.md` §3, and `docs/network/ROLE-MODEL.md` for the
design context this implements.

The agentfunnel-side half (composeSystemPrompt, loadToolPolicy, the
skill-gating MCP surface) is documented in
`runtime/cmd/agentfunnel/README.md`.

### What's new on `Brief`

| Field            | Type              | Carries                                                              |
|------------------|-------------------|-----------------------------------------------------------------------|
| `Role`           | `string`          | The role **label** (short name, e.g. `"builder"`) — M1 Unit 4 pool leases stamp this for accountability |
| `RolePrompt`     | `string`          | The **resolved role system-prompt text** (not a role name/id) — M1 Unit 3's role overlay |
| `WorkItemID`     | `string`          | The pool work-item id (informational — distinct from `Ticket`)        |
| `SkillAllowlist` | `[]string`        | Exact skill names this spawn may see                                  |
| `PolicyFragment` | `*funnel.ToolPolicy` | A tool-policy overlay applied over the static `-policy` file        |
| `Personality`    | `string`          | Thin, display-only label (name/voice/chat attribution)                |

`Role` and `RolePrompt` were reconciled at the M1 Wave 2 fold: Unit 3 and
Unit 4 both independently added a `Role` field with incompatible meanings
(resolved prompt text vs. a role label). The label meaning kept the `Role`
name (it is what pool-lease accountability reads); the prompt-text meaning
was renamed to `RolePrompt`. All six fields are additive and optional. A
`Brief` with none of them set reproduces today's exact ConfigMap, Job args,
env, and labels — this is tested directly (`TestBriefConfigMapData`,
`TestBuildJob_RoleAtSpawn`, `TestBriefRoleAtSpawnFields`,
`TestBriefRoleLabelField`).

### Design choice: `RolePrompt` carries the RESOLVED PROMPT TEXT, not a role id

The build spec offered three delivery options for the role prompt: baked
into the image, delivered via a ConfigMap keyed by role name, or carried as
a resolved string on the brief. We took the third, simplest option: **the
orchestrator resolves the role name (e.g. `builder`, `tester`, `reviewer`)
against `docs/network/roles/*.yaml` at dispatch time and sets
`Brief.RolePrompt` to the resulting prompt text.** dispatch and agentfunnel
never look up a role registry themselves — they only move an opaque string.
This keeps the runtime free of any role-schema coupling; if the role
registry format changes, only the orchestrator's resolution step needs to
change.

`WorkItemID` and `Personality` are informational at this layer: threaded
into Job labels (`nexus.dispatch/work-item`, `nexus.dispatch/personality`)
and env (`CW_WORK_ITEM_ID`, `CW_PERSONALITY`) for accountability and log
correlation, mirroring the existing `CW_NEXUS_ID` pattern. `Personality` is
NOT wired to override the broker-resolved `res.Personality` bundle that
`composeSystemPrompt` already layers in (see the agentfunnel README) — that
remains the existing personality pipeline. Wiring `Brief.Personality` into
chat attribution is left to the pool-dispatch orchestrator unit (a later
ticket), out of scope here.

### Transport: the SAME brief ConfigMap, extra keys

`BuildJob` already mounts a `brief-<taskID>` ConfigMap at `/etc/dispatch`
(the `-brief-file /etc/dispatch/brief.md` seed). A Kubernetes ConfigMap
volume mounts **every** key in `Data` as a file in that directory — no extra
volume or mount is needed. `briefConfigMapData` (brief.go) builds this map:

- `brief.md` — always present, `Brief.Task` (unchanged).
- `role.md` — present only when `Brief.RolePrompt != ""`.
- `policy.json` — present only when `Brief.PolicyFragment != nil` (JSON-marshaled `funnel.ToolPolicy`).

`builderArgs` (jobspec.go) only passes `-role-file
/etc/dispatch/role.md` / `-policy-fragment-file /etc/dispatch/policy.json`
to agentfunnel when the corresponding Brief field is set — so an empty
RolePrompt/PolicyFragment means agentfunnel's command line is byte-identical
to before this change.

`SkillAllowlist` rides as a Job env var instead of a ConfigMap file
(`CW_SKILL_ALLOWLIST`, comma-joined) — it's a short list of names, not
prose/JSON worth a file, and env is the established pattern for spawn
metadata (`CW_NEXUS_ID`, `CW_ASPECT_NAME`, …) already in `BuildJob`.

`WorkItemID`/`Personality` also ride as env (`CW_WORK_ITEM_ID`,
`CW_PERSONALITY`) plus Job labels, for the same reason.

### `PutBriefConfigMap` signature change

`K8sIface.PutBriefConfigMap` changed from
`(ctx, taskID, brief string) error` to
`(ctx, taskID string, data map[string]string) error` so the SAME
ConfigMap-Data mechanism above delivers the multi-file brief. This is a
source-breaking change to the interface (every implementer — `K8s`, and the
`fakeK8s`/`fakeLogK8s` test doubles — was updated in this change) but NOT a
behavior-breaking one: `provisionRun` still calls it with exactly
`{"brief.md": b.Task}` when the brief carries no role-at-spawn fields.

### Role-at-spawn live-verify path (for the orchestrator to run)

This unit's tests cover the wiring in isolation (unit tests, no live
cluster). To observe the end-to-end effect on a real worker:

1. Dispatch a brief with a `role_prompt` and `skill_allowlist` set, e.g. via
   the `!dispatch`-command path is NOT sufficient (it has no JSON header) —
   use the fenced-JSON header path:

   ````
   ```json
   {"agent":"anvil","repo":"CarriedWorldUniverse/nexus","ticket":"NEX-TEST-1",
    "role_prompt":"You are a tester. Write and run tests only; do not edit application code.",
    "skill_allowlist":["test-run","bash","read"],
    "policy_fragment":{"default_allow":true,"tools":{"write":false,"edit":false}}}
   ```
   Verify the flag works end-to-end.
   ````

2. After the Job starts, `kubectl get configmap brief-<taskID> -o yaml` in
   the `nexus` namespace and confirm it has THREE keys: `brief.md`,
   `role.md` (the role prompt text), `policy.json` (the fragment).
3. `kubectl get job <job-name> -o jsonpath='{.spec.template.spec.containers[0].args}'`
   and confirm `-role-file /etc/dispatch/role.md` and
   `-policy-fragment-file /etc/dispatch/policy.json` are present, and
   `.spec.template.spec.containers[0].env` has `CW_SKILL_ALLOWLIST=test-run,bash,read`.
4. `kubectl logs <pod>` — agentfunnel logs
   `"agentfunnel: starting deliberation loop" ... system_prompt_bytes=<N>`;
   confirm `<N>` grew by roughly `len(role prompt) + len("\n\n---\n\n")`
   versus a Job with no RolePrompt. (There is no direct log line dumping the
   full composed prompt — cross-check against `composeSystemPrompt`'s unit
   tests for the exact ordering: central ⊕ role ⊕ personality.)
5. `kubectl logs <pod>` also carries `"agentfunnel: tool policy loaded"
   default_allow=... denied_tools=...` — confirm it reflects the merged
   fragment (e.g. `denied_tools=2` for `write`+`edit` denied).
6. For skill gating: this Job does not yet spawn `nexus-skills-mcp` as a
   subprocess (no `.mcp.json`/tool-config wiring exists in this repo yet —
   see the agentfunnel README's "known gap" note). To verify the primitive
   itself today, run `nexus-skills-mcp` directly with
   `CW_SKILL_ALLOWLIST=test-run,bash,read` in its environment (as the Job
   sets it) and call `search_skills`/`get_skill` over its stdio MCP
   interface — confirm only the three allowed skills are ever returned.
