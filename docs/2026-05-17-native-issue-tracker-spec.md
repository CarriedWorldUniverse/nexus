# Native Issue Tracker — Design Spec

**Date**: 2026-05-17
**Epic**: [[NEX-137]]
**Status**: Draft — pending operator review
**Author**: shadow (with operator collaboration via /superpowers:brainstorming)

## Purpose

Replace the temporary Jira tracker (carriedworlduniverse.atlassian.net) with a nexus-resident, AI-first issue tracker. Aspects work tickets natively; the operator reviews + edits via dashboard. Jira was always temporary; this epic ends the dependency.

## Why

- Jira is external infra. Aspects need full-user-scope API tokens per aspect. The `nexus-jira-mcp` exposes ~9 of dozens of REST endpoints — the rest is unreachable from aspect code without expanding the MCP.
- Work-routing convention (v1.2) lives in spec text, not in tracker data. Routing decisions aren't projectable into nexus-side flows.
- AI-first storage means markdown end-to-end. Jira's ADF round-trip is friction.
- Aspects can't autonomously work tickets without a run-loop ([[NEX-138]]) and a tracker that's local + fast.

## Scope summary

This spec covers the **native issue tracker** only. Dependencies tracked separately:

- [[NEX-138]] — autonomous run-loop primitive (headless equivalent of `/goal`). Tracker designed to be loop-callable, but ships before NEX-138 with operator-driven flow.
- [[NEX-139]] — nexus file store with portable `nexus://` references. Attachments depend on this.
- [[NEX-140]] — interchange extension for external sender classes (GitHub webhooks). External-sync features depend on this.

## Core decisions

| # | Topic | Decision |
|---|-------|----------|
| 1 | Storage model | Hybrid: relational core for queries + backlog; markdown documents are the aspect-facing artifact (materialised from row + timeline) |
| 2 | DB | Own SQLite (`issues.db`), parallel to `broker.db`. Separate WAL/lock/backup. In-process under nexus.exe supervisor. |
| 3 | "Get next ticket" | Ranked ready pool per aspect/team. Priority set by planners (any aspect, audited). Blocked tickets fall out of pool until blocker clears. |
| 4 | Priority controls | All changes logged in timeline with reasoning. Operator can mark `priority-locked` to freeze. Aspect changing priority on a ticket it's working pings operator. |
| 5 | Routing | Assignee is either a specific aspect OR a named team. Teams are operator-defined sets. Default team for engineering: `oss-nexus-dev` = {keel, shadow, anvil, plumb}. Unity work pinned to wren. |
| 6 | Workflow (per issue type) | **Epic**: Brief → Sketch/Refined → In Development → Delivered. **Story/Task/Bug**: To Do → In Progress → Blocked → In Review → Done → Cancelled. **Subtask**: same as Story. |
| 7 | Definition of Done | Markdown checklist (`- [ ]`). All items must be ticked before transition to Done/Delivered. Workflow validator enforces. |
| 8 | Comment edits | **Immutable**, append-only. Aspects must plan before posting. Self-correction = new comment. |
| 9 | Permissions | Soft guards, full audit: close-someone-else requires rationale; reporter immutable; no deletes (only Cancelled state); DoD modified mid-flight pings operator; Epic-archive is operator-only. |
| 10 | Notifications | Operator stream (push assignments + mentions to assignee, all events to an operator activity stream). Aspects on run-loops also pull `issue_list_my_updates`. |
| 11 | External links | GitHub issues inbound only at first: filed → shadow/keel validate → recommendation → operator final accept → internal issue spawned + linked. Close gate: ticket can't close until linked PR merged + reviewed. |
| 12 | Migration | Dual-write `nexus-jira-mcp` from Phase 1, cutover at Phase 6. Reverse-importer ready for rollback during 24h window. |
| 13 | Search | Structured filter object primary. Optional `where: <jql>` escape hatch later (Phase 5+). |
| 14 | Attachments | `nexus://` references via [[NEX-139]] file store. No native blob storage. |
| 15 | Webhook intake | Via [[NEX-140]] (interchange extension). v1 falls back to IMAP email parsing until 140 ships. |
| 16 | Backups | Native `nexus snapshot` command: brief write pause, atomic per-DB `.backup`, hardlinked blob root, tarball with manifest to Drive. |
| 17 | Issue keys | Per-project monotonic sequence: `NEX-1`, `WAKE-1`. Cross-project move supported via `issue_reassign_project` with `key_aliases` for forever-stable lookup. |

## Data model

### Tables (`issues.db`)

**`projects`**
- `key` (PK, e.g. `NEX`, `WAKE`)
- `name`, `description`
- `default_team` (FK to `teams.id`, nullable)
- `archived` (bool)

**`project_sequences`**
- `project` (PK, FK to projects.key)
- `next_seq` (int, transactional counter)

**`teams`**
- `id` (PK), `name` (unique, e.g. `oss-nexus-dev`)
- `description`

**`team_members`**
- `team_id` (FK), `aspect` (text — name from broker `aspects` table)
- PRIMARY KEY (team_id, aspect)

**`issues`**
- `key` (PK, e.g. `NEX-137`)
- `project` (FK to projects.key)
- `seq` (int — denormalised from project_sequences for clarity)
- `type` (Epic | Story | Task | Subtask | Bug)
- `status` (text — workflow state, validated per-type)
- `summary` (text)
- `description` (markdown)
- `definition_of_done` (markdown checklist; nullable for Subtask/Bug)
- `priority` (Lowest | Low | Medium | High | Highest)
- `priority_locked` (bool, operator-only)
- `assignee_aspect` (text, nullable — direct assignment)
- `assignee_team` (FK to teams.id, nullable — team queue)
- `reporter` (text, immutable after create)
- `created_at`, `updated_at`
- `parent_key` (FK to issues.key, nullable — for Story/Task→Epic, Subtask→Story)

Either `assignee_aspect` OR `assignee_team` is set, not both. Unassigned tickets have neither.

**`events`** (single timeline table, discriminator column)
- `id` (PK auto-increment)
- `issue_key` (FK to issues.key)
- `seq` (int per issue — ordered timeline)
- `kind` (enum: `comment`, `transition`, `field_change`, `link_added`, `link_removed`, `attachment_added`, `attachment_removed`, `project_moved`, `external_sync`, `conflict`)
- `actor` (text — aspect name)
- `at` (timestamp)
- `payload` (JSON — kind-specific structure)

**`links`** (internal)
- `from_key` (FK to issues.key)
- `to_key` (FK to issues.key)
- `type` (blocks | is-blocked-by | relates-to | duplicates | parent | subtask-of)
- `created_at`, `created_by`
- PRIMARY KEY (from_key, to_key, type)

**`external_refs`**
- `id` (PK)
- `issue_key` (FK)
- `provider` (github | jira | cairn | linear | slack | other)
- `external_id` (text — provider-specific)
- `url` (text)
- `sync_policy` (inbound-only | bidirectional | inbound-on-create-only)
- `last_synced_at`, `sync_state` (ok | stale | error)
- `metadata` (JSON — provider-specific extras)

**`attachments`**
- `id` (PK)
- `issue_key` (FK)
- `nexus_ref` (text — `nexus://files/...` from NEX-139)
- `filename` (text — display name)
- `size_bytes`, `sha256`
- `added_at`, `added_by`

**`watchers`**
- `issue_key` (FK)
- `aspect` (text)
- `since` (timestamp)
- PRIMARY KEY (issue_key, aspect)

**`key_aliases`**
- `old_key` (PK — e.g. `NEX-145`)
- `new_key` (text — current canonical key, e.g. `OSS-7`)
- `moved_at`

### Materialised markdown view

Aspects don't read rows directly via MCP. They get a **markdown document** materialised from `issues` + `events`:

```
---
key: NEX-137
project: NEX
type: Epic
status: In Development
priority: High
assignee_team: oss-nexus-dev
reporter: shadow
created: 2026-05-17T...
parent: null
---

# Native issue management — nexus-resident tracker

## Description

(markdown body of issues.description)

## Definition of Done

- [x] Done item 1
- [ ] Pending item 2

## Links

- blocks: NEX-200
- relates-to: NEX-138
- external: github.com/CarriedWorldUniverse/nexus/issues/42

## Attachments

- concept-v3.psd → nexus://issues/WAKE-7/concept-v3.psd

## Timeline

### 2026-05-17 09:15 — shadow (created)
(field initial values...)

### 2026-05-17 10:02 — anvil (transition: To Do → In Progress)
Reason: starting work.

### 2026-05-17 10:30 — anvil (comment)
Investigated; using approach B for the schema.

...
```

This is the canonical aspect-facing artifact. The MCP `issue_get` returns it as a string. Edit tools take partial markdown patches or structured field updates.

## MCP surface (`nexus-issue-mcp`)

| Tool | Purpose |
|------|---------|
| `issue_create` | Create an issue. Required: project, type, summary. Optional: description, DoD, assignee, parent, labels. |
| `issue_get` | Returns materialised markdown document for a key (or aliased key). |
| `issue_get_raw` | Returns structured JSON (for dashboard, not aspect use). |
| `issue_update` | Patch fields (summary, description, DoD, priority, labels, components). Workflow-gated. |
| `issue_transition` | Move to a new status. Validates: per-type transitions allowed, DoD complete for terminal states, no blocked-by-open-issue if transitioning to Done. |
| `issue_assign` | Set assignee_aspect or assignee_team. Either, not both. |
| `issue_comment` | Append a comment (markdown). Immutable. |
| `issue_link` | Add internal link (blocks, relates, parent, etc.) |
| `issue_unlink` | Remove an internal link. |
| `issue_link_external` | Add external link with provider + sync_policy. |
| `issue_link_artifact` | Attach a `nexus://` file reference (calls into NEX-139). |
| `issue_watch` / `issue_unwatch` | Manage own watcher row. |
| `issue_search` | Structured filter: project, type, status[], assignee, team, labels[], priority, parent, created_range, updated_range, order_by. |
| `issue_list_my` | Convenience: issues assigned to caller's aspect (or any of caller's teams). |
| `issue_list_ready` | Top of the ready pool for caller (excludes blocked, ordered by priority + age). Used by run-loops. |
| `issue_list_my_updates` | Since-timestamp diff: events that touched caller's watched/assigned issues. Pull-mode notifications for run-loops. |
| `issue_reassign_project` | Move issue between projects. Allocates new key in destination; records alias. |
| `issue_validate_external_inbound` | Operator-aspect tool: a GitHub-issue payload comes in via NEX-140; this tool emits a validation recommendation (replicate / replicate-with-edits / reject) + reasoning. Surfaces to operator for final accept. |

Dashboard hits the same surface plus an `issue_get_raw` for structured rendering.

## Notification flow

Two channels, both backed by broker chat:

1. **Push (high signal, per-recipient)** — broker DM to:
   - The assignee_aspect (or each team member) when a ticket is assigned or first lands in the team queue
   - Aspects mentioned via `@aspect` in comments or descriptions
   - Watchers when an issue transitions to `Blocked` or has a blocker cleared

2. **Operator activity stream (passive)** — a single chat thread (or dashboard tab) the operator can leave open:
   - Every transition, comment, link, attachment, assignment across all issues
   - One-line summary per event with key + actor + action
   - No pings — visible if and when the operator looks
   - Aspects can also subscribe to the stream (shadow does by default)

For run-loops, push is the wake signal; pull (`issue_list_my_updates`) is the recovery / catch-up path.

## Workflow validator

State machine enforced on every `issue_transition` call. Rules per type:

**Epic**:
- Brief → Sketch/Refined → In Development → Delivered
- Any state → Cancelled
- Cannot transition to Delivered unless DoD checklist 100% ticked

**Story / Task / Bug**:
- To Do ↔ In Progress, To Do → Cancelled
- In Progress → Blocked ↔ In Progress (or → In Review)
- In Review → In Progress (kickback) or → Done
- Any state → Cancelled
- Cannot transition to Done unless DoD complete (when DoD is present) AND no open blocked-by links pointing to non-terminal issues
- If linked PR exists and `sync_policy = bidirectional`, cannot transition to Done unless PR is merged

**Subtask**: same as Story.

## External-sync engine

Worker goroutine inside `nexus/issues/sync/`. Reads from `sync_jobs` table (durable across nexus.exe restarts).

**Inbound (from NEX-140 interchange)**:
- Interchange delivers a normalized event to nexus's webhook inbox
- Issues sync worker dequeues, looks up matching external_refs by `(provider, external_id)`
- Updates linked issue: log `external_sync` event in timeline, mirror status if policy allows, log `conflict` event if local + external disagree on terminal state

**Inbound from GitHub specifically (validated flow)**:
- New GH issue event with no matching `external_refs` row → enqueue for validation
- `issue_validate_external_inbound` invoked (by shadow or keel) → recommendation
- Recommendation surfaces to operator via push notification
- Operator confirms → tracker creates internal issue + `external_refs` row linking back

**Outbound** (when implemented in Phase 3):
- Only on bidirectional `external_refs` with `can_push_terminal_state: true`
- On transition to Done with merged-PR-gate satisfied: post a comment to GH PR + close GH issue
- Failures are logged + retried via `sync_jobs`; don't block the local transition

## Cross-project move

`issue_reassign_project(key, new_project, reason)`:

1. Verify caller permission (operator-only by default; configurable later)
2. Allocate new key from destination project's sequence (transactional)
3. UPDATE issues: change `project`, `seq`, `key`. v1 disallows cross-project parents — if the moved issue had children in the old project, the move is rejected; if it had a parent in the old project, the parent link is dropped (recorded in `field_change` event).
4. INSERT into `key_aliases` (old_key → new_key)
5. Write `project_moved` event with old_key, new_key, actor, reason
6. Notify watchers + assignee

Lookup paths (`issue_get`, `issue_link`, etc.) check `key_aliases` if the key isn't found in `issues`. External links continue to work since they reference by URL, not by our key.

## Phases (implementation roadmap)

| Phase | Goal | Size | Exit criteria |
|-------|------|------|---------------|
| 0 | Prep, ADR, schema review, `issues.db` migration runner | 1 aw | Empty DB boots, `/healthz/issues` ok |
| 1 | MV issue store + MCP create/get/update/transition/list/search + dual-write shim | 2 aw | Aspect can round-trip via MCP; Jira still authoritative |
| 2 | Comments + timeline + watchers + push + operator stream | 1.5 aw | Full activity flowing; chat pings working |
| 3 | Internal links + external sync (via NEX-140 GH inbound validated flow) | 2 aw | GH issues land via interchange → validation → internal issue |
| 4 | Attachments via NEX-139 + FTS5 + structured filter search | 1.5 aw | `nexus://` refs round-trip; structured filters work |
| 5 | Dashboard UI (list, detail, edit) | 2 aw | Operator triages a day's work without touching Jira |
| 6 | Jira migration tool + cutover | 1.5 aw | All NEX-* migrated; Jira read-only; aspects on native MCP |
| 7 | Deprecate `nexus-jira-mcp` | 0.5 aw | Binary removed |

**Total**: ~12 aspect-weeks across the engineering team (keel/shadow/anvil/plumb).

## Cutover gate (must all be true)

- Phases 1–5 shipped and used by aspects ≥1 week
- Dual-write divergence nightly report shows <0.1% drift for 3 consecutive days
- Importer dry-run on prod Jira completes clean with zero unresolved references
- Dashboard usable for a full operator triage session without Jira fallback
- Backup of both `issues.db` + file-store root verified restoreable
- Rollback path (reverse-importer) tested in staging

## Out of scope (v1)

- Sprints / boards / time tracking / worklogs
- Per-field permissions / heavy ACL
- Custom workflows per project (workflow is per-type, hardcoded)
- Confluence-style wiki pages
- Self-service onboarding of new external providers
- Cross-host file fetch (NEX-139 v1 limit)
- Comment edit history (immutable by design)
- Outbound destructive ops on external systems other than `closed` mirror on GH PRs

## Open questions (post-spec, pre-build)

- Mentions syntax: `@anvil` resolved against `aspects` table — case-sensitive? (recommend case-insensitive)
- Cancelled-ticket retention: prune after N days, or keep forever? (recommend keep forever, dashboard hides by default)
- Workflow strict-mode escape hatch: should operator be able to force a transition that violates the validator? (recommend yes, logged as `forced_transition` event)
- Cross-project parent links: allowed (e.g., WAKE story has NEX epic as parent)? Adds resolution complexity; defer to "no" in v1.
- Markdown flavour: GFM via goldmark, with sanitiser tuned for the dashboard. Lock GFM.
- Subtask DoD: required or optional? Recommend optional (Story/Task/Bug make it discretionary, Epic always required).

## Risks

- **Dual-write drift** — mitigated by nightly divergence report + cutover gate.
- **NEX-140 timing** — Phase 3 depends on interchange extension. If 140 slips, Phase 3 falls back to IMAP email parsing as bridge.
- **NEX-139 timing** — Phase 4 attachments depend on file store. If 139 slips, Phase 4 ships without attachments; story re-opened later.
- **Schema churn during Phase 1–2** — early aspects on the new store may need data migrations. Mitigate by tagging Phase 1 as "alpha schema", reserve right to non-backward-compatible changes pre-Phase-2 exit.
- **Cutover day** — operator drives. Reverse-importer + 24h hard rollback window.

## References

- Epic: NEX-137 (this spec is the design doc for it)
- Sibling deps: NEX-138, NEX-139, NEX-140
- Existing related docs in `docs/`:
  - `2026-05-04-files-subsystem-spec.md` — predecessor thinking for NEX-139
  - `2026-05-05-issue-triage-analysis.md` — earlier tracker-shape notes
  - `2026-05-05-storage-abstraction-spec.md` — storage patterns
