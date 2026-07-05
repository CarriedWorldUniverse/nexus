# nexus/workgraph

A thin adapter that persists the orchestrator's work graph as **ledger
issues on the sovereign ledger** (`ledger.cwb.svc:8081`). It wraps existing
ledger gRPC RPCs — `CreateIssue`/`GetIssue`/`UpdateIssue`/`TransitionIssue`/
`AddLink`/`ListLinks`/`ListReadyIssues`/`ClaimIssue`/`CommentIssue` — it does
not implement new tracker logic. ledger already implements the dependency
engine (`issue_links type=blocks` + `ListReadyIssues` = "ready when all deps
done") and the pool-lease (`ClaimIssue` is atomic).

Runtime-only state (dispatched-vs-running, pool lease bookkeeping) stays in
`nexus/runs` — this package never writes transient state to the ledger, only
the durable graph.

## work_item <-> ledger Issue mapping

Per `docs/network/handoff.schema.json`'s `work_item`/`result` shapes, plus
the graph-only fields (`status`, `stream_id`, `depends_on`) the adapter needs
to place an item in the dependency graph:

| work_item field | ledger representation |
|---|---|
| `id` | issue key (`Issue.key`, assigned by `CreateIssue` — not settable) |
| `role` | **both** `Issue.skills[0]` (label, informational) **and** `Issue.assignee_aspect` (see "Role -> ready", below — this is what actually gates `ListReadyIssues` on the live ledger) |
| `task_spec` | `Issue.description` |
| `acceptance_criteria[]` | `Issue.definition_of_done`, newline-joined (ledger enforces DoD on the Done transition) |
| `depends_on[]` | `issue_links type=blocks`, blocker -> this (`AddLink(key=blocker, to_key=this)`) |
| `status` | workflow state, see the table below |
| `stream_id` | `Issue.parent_key` (epic) — a stream is an epic subtree |
| `handoff` (cairn_line/artifacts/base_knowledge/personality/origin) | a JSON comment tagged `cwb:handoff` (no first-class column) |
| `result` (full blob) | a JSON comment tagged `cwb:result` (one comment per result; `GetWorkItem` folds every `cwb:result` comment into `PriorResults`, in timeline order) |

`Issue.type` is always `"Task"` — ledger's live type vocabulary is
`Epic|Story|Task|Subtask|Bug` (capitalized, checked against the real
ledger's e2e; lowercase `"task"` is rejected).

### Role -> ready: the pool-model / ledger-aspect mismatch

The live ledger's `ListReadyIssues` is **aspect-assigned**, confirmed
against the real sovereign ledger: the query is `WHERE assignee_aspect =
<caller's cwb-subject> AND status IN ('To Do','In Progress') AND <DoR> AND
NOT <blocked by a non-terminal 'blocks' edge>`. `assignee_team` is wired in
the proto (`ListReadyIssuesRequest.aspect`/`skills`) but not honored
server-side today — only the calling identity's `cwb-subject` matters.

That's a mismatch with the pool model this adapter serves: the pool wants
"ready for role X, claimable by any worker of that role" — not "ready for
one specific ledger identity". The mapping this adapter uses:

- `CreateWorkItem` sets `assignee_aspect = wi.Role` — the role is who the
  ledger thinks "owns" the issue.
- `ListReady(ctx, role, stream)` queries the ledger **as that role**: it
  overrides the outgoing `cwb-subject` to `role` for that one call (see
  `Client.ctxAs`), rather than presenting the adapter's own `Subject`. Any
  worker pool member of role X gets the same ready list by querying with
  `role = "X"`; `ClaimIssue` (atomic) is still what decides who actually
  gets a specific item.
- `role` is required — `ListReady(ctx, "", stream)` returns an error rather
  than silently querying as nobody (which the live ledger will just no-op on
  reads-as-blank-subject, no items).

### Status <-> ledger workflow state

| `workgraph.Status` | ledger workflow state |
|---|---|
| `queued` | `To Do` |
| `ready` | `Ready to Start` (see note below) |
| `dispatched`, `running` | `In Progress` |
| `done` | `Done` |
| `blocked` | `Blocked` |
| `cancelled` | `Cancelled` |
| `rejected` | *(none — see Rework, below)* |

**`ready` is never a `Transition` target the way the others are written by
`RecordResult`/`Cancel`** — an item's ledger status stays `To Do` until
something explicitly moves it (or a caller calls `Transition(ctx, id,
StatusReady)` themselves). `ListReady` (`ListReadyIssues`) is the actual
"ready when all deps done" computation; every `WorkItem` it returns has its
`Status` field forced to `StatusReady` regardless of what's stored on the
issue, because that's what ListReadyIssues membership *means*. `dispatched`
and `running` both fold to ledger's single `In Progress` state — the finer
runtime split belongs to `nexus/runs`, not the ledger.

`rejected` has no ledger workflow state at all: a reject is `RecordResult`
(records the `cwb:result` comment, leaves the issue's own status alone) plus
a separate `Rework` call that creates the follow-up work item. See below.

### The fixed workflow

The sovereign ledger's default workflow states are unknown/unverified from
this checkout (ledger's server source isn't in the nexus tree), so
`EnsureProject` explicitly sets the project's workflow (via
`SetProjectWorkflow`) to exactly the six states above, permissive
transitions between all of them **including every state to itself**, with
the DoD gate only on `Done` (per ledger's "required DoD on Done transition"
behavior). This makes the Status<->ledger-state fold exact regardless of
what the ledger would have defaulted to, and self-heals on every
`EnsureProject` call (idempotent — safe to call on every boot).

The self-loop (`X -> X`) is required, confirmed against the live ledger:
`Transition`/`Cancel` are meant to be idempotent-safe (e.g.
`Cancel(requeue=true)` on an item that's still `To Do` — never dispatched —
is a legitimate no-op-ish "requeue"), and the live ledger rejects a
same-state `TransitionIssue` call as "not allowed by workflow" unless the
workflow explicitly permits it.

### definition_of_done is a checklist, not free text

Confirmed against the live ledger: `TransitionIssue` to `Done` is rejected
("definition of done has unticked items") unless every line of
`definition_of_done` is a ticked markdown checkbox. `CreateWorkItem` writes
`acceptance_criteria` as an **unticked** checklist (`"- [ ] <criterion>"` per
line); `Transition(ctx, id, StatusDone)` ticks every line (`"- [x] "`) via
`UpdateIssue` immediately before the `TransitionIssue` call (see `tickDoD`).
`GetWorkItem` strips both markers when folding `definition_of_done` back to
`AcceptanceCriteria`, so round-tripping a work item never leaks the
checklist syntax. This adapter has no per-item DoD-completion API — reaching
`StatusDone` at all is the signal that the acceptance criteria were met
(verified upstream, e.g. by the builder/tester actually running the tests).

## EnsureProject

The sovereign ledger starts **fresh** — no orgs, no projects. `EnsureProject`
idempotently creates the org+project a `Client` is scoped to (`Client.Org` /
`Client.Project`, defaulting to `DefaultOrg = "carriedworld"` and
`DefaultProject = "NET"`) and asserts the fixed workflow above. Call it once
at startup before filing any work items:

```go
conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
c := workgraph.New(conn, workgraph.DefaultOrg, "nexus-orchestrator", workgraph.DefaultProject)
if err := c.EnsureProject(ctx); err != nil { ... }
```

`EnsureProject`'s org bootstrap, confirmed against the real ledger, is the
full sequence a fresh org needs, not just `CreateOrg`:

1. `GetOrg` — if absent, `CreateOrg(slug, name)`.
2. `CreateUser(id=Client.Subject, kind="ai")` — **always attempted**, not
   only when the org didn't exist. `kind` must be `"ai"`; the live ledger
   rejects other values (e.g. `"agent"`).
3. `AddMember(org, Client.Subject, role="owner")` — **always attempted**,
   not only on first org-create. A pre-existing org can still be missing
   this caller as a member (e.g. a different `Subject` bootstrapped it, or
   membership was never granted) — `ProjectService.CreateProject` requires
   the caller to be a member, so this can't be conditioned on "did we just
   create the org."

Both `CreateUser` and `AddMember` are treated as idempotent no-ops when the
ledger reports the resource already exists (see `isAlreadyExists`, below).

`EnsureProject` calls `AdminService.GetOrg`/`CreateOrg`/`CreateUser`/
`AddMember` and `ProjectService.ListProjects`/`CreateProject` — this assumes
the mTLS identity dialing in has admin-service scope on the sovereign
ledger. If it doesn't, `EnsureProject` will fail cleanly with a wrapped gRPC
error; the org and project can then be created out-of-band once, after which
`EnsureProject`'s remaining GetOrg/ListProjects/SetProjectWorkflow calls
should succeed with a lower-privilege identity.

### Matching the live ledger's actual error shapes

The sovereign ledger does **not** consistently use clean gRPC status codes
for "not found" / "already exists" — confirmed against the real ledger:
`GetOrg` on a missing org returns `codes.Internal` with a `"not found"`
message, not `codes.NotFound`; a duplicate create surfaces a
`"UNIQUE constraint failed"` message, not `codes.AlreadyExists`.
`isNotFound`/`isAlreadyExists` (in `ensure.go`) match the clean code **or**
a message substring, so `EnsureProject` stays idempotent against both a
strict future ledger and the live one's current behavior.

### cwb-scopes

Every RPC also carries a `cwb-scopes` metadata header (`Client.Scopes`,
default `["issue:admin"]`) — confirmed required against the live ledger: on
the direct mesh path (no gateway in front), the caller self-asserts scopes
and the mTLS cert is the trust boundary; without `cwb-scopes` every
authorized RPC is rejected. The orchestrator manages the whole graph
(create/transition/cancel + project bootstrap), so `issue:admin` (the
superset scope) is the default.

## Dial (mTLS)

`DialCreds()` builds gRPC transport credentials from:

- `WORKGRAPH_TLS_CERT` / `WORKGRAPH_TLS_KEY` / `WORKGRAPH_TLS_CA` — the cwb
  mesh client cert/key/CA (mirrors `nexus/cmd/nexus`'s `custodianDialCreds`/
  `almanacDialCreds`)
- `WORKGRAPH_DEV_INSECURE=1` — dial without mTLS (local dev only)

```go
creds, err := workgraph.DialCreds()
conn, err := grpc.NewClient("ledger.cwb.svc:8081", grpc.WithTransportCredentials(creds))
c := workgraph.New(conn, org, subject, project)
```

Every RPC the `Client` makes carries `cwb-subject`/`cwb-org` metadata (the
`Subject`/`Org` fields) — `CreateProject` in particular reads the
organisation from this context, not the request body.

## API surface

- `CreateWorkItem(ctx, WorkItem) (id string, err error)`
- `GetWorkItem(ctx, id) (WorkItem, error)`
- `ListReady(ctx, role, stream string) ([]WorkItem, error)` — `role` is
  required (see "Role -> ready", above); `stream == ""` means no stream
  filter
- `Transition(ctx, id string, status Status) error`
- `RecordResult(ctx, id string, result Result) error` — records the
  `cwb:result` comment; `done` transitions to `Done`, `blocked` transitions
  to `Blocked`, `pass`/`reject` leave the issue's status untouched
- `Rework(ctx, rejectedID string, newSpec WorkItem) (newID string, err error)`
  — creates a new work item (inheriting `Role`/`StreamID` from the rejected
  item when `newSpec` doesn't set them), carries the rejected item's full
  `PriorResults` forward (unless `newSpec` already supplies its own), sets
  `Origin = rework`, and adds a `relates-to` back-edge `newID -> rejectedID`
  (the ledger link vocabulary is `blocks` | `relates-to`; `relates-to` is
  workgraph's convention for the back-edge — there's no dedicated
  "rework-of" link type)
- `Claim(ctx, id, agent string) error` — atomic; returns `ErrAlreadyClaimed`
  if another agent got there first
- `Cancel(ctx, id string, requeue bool, reason string) error` —
  `requeue=true` records `reason` as a `cwb:result` comment (verdict
  `blocked`) and transitions back to `queued` (`To Do`); `requeue=false`
  transitions to `cancelled` and best-effort transitions direct dependents
  (issues this one blocks) to `blocked`, since they can never become ready
  now

## Deviations / open questions

- **`ClaimIssue` conflict code**: `Claim` maps both `codes.FailedPrecondition`
  and `codes.AlreadyExists` to `ErrAlreadyClaimed` — the exact gRPC code the
  live ledger returns for "already claimed" isn't verified from this
  checkout (only the fake-ledger tests exercise this path). `TestLiveWorkGraph`
  doesn't exercise the conflict path — worth a manual check.
- **`ready` as a stored workflow state**: `ListReady` membership (now
  correctly aspect-scoped, see "Role -> ready" above), not a stored status,
  is treated as the source of truth for `StatusReady`. `Transition(ctx, id,
  StatusReady)` is implemented (moves the issue to `Ready to Start`) but
  nothing in this adapter calls it automatically; confirm this matches how
  unit 5/6 (which build on this) expect to consume readiness.
- **`EnsureProject`'s admin-service calls**: assumes the workgraph mTLS
  identity has `AdminService` scope (`CreateOrg`/`GetOrg`/`CreateUser`/
  `AddMember`). If the sovereign ledger's mesh policy scopes admin ops to a
  different identity, the org bootstrap will need to run out-of-band once.
- **Terminal blocker statuses**: a `blocks` edge stops holding its dependent
  back once the blocker reaches `Done` (`terminalBlockerStatuses` in
  `fake_test.go`). Whether a `Cancelled` blocker also unblocks its
  dependents on the live ledger is unconfirmed — the live e2e doesn't
  exercise that path. `Cancel(requeue=false)` on this adapter's side
  independently transitions direct dependents to `Blocked` rather than
  relying on the ledger to do so.
- **`assignee_team` is unused**: the proto has it (`ListReadyIssuesRequest`
  and `Issue.assignee_team`) but the live ledger's `ListReadyIssues` doesn't
  honor it today — only `cwb-subject` (via `assignee_aspect` equality)
  gates readiness. If/when the ledger wires team-based readiness, revisit
  whether role should map to `assignee_team` instead of overriding
  `cwb-subject` per-call.
