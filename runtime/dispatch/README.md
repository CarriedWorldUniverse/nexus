# runtime/dispatch — role-at-spawn (M1 Unit 3)

This note documents the dispatch-side half of role-at-spawn: `Brief` gains
`{Role, WorkItemID, SkillAllowlist, PolicyFragment, Personality}` and
threads them into the builder Job so agentfunnel can stamp a spawned
worker with its role, scoped skills, and tool policy at boot. See
`docs/network/build-specs/unit3-role-at-spawn.md`,
`docs/network/PHASE2-DESIGN.md` §3, and `docs/network/ROLE-MODEL.md` for
the design context this implements.

The agentfunnel-side half (composeSystemPrompt, loadToolPolicy, the
skill-gating MCP surface) is documented in
`runtime/cmd/agentfunnel/README.md`.

## What's new on `Brief`

| Field            | Type              | Carries                                                              |
|------------------|-------------------|-----------------------------------------------------------------------|
| `Role`           | `string`          | The **resolved role system-prompt text** (not a role name/id)          |
| `WorkItemID`     | `string`          | The pool work-item id (informational — distinct from `Ticket`)        |
| `SkillAllowlist` | `[]string`        | Exact skill names this spawn may see                                  |
| `PolicyFragment` | `*funnel.ToolPolicy` | A tool-policy overlay applied over the static `-policy` file        |
| `Personality`    | `string`          | Thin, display-only label (name/voice/chat attribution)                |

All five are additive and optional. A `Brief` with none of them set
reproduces today's exact ConfigMap, Job args, env, and labels — this is
tested directly (`TestBriefConfigMapData`, `TestBuildJob_RoleAtSpawn`,
`TestBriefRoleAtSpawnFields`).

## Design choice: `Role` carries the RESOLVED PROMPT TEXT, not a role id

The build spec offered three delivery options for the role prompt: baked
into the image, delivered via a ConfigMap keyed by role name, or carried
as a resolved string on the brief. We took the third, simplest option:
**the orchestrator resolves the role name (e.g. `builder`, `tester`,
`reviewer`) against `docs/network/roles/*.yaml` at dispatch time and sets
`Brief.Role` to the resulting prompt text.** dispatch and agentfunnel
never look up a role registry themselves — they only move an opaque
string. This keeps the runtime free of any role-schema coupling; if the
role registry format changes, only the orchestrator's resolution step
needs to change.

`WorkItemID` and `Personality` are informational at this layer: threaded
into Job labels (`nexus.dispatch/work-item`, `nexus.dispatch/personality`)
and env (`CW_WORK_ITEM_ID`, `CW_PERSONALITY`) for accountability and log
correlation, mirroring the existing `CW_NEXUS_ID` pattern. `Personality`
is NOT wired to override the broker-resolved `res.Personality` bundle
that `composeSystemPrompt` already layers in (see the agentfunnel
README) — that remains the existing personality pipeline. Wiring
`Brief.Personality` into chat attribution is left to the pool-dispatch
orchestrator unit (a later ticket), out of scope here.

## Transport: the SAME brief ConfigMap, extra keys

`BuildJob` already mounts a `brief-<taskID>` ConfigMap at `/etc/dispatch`
(the `-brief-file /etc/dispatch/brief.md` seed). A Kubernetes ConfigMap
volume mounts **every** key in `Data` as a file in that directory — no
extra volume or mount is needed. `briefConfigMapData` (brief.go) builds
this map:

- `brief.md` — always present, `Brief.Task` (unchanged).
- `role.md` — present only when `Brief.Role != ""`.
- `policy.json` — present only when `Brief.PolicyFragment != nil` (JSON-marshaled `funnel.ToolPolicy`).

`builderArgs` (jobspec.go) only passes `-role-file
/etc/dispatch/role.md` / `-policy-fragment-file /etc/dispatch/policy.json`
to agentfunnel when the corresponding Brief field is set — so an empty
Role/PolicyFragment means agentfunnel's command line is byte-identical to
before this change.

`SkillAllowlist` rides as a Job env var instead of a ConfigMap file
(`CW_SKILL_ALLOWLIST`, comma-joined) — it's a short list of names, not
prose/JSON worth a file, and env is the established pattern for spawn
metadata (`CW_NEXUS_ID`, `CW_ASPECT_NAME`, …) already in `BuildJob`.

`WorkItemID`/`Personality` also ride as env (`CW_WORK_ITEM_ID`,
`CW_PERSONALITY`) plus Job labels, for the same reason.

## `PutBriefConfigMap` signature change

`K8sIface.PutBriefConfigMap` changed from
`(ctx, taskID, brief string) error` to
`(ctx, taskID string, data map[string]string) error` so the SAME
ConfigMap-Data mechanism above delivers the multi-file brief. This is a
source-breaking change to the interface (every implementer — `K8s`, and
the `fakeK8s`/`fakeLogK8s` test doubles — was updated in this change) but
NOT a behavior-breaking one: `provisionRun` still calls it with exactly
`{"brief.md": b.Task}` when the brief carries no role-at-spawn fields.

## Live-verify path (for the orchestrator to run)

This unit's tests cover the wiring in isolation (unit tests, no live
cluster). To observe the end-to-end effect on a real worker:

1. Dispatch a brief with a `Role` and `SkillAllowlist` set, e.g. via the
   `!dispatch`-command path is NOT sufficient (it has no JSON header) —
   use the fenced-JSON header path:

   ````
   ```json
   {"agent":"anvil","repo":"CarriedWorldUniverse/nexus","ticket":"NEX-TEST-1",
    "role":"You are a tester. Write and run tests only; do not edit application code.",
    "skill_allowlist":["test-run","bash","read"],
    "policy_fragment":{"default_allow":true,"tools":{"write":false,"edit":false}}}
   ```
   Verify the flag works end-to-end.
   ````

2. After the Job starts, `kubectl get configmap brief-<taskID> -o yaml` in
   the `nexus` namespace and confirm it has THREE keys: `brief.md`,
   `role.md` (the role text), `policy.json` (the fragment).
3. `kubectl get job <job-name> -o jsonpath='{.spec.template.spec.containers[0].args}'`
   and confirm `-role-file /etc/dispatch/role.md` and
   `-policy-fragment-file /etc/dispatch/policy.json` are present, and
   `.spec.template.spec.containers[0].env` has `CW_SKILL_ALLOWLIST=test-run,bash,read`.
4. `kubectl logs <pod>` — agentfunnel logs
   `"agentfunnel: starting deliberation loop" ... system_prompt_bytes=<N>`;
   confirm `<N>` grew by roughly `len(role prompt) + len("\n\n---\n\n")`
   versus a Job with no Role. (There is no direct log line dumping the
   full composed prompt — cross-check against `composeSystemPrompt`'s
   unit tests for the exact ordering: central ⊕ role ⊕ personality.)
5. `kubectl logs <pod>` also carries `"agentfunnel: tool policy loaded"
   default_allow=... denied_tools=...` — confirm it reflects the merged
   fragment (e.g. `denied_tools=2` for `write`+`edit` denied).
6. For skill gating: this Job does not yet spawn `nexus-skills-mcp` as a
   subprocess (no `.mcp.json`/tool-config wiring exists in this repo
   yet — see the agentfunnel README's "known gap" note). To verify the
   primitive itself today, run `nexus-skills-mcp` directly with
   `CW_SKILL_ALLOWLIST=test-run,bash,read` in its environment (as the Job
   sets it) and call `search_skills`/`get_skill` over its stdio MCP
   interface — confirm only the three allowed skills are ever returned.
