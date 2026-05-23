# Orchestration Redesign — Design Spec

**Date:** 2026-05-24
**Status:** Draft — closes the brainstorming pass; ready for amendment to NEX-176 / NEX-137 + new tickets for `!decide` primitive + dashboard decision queue surface.

## Goal

Make nexus auto-complete epic → story → task → subtask hierarchies without operator babysitting, surfacing the operator only when a *decision* requires judgment. Replace the current "shadow manually dispatches, operator monitors" pattern with a two-layer orchestration: a routine scheduler that handles mechanical dispatch flow, plus keel's Frame as the supervisor that handles exceptions and judgment-required decisions.

## Why

Nexus's lineage: VSC AI addons (one tool per editor) → terminal multiplexing (one session per task, you switch between) → nexus (one substrate, work flows itself, operator surfaces only for decisions). The current state still sits at "operator must monitor" — workflows feel forced, brittle, and stall when aspects don't proactively pick up unblocked work. Concretely:

- Coordination steps are imperative — aspects must remember update-ticket, post-chat, ping-next-aspect; drop a step and the work stalls invisibly
- State is split across ticket store + chat + presence; operator has to merge three views
- No automatic continuation — when aspect A finishes, B doesn't auto-pickup; A has to remember to hand off
- No staleness detection — quiet aspects look the same as working aspects
- Operator becomes orchestrator-of-last-resort, filling gaps the system should fill

## Existing groundwork (this design amends + composes with)

Four existing specs cover the ground this design builds on:

- **NEX-176** "Keel (Frame) as queue manager" (2026-05-17) — original queue-manager spec; pull-dispatch-watch-merge-transition cycle, brief template engine, stuck detection, operator pause/resume. **This spec amends NEX-176 with the two-layer split (scheduler subsystem + supervisor turns) + team-aware routing + `!dispatch` integration.**
- **NEX-137** "Native issue management — Jira replacement" (2026-05-17, Epic) — ledger spec. Full ticket model, MCP tool surface (`issue_create / get / update / transition / assign / link / comment / attach / search / watch / list_my / list_ready`), migration plan from Jira. **This spec amends NEX-137 with: `ready_to_start` status, project-scoped team schema, `decisions` table, `audit_event` table, team CRUD MCP tools.**
- **NEX-138** "Autonomous run-loop primitive" (2026-05-17, Epic) — worker-side autonomous loop (`anvil: work the oss-go backlog until empty`). Distinct from this spec's *coordinator-side* loop. The two are complementary; NEX-138's primitives could be used by scheduler-dispatched workers.
- **NEX-259** "Broker-owned aspect status — presence + busy/idle for dispatch scheduling" (2026-05-23) — presence spec. Four states: `online / busy / idle / offline`. **Load-bearing input for this design's scheduler routing decisions.**

Plus the **comms-actions plan** (`docs/2026-05-17-comms-protocol-actions-plan.md`, branch `docs/comms-protocol-actions-plan`, commit `cb3bd58`, never pushed) — the `!dispatch` chat-parser action infrastructure. This spec assumes that plan ships as-is for the parser + action-registry primitives; adds `!decide` + `!answer` as additional actions in the same family.

## 1. Architecture overview

Two existing systems + a new orchestration loop in the middle.

**ledger** (NEX-137) = pure persistence + Jira-equivalent hierarchy (epic / story / task / subtask). Stores ticket state, assignee (aspect-id OR team-name), status, parent/child links, dependencies. Emits change events. Does not run workflow rules itself.

**nexus broker** = hosts the orchestration loop (broker process owns it; the scheduler subsystem is new code inside nexus.exe; keel's Frame is the existing in-process Frame, now wearing a supervisor role).

**aspects** = first-class chat participants (unchanged). Receive `!dispatch` with ticket context; work the task; update ticket state via MCP tools; close the ticket when done.

The "operator-only-for-decisions" property comes from two pieces:
1. The orchestration loop keeps work flowing without operator intervention
2. A new `!decide` action (Section 7) lets aspects pause work and surface questions

## 2. Teams + assignment model

**Teams are first-class, project-scoped, operator-curated, persist in ledger.** Schema: `team { id, project_id, name }` + `team_member { team_id, aspect_id }`. The aspect must already be a project member (`project_member` row). Same team name in different projects = different teams. Cross-project teams not supported v1.

Assignment field on a ticket: `assignee = "keel"` (individual) or `assignee = "@backend-team"` (team) — same column, prefix convention disambiguates. Keeping teams in ledger means assignment is queryable by the same database the rest of the ticket lives in.

**Team CRUD surface (v1):** chat-based admin commands (`!team create backend-team keel anvil`, `!team add backend-team plumb`) since admin-via-chat matches the comms model. Dashboard config panel can layer later.

**Pick mechanic for team-assigned tickets:**
- Read team members from ledger (within the ticket's project)
- Filter to idle members (`aspectActivity` state=`idle` AND presence=`online`, per NEX-259)
- Round-robin within the idle subset; cursor persists in ledger per-team (survives broker restarts)
- If no team members idle, ticket sits at `ready_to_start`; scheduler tracks "queued for team-X" in memory; re-evaluates on next aspect-becomes-idle event

**Individual-assigned tickets** skip round-robin but still go through the idle gate. If the specific aspect is busy, ticket queues; no work-stealing (assignment is a commitment to a specific aspect).

## 3. Ticket lifecycle + `ready_to_start`

**Statuses (amendment to NEX-137):**
- `open` — created; not started; may not be ready
- `ready_to_start` — **NEW** — assigned + unblocked; waiting for dispatch. Explicit status so the scheduler subscribes to `status_changed → ready_to_start` events directly rather than computing ready-ness on every event. Queryable as `list_ready`.
- `in_progress` — an aspect is actively working it
- `blocked` — explicit operator/aspect mark; needs intervention (including `blocked awaiting_decision`)
- `in_review` — work done, pending review (optional state)
- `done` — completed, accepted
- `cancelled` — won't do

**Promotion rules (broker maintains, fired on ledger events):**
- On `ticket.assigned` (or `reassigned`): if no blocking-by deps in non-terminal state, promote `open → ready_to_start`
- On any `ticket.closed`: for each blockee, re-check; promote if all blockers now terminal
- `queued for team-X` (no idle members) is a runtime concept, not a ticket status — ticket sits at `ready_to_start`, scheduler holds the wait

**Dependencies:**
- **Hierarchy** (parent-child): epic → story → task → subtask. Children don't block siblings; only explicit edges block.
- **Edges** (`blocks` / `blocked-by`): explicit links between any two tickets. `blocked-by` means "this can't start until linked ticket reaches `done`."

**Who transitions what:**
- Assigned aspect transitions `ready_to_start → in_progress` on dispatch (usually auto via dispatch path)
- Assigned aspect transitions `in_progress → done` on completion
- Assigned aspect can transition to `blocked` on hitting a wall
- Operator can override any transition

## 4. Dispatch mechanic

**`!dispatch` is a chat-parser action** (comms-actions plan, refined in `cb3bd58`). Two invocation shapes:

- **Ticket-preferred:** `!dispatch <ticket-ref>` — resolves to a ledger ticket; pulls description, acceptance criteria, links, originating thread; dispatches fresh-context worker against that context.
- **Inline-text fallback:** `!dispatch <freeform text>` — auto-creates a ledger ticket from the inline text; ticket-preferred remains encouraged because it captures intent up-front.

**Dispatch flow (single end-to-end):**
1. Parse the action from chat content
2. Resolve ticket (lookup or auto-create)
3. Pick dispatchee (individual or team round-robin-among-idle)
4. If no dispatchee idle: leave ticket in `ready_to_start`, track "queued for X", return
5. Spawn fresh-context worker via existing hand-dispatch infra (`KindDispatch` family). Worker's system prompt is **identity-framed**: dispatchee's NEXUS.md + SOUL.md + PRIMER composed in (Phase 4 of the May 17 plan, still load-bearing).
6. Auto-transition ticket `ready_to_start → in_progress`, set `current_dispatch_thread = <originating-thread-id>` and `current_worker = <ephemeral-worker-id>`
7. Emit `KindDispatchAccepted` audit frame on the originating thread

**Auto-dispatch (no chat action needed):** when the scheduler sees a ticket hit `ready_to_start` via ledger event (not via a chat-typed `!dispatch`), it runs the same flow from step 3 onward. The chat-typed entry point and the scheduler entry point share the same pipeline.

**Ticket-close:** worker transitions ticket to `done`; broker observes the closure; re-evaluates blockees per Section 3; fires next-in-chain dispatches.

## 5. Two-layer split — scheduler subsystem vs supervisor turns

**Routine scheduler subsystem** (new module inside nexus.exe, NOT keel; non-LLM, event-driven):
- Subscribes to: ledger events (`ticket.status_changed`, `ticket.assigned`, `ticket.closed`) + broker presence events (NEX-259 P2 `turn.start`/`turn.end` → aspect_status transitions)
- Holds in memory: per-team round-robin cursors; "queued for X" entries (ticket-id → assignee-or-team waiting on idle)
- On `ticket.status_changed → ready_to_start`: resolve assignee; dispatch or queue
- On aspect transitions to idle: check queued-for-X; fire oldest match
- On `ticket.closed`: re-check blockees per Section 3
- On stuck-condition (`in_progress` no events for 60 min default): emit `scheduler.stuck` event for keel
- State persistence: round-robin cursors in ledger; in-memory queued-for-X rebuilds on restart from `ready_to_start` + presence

**Keel's supervisor turn** (LLM aspect, event-driven wake, NOT continuous tick):
- Wakes on: `scheduler.stuck` events; `!decide` escalations from workers (Section 7); operator chat commands (`keel, pause` / `skip nex-N` / `resume`); routing-ambiguity events the scheduler flags
- Per turn: reads event + ticket context; decides action; emits tool call (re-dispatch / escalate / pause / reorder)
- Doesn't poll: events bring work to keel; sleeps between events with no token cost

**Why two-layer:** an LLM aspect is inherently reactive ("lazy") — it won't, on its own initiative, scan the ready queue between turns and decide to dispatch. NEX-176 calls keel "the queue manager" but if that implementation is itself an LLM tick-loop, it burns tokens continuously on a question that's mostly answered the same way ("no new work"). Split: routine flow goes through a dumb scheduler (mechanical, cheap, fast); judgment goes through keel (LLM, slow, smart, only fires when needed).

**Cross-layer contract:** scheduler is the routine actuator; keel is the judgment layer; operator sees only what keel chooses to escalate via the decision queue (Section 7). The "operator-only-for-decisions" promise is delivered by this contract.

## 6. Independent observation layer (audit principle)

**Principle:** the orchestration pipeline (scheduler + keel + workers) is the system being executed. Anything that watches it for correctness must be **outside** that pipeline. A system that monitors itself misses exactly the failures that disable its monitoring.

**v1 instantiation:**

| Observer | Role | Independence |
|---|---|---|
| **Operator** (with tooling) | Primary third party. Reviews decisions, spot-checks stuck work, sanity-checks drift. | Different lifecycle; dashboard surface; doesn't depend on scheduler/keel health to function. |
| **Audit event stream** | Every scheduler action + keel turn + worker dispatch emits a structured audit event (timestamp, actor, decision, context-ref). Stored in ledger `audit_event` table, append-only. | Captured at emit-time; consumers (dashboard, future auditor, metrics) decoupled from runtime. |
| **External metrics surface** | Counters + gauges: tickets-in-each-status, dispatches/hour, stuck-event-rate, decision-queue-depth, time-to-decision. Exposed via existing observability hub. | Read by external monitoring tool; decoupled from pipeline correctness. |

**Out of scope for v1 (architecture leaves room for):**
- **Auditor aspect** — dedicated aspect that reads ledger + audit log + presence, looks for anomalies, surfaces findings. v2 — can read the v1 audit log once it exists.
- **Adversarial probe aspect** — files synthetic edge-case tickets to verify orchestration handles them. v3 — only when system is stable enough that probing is the real risk.

**What this adds to v1:**
- `audit_event` table in ledger schema (small addition to NEX-137: `{actor, action, target_ticket, payload, timestamp}`)
- Scheduler + keel + dispatch each emit audit events at every decision point (success AND skip — e.g. "didn't dispatch because nobody idle")
- Dashboard reads from audit-log + ledger, not scheduler in-memory state — surfaces what actually happened, not what scheduler thought happened

## 7. `!decide` decision primitive + operator decision queue

**`!decide` is a chat-parser action** (same family as `!dispatch`). Two invocation shapes:
- **Open question:** `!decide should we deprecate the legacy auth path?` — free-form, unrestricted response shape
- **Multiple-choice:** `!decide [option-a / option-b / option-c] which renderer should the dashboard pick?` — square-bracket prefix declares options; operator's response validated against them

Both shapes emitted by a worker mid-task (or by keel during a judgment turn). No special tool needed — it's still chat.

**Flow when `!decide` fires:**
1. Parser intercepts; looks up worker's current ticket via thread→ticket link
2. Ticket transitions `in_progress → blocked` with reason=`awaiting_decision`
3. New row in ledger `decisions` table: `{id, ticket_id, asker_aspect, question, options?, fired_at}`
4. `decision.created` audit event emitted
5. Decision appears in operator's decision queue (dashboard, Section 8)
6. Worker dispatch concludes its turn — worker is NOT held captive. Sleeps until decision resolves.

**Operator response (v1 — chat-typed, matches comms-substrate principle):**
- `!answer <decision-id> <response>` posted in the originating thread
- Parser captures; validates against options if multi-choice; rejects with helpful error if invalid
- Records `{decision_id, answer, answered_by, answered_at}` in ledger
- Emits `decision.resolved` audit event
- Ticket transitions `blocked → ready_to_start` (scheduler re-dispatches per normal flow)
- Re-dispatched worker gets fresh context with original question + answer injected

**Multiple aspects on related decisions (v1):** each `!decide` independent. No coalescing logic. Operator can answer similarly or differently.

**Decision timeout:** decision sits in queue indefinitely. Scheduler emits `decision.stale` audit event after N hours (default 24h, configurable); keel's supervisor turn surfaces it via chat ("operator — decision #45 from anvil has been waiting 24h, still relevant?"). Operator answers or marks moot.

**Audit trail:** every decision (question + options + answer + ticket + asker + answerer + timestamps) lives in ledger as first-class record.

## 8. Trust surface integration

The Feed redesign (PRs #116-120) gave thread sidebar + per-thread presence strip + autoscroll + since-you-left + persistence. Four additions land the orchestration surfaces.

**1. Decisions — new first-class view + sidebar badge**
- New navigation entry alongside Status/Chat/Feed/Observe: **"Decisions"**
- List of pending `decisions` rows: question + asker + ticket-link + age + multi-choice options
- Click → expanded view with full ticket context, thread snippet, inline answer form (form posts `!answer <id> <response>` to originating thread)
- Sidebar badge — count of pending decisions appears on Decisions nav entry; mirrored in Feed sidebar top region

**2. Ticket-aware thread enrichment**
- Each thread tied to a ticket (via dispatch link captured at spawn) gets a header bar: `NEX-247 · in_progress · @keel-1 (worker)`
- Hover for parent/epic links
- When ticket is `blocked awaiting_decision`, header shows `awaiting decision #45` with link
- When `in_progress` past staleness threshold, header shows amber "stuck" badge
- v1 ticket-detail link goes to existing jira/ledger URL (dashboard ticket-detail view deferred)

**3. Activity strip extensions** (additions to PR #118's `PresenceStrip`)
- New pill substate: `queued for me` — busy aspect with `ready_to_start` tickets waiting. Renders as `+N` badge ("@anvil • thinking +3")
- Pill tooltip: in-flight count, queued count, current tool
- Same vocabulary applies to sidebar dots — team row with all members busy + tickets queued shows `+N` overlay

**4. Scheduler health indicator**
- Top of sidebar thin status bar — green when audit-event stream is active, amber on "no scheduler activity in M minutes", red if audit subscription dropped
- Mirrors Section 6 principle — reads audit log, not scheduler's in-memory state

**State additions:**
- Decisions list: ledger `decisions` table (Section 7)
- Ticket-aware thread enrichment: new field on chat threads — `current_ticket_id` (nullable) — populated at dispatch time
- Activity strip extensions: derived from `aspectActivity` + ledger queries
- Scheduler health: derived from audit-log timestamps

**Out of scope for v1:** ticket-detail view in dashboard, audit log browser, team management UI, decision analytics. All data captured; UIs come later.

## 9. Failure modes + error handling

| Failure | Detection | Recovery |
|---|---|---|
| Scheduler misses a ledger event (network blip, restart mid-event) | Periodic reconciliation sweep — every N min query ledger for `ready_to_start` tickets; compare to in-memory queued-for-X | Re-evaluate from scratch — dispatch ready+idle, queue rest. Idempotent (status is in ledger). |
| Worker crashes mid-task | Presence transitions to `offline` AND ticket stuck past staleness threshold | Scheduler emits `ticket.stuck` event; keel decides — re-dispatch, escalate, or block. No auto-retry without judgment. |
| Dispatch race (two events near-simultaneous, same idle aspect) | Atomic aspect_status check just before spawn; abort if state changed | Re-queue. Optimistic check + retry; benign failure mode. |
| Operator double-answers a decision | `answered_at` already set | Reject with helpful error; audit-emit the rejected attempt. Don't silently overwrite. |
| Operator's `!answer` references wrong decision/thread | Lookup fails or thread mismatch | Parser-level error; post back as chat reply; don't fail silently. |
| Circular dependency cascade (A closes unblocks B unblocks A) | Re-evaluation hop counter; halt if same ticket re-evaluated > N times in one batch | Surface to keel; ledger doesn't enforce DAG-ness so broker must be defensive. |
| Audit-event-stream consumer down | Gap in events received | Dashboard renders scheduler health amber/red. Doesn't affect scheduler itself. |
| Keel's supervisor turn errors/hangs | Per-turn deadline timer | Re-queue event for next turn. After N consecutive failures, scheduler escalates directly to operator (`scheduler.escalation`) bypassing keel. |
| Operator chat command targets unknown ticket/aspect | Keel's supervisor turn validates | Keel posts chat reply with error; no state change. |

**Common pattern:** failures surface via audit stream + ultimately to operator. System never silently swallows a failure — third-party-observer principle (Section 6) demands.

## 10. Testing strategy

**Unit tests (per-component):**
- Scheduler subsystem: ledger event → routing decision; round-robin advancement; skip-busy; reconciliation idempotency
- Parser actions: strengthened cases from `cb3bd58` cover `!dispatch`; add `!decide` + `!answer` (multi-choice validation, double-answer rejection, wrong-thread rejection)
- Decision queue lifecycle: create → pending → answered → resolved (each transition emits correct audit event)
- Team resolution: project-scoped members → filter-by-idle → round-robin pick

**Integration tests (real ledger + broker):**
- End-to-end ticket flow: file → assign → `ready_to_start` → scheduler dispatch → fake worker closes → next-in-chain fires. Validate audit trail per Section 6 principle.
- Team flow: same but team-assigned; verify round-robin + skip-busy
- Decision flow: worker `!decide` → ticket `blocked` → operator `!answer` → ticket `ready_to_start` → re-dispatch with answer in context
- Cross-layer escalation: trigger `scheduler.stuck` → keel supervisor fires → keel's action applied

**Failure injection (one per Section 9 mode):**
- Drop ledger event → reconciliation recovers
- Kill worker mid-task → presence offline + stuck event after threshold
- Two events for same aspect simultaneously → optimistic check + retry; no double-dispatch
- Circular dependency → hop-counter halt + keel escalation

**Headline smoke test:**
- File one epic with 3 sequential stories, each with 1 task; assign each to specific aspects/teams
- Start orchestration; do nothing else
- Verify: tickets advance to `done` in order, all dispatches unattended, audit log shows full trail, no operator decisions required
- Time-budget regression signal — completes within N minutes for trivial tickets

**Test infrastructure additions:**
- Stub aspect for tests — speaks dispatch protocol, transitions tickets via MCP, optionally injects failures
- Audit-log inspection helpers — `expect_audit_events([...])` with field-level matching
- Time-control hooks — fast-forward `time.Now` past staleness thresholds
- Ledger test fixture — in-memory or isolated-per-test project

**Coverage threshold for shipping:** every Section 9 failure mode has at least one injection test; headline smoke is green; per-component units cover routing/decision/parser logic. Audit-trail validation in every integration test.

## Deliverable framing (jira mapping)

| Work item | Maps to |
|---|---|
| Amendment: two-layer split, team-aware routing, `!dispatch` integration | **NEX-176** (queue manager) — comment with this spec as the implementation guide |
| Amendment: `ready_to_start` status, team schema (project-scoped), `decisions` table, `audit_event` table, team CRUD MCP tools | **NEX-137** (ledger) — comment with schema additions |
| Confirmation: this design consumes NEX-259's aspect_status as routing input; P4 MCP tools needed for keel's supervisor query | **NEX-259** (presence) — comment with consumer detail |
| Note: complementary to coordinator-side loop here; NEX-138 primitives could be used by scheduler-dispatched workers | **NEX-138** (worker autonomous loop) — comment |
| **NEW STORY**: `!decide` decision primitive + operator chat-level `!answer` flow | Create new story, link to NEX-176 + NEX-137 |
| **NEW STORY**: Dashboard decision queue surface + ticket-aware thread enrichment + queued-for-me badge + scheduler health indicator | Create new story, link to NEX-176 + the new `!decide` story |

## Open questions / out of scope

**Open (acceptable to defer to implementation time):**
- Ledger event-stream mechanism (webhook vs WS vs queue) — confirm during NEX-137 implementation
- Decision timeout default (24h proposed) — tunable; default refined after dogfooding
- Reconciliation sweep cadence (every N min) — tunable; default refined after dogfooding
- Team CRUD chat command syntax — proposed `!team create/add/remove`; refine if conflicts with other action names

**Explicitly out of scope:**
- Worker-pool model (Approach A from brainstorming) — pre-assigned model is v1
- Split-view dashboard (multiple focused threads side-by-side) — single focused thread v1
- Auditor aspect — v2 (operator + audit log is sufficient v1)
- Adversarial probe aspect — v3
- Dashboard ticket-detail view — use jira/ledger URLs v1
- openclaw / hermes interoperability — explicitly out of scope per operator
