# PR lifecycle: watcher → review → fix queue → operator merge

**Status:** spec, 2026-07-09 · **Author:** shadow (operator-directed design) · **Related:** ACCEPTANCE-GATE-HARDENING.md, OBSERVABLE-CRITERIA.md, BUILDER-CAIRN-MIGRATION.md.

## Why

Nothing in the pipeline owns a PR after it is created. The acceptance gate verifies a PR *exists and substantiates* at `task_done`, then the worker despawns and the PR sits unowned. The 2026-07-09 audit found **33 stranded open PRs** across the org — stale M1 snapshots, eval artifacts, and real work nobody landed — and had to triage them by hand. Without a terminal owner the pile regrows.

This spec closes the loop: every pool PR gets reviewed, review findings get fixed, and the finished PR lands in front of the operator as the single merging authority.

## Design principles

1. **The PR is the state store.** All lifecycle state — verdict, outstanding items, round count, reviewed head — lives on the PR itself (a structured review comment + labels), where every actor reads the same thing: fixer agents via `gh pr view`, re-reviewers via the diff, the operator via the GitHub UI. No parallel state database; nothing to drift.
2. **The watcher is stateless.** Each poll (or webhook event) reconstructs the lifecycle from PR state and decides the one next action. Restarts are free; missed events self-heal on the next poll.
3. **Fixes are queue work, not personality work.** A fix item goes into the open work queue for any free builder. Ownership rides the work item + branch, not a personality. cairn makes this safe: any builder can `express` the existing line, and per-commit reconciliation keeps the fixer writing against latest `main`.
4. **Reviews are pinned *away* from the author.** A personality reviewing its own PR is the self-report trap (the whole gate-hardening arc). The seeder pins review items `--personality <≠ author>`; the author is identified from the PR's commits (cairn stamps author email + `Change-Id` trailers).
5. **Evidence over bookkeeping.** Checked boxes and markers are bookkeeping, never authority. A re-review verifies each outstanding item against the actual diff at the new head — same principle as the acceptance gate judging the diff, not the narrative.
6. **Merges stay active and human.** `allow_auto_merge=false` org-wide (operator policy, 2026-07-09). The pipeline's terminal is *approved → operator notified*; the operator merges. Agents never merge and never arm auto-merge.

## Event sources

```
cairn-server webhook (repos we host)   ─┐
                                        ├─→ normalized PR event → state machine → nexus workitem create
GitHub poller (repos hosted on GitHub) ─┘
```

- **Poller (ships first).** The pool's repos live on GitHub today, so a `pr-watch` seeder does the work now: a CronJob in the drain-seeder pattern (~10 min), listing open non-draft PRs on `builder/*` branches in registered repos via `gh`, deciding per PR, seeding work items with `--dedupe`. Polling is sovereign — no public ingress into the tailscale-only broker.
- **Webhook (lands with the cairn-server migration).** The cairn server already models PRs-as-ledger-issues; a PR-created / review-submitted / push event hook is in-family. It emits an HTTP POST to the broker carrying the same normalized event the poller synthesizes — a clean VCS-server ↔ workgraph seam (cairn-server never writes the ledger directly). Downstream is identical; the webhook only removes poll latency for repos we host.

Normalized event (either source): `{repo, pr_number, branch, head_sha, author_email, state: opened|review_submitted|synchronized|closed}`.

## The state machine

Evaluated per open, non-draft PR on a `builder/*` branch. "Marker" = the machine-readable header of the latest lifecycle review comment (below).

| Observed PR state | Action |
|---|---|
| No lifecycle review yet for the current head | Seed **review** item, pinned `--personality` ≠ author |
| Marker `verdict=changes-requested`, head unchanged since review, no fix item in flight | Seed **fix** item into the open queue (unpinned) |
| Marker `verdict=changes-requested`, head **advanced** past `head=<sha>` | Seed **re-review** item (pinned ≠ author); increment `round` |
| Marker `verdict=approved` | Terminal: apply label `ready-to-merge`, notify the operator |
| `round` ≥ **3** without approval | Apply label `escalated`, notify the operator, stop seeding |
| PR closed/merged/draft | No action |

- **Dedupe keys:** review = `{repo, pr, head_sha}`; fix = `{repo, pr, round}`. Both ride `workitem create --dedupe` semantics so a slow run never gets a concurrent duplicate.
- **"Fix in flight"** is detected from the work queue (an open fix item for `{pr, round}`), not from PR state — the one read the watcher makes outside the PR.
- The round cap is **cost control as much as loop safety**: each review or fix round is a full model run (~4–6 min/turn on Ornith with APC off).

## The review-comment contract

The reviewer's comment is **the fix contract** — it must contain all state needed to fix the outstanding items, readable by any agent or human with no other context:

```markdown
<!-- pr-lifecycle: verdict=changes-requested round=2 head=4f2c91a -->
## Outstanding
- [ ] runtime/dispatch/jobspec.go:214 — CW_ROLE env appended even when Brief.Role
      is empty — guard the append — verify: TestBuildJob_RoleEnv passes in the diff
- [ ] README missing the new flag — add to §dispatch — verify: section present in diff
## Context
repo=CarriedWorldUniverse/nexus  branch=builder/NET-91
original-criteria: <the work item's acceptance criteria, verbatim>
```

Rules:
- **Every outstanding item is observable-criteria form** (OBSERVABLE-CRITERIA.md): location → defect → required change → how to verify *from the artifact*. No "improve error handling" without a checkable verify clause.
- The `<!-- pr-lifecycle: … -->` marker carries `verdict` (`approved` | `changes-requested`), `round` (int), `head` (the SHA reviewed). This is the watcher's entire memory.
- An **approval** posts the marker with `verdict=approved` and an empty Outstanding section.
- The reviewer reviews **the diff at `head`**, using the review/security/house-style skills (the reviewer role's audited allowlist).
- The contract is posted as a **PR comment** (`gh pr comment`), never a formal GitHub review: all pool agents share the `nexus-cw` identity and GitHub rejects self-review (422, learned live on the first phase-2 run). The marker is the verdict's authority; GitHub review state is not used.

## Fix items

The seeded fix brief is a pointer plus discipline, because the state is on the PR:

> Resolve the outstanding review items on `<PR URL>`. Read the latest `pr-lifecycle` review comment; every unchecked item is your task list. Clone, `cairn express <branch>` (the line exists — you are continuing it), make each item verifiable, `cairn commit && cairn push`. Tick each resolved box, or reply per-item with grounds if you believe an item is wrong. Do NOT merge, do NOT open a new PR, do NOT commit built binaries.
> Criteria: every Outstanding box ticked or answered, fixes pushed to `<branch>`, no binary files in the diff.

- Ticking a box is self-report; the subsequent **re-review verifies against the diff** and either approves or posts a new round.
- A disagreement that survives a round (fixer replies, reviewer re-asserts) is a judgment call → that is what `escalated` is for.

## Terminal & escalation

- **Approved:** label `ready-to-merge`, notify the operator (chat message via the broker's channel to the operator's inbox; a daily digest fallback). The operator merges — actively, per org policy. Nothing in this pipeline merges or arms auto-merge.
- **Escalated:** label `escalated` + notify, with the last review comment as the summary. Causes: round cap, unresolvable disagreement, or a fixer/reviewer run that fails its own acceptance gate twice.

## Code changes

1. **`nexus pr-watch` subcommand** (broker binary): one pass of the state machine over registered repos (`gh` for GitHub-hosted). Flags: `--repos owner/name[,…]`, `--dry-run`. Idempotent; all seeding via existing `workitem create` (with `--personality` for reviews, `--dedupe` always).
2. **CronJob `pr-watch`** (drain-seeder pattern, ~10 min, suspended-by-default until validated live).
3. **Reviewer brief template** in the seeder: instructs diff-grounded review + the comment contract above. Reviewer role allowlist already exists (#438): `review,security,house-style,development`.
4. **Marker parser** (tiny, shared): parse/emit the `pr-lifecycle` comment header; used by the watcher and available to the funnel for gate checks on reviewer runs.
5. **cairn-server webhook** (later, with the hosting migration): emit the normalized event as HTTP POST to the broker; the broker feeds the same state machine.
6. **Labels:** `ready-to-merge`, `escalated` created in registered repos.

## Sequencing

1. Marker parser + `nexus pr-watch --dry-run` against the live open PRs (read-only validation of the state machine).
2. Reviewer path live on ONE real PR (seed review → structured comment lands).
3. Fix path live: a changes-requested round → queue fix → re-review → approved → operator notified.
4. Enable the CronJob.
5. (With cairn-server hosting) webhook producer.

## Non-goals

- **Merging.** The operator is the only landing authority; this pipeline ends at *ready-to-merge*.
- Reviewing human/shadow-authored PRs (scope: `builder/*` branches; widen later if wanted).
- Replacing the acceptance gate — the gate still judges the builder's run at `task_done`; this owns what happens *after* the PR exists.
- A state database — if the design ever needs one, the "PR is the state store" principle has been violated; fix the design instead.
