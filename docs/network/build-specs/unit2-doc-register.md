# M1 Unit 2 — Document register + operator approval (build spec)

**Goal:** specs/plans/designs are first-class, lifecycle-managed, operator-approvable — never a pile of files. A doc that isn't attached to a work-item with a status doesn't exist. This is the operator+shadow shared workbench (§9). Ref: PHASE2-DESIGN §9, §9a (console reads this).

## The document shape (§9)
```
document: {id, kind: spec|plan|design|report, title, version,
           status: draft|awaiting_approval|approved|approved_with_changes|rejected|superseded,
           work_item_id,        -- every doc belongs to a job
           cairn_ref,           -- MD content in cairn (versioned, diffable)
           approvals: [{by, verdict, comments, at}]}
```

## Design (dogfood: ledger lifecycle + cairn content)
- **Content in cairn**: the MD body is a cairn-stored file/blob (versioned) — `cairn_ref` points at it. Reuse the sovereign cairn (cairn.cwb.svc / the workgraph's cairn access pattern if one exists, else a cairn gRPC client like workgraph's ledger client).
- **Lifecycle in ledger OR a dedicated store**: the register index + status + approvals. SIMPLEST that fits the dogfood mapping: a new `nexus/docregister` package with a store (mirror `nexus/workgraph` or `nexus/runs` idiom) that keeps doc metadata + status + approvals; OR model docs as a ledger issue-kind. Pick the leaner one and document why. If a dedicated store, note it as the register's home.
- **Reachable from croft**: shadow drafts/revises from planning sessions; the API must be callable from croft (a broker REST endpoint on admin.go, or a gRPC service). shadow gets read/draft/revise; **verdicts are operator-only**.

## API (a package + broker endpoints)
- `CreateDoc(kind,title,workItemID,mdContent) -> id` (status=draft; writes MD to cairn, index to store)
- `GetDoc(id)`, `ListDocs(filter: kind/status/stream)`
- `SubmitForApproval(id)` (draft -> awaiting_approval)
- `Approve(id, by, comments)` / `ApproveWithChanges(id, by, editedMD, comments)` (commits new cairn version) / `Reject(id, by, reasons)` — these three record an approval + set status; **verdict endpoints MUST be requireAdmin (operator-only)**; draft/revise/read endpoints are the shared workbench (broker-authenticated).
- `Supersede(id)` for a replaced doc.

## Constraints
- cairn line `builder/m1-unit2-doc-register` off `builder/m1-unit6-orchestrator` (the full core). `cairn commit`, no push.
- Verdict endpoints requireAdmin; workbench endpoints broker-authenticated.
- Reuse workgraph's ledger/cairn dial patterns if present; don't hand-roll mTLS if a pattern exists.

## Acceptance
1. `go build ./...` + `go vet` clean; existing tests pass.
2. Unit tests (fakes): the lifecycle transitions (draft→awaiting_approval→approved/approved_with_changes/rejected/superseded); ApproveWithChanges commits a new cairn version + records the approval; verdict endpoints reject non-admin (requireAdmin); ListDocs filters by kind/status.
3. README: the doc shape, the ledger-vs-cairn split, the workbench-vs-verdict access boundary, the live-verify path.
4. If a live cairn/ledger e2e is feasible (env-gated, against the sovereign node), include it like unit-1's; else document the live-verify path.
