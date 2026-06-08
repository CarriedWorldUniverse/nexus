# Work UI — Phase 2: Control Actions (Design)

**Date:** 2026-06-09
**Status:** Design — pending review
**Phase:** 2 of 5. Builds on Phase 1 (Watch + run spine, live + deployed). Adds the "act" half of mission-control.

## Goal

Let the operator act on the dispatch-native world from the UI — dispatch a new run, cancel/stop a run, reply into a run's thread, and configure an agent — with everything still doable via shadow/CLI (the UI is a parallel surface, not the only path).

## Scope

**In scope (Phase 2):** the four control actions.
1. Dispatch a new run (compose form → post `!dispatch`).
2. Cancel/stop a run (graceful + force).
3. Reply into a thread.
4. Configure an agent (fuller — provider/model, judge/summarizer, personality, provider-binding, mcp, + a new dispatch-enabled flag).

**Out of scope:** Phase 3 (Converse as a first-class area), Phase 4 (the Configure *area* re-IA), Phase 5 (dedicated mobile). Phase 2's "configure an agent" is a **per-agent** editor opened from the Team panel that reuses existing admin endpoints; the top-level Configure area's IA is Phase 4.

## Architecture

Control actions layered over the Phase 1 run spine. Only **two genuinely new backend pieces** — `run.cancel` (+ `K8s.DeleteJob`) and the per-agent `dispatch-enabled` flag; everything else reuses existing paths.

Reused paths (confirmed in the codebase):
- **`!dispatch` intercept** (`broker/chat_send.go` → `dispatch_intercept.go`) — dispatch-from-UI just posts a `!dispatch` chat message via the existing operator chat-send.
- **`aspect.say`** (`KindAspectSay` / `handleOperatorAspectSay`, `AspectSayPayload`) — reply-into-thread.
- **Admin per-aspect config endpoints** — `/api/admin/aspects/{name}/model-config` (provider/model + judge/summarizer), `/api/admin/aspect/{name}/personality`, `/api/admin/aspects/{name}/provider-binding`, `/api/admin/aspects/{name}/mcp_profile`, `/api/admin/roster`. These already back `SettingsAspects.js`.
- **agentfunnel `SIGTERM` handling** (`signal.NotifyContext(…, syscall.SIGTERM)`, main.go:319) → ctx cancel → `wsClient.Run` returns → per-agent-home `cleanDespawn` (home merge) → exit. Graceful cancel rides this; no agentfunnel change.
- **NEX-528 Part B** — a deleted Job emits a terminal `JobDone` → the runner frees the agent.

### 1. Dispatch a new run

A compose panel (agent, repo, ticket, brief; agent defaults from the roster, provider optional) builds a `!dispatch <agent>%<provider> repo=… ticket=… <brief>` line and posts it as an operator chat message (the existing send path the dashboard already uses). The broker intercept handles it exactly as a CLI dispatch; the run appears in the Watch feed via the Phase 1 `runs.update` push. **No new backend.**

### 2. Cancel/stop a run

New operator RPC `run.cancel {run_id, force}`:
- Resolve the Job by the `nexus.dispatch/run-id=<run_id>` label (the Job carries it — jobspec.go:53).
- New `K8s.DeleteJob(ctx, name, gracePeriod)`:
  - **graceful** (`force:false`) → delete with a grace window (e.g. 30s) + `PropagationPolicy=Foreground` → `SIGTERM` to the pod → agentfunnel ctx-cancels → `cleanDespawn` (home merges) → exits before `SIGKILL`.
  - **force** (`force:true`) → `gracePeriodSeconds=0` → immediate `SIGKILL`.
- Either way the Job deletion fires NEX-528's `emitJobDeleted` → terminal `JobDone(OK=false)` → the runner frees the agent and marks the run `cancelled` (a `RunsRecorder.RecordRunDone(..., "cancelled", …)` mapping, so the run shows `cancelled` in the feed).
- Returns `{ok, message}`. Unknown/already-finished run → benign result, not an error.

UI: a **Stop** and **Force-kill** affordance on the run card / timeline header for an active run, **Force-kill behind a confirm** (and Stop with a light confirm). Read-only runs (already complete) show neither.

### 3. Reply into a thread

A reply box at the foot of the unified timeline (for the selected run). Sends `aspect.say {thread, content}` (reuse `AspectSayPayload`) into the run's thread — reaching the builder's inbox (the builder subscribes to its thread). The reply appears in the timeline via the existing chat push. **UI + existing path.**

### 4. Configure an agent (fuller)

The Team panel gains a per-agent **Configure** affordance opening a per-agent config editor that reuses the existing admin endpoints (the same ones `SettingsAspects.js` uses): provider/model + judge/summarizer (`model-config`), personality (`/aspect/{name}/personality`), provider-binding, mcp_profile. Plus **one new piece**: a per-agent **`dispatch-enabled`** flag.
- New: store the flag (per-aspect config — extend the aspects store / a small admin endpoint `PUT /api/admin/aspects/{name}/dispatch-enabled`), default true for the current on-demand devs, surfaced in the roster.
- The `!dispatch` intercept (`dispatch_intercept.go`) checks the flag before `runner.Submit`; a disabled agent's dispatch is rejected with a clear chat reply ("agent X is dispatch-disabled").
- UI: a toggle in the Team-panel per-agent editor; the Team roster shows the dispatch-enabled state.

## Data flow

- **Dispatch:** UI form → operator `chat.send` (`!dispatch …`) → intercept → runner → `runs.update` push → feed (Phase 1).
- **Cancel:** UI button → `run.cancel` RPC → `K8s.DeleteJob` → (NEX-528) `JobDone` → runner frees agent + `RecordRunDone(cancelled)` → `runs.update` push flips the card to `cancelled`.
- **Reply:** UI reply box → `aspect.say` → thread (builder inbox) → chat push → timeline.
- **Configure:** UI editor → existing admin endpoints (GET to load, PUT to save) + the new `dispatch-enabled` endpoint → roster reflects it.

## Error handling / safety

- All four are **write actions**; gated on operator auth (the existing operator/admin bearer — same gate as `aspect.say`/admin endpoints). No new auth model.
- **Cancel confirm:** Force-kill requires an explicit confirm; Stop a light confirm (it abandons in-flight work). The RPC is idempotent — cancelling an already-finished run is a no-op success.
- **Dispatch validation:** the compose form validates agent ∈ roster + required fields before posting; a dispatch-disabled agent is rejected by the intercept with a chat reply (the UI surfaces it).
- **DeleteJob races:** if the Job is already gone (completed/TTL'd) when cancel fires, treat as success (the run already ended).
- **Config save failures:** surface the admin endpoint's error inline; don't leave the editor in a half-saved state (load-then-save per field group, mirroring SettingsAspects).

## Testing

- **run.cancel:** unit-test Job resolution by `run-id` label + `K8s.DeleteJob` grace-period selection (fake clientset, k8s_test.go pattern); assert graceful uses a non-zero grace + force uses 0; assert idempotent on a missing Job. Assert `RecordRunDone(cancelled)` is invoked.
- **dispatch-enabled gate:** unit-test the intercept rejects a disabled agent and admits an enabled one.
- **dispatch-from-UI:** the composed `!dispatch` string round-trips through the existing parser (table test the line builder against the parser).
- **Frontend:** compose form posts the right `!dispatch` line; cancel buttons call `run.cancel` with the right `force`; reply posts `aspect.say`; the per-agent editor loads + saves each field group + toggles dispatch-enabled. Manual + existing harness (JS not in CI).

## File structure

**Backend (Go):**
- Modify `runtime/dispatch/k8s.go` — add `DeleteJob(ctx, name string, gracePeriodSecs *int64)`.
- Create `nexus/broker/run_cancel_rpc.go` — `run.cancel` handler (resolve Job by label, delete, map run → cancelled); register in `dispatchOperatorFrame`.
- Modify `nexus/frames/frames.go`/`payloads.go` — `KindRunCancel`/`KindRunCancelResult` + payloads.
- Modify `runtime/dispatch/runner.go` (+ `runs_recorder.go`) — a cancel→`RecordRunDone("cancelled")` mapping (or reuse OnJobDone with a cancelled marker).
- Modify `nexus/broker/dispatch_intercept.go` — check the `dispatch-enabled` flag before Submit.
- Create/modify the aspects store + a small `nexus/broker/admin_dispatch_enabled.go` — the per-agent `dispatch-enabled` flag endpoint + roster surfacing.

**Frontend (`broker/static/dashboard/`):**
- Create `js/views/panels/DispatchComposePanel.js` — the compose form.
- Modify `js/views/WatchView.js` — Stop/Force buttons on the run card/timeline + the reply box; wire `run.cancel` + `aspect.say`.
- Modify `js/views/panels/TeamPanel.js` — per-agent Configure affordance + dispatch-enabled toggle.
- Create `js/views/panels/AgentConfigPanel.js` — the per-agent config editor (reusing the admin endpoints).
- Modify `js/api.js` — `runCancel`, `dispatchCompose` (post `!dispatch`), `replyToThread` (`aspect.say`), the per-agent config GET/PUT wrappers.

## Decomposition (3 tickets)

- **2a — Cancel:** `run.cancel` RPC + `K8s.DeleteJob` (graceful/force) + run→`cancelled` mapping + UI Stop/Force buttons + confirm. The real new backend.
- **2b — Dispatch-compose + Reply:** the compose form (post `!dispatch`) + the timeline reply box (`aspect.say`) + their `api.js` wrappers. UI over existing paths.
- **2c — Fuller agent config:** the Team-panel per-agent config editor (reusing admin endpoints) + the new `dispatch-enabled` flag (store + endpoint + intercept gating + roster surfacing).

Order: 2a first (new backend), then 2b (quick wins), then 2c (the broadest). 2a and 2b/2c are largely independent (different files) and can parallelize; 2c's flag touches the intercept which 2b's dispatch-compose also relates to — sequence 2c after 2b to avoid intercept churn.

## Open questions / decisions

1. **Cancelled-run home merge on graceful stop** — graceful `SIGTERM` lets `cleanDespawn` merge the home. For a *cancelled* run we may not want its partial work merged into the agent's persistent home. Decision: graceful cancel still merges (the home is the agent's memory, and a clean wind-down is the point of "graceful"); force-kill skips it (SIGKILL, no merge). Revisit if cancelled-run memory proves noisy.
2. **dispatch-enabled default + roster policy** — default `true` for the current on-demand devs (anvil/plumb/harrow); the flag makes the existing routing policy explicit/storable without changing today's behaviour.
