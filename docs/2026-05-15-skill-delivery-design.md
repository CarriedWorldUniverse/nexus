# nexus-supplied skills on demand (design draft)

**Status:** v0 draft, no review yet. Drafted by shadow 2026-05-15. Operator-requested.
**Canonical location TBD** — Drive + repo-mirror once approved (same pattern as work-routing policy).

---

## Problem

Today, claude-code skills live per-host in `~/.claude/plugins/.../skills/*` and `~/.claude/projects/*/SKILLS.md` etc. Adding or updating a skill means touching every host running an aspect. Aspects on different hosts drift; rolling out a refined "code-review-checklist" requires copying files around. There's no central catalog, no versioning, no per-aspect subscription, no audit on what skill an aspect actually applied.

We want:
- **One catalog** in nexus holding skill bodies.
- **Per-aspect subscriptions** — shadow's set differs from harrow's differs from forge's.
- **On-demand fetch** — the model decides it needs a skill, calls a tool, gets the body, applies it inline.
- **Versioning + audit** — what skill, what version, when applied.
- **No per-host installation** — adding a new skill is `nexus skill upsert ...`, not a 14-host scp dance.

---

## Two delivery shapes

### Shape A — preload-on-session-start

Aspect connects to nexus, fetches its subscribed skill set as part of the validate handshake (or shortly after). agentfunnel / agora writes the skills to a local `~/.claude/skills-cache/<aspect>/` dir before spawning the claude-code subprocess. claude-code picks them up via its existing discovery mechanism.

**Pros:** Compatible with claude-code's existing skill loader; no model-side change needed.
**Cons:** All skills loaded into context even when not used (cost); static for the session (can't update mid-thread); host still has files (clean-up story).

### Shape B — just-in-time mid-turn (recommended)

Skills aren't preloaded. The model has two new tools:

- `nexus_skills.list(aspect?)` — returns the catalog of skills this aspect is authorized to fetch. Returns: name, version, description, intended-use.
- `nexus_skills.fetch(name, version?)` — returns the full skill body. Default version = current. Returns: markdown body of the skill.

When the model is mid-turn and recognizes a skill would apply — "I'm about to write a spec; let me see if there's a spec-template skill" — it calls `list`, finds candidates, calls `fetch`, and applies the returned body to its current work. The skill becomes inline context for the current turn.

**Pros:**
- Context cost paid only when needed.
- Central catalog with live versioning; next turn picks up updates automatically.
- Per-aspect access enforced at the broker.
- No host pollution; nothing on local disk.
- Maps naturally to the existing agent-network tool-use loop.
- Audit trail: every fetch is logged with `(aspect, skill, version, msg_id)`.

**Cons:**
- Model has to know skills exist and call `list` to discover them.
- Some skills aren't well-suited to inline-body shape (e.g. skills that invoke hooks or write files); those need Shape A or a different surface entirely.

### Recommended: Shape B as the primary surface. Shape A as a fallback for skills that don't fit the inline pattern (file-writing, hook-installing).

---

## Catalog schema (v1)

New `skills` table in nexus.db:

```sql
CREATE TABLE skills (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,                  -- e.g. "thorough-review-checklist"
  version INTEGER NOT NULL,            -- monotonic per name
  body TEXT NOT NULL,                  -- markdown content
  description TEXT NOT NULL,           -- one-line summary shown in list()
  intended_use TEXT NOT NULL,          -- when/why a model should fetch it
  owner_aspect TEXT,                   -- the aspect that maintains the skill (e.g. "harrow" for research skills)
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  UNIQUE (name, version)
);
```

Per-aspect subscription via `aspect_skill_grants`:

```sql
CREATE TABLE aspect_skill_grants (
  aspect_id INTEGER NOT NULL,
  skill_name TEXT NOT NULL,
  granted_at TIMESTAMP NOT NULL,
  PRIMARY KEY (aspect_id, skill_name)
);
```

Or simpler v1: a JSON `subscribed_skills` array on the aspects row. Skills not in the array are not list-able / fetchable by that aspect.

Audit log: `skill_fetches` table mirroring the credentials audit pattern. `(aspect, skill, version, trigger_msg_id, fetched_at)`.

---

## Tool surface

Two new MCP tools served by a new `nexus-skills-mcp` server (analogous to `nexus-comms-mcp`):

### `nexus_skills.list` (search-capable)

```
input: {
  aspect?: string,    -- defaults to self; ops may query other aspects' catalogs
  query?: string      -- natural-language intent ("about to write a security spec")
}
output: [
  {
    name: string,
    version: int,
    description: string,
    intended_use: string,
    relevance?: float  -- when query supplied, ranked match score
  }
]
```

Returns the skills the calling aspect (or queried aspect, if permitted) can fetch. Lightweight — no bodies. When `query` is supplied, results are ranked by relevance.

**Search implementation (v1):** keyword match against `description + intended_use` fields, ranked by hit count. Cheap, no infra. **Future:** embedding/semantic search if telemetry shows keyword matching missing relevant skills (e.g. operator searches "security review" but the skill is described as "threat-modeling"). Add when needed.

This is the **ToolFinder** pattern from claude-code's local skill system: the model doesn't memorize what's available; it queries by intent before committing to an approach. Reduces context cost (no need to paste-load skill descriptions into every system prompt) AND raises hit-rate on actually-relevant skills.

### `nexus_skills.fetch`

```
input: { name: string, version?: int }    -- omit version for "current"
output: {
  name: string,
  version: int,
  body: string,                            -- the full markdown skill content
  fetched_at: timestamp
}
```

Returns the skill body. Audit row written server-side before returning.

---

## Permissions

- **List/fetch own catalog:** any authenticated aspect, scoped to its subscriptions.
- **List/fetch another aspect's catalog:** operator + maintainer aspects only. (For example: harrow might want to inspect shadow's skill set when designing a new survey skill.)
- **Upsert / version-bump:** operator (via admin REST) + the skill's `owner_aspect`.
- **Subscription grants:** operator only.

JWT-gated; the broker enforces. Patterns mirror existing credentials store.

---

## Operator CLI

Same pattern as the credentials CLI being designed in NEX-78:

```
nexus skill upsert <name> --body <file> --description "..." --intended-use "..." [--owner-aspect <name>]
nexus skill list [--aspect <name>]
nexus skill grant <aspect> <skill>
nexus skill revoke <aspect> <skill>
nexus skill versions <name>      # show version history
nexus skill rollback <name> <version>
```

---

## Versioning & rollout

- Upsert = write a new row with `version = max(version) + 1`. Old versions stay queryable.
- Fetch without `version` arg = current.
- Fetch with explicit `version` = pinned (for reproducibility / debugging).
- Rollback = mark a prior version as current. Doesn't delete; just shifts the "current" pointer.

This gives us a deploy/rollback pattern for skills the same way we'd version a config.

---

## Aspect-side adoption

Each aspect's keyfile/aspect.json grows a list of subscribed skill names (or it's set via the admin REST). The aspect's MCP config includes `nexus-skills-mcp` so the model can call `list` and `fetch`.

Worker aspects that don't run claude-code (or where Shape B doesn't fit) can use Shape A — fetch all subscribed skills on startup, write to local `~/.claude/skills-cache/<aspect>/`.

---

## Migration from existing local skills

Today's skills live in `~/.claude/plugins/superpowers/skills/*.md` and similar. Path forward:

1. Decide which existing skills should be centrally managed vs stay local (e.g. plugin-specific superpowers skills probably stay; aspect-specific behavior skills move).
2. For ones to centralize: `nexus skill upsert` each one, copy body verbatim.
3. Grant to relevant aspects.
4. Once verified, remove local copies (optional; can coexist initially).

---

## What skills actually contain

Skills are **markdown recipes**, not code. A skill body explains:

- **When this applies** — situations where the recipe is relevant.
- **The approach** — the sequence of reasoning / steps.
- **Which tools to use** — names of existing tools (MCP-loaded or claude-native) the recipe leverages. Skills don't ship their own tool implementations; they're recipes pointing at the agent's already-available tool surface.
- **Quality gates** — checks to apply before considering the work done.

Example: a `thorough-code-review` skill might say: "Use Read on each changed file. Use Grep to find call-sites of modified functions. Walk the diff once for correctness, once for style. Apply the work-routing policy when judging severity. Surface findings as a structured list with confidence markers."

The skill makes the agent BETTER at code review without adding new tools — same surface, structured guidance.

This matches claude-code's skill model exactly. We're centralizing the catalog + making it discoverable, not changing what a skill IS.

---

## Discovery & planner prompt integration

The discovery problem: the model has to call `list` to know skills exist. If it doesn't think to look, it never uses them.

**Mitigation 1 — planner soul prompt.** Use the system-prompt extension path (same surface as the notify-operator convention and the work-routing policy):

> "When approaching a task you don't immediately know how to do well — code review, security analysis, spec writing, complex decomposition, anything with quality gates — before committing to an approach, call `nexus_skills.list` with a short intent description. Read returned skills' intended-use; fetch and apply any that fit. Skills are network-maintained recipes from peer aspects who've solved similar problems before."

Costs ~150 tokens in the system prompt; substantial behavior change.

**Mitigation 2 — soul-side category hints.** Planner soul enumerates skill *categories* without paste-loading their bodies: "Planning, code review, security analysis, research synthesis, decomposition, spec-drafting — all have nexus-skills catalogs. Query before starting." Sets up the discovery reflex.

**Mitigation 3 — auto-list on planner-class turns** (heavier, defer): every planner turn's TurnRequest auto-prepends a `nexus_skills.list` result as inbox context. Removes the discovery step entirely; cost is the list payload per turn. Use only if Mitigations 1+2 prove insufficient.

---

## Open questions

1. **(superseded by Discovery section above)** — kept as a slot so question numbering doesn't shift.

2. **Shape B + claude-code subprocess.** claude-code under `claude -p` runs its own agent loop; can it call `nexus_skills.fetch` mid-turn and incorporate the result in the same turn? Probably yes via MCP tool calls, but worth verifying.

3. **Skill chaining.** A skill body may reference other skills ("see also: <other-skill>"). Does the model auto-fetch referenced skills, or wait until the operator/planner explicitly asks? Probably the latter for v1 — recursive fetching is a future feature.

4. **Versioning during a single turn.** If a skill is upserted while a turn is running, the next fetch in the same turn would get the new version. Acceptable (live-update) or worth pinning version per turn? Probably acceptable; rollouts are rare; pin-per-turn adds complexity.

5. **Skill body size limits.** Long skills (>10K) eat context. Set a soft cap? Refactor skills into sub-skills if they're too long?

6. **Maintainer-aspect semantics.** Does the owner_aspect get auto-notified when their skill is fetched? Useful for telemetry but not blocking.

7. **Skills with embedded code or hooks.** If a skill needs to install a hook or write a file, Shape B's inline-body doesn't work. Either restrict skills to be content-only, OR have Shape A as the alternative delivery for "thicker" skills.

---

## Procedural memory — auto-creation from experience

Inspired by Hermes Agent §1.2 (per harrow's breakdown at `~/Google Drive/My Drive/nexus/general/2026-05-15-hermes-feature-breakdown.md`). The catalog above is one-way: operator + maintainer aspects upsert skills, models consume them. Procedural memory adds the reverse path — **skills crystallize from successful execution**, then become part of the catalog for the next agent to discover.

### Creation trigger — runtime nudge

The model doesn't decide on its own to write a skill. The runtime prompts: after a turn (or a sequence of turns) that completed a non-trivial task successfully, the runtime nudges the model with something like:

> "You just completed a multi-step task that took N tool calls and produced result R. Is there a reusable procedure worth saving as a skill for future similar tasks? If yes, draft it; if no, say so."

The model either drafts a skill body (markdown recipe per §"What skills actually contain") or declines. Either way, the runtime gets a signal that distinguishes "worth saving" from "ephemeral."

This is Hermes' "periodic memory nudges" applied specifically to procedural memory. The cost is the nudge turn (one extra inference per candidate-worthy session); the value is the catalog grows from real experience rather than only from curated authoring.

### Self-improvement during use

Hermes' insight: skills aren't immutable once authored. When the model invokes a skill and discovers mid-execution that the recipe is wrong, suboptimal, or missing a step, the runtime allows in-place editing of the skill body. Next invocation gets the improved version automatically.

This forces **versioning at the catalog layer**:
- Every skill has a version number that bumps on edit.
- The catalog retains version history (rollback path).
- Audit logs record who/what triggered each edit (model-self-edit vs operator-edit) and from which trigger session.

Without versioning, model-self-edits are catastrophic — a hallucinated "improvement" overwrites a working skill with no recovery.

### Submission path

The auto-creation path needs operator gating before publishing into the live catalog:

- Model drafts a skill (in response to a nudge) → enters a **proposed** state.
- Operator (or a designated reviewer aspect) approves/rejects → on approve, the skill becomes **published**.
- Direct in-place edits during invocation similarly enter **proposed-edit** state and require approval before becoming the current version.

This avoids the model silently polluting the catalog with low-quality or hallucinated skills. v1 can run review-gated; v2 may auto-publish skills from trusted-aspect models if telemetry shows the gating bottleneck isn't paying for itself.

### Memory substrate context

Procedural memory sits on top of an episodic/semantic memory layer per Hermes §1.1:
- **Episodic** — what happened in this session (Hermes uses FTS5 + LLM summarization for cross-session recall).
- **Semantic** — generalized facts (our `memory/` directories with type-tagged memories — user / project / reference / feedback).
- **Procedural** — crystallized how-to (the skill catalog).

Nexus already has the semantic layer via `~/.claude/projects/<aspect>/memory/`. We don't yet have:
- An episodic search layer (FTS5 across sessions).
- The runtime nudges that prompt the model to curate.

Episodic search is a separate story (see NEX-XX, filed). Runtime nudges for the procedural layer specifically are in scope for the skill-delivery design and would land in the same agentfunnel/agora runtime pass that wires `nexus_skills.list/fetch`.

### Implications for v1 acceptance

Extend the v1 acceptance criteria:
- An agent can submit a draft skill via `nexus_skills.propose(name, body, description, intended_use, source_session_id)`.
- The draft sits in **proposed** state until operator (or designated reviewer) approves.
- Published skills bump version on edit; prior versions remain queryable.
- Audit trail captures author (operator vs aspect-self), trigger session, version diff.

**Out of v1 for procedural memory** (defer):
- Runtime nudge implementation (model-side prompting on turn-end).
- Auto-edit-during-invocation flow.
- Cross-aspect skill sharing (Aspect A's auto-created skill auto-published into Aspect B's catalog).

The catalog + tool surface + version handling land in v1; the nudge + auto-creation layer lands in v2 once we know the catalog is useful.

---

## Lanes (if greenlit)

- **keel-cli (broker/admin):** schema, admin REST, JWT-gated WS frame for skills.fetch + skills.list, audit log, CLI subcommands.
- **shadow (cross-aspect coordination):** new `nexus-skills-mcp` server (mirrors comms/jira/imap MCP pattern); operator coordination on initial skill migration; this design doc.
- **anvil:** consumer-side MCP integration into agentfunnel + agora; tests.
- **operator:** decides initial catalog, grants per-aspect.

---

## v1 acceptance

- Operator can `nexus skill upsert` a skill via CLI.
- Operator can grant the skill to shadow.
- shadow's claude-code session can call `nexus_skills.list` and see the skill.
- shadow's claude-code session can call `nexus_skills.fetch` and receive the body.
- An audit row is written for the fetch.
- Another aspect (without the grant) calling `fetch` for the same skill is denied.

---

## Not in v1

- Recursive skill chaining.
- Subscription-driven auto-loading (Shape A) — defer until we hit a skill that doesn't fit Shape B.
- Dashboard surface for skill management.
- Live-push notifications when a skill is updated.
- Skill-specific permission grants beyond per-aspect (e.g. per-thread or per-component scoping).
