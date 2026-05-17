# Developer standards

Authoritative protocol for aspects contributing code to Nexus + sibling repos (ledger, agora, vessel, etc.). Shadow's dispatch briefs paste the canonical block from this doc into every ticket; this file is the source of truth.

Source incident: PR #58 (NEX-174) bundled three unrelated commits, opened off stale `main` causing CONFLICTING state, included a helper (`PrimarySurfaceFromMetadata`) with zero callers. Review caught it only because shadow happened to look. These standards exist so the next aspect doesn't repeat any of those mistakes.

## Workspace layout

**Per-aspect clone + per-ticket worktree.** Each aspect maintains its own clone of every repo it touches at `<aspect-home>/repos/<repo-name>/`. Do not share a central checkout across aspects — branch state, uncommitted work, and rebases collide.

Within your clone, the main checkout **stays on `main`**. Never do feature work in the main checkout. Per-ticket work happens in a sibling git worktree.

### Canonical flow

```bash
# 1. Update main
cd <aspect-home>/repos/<repo-name>
git checkout main && git pull origin main

# 2. Create per-ticket worktree
git worktree add ../<repo-name>-nex-XXX-slug -b feature/nex-XXX-slug

# 3. Do work in the worktree
cd ../<repo-name>-nex-XXX-slug
# … commits, push, open PR, review, merge

# 4. MANDATORY cleanup after merge
cd ../<repo-name>
git worktree remove ../<repo-name>-nex-XXX-slug
git branch -d feature/nex-XXX-slug
git pull origin main
```

The cleanup step is non-optional. Skipping it leaves stale `.git/worktrees/<name>/` entries that accumulate indefinitely. If you ever delete a worktree directory manually, run `git worktree prune`.

## PR protocol

**Scope.** One ticket per PR. If you find an unrelated bug or refactor opportunity while working, **file a separate ticket and PR**. Do not bundle. Reviewers cannot separately approve or revert bundled changes; bundling forces all-or-nothing.

**Branch naming.** `feature/nex-XXX-short-slug` for features, `fix/nex-XXX-…` for bug fixes, `chore/…` / `docs/…` as appropriate. The ticket key must appear in the branch name.

**Rebase.** Rebase against `origin/main` immediately before opening the PR and again before requesting review if main has moved. Open PRs must be in a non-CONFLICTING state.

**Commits.** Conventional-commit subject line referencing the Jira key:

```
feat(autospawn): NEX-174 — auto-start aspects with agora surface support
```

Body describes the *why*; the diff describes the *what*. Do not pad commit bodies with file-by-file restatements of the diff.

**Tests.** New code paths get tests — especially HTTP handlers, WebSocket frame handlers, and DB writes. A "manual test plan" checklist alone is not sufficient for these. Existing tests must pass before opening the PR.

**No dead code.** Do not ship helpers, types, or fields with zero callers. Either wire them up in this PR or remove them. If a helper is genuinely staged for a follow-up, **file the follow-up ticket and reference it in the PR body** explaining the gap.

**CI.** Verify CI is green before requesting review. Do not open a PR and walk away — the reviewer should not be the one discovering build failures.

**Self-review pass.** Before requesting review:

1. Re-read your diff end to end.
2. Confirm the PR description matches what shipped (file count, summary, scope).
3. Confirm the linked ticket's acceptance criteria are all hit.
4. If any criterion isn't hit, update the PR description with the gap and the follow-up ticket.

**Approval.** Aspects do not self-approve or self-merge. Reviewer is shadow (default) or another aspect designated in the dispatch brief. The operator drops the final merge unless otherwise authorized for the specific ticket.

## Why these rules exist

- **One ticket per PR** — reviewers can approve/revert atomically. Bundled PRs require either reviewing unrelated code or trusting the author on the side-scope, both of which break the review contract.
- **Rebase before opening** — CONFLICTING PRs block CI runs and waste reviewer attention. Aspects own their conflict resolution.
- **Worktrees + per-aspect clones** — multiple aspects working the same repo concurrently is the steady state. Shared checkouts collide on branch state; worktrees inside one clone collide on uncommitted work; per-aspect clones + per-ticket worktrees compose isolation cleanly.
- **No dead code** — helpers with no callers are either an incomplete feature or future-bait. Both deserve a ticket, not a silent merge.
- **Tests on handlers** — the manual-test-only path keeps shipping unintended-bug-as-feature regressions. Handlers and DB writes are exactly where the contract has to be enforced in code.

## When to escalate

If a rule clashes with the ticket's actual requirements (e.g. genuinely intertwined scope that can't be cleanly split), **raise it in the dispatch thread before opening the PR**, not in the PR description. Standards bend by negotiation, not unilateral exception.
