# nexus/docregister

M1 Unit 2: the document register + operator approval gate
(`PHASE2-DESIGN.md` §9, §9a). Specs, plans, designs, and reports become
first-class, lifecycle-managed, operator-approvable documents — never a
pile of files in a folder. **A doc that isn't attached to a work-item with a
status doesn't exist.**

This is the **operator + shadow shared workbench**: shadow drafts and
revises from planning sessions; **verdicts are operator-only**. The API is
reachable from croft via the broker's REST surface (see "Broker endpoints",
below), not internal to the orchestrator.

## The document shape (§9)

```
document: {id, kind: spec|plan|design|report, title, version,
           status: draft|awaiting_approval|approved|approved_with_changes|rejected|superseded,
           work_item_id,        -- every doc belongs to a job
           cairn_ref,           -- MD content in cairn (versioned, diffable)
           approvals: [{by, verdict, comments, at}]}
```

`Document` (`types.go`) mirrors this exactly. `ListFilter{Kind, Status,
Stream}` narrows `ListDocs` — `Stream` filters on `work_item_id`.

## The ledger-vs-cairn split, and why

**Content in cairn, lifecycle in a dedicated store** — not a ledger
issue-kind. Two independent decisions:

### Content: cairn (via git, not gRPC)

The MD body needs versioning and diffing — cairn already does that; it's a
git host. The natural way to store it is a **real git commit against a
cairn-hosted line**, not an RPC call. Confirmed by reading cwb-proto's
`cwb.cairn.v1` package (`proto/cwb/cairn/v1/cairn.proto`): it has exactly
three services — `RepoService` (create/list repos), `PullService`
(open/get/merge/list pull requests), `OrgService` (purge) — repository
*administration*, not blob content. The package's own doc-comment says why:
"cairn's git wire protocol (Smart-HTTP + SSH) is NOT here: git can't be
gRPC and stays on its existing transports." There is no cairn gRPC RPC to
reuse for "commit this markdown"; hand-rolling one would be new tracker
logic cairn doesn't expose. So `CairnContent` (`cairn_content.go`) is an
interface with a real implementation, `GitCairnContent`, that shells out to
`git` against a configured working-directory checkout of a cairn line — the
same "the cairn line checkout is the shared workspace" convention
`PHASE2-DESIGN.md` §8 already establishes for builder/tester/reviewer
artifacts.

- `cairn_ref` convention: `"<repo-relative-path>@<git-commit-sha>"`, e.g.
  `docs/spec/doc-a1b2c3d4e5f60718.md@5b9c3f394d95ef3e398727af33c75e15e50a49cd`.
  Path is `docs/<kind>/<doc-id>.md` (`docPath` in `cairn_content.go`) so
  specs/plans/designs/reports don't collide.
- `Commit` writes the file, `git add`s, `git commit`s, and returns the new
  ref (`<path>@<HEAD sha>`); `Fetch` does `git show <sha>:<path>`. Old
  refs stay fetchable after later commits — that's the versioning/diffing
  cairn provides for free.
- `GitCairnContent` deliberately does **not** push. Pushing to the
  sovereign cairn remote is a separate, credentialed operation — the same
  boundary this very build works under (`cairn commit`, no push). A real
  deployment either points `RepoDir` at a checkout with push credentials
  and pushes on a cadence, or treats the local commit as the register's
  durable copy and relies on an operator/CI step to push. This is a
  documented convention, not a hand-rolled cairn client.
- A `CairnContent` implementation is free to use a different ref
  convention as long as its own `Commit`/`Fetch` agree — `RefFormat`
  documents the one `GitCairnContent` uses.

### Lifecycle: a dedicated `docregister` store, not a ledger issue-kind

`PHASE2-DESIGN.md`'s dogfood mapping (§10) allows either "ledger
(lifecycle/approvals)" or a "SIMPLEST that fits" dedicated store; this
build picks the **dedicated store** (`store.go`, sqlite-backed,
`nexus/runs`'s idiom: a narrow `Store` interface + a `SQLStore`), because:

- **Leaner for what a document actually needs.** `nexus/workgraph`
  (`adapter.go`) already does substantial work mapping `WorkItem` onto
  ledger's issue/workflow/DoD/comment model (see its README's "Role ->
  ready" and "definition_of_done is a checklist" sections) — that
  complexity earns its keep for *work items*, which need dependency edges,
  claim semantics, and a pool-assignment model. A document's lifecycle is
  a flat 6-state machine plus an append-only approvals list; forcing it
  through ledger's issue/comment/workflow machinery (as workgraph does for
  handoff/result blobs, via JSON-tagged comments) would be strictly more
  code for less benefit here — there's no dependency graph, no claiming,
  no pool.
- **A document is not a job.** It *belongs to* one (`work_item_id`
  cross-references the ledger issue that's the actual unit of dispatched
  work — see §9's "every doc BELONGS to a job; orphans impossible"), but
  the document's own approve/reject/supersede verdicts are a different
  state machine than the work-item's queued/dispatched/done one. Keeping
  them separate avoids overloading ledger's `Issue.status` with meanings
  it wasn't designed for (ledger has no `approved_with_changes` or
  `superseded` workflow state, and `TransitionIssue`'s DoD-checklist gate
  is workgraph's problem to solve, not docregister's).
- **Cheaper to build and test.** Mirrors `nexus/runs`'s exact pattern (a
  `Store` interface, `SQLStore` with `Migrate`/CRUD, sqlite
  `:memory:` in tests) rather than `nexus/workgraph`'s far larger surface
  (mTLS dial, narrow gRPC client interfaces per service, ledger
  quirks-matching in `ensure.go`). The register's unit tests
  (`register_test.go`) run in milliseconds against real sqlite, no fakes
  needed for the lifecycle logic itself — only `CairnContent` is faked.

If a future need arises to make documents queryable through the same
ledger UI as work items (e.g. so the console's single graph view shows
both), the register's `Store` interface is the seam to swap in a
ledger-backed implementation without touching `register.go`'s lifecycle
logic or the broker layer.

## Lifecycle (state machine)

```
draft --SubmitForApproval--> awaiting_approval
awaiting_approval --Approve--> approved
awaiting_approval --ApproveWithChanges--> approved_with_changes  (commits a NEW cairn version)
awaiting_approval --Reject--> rejected
{approved, approved_with_changes, rejected} --Supersede--> superseded
```

`Revise` (commit an edited MD body, bump `Version`, no status change) is
legal from `draft` or `awaiting_approval` — the collaborative
drafting/refinement the workbench exists for. It is **not** legal once a
verdict has landed (`approved`/`approved_with_changes`/`rejected`/
`superseded`) — supersede a decided doc with a new one instead of mutating
it in place, so the approval history stays meaningful.

Every other out-of-order call (`Approve` on a draft, double-`Supersede`,
etc.) returns `ErrInvalidTransition`. See `register_test.go`'s
`TestInvalidTransitions` / `TestRevise` for the exhaustive table.

`ApproveWithChanges` is the one verdict that mutates content: it calls
`CairnContent.Commit` with the operator's edited MD (a new cairn version,
`Document.Version` bumped), *then* records the `approve_with_changes`
approval and moves the status — in that order, so a content-commit failure
never leaves a phantom approval recorded.

## API surface (`register.go`)

- `CreateDoc(ctx, kind, title, workItemID, mdContent) (id, error)` —
  status=draft, version=1, commits mdContent to cairn, indexes in the store.
- `GetDoc(ctx, id) (Document, error)` — metadata + approvals (not content).
- `GetContent(ctx, id) (string, error)` — the current MD body, read through
  to cairn via `Document.CairnRef`.
- `ListDocs(ctx, ListFilter) ([]Document, error)` — filter by kind/status/
  stream (work_item_id).
- `Revise(ctx, id, editedMD) error` — draft/awaiting_approval only.
- `SubmitForApproval(ctx, id) error` — draft -> awaiting_approval.
- `Approve(ctx, id, by, comments) error`
- `ApproveWithChanges(ctx, id, by, editedMD, comments) error`
- `Reject(ctx, id, by, reasons []string) error`
- `Supersede(ctx, id) error`

`Register` performs **no authorization** — it's a plain library. The
workbench-vs-verdict access boundary is enforced entirely by the broker
layer (below); any caller with a `*Register` can call any method, which is
why the broker is the only place these methods should be reachable from
outside a trusted process.

## Broker endpoints (`nexus/broker/docregister_rest.go`)

Two trust tiers over the same `docregister.Register`, gated on
`Config.DocRegister` being configured (nil = both surfaces 404, matching the
`WorkerStatusStore`/`Credentials` convention elsewhere in `nexus/broker`):

**Workbench — broker-authenticated (`b.auth`), reachable from croft:**

| Method | Path | Register call |
|---|---|---|
| POST | `/api/docs` | `CreateDoc` |
| GET | `/api/docs?kind=&status=&stream=` | `ListDocs` |
| GET | `/api/docs/{id}` | `GetDoc` |
| GET | `/api/docs/{id}/content` | `GetContent` |
| POST | `/api/docs/{id}/revise` | `Revise` |
| POST | `/api/docs/{id}/submit` | `SubmitForApproval` |

**Verdicts — `requireAdmin` (operator-only):**

| Method | Path | Register call |
|---|---|---|
| POST | `/api/admin/docs/{id}/approve` | `Approve` |
| POST | `/api/admin/docs/{id}/approve-with-changes` | `ApproveWithChanges` |
| POST | `/api/admin/docs/{id}/reject` | `Reject` |
| POST | `/api/admin/docs/{id}/supersede` | `Supersede` |

This is the separation-of-duties lesson from `nexus/broker/admin.go`'s
worker-status and credentials surfaces, applied to documents: drafting and
deciding are different privilege levels even though they operate on the
same record. A non-admin (peer-aspect) token gets a clean 403
`admin_required` from every verdict route; the workbench routes are open to
any broker-authenticated caller (shadow included). See
`docregister_rest_test.go`'s `TestDocRegister_VerdictEndpointsRejectNonAdmin`
/ `TestDocRegister_WorkbenchReachableByNonAdmin`.

Verdict request bodies (`docVerdictBody`): `{"by": "...", "comments": "...",
"reasons": [...], "md_content": "..."}` — `by` is required on every verdict
(the accountable operator identity); `md_content` is read only by
approve-with-changes; `reasons` only by reject.

## Live-verify path

`e2e_test.go`'s `TestLiveDocRegister` mirrors `nexus/workgraph`'s
`TestLiveWorkGraph` discipline: skips cleanly unless opted in, doesn't
require network credentials to run in CI, but is real when pointed at real
git.

```
export DOCREGISTER_E2E_CAIRN_REPO_DIR=/path/to/a/cairn-line-checkout
go test ./nexus/docregister/... -run TestLiveDocRegister -v
```

`DOCREGISTER_E2E_CAIRN_REPO_DIR` must be a git working directory — a real
checkout of a cairn-hosted line (e.g. a scratch line expressed via `cairn
express`, or any checkout you don't mind this test committing scratch docs
into). The test runs the full lifecycle — `CreateDoc` -> `SubmitForApproval`
-> `ApproveWithChanges` (asserts a new `cairn_ref`/version) ->
`GetContent` -> `Supersede` — as **real git commits** in that checkout. It
does **not** push (see "Content: cairn" above) — the sovereign cairn line is
untouched unless you push the checkout yourself afterward. This is the same
"cairn IS git, verified against a real checkout" discipline
`GitCairnContent` uses everywhere; there's no separate gRPC dial/mTLS story
to verify the way `workgraph`'s `TestLiveWorkGraph` verifies `DialCreds()`
against the sovereign ledger, because content storage here has no gRPC path
at all (see above).

`cairn_content_test.go`'s `TestGitCairnContent_CommitAndFetch` already
exercises the real git mechanics (commit, sha, `git show`) against a
throwaway `git init` temp dir on every `go test` run — the live e2e above
is specifically for verifying against an actual cairn-hosted line, not for
verifying the git plumbing itself (that's covered unconditionally).

## Files

- `types.go` — `Document`, `Approval`, `Kind`, `Status`, `Verdict`,
  `ListFilter`, sentinel errors.
- `store.go` — `Store` interface + sqlite `SQLStore` (lifecycle index).
- `cairn_content.go` — `CairnContent` interface + `GitCairnContent` (real
  git-backed content storage).
- `register.go` — `Register`, the lifecycle API composing `Store` +
  `CairnContent`.
- `fake_test.go` / `register_test.go` — lifecycle unit tests against real
  sqlite + a fake `CairnContent`.
- `cairn_content_test.go` — `GitCairnContent` unit tests against a
  throwaway local git repo.
- `e2e_test.go` — env-gated live e2e against a real cairn-line checkout.
