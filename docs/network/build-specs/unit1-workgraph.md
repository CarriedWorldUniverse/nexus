# M1 Unit 1 — Work-Graph adapter on ledger (build spec)

**Goal:** a nexus package that persists the orchestrator's work graph as **ledger issues on the sovereign ledger** (ledger.cwb.svc:8081 → the robo-dog ledger-rd). This is the foundation units 5/6 build on. Standalone-testable: no orchestrator needed yet.

## Why ledger (audit-confirmed)
ledger already implements the dependency engine — `issue_links type=blocks` + `ListReadyIssues` = "ready when all deps done"; atomic `ClaimIssue` = pool-lease; workflow states + required DoD on Done. Unit 1 is a thin ADAPTER over these RPCs (`CreateIssue/GetIssue/UpdateIssue/TransitionIssue/AddLink/ListLinks/ListReadyIssues/ClaimIssue/CommentIssue`), NOT new tracker logic.

## work_item ↔ ledger Issue mapping (per handoff.schema.json)
| work_item field | ledger representation |
|---|---|
| id | issue key |
| role | label (ledger skills/label tags, v14) |
| task_spec | issue description |
| acceptance_criteria[] | issue DoD field (ledger enforces DoD on Done transition) |
| depends_on[] | `issue_links` type=blocks (blocker → this) |
| status | workflow state — map: queued→"To Do", ready→"Ready to Start", dispatched/running→"In Progress", done→"Done", rejected→(rework: new issue + link), blocked→"Blocked", cancelled→"Cancelled" |
| stream_id | parent_key (epic) — a stream is an epic subtree |
| handoff (full blob) | a structured JSON comment tagged `cwb:handoff` (no first-class column — convention) |
| result (full blob) | a structured JSON comment tagged `cwb:result` |
| origin / personality / base_knowledge / prior_results | inside the handoff/result blobs |

Runtime-only fields (dispatched/running, pool lease) stay in nexus/runs — do NOT push transient state to ledger; ledger holds the durable graph.

## API surface (Go package `nexus/workgraph`)
A gRPC client to the sovereign ledger (dial ledger.cwb.svc:8081 with the cwb mesh client cert — mirror how `cwbproxy`/nexus's existing ledger client auth works; reuse `nexus/ledger` client or cwb-client if present). Methods:
- `CreateWorkItem(ctx, WorkItem) (id, error)` — CreateIssue + labels + DoD + link depends_on
- `GetWorkItem(ctx, id) (WorkItem, error)` — GetIssue + ListLinks + fold the cwb:handoff/result comments
- `ListReady(ctx, stream?) ([]WorkItem, error)` — ListReadyIssues (deps satisfied)
- `Transition(ctx, id, status) error` — TransitionIssue with the state map
- `RecordResult(ctx, id, result) error` — CommentIssue tagged cwb:result + transition (done/rejected/blocked)
- `Rework(ctx, rejectedID, newSpec) (newID, error)` — new work_item with a back-edge link + prior_results carrying the rejecting result
- `Claim(ctx, id, agent) error` — ClaimIssue (atomic; surfaces ErrAlreadyClaimed)
- `Cancel(ctx, id, requeue bool, reason) error` — **§2.1**: requeue=true → transition back to "To Do" (queued) + append reason to prior_results; requeue=false → "Cancelled" + dependents surfaced as blocked

## Prereq the builder must handle
The sovereign ledger is FRESH (empty — no orgs/projects). Bootstrap: create (idempotently) an org + a project (e.g. org "carriedworld", project "NET" for the work-graph) if absent, so issues have a home. Make it a `EnsureProject` helper the adapter calls, or a one-time seed documented in the package README.

## Constraints / conventions
- cairn line off nexus main: `builder/m1-unit1-workgraph`. Commit with `cairn commit`, not git.
- Follow existing nexus package conventions (look at `nexus/runs`, `nexus/ledger` client for the mTLS dial + error-wrapping style).
- Contract: tool/RPC failures → wrapped errors the caller handles; don't panic.
- TDD: table tests. Where possible test against a fake ledger (mirror `internal/grpcapi/grpcapi_test.go`'s fake). ALSO provide one env-gated live e2e (`WORKGRAPH_E2E_LEDGER=ledger.cwb.svc:8081`, skip without) that: creates 2 work-items A→B (B blocks-on A), asserts ListReady returns only A, transitions A done, asserts B now ready, cancels B with requeue, asserts B back to queued. Mirror the bridle e2e discipline.

## Acceptance criteria
1. `go build ./...` + `go vet` clean; `cairn commit` on the line.
2. Fake-ledger table tests pass for every method incl. the cancel/requeue + rework back-edge paths.
3. The live e2e (env-gated) passes against the sovereign ledger — create→link→ListReady→transition→ready→cancel-requeue verified with real RPCs.
4. A package README documenting the work_item↔Issue mapping + the EnsureProject bootstrap.
5. No transient/runtime state written to ledger (durable graph only).

## Build log — verify gate caught 7 real bugs the fake tests missed (2026-07-05)
Builder delivered unit-1: 12 fake-ledger tests green, clean build. But the **live e2e against the real sovereign ledger** (which fakes can't exercise) surfaced 7 real-ledger-API mismatches the fakes passed right over:
1. adapter attached NO `cwb-scopes` → every authorized RPC failed (fakes don't enforce scope). Fixed: Scopes field default `issue:admin`, asserted on the mesh-direct path.
2. `isNotFound`/`isAlreadyExists` matched clean gRPC codes, but ledger returns `codes.Internal` + message ("not found" / "UNIQUE constraint failed"). Fixed: match by message too.
3. fresh-ledger org bootstrap needs CreateOrg → CreateUser(kind="ai", NOT "agent") → AddMember(role="owner"), membership ensured ALWAYS not only on first create.
4. issue Type must be "Task" (capitalized; ledger wants Epic|Story|Task|Subtask|Bug).
5. adminClient interface widened for CreateUser/AddMember.
6-7. (remaining, handed back) ledger `ListReadyIssues` is ASPECT-ASSIGNED (`WHERE assignee_aspect = caller AND status IN (To Do,In Progress) AND DoR AND NOT blocked`) — pool's role-based "claimable by any" must map onto this (CreateWorkItem sets assignee_aspect=role; workers query as their role + ClaimIssue). Plus the fakes must be upgraded to ENFORCE these behaviors so they stop giving false greens.

**LESSON (load-bearing for the whole network):** the verify gate against the REAL dependency is non-negotiable — fake-only tests gave a 100% false green while the real ledger rejected every single call. This is the ROLE-MODEL's mandatory verify/gate design proving itself on the first real build. Bakes into every unit: builder self-verify is not enough; an env-gated live e2e against the actual pillar is required before a unit passes.
