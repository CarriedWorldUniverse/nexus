---
name: nexus-jira
description: 'Use when creating, updating, transitioning, or commenting on jira issues via the nexus-jira MCP tools — the exact param surface, valid transitions, and parent rules. ~35 transcript-audited errors came from guessing this surface.'
when_to_use: 'When creating, updating, transitioning, or commenting on jira issues via the nexus-jira MCP tools — exact param surface, valid transitions, parent rules.'
---

# nexus-jira MCP — the real tool surface

Reference distilled from the live tool schemas + transcript-audited failure signatures (2026-07-08). When in doubt, ToolSearch the schema — don't guess params.

## Params that get guessed wrong (the failure class)

- **`jira_comment(key, body)`** — exactly those names. NOT `issue_key`, NOT `comment` (7 failures: "key and body are required"). Body is plain text; newlines become paragraphs.
- **`jira_update_status(key, status, comment?)`** — status by NAME from the workflow: **To Do · In Progress · In Review · Done**. **There is no "Cancelled"** (4 failures) — cancel = transition to Done with a comment saying cancelled/won't-do, or leave in To Do with a closing comment.
- **`jira_create(summary, issue_type, …)`** — required: `summary`, `issue_type` (one of Epic, Story, Task, Subtask, Bug). Optional: `parent` (issue KEY), `project` (KEY, e.g. "WKS"; defaults to the client's bound project), `description` (markdown — rendered as an ADF code block), `labels`, `component`.
- **`jira_update(key, …)`** — only provided fields are written. `labels: []` clears all labels; `component: ""` clears component; omit = unchanged.

## Parent rules (8 failures: "Please select valid parent issue")

- Parent must be a **valid hierarchy parent for the child type**: Stories/Tasks/Bugs parent to an **Epic**; **Subtasks** parent to a Story/Task. A Story cannot parent a Story.
- The parent must exist **in the same project** you're creating into. Cross-project parenting fails.
- No parent field for Epics.

## Project routing (4 failures: "target project doesn't exist or no permission")

- Omit `project` → the MCP client's bound project (aspect-dependent). Pass `project` explicitly when filing elsewhere (e.g. "WKS" for WakeStone) — and verify the key exists (`jira_search` on it) before batch-creating; a bad key fails every create in the batch.

## Cheap look-before-write

- `jira_get(key)` before update/transition when state matters — transitions are state-dependent.
- `jira_search` to confirm a project key or find a parent's key.
- Batch-creating an epic + children: create the Epic first, capture its key, then children with `parent=<epic-key>` — don't parallelize the children ahead of the parent existing.
