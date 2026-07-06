# M1 Unit 3 — Role-at-spawn (build spec)

**Goal:** dispatch carries {role, skills, policy, personality}, and a spawned worker is stamped with its ROLE (prompt overlay) + scoped skills + tool policy at boot. Foundation for the pool: an interchangeable agent slot becomes {personality + role} per work-item. Ref: PHASE2-DESIGN §3.

## Touchpoints (from the nexus-core audit — verify against current main)
- `runtime/dispatch/brief.go` — `Brief` struct (~L12-41). ADD fields: `Role string`, `WorkItemID string`, `SkillAllowlist []string`, `PolicyFragment` (a ToolPolicy overlay — see funnel.ToolPolicy shape in `nexus/frame/funnel/policy.go`), `Personality string`. Additive; mirror the existing `SessionJWT json:"-"` non-enqueue pattern where a field shouldn't serialize into the queue.
- `runtime/dispatch/jobspec.go` — `BuildJob`/`JobConfig`/`builderArgs` (~L281). Thread the new Brief fields into the brief ConfigMap that becomes `-brief-file` (or new env/flags on the Job). The funnel already ingests `-brief-file` as the seed/DoD.
- `runtime/cmd/agentfunnel/main.go`:
  - `composeSystemPrompt` (~L931) — PREPEND the role prompt (from the role def) ABOVE the thin personality. Role prompt source: the brief carries the role name; load the role's system-prompt text (the role defs live at `docs/network/roles/*.yaml` — decide delivery: baked, configmap, or brief-carried string. SIMPLEST: brief carries the resolved role-prompt string, set by the orchestrator at dispatch. Document the choice.)
  - `loadToolPolicy` (~L99/L1429) — accept a spawn-supplied `PolicyFragment` overlay INSTEAD of only the static `-policy` file. MIND the load-once-thread-through invariant (main.go ~L1394-1397 re-registers the same policy on every binding refresh — the refresh loop must not clobber the spawn overlay). This completes the recorded Tier-B TODO.
  - Skill gating (NEW primitive) — filter `.agents/skills` materialization to the role's `SkillAllowlist` at spawn. Today `agentskills.go`/`nexus-skills-mcp` serve ALL skills ungated. Gate at the materialization/skills-MCP surface per worker.

## Constraints
- cairn line off main: `builder/m1-unit3-role-at-spawn`. `cairn commit`, no push.
- Additive + backward-compatible: an empty Role/SkillAllowlist/PolicyFragment = today's behavior (no overlay, all skills, static policy). Existing dispatch/`!dispatch` paths must not break.
- Follow existing conventions; reuse funnel.ToolPolicy for the fragment shape.

## Acceptance
1. `go build ./...` + `go vet` clean; existing dispatch tests still pass (`runner_test.go`, `spawn_test.go`).
2. Unit tests: Brief round-trips the new fields through the ConfigMap/brief-file; composeSystemPrompt prepends role prompt above personality (table test); loadToolPolicy applies a spawn PolicyFragment over/instead-of the static file; skill materialization filters to the allowlist (empty = all).
3. A README documenting: where the role prompt comes from, the policy-overlay precedence, and the skill-gating mechanism.
4. Document the live-verify path (dispatch a Job with a Role+SkillAllowlist, observe the worker's composed prompt + available skills) even if not run — mark it for the orchestrator to run.
