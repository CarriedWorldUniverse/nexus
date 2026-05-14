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

### `nexus_skills.list`

```
input: { aspect?: string }    -- defaults to self; ops may query other aspects' catalogs
output: [
  {
    name: string,
    version: int,
    description: string,
    intended_use: string
  }
]
```

Returns the skills the calling aspect (or queried aspect, if permitted) can fetch. Lightweight — no bodies.

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

## Open questions

1. **Discovery problem.** The model has to call `list` to know skills exist. If it doesn't think to look, it never uses them. Mitigation: the planner soul prompt enumerates available skill *categories* ("when X situation, consider fetching a skill") without paste-inlining all of them. Or: `list` is auto-called at the start of every turn (cheap, returns names + descriptions only).

2. **Shape B + claude-code subprocess.** claude-code under `claude -p` runs its own agent loop; can it call `nexus_skills.fetch` mid-turn and incorporate the result in the same turn? Probably yes via MCP tool calls, but worth verifying.

3. **Skill chaining.** A skill body may reference other skills ("see also: <other-skill>"). Does the model auto-fetch referenced skills, or wait until the operator/planner explicitly asks? Probably the latter for v1 — recursive fetching is a future feature.

4. **Versioning during a single turn.** If a skill is upserted while a turn is running, the next fetch in the same turn would get the new version. Acceptable (live-update) or worth pinning version per turn? Probably acceptable; rollouts are rare; pin-per-turn adds complexity.

5. **Skill body size limits.** Long skills (>10K) eat context. Set a soft cap? Refactor skills into sub-skills if they're too long?

6. **Maintainer-aspect semantics.** Does the owner_aspect get auto-notified when their skill is fetched? Useful for telemetry but not blocking.

7. **Skills with embedded code or hooks.** If a skill needs to install a hook or write a file, Shape B's inline-body doesn't work. Either restrict skills to be content-only, OR have Shape A as the alternative delivery for "thicker" skills.

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
