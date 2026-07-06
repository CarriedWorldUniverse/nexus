# runtime/cmd/agentfunnel — role-at-spawn (M1 Unit 3)

This note documents the agentfunnel-side half of role-at-spawn: how a
spawned builder Job's system prompt, tool policy, and skill visibility
get stamped from the dispatch `Brief`. See `runtime/dispatch/README.md`
for the dispatch-side half (Brief fields, ConfigMap/env transport), and
`docs/network/build-specs/unit3-role-at-spawn.md` /
`docs/network/PHASE2-DESIGN.md` §3 / `docs/network/ROLE-MODEL.md` for the
design this implements.

## New flags

- `-role-file <path>` — the resolved role system-prompt text for this
  spawn (plain text, no format). Read once at startup. `BuildJob` only
  passes this flag when `Brief.Role != ""`.
- `-policy-fragment-file <path>` — a `funnel.ToolPolicy` JSON overlay for
  this spawn. Read once at startup. `BuildJob` only passes this flag when
  `Brief.PolicyFragment != nil`.

Both are read right after `flag.Parse()`, BEFORE `loadToolPolicy` and
`composeSystemPrompt` are called — both need them, and both are computed
once at startup, well before the seed brief (`-brief-file`) is read for
the goal-pursuit loop. A missing/malformed file at a non-empty path fails
fast (`fail(log, ...)`), matching `-policy`'s existing posture: `BuildJob`
only ever names these files when they exist in the ConfigMap, so a read
failure here is a real bug, not a normal "not configured" case.

## `composeSystemPrompt`: where the role prompt lands

```
composeSystemPrompt(res *keyfile.ValidationResult, rolePrompt string) string
```

Section order (unchanged sections keep their `\n\n---\n\n` join):

```
central.nexus_md  ⊕  rolePrompt  ⊕  aspect.nexus_md ⊕ aspect.soul_md ⊕ aspect.primer_md
                      (or aspect.personality.composed, when set)
```

The role prompt sits ABOVE the (thin) personality but BELOW central — org-wide
base knowledge always applies first, then the job-specific role overlay,
then cosmetic personality decoration. This matches ROLE-MODEL.md §3:
"capability = role (skills) + task spec + base knowledge... Personality
is thin — decoration, not capability." An empty `rolePrompt` (no `Role`
on the brief — the default) is dropped from the join exactly like any
other empty section, so `composeSystemPrompt(res, "")` is byte-identical
to the pre-role-at-spawn `composeSystemPrompt(res)`.

## `loadToolPolicy` / `applyPolicyFragment`: overlay precedence

```
loadToolPolicy(path string, fragment *funnel.ToolPolicy) (funnel.ToolPolicy, error)
```

1. **Tier A (base):** `path` resolves exactly as before — empty path →
   permissive default (`DefaultAllow: true`); non-empty path → read +
   JSON-decode, fail fast on missing/malformed.
2. **Tier B (spawn overlay):** `fragment`, when non-nil, is applied over
   the Tier-A base by `applyPolicyFragment`. This is the "Tier B" the
   pre-existing code comment recorded as a follow-on — delivered
   **per-spawn** (via the brief/Job, `-policy-fragment-file`) rather than
   centrally in the Nexus via `keyfile.ValidationResult` (the other
   option the comment floated). Per-spawn was chosen because role-at-spawn
   fragments vary by WORK-ITEM/role, not by aspect identity, and the
   brief is already the per-spawn channel for everything else in this
   unit.

**Field-by-field overlay precedence** (`applyPolicyFragment`):

- `DefaultAllow` always takes the fragment's value when a fragment is
  present — a fragment's mere presence is the role's explicit decision
  about it (unlike the other fields, this one has no "unset" JSON
  representation to distinguish from an intentional `false`).
- `Tools` / `Escalate` / `BashDeny` / `WritePathAllow`: a field the
  fragment SETS (a non-nil map/slice after JSON-unmarshal — including an
  explicit empty one, e.g. `"write_path_allow": []` for a read-only role)
  REPLACES the base field outright. A field the fragment OMITS (absent
  JSON key → nil after unmarshal) leaves the base field untouched.
- A nil `fragment` (the default — no `PolicyFragment` on the brief) is a
  total no-op: `loadToolPolicy(path, nil)` behaves exactly as
  `loadToolPolicy(path)` did before this change.

See `policy_load_test.go`'s `TestApplyPolicyFragment` table for the exact
matrix, and `TestLoadToolPolicyWithFragment` for the integration path.

### The "refresh loop can't clobber the overlay" invariant

`policy` (the merged Tier-A+B value) is computed exactly ONCE, at
startup, and stored in a plain local variable. Every `newBindingHarness`
call — the initial `bindingCache.Store` at startup, the P3c
escalator-equipped re-store right after, AND the binding-refresh closure
inside `tokenProvider` (fires on JWT re-validate, ~1h cadence) — closes
over that SAME `policy` variable and re-registers the SAME value. None of
these call sites reload from `*policyPath` or recompute the fragment
overlay, so there is nothing that could clobber a spawn overlay on
refresh: the merged policy is fixed for the whole process lifetime,
computed once, before the refresh loop closure is even defined.

## Skill gating: `agentskills.FilterAllowlist` / `AllowedName`

The skill-gating primitive is a pure filter in the root `agentskills`
package (`FilterAllowlist(skills, allow []string) []Skill`,
`AllowedName(name string, allow []string) bool`) — an empty/nil `allow`
is the back-compat no-op (every skill visible, today's ungated
behavior); a non-empty one scopes to exactly those names.

It's wired into `runtime/cmd/nexus-skills-mcp` (the stdio MCP server
that serves `search_skills`/`get_skill`):

- `-skill-allowlist <comma,separated,names>` flag, falling back to the
  `CW_SKILL_ALLOWLIST` env var (which `BuildJob` sets from
  `Brief.SkillAllowlist` — see the dispatch README) when the flag is
  unset. Both empty → nil allow list → all skills.
- `search_skills` filters its hits through `FilterAllowlist`.
- `get_skill` denies (MCP tool error, not a transport error) any name
  outside the allow list via `AllowedName`.

### Known gap: `nexus-skills-mcp` isn't yet spawned per-worker

`nexus-skills-mcp` is a built, tested, standalone binary — but nothing in
this repo currently launches it as a per-worker subprocess/MCP server
(no `.mcp.json` generation, no `--mcp-config` wiring for claude-code, no
bridle MCP-client registration for native-API providers). `CW_SKILL_ALLOWLIST`
IS already threaded onto the Job's env by `BuildJob` (this unit), so the
gating mechanism will work correctly the moment that wiring lands — this
unit delivers the primitive + its env-based configuration, not the
process-launch wiring itself (out of scope: that's a separate MCP-wiring
unit, mirrored by the existing `docs/2026-06-09-agent-skills-wiring-plan.md`).

## Live-verify path (for the orchestrator to run)

See `runtime/dispatch/README.md`'s "Live-verify path" section — it covers
the full Brief → ConfigMap → Job → agentfunnel chain, including the
skill-gating verification workaround for the known gap above.
