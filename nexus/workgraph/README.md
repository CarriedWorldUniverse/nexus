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
| `role` | `Issue.skills[0]` (ledger's skills/label tags) |
| `task_spec` | `Issue.description` |
| `acceptance_criteria[]` | `Issue.definition_of_done`, newline-joined (ledger enforces DoD on the Done transition) |
| `depends_on[]` | `issue_links type=blocks`, blocker -> this (`AddLink(key=blocker, to_key=this)`) |
| `status` | workflow state, see the table below |
| `stream_id` | `Issue.parent_key` (epic) — a stream is an epic subtree |
| `handoff` (cairn_line/artifacts/base_knowledge/personality/origin) | a JSON comment tagged `cwb:handoff` (no first-class column) |
| `result` (full blob) | a JSON comment tagged `cwb:result` (one comment per result; `GetWorkItem` folds every `cwb:result` comment into `PriorResults`, in timeline order) |

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
transitions between all of them, with the DoD gate only on `Done` (per
ledger's "required DoD on Done transition" behavior). This makes the
Status<->ledger-state fold exact regardless of what the ledger would have
defaulted to, and self-heals on every `EnsureProject` call (idempotent — safe
to call on every boot).

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

`EnsureProject` calls `AdminService.GetOrg`/`CreateOrg` and
`ProjectService.ListProjects`/`CreateProject` — this assumes the mTLS
identity dialing in has admin-service scope on the sovereign ledger. If it
doesn't, `EnsureProject` will fail cleanly with a wrapped gRPC error; the org
and project can then be created out-of-band once, after which
`EnsureProject`'s remaining GetOrg/ListProjects/SetProjectWorkflow calls
should succeed with a lower-privilege identity.

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
- `ListReady(ctx, stream string) ([]WorkItem, error)` — `stream == ""` means
  no stream filter
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

## Deviations / open questions for the live e2e to confirm

- **`ClaimIssue` conflict code**: `Claim` maps both `codes.FailedPrecondition`
  and `codes.AlreadyExists` to `ErrAlreadyClaimed` — the exact gRPC code the
  live ledger returns for "already claimed" isn't verified from this
  checkout (only the fake-ledger tests exercise this path). If the live
  ledger uses a different code, `TestLiveWorkGraph` won't catch it directly
  (it doesn't exercise the conflict path) — worth a manual check.
- **`ready` as a stored workflow state**: see the note above — `ListReady`
  membership, not a stored status, is treated as the source of truth for
  `StatusReady`. `Transition(ctx, id, StatusReady)` is implemented (moves the
  issue to `Ready to Start`) but nothing in this adapter calls it
  automatically; confirm this matches how unit 5/6 (which build on this)
  expect to consume readiness.
- **`EnsureProject`'s admin-service call**: assumes the workgraph mTLS
  identity has `AdminService` scope (`CreateOrg`/`GetOrg`). If the sovereign
  ledger's mesh policy scopes admin ops to a different identity, the org
  bootstrap will need to run out-of-band once (see above).
