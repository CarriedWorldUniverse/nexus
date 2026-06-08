# Work UI — Phase 1: Watch + Run Spine + Env-Health (Design)

**Date:** 2026-06-09
**Status:** Design — pending review
**Phase:** 1 of 5 (see Phasing). This phase carries the new backend architecture and resolves the core complaint.

## Problem

The dashboard was built around the old aspects-as-conversation model (FeedView / Chat / ObserveView). Against the dispatch-native platform, two things the operator most needs are not visible:

- **Chat / dispatch conversation** — `!dispatch` posts are stored but not fanned to recipients; topics are deferred (everything is one bucket); progress/summary posts aren't threaded under the command (NEX-516). Dispatch conversation doesn't surface coherently.
- **Activity** — agent activity (L3 turn frames) is written to JSONL on disk and pushed live over `subscribe.observe`, but there is **no HTTP/RPC endpoint to fetch history**. After a page reload there is nothing; a dispatched builder's run — the thing you most want to watch — has no home in the UI.

There is no **dispatch run** as a first-class object anywhere. Runs are inferred from thread shape by a heuristic. The runner already has the real identity (`run_id` = `run-{uuid}`, agent, ticket, thread, `JobDone`); none of it is queryable.

## Goal

A read-only **Watch** surface that makes a dispatch run's complete story visible — command, the builder's activity, and outcome — live and historical — by making the run a first-class, persisted, queryable object. Plus an openable Team panel and a read-only Env-health panel.

## Scope

**In scope (Phase 1):**
- A persisted **runs** read-model and the runner writes to it.
- New WS RPCs: `runs.list`, `run.get` (unified timeline), `activity.history` (backfill), `env.health`.
- Activity frames tagged with `run_id` for exact run↔activity association.
- The shell: three-area nav (Converse · Watch · Configure) with only **Watch** active; panel-toggle affordance.
- The Watch stable surface: **run feed** + **unified run timeline**.
- Openable **Team** panel (roster + live state) and **Env-health** panel (read-only).

**Out of scope (later phases):**
- Phase 2 — control actions (dispatch, cancel, reply-into-thread, configure-agent).
- Phase 3 — Converse as a first-class area.
- Phase 4 — Configure re-IA.
- Phase 5 — dedicated mobile experience.
- No always-visible strips. No write/control paths in Phase 1.

## Architecture

### The run spine (new backend object)

A **run** is the unit of the whole console. We persist it in a small `runs` table (sqld, alongside `chat_messages`) so it is durable and queryable independent of the k8s Job TTL (Jobs vanish ~300s after completion).

```
runs:
  run_id          TEXT PRIMARY KEY    -- run-{uuid}
  ticket          TEXT                -- NEX-xxx (or ad-hoc id)
  agent           TEXT                -- e.g. anvil
  thread          TEXT                -- chat thread/topic key (usually = ticket)
  dispatch_msg_id INTEGER             -- the !dispatch chat message id (thread root)
  parent_run_id   TEXT NULL           -- recursive dispatch lineage
  command         TEXT                -- the !dispatch brief text (trimmed)
  status          TEXT                -- queued | running | complete | failed | cancelled
  started_at      INTEGER             -- unix ms
  completed_at    INTEGER NULL
  pr_url          TEXT NULL
  duration_secs   INTEGER NULL
  repo            TEXT NULL
```

The runner is the single writer:
- On **reserve** (`runner.Submit`, where `run_id` is minted): `INSERT` the row (`status=running`, `started_at`, command/agent/ticket/thread/repo/dispatch_msg_id).
- On **`JobDone`** (the existing `WatchJobs` callback): `UPDATE` (`status`, `completed_at`, `duration_secs`, `pr_url`).

This is purely additive — the runner already holds every field at those two points (see `runtime/dispatch/runner.go` `provisionRun`/`Submit` and `JobDone`).

### Activity ↔ run association

L3 activity frames are per-aspect today (`jsonlsink` writes `<root>/<aspect>/<YYYY-MM-DD>.jsonl`; the observability Hub keys by aspect). To bind a frame to a run exactly, the dispatched builder tags its emitted frames with its `run_id`.

- The builder pod already has `CW_DISPATCH_RUN_ID` in env (jobspec). The agentfunnel observability emit path includes `run_id` in the frame envelope (presence/turn/chat frames).
- `activity.history(run_id)` then filters precisely.
- **Fallback** for untagged frames (persistent aspects, historical data): window the aspect's frames to `[started_at, completed_at]`. For dispatch builders — one run per pod — the time window is already exact, so the fallback is lossless for Phase 1's primary case.

### The unified timeline

`run.get` returns the run row plus a single time-ordered list of **timeline items**, merging two sources:

```
TimelineItem {
  kind: "chat" | "activity"
  at:   int    // unix ms (sort key)
  // when kind=chat (from chat_messages, thread = run.thread):
  chat?: { msg_id, from, content, reactions, reply_to }
  // when kind=activity (from L3 frames, run_id-tagged):
  activity?: { type: "turn"|"tool"|"thought"|"presence", text, tool, state }
}
```

Merge rule: union the run's chat-thread messages and its activity frames, sort by `at`. The frontend renders chat items with the existing `MessageBubble` and activity items with a compact activity-line component. Ties (equal `at`) order chat before activity (the command precedes the work it triggers).

### Env-health (read-only)

A new `env.health` RPC. The broker (already an in-cluster pod with a k8s client and the `nexus-broker-dispatch` Role — note: needs `pods`/`pvc`/`deployments` read verbs, see Open Questions) returns a snapshot:

```
EnvHealth {
  components: [ {name, kind, healthy, detail} ]   // broker, sqld, gemma — pod ready?
  pods:       {running, total}
  pvcs:       [ {name, status} ]                  // aspect-home-*, nexus-builder-repos: Bound?
  last_deploy: {what, at} | null                  // broker deployment's last rollout
}
```
Read-only. Polled by the panel when open (e.g. every 15s). No control verbs.

### Frontend shell + Watch

Evolve the existing Preact + htm SPA (`nexus/broker/static/dashboard/`, served via `go:embed`, no build step). Reuse the WS RPC client (`comms.js`), `MessageBubble`, `subscribe.observe`/`subscribe.chat`, auth.

- **Shell** (`app.js`): top nav with three areas — Converse · Watch · Configure — only **Watch** routed in Phase 1 (the others are visible-but-inert placeholders so the nav is honest about what's coming). A panel-toggle affordance for Team / Env-health. No always-visible strips.
- **Watch** (new `WatchView.js`): the stable surface = **run feed** (left, `runs.list`) + **unified timeline** (center/right, `run.get` for the selected run, live-updated via `subscribe.observe` + `subscribe.chat`).
- **Team panel** (openable): roster + live state. Reuse `roster.list` + `subscribe.roster` + the existing `activity.js` model.
- **Env-health panel** (openable): renders `env.health`, polled while open.

## Data flow

**On load / select a run:**
1. `runs.list` → populate the run feed.
2. Select a run → `run.get(run_id)` → the run row + the merged historical timeline (backfilled chat + activity).
3. Subscribe live: `subscribe.observe` (the run's agent) + `subscribe.chat` (the run's thread) → append new items to the timeline in place.

**Live state:** an active run's status/feed entry updates from `subscribe.observe` presence/turn frames and from the runner's row updates surfaced via a lightweight `runs.update` push (reuse the observability Hub broadcast pattern).

**Historical:** everything comes from the durable `runs` table + `chat_messages` + the `activity.history` backfill of the JSONL. Page reload is fully reconstructable (the current gap).

## Error handling

- `run.get` for an unknown/expired `run_id` → empty timeline + a clear "run not found / aged out" state; never a blank panel.
- `activity.history` when the JSONL for the window is gone (rotation/retention) → return what exists + a "partial history" marker; the timeline still renders chat.
- `env.health` when a k8s read fails (RBAC/transient) → mark that component `healthy: false, detail: "unreadable"` rather than failing the whole snapshot.
- WS drop → the existing reconnect path re-subscribes; on reconnect, re-run `run.get` for the open run to backfill any gap (don't assume the live stream was continuous).

## Testing

- **runs read-model:** unit-test the runner's insert-on-reserve and update-on-`JobDone` against a fake store; assert a reserved run is `running`, a `JobDone` run is `complete|failed` with duration/PR.
- **timeline merge:** table-test the chat+activity interleave (ordering, tie-break, kind tagging) with fixed timestamps.
- **activity tagging:** assert emitted frames carry `run_id`; `activity.history(run_id)` returns only that run's frames; time-window fallback for untagged frames.
- **env.health:** fake k8s client (the existing `fake.NewSimpleClientset` pattern in `k8s_test.go`) → assert component/pod/pvc mapping; one unreadable component degrades only itself.
- **frontend:** the run feed renders from `runs.list`; selecting a run loads the merged timeline; live frames append; Team/Env-health panels toggle open/closed (no always-visible chrome). Manual + the existing dashboard test harness.

## File structure

**Backend (Go, `nexus/`):**
- Create `nexus/broker/runs_store.go` — the `runs` read-model (interface + sqld impl) and migration.
- Create `nexus/broker/runs_rpc.go` — `runs.list`, `run.get` WS handlers (timeline merge lives here).
- Create `nexus/broker/activity_history.go` — `activity.history` (reads jsonlsink files for a run/window).
- Create `nexus/broker/env_health.go` — `env.health` (k8s reads).
- Modify `runtime/dispatch/runner.go` — write the run row on reserve and on `JobDone`.
- Modify the agentfunnel observability emit path — tag frames with `run_id`.
- Modify `nexus/broker/ws.go` — register the new RPC kinds.

**Frontend (`nexus/broker/static/dashboard/`):**
- Modify `js/app.js` — three-area shell nav + panel toggles.
- Create `js/views/WatchView.js` — run feed + unified timeline.
- Create `js/views/panels/TeamPanel.js`, `js/views/panels/EnvHealthPanel.js`.
- Modify `js/api.js` / `js/comms.js` — add the new RPC calls + the `runs.update` push.
- Create `css/watch.css` — Watch surface + panel styles (reuse tokens; `chat.css` MessageBubble unchanged).

## Phasing (context)

1. **Phase 1 (this doc)** — Watch + run spine + env-health panel. Read-only.
2. Phase 2 — control: dispatch, cancel, reply-into-thread, configure-agent.
3. Phase 3 — Converse as a first-class area.
4. Phase 4 — Configure re-IA.
5. Phase 5 — dedicated mobile experience.

## Resolved decisions (confirmed 2026-06-09)

1. **Broker RBAC for env.health** — CONFIRMED include. Add `deployments` get/list (and confirm pods/pvc get/list) to the `nexus-broker-dispatch` Role in `carriedworld-cloud/hosting/services/nexus-broker-dispatch-rbac.yaml` (same manifest as the per-agent-home PVC RBAC fix). The plan includes this manifest change + apply.
2. **Active-run updates** — CONFIRMED push. A lightweight `runs.update` push over the existing observability Hub broadcast pattern carries active-run status changes; the frontend updates the feed entry in place. (Not interval polling.)
3. **Activity tagging** — CONFIRMED. `run_id`-tagging of emitted activity frames is the primary run↔activity association (agentfunnel observability emit path includes `run_id`); time-windowing remains the lossless fallback for untagged/historical frames.

## Out of scope (noted)

- **Activity retention** — `activity.history` is bounded by JSONL retention/rotation; a retention/compaction policy is a separate follow-up, not part of Phase 1.
