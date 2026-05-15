# Git workflow — trunk-based, review-gated

**Status:** v0 · **Drafted:** shadow · **Operator:** `<operator>` · **Effective:** immediate for new work

All code commits across the CarriedWorldUniverse repos follow trunk-based development. main is always releasable; all work merges via reviewed PR.

## The rule

1. **Never push directly to `main`.** main is branch-protected on every public repo and rejects direct pushes from non-admins. Even admins (operator) should follow PR flow except for genuine emergencies.
2. **Work on a feature branch.** Branch off main, do the work, push the branch, open a PR.
3. **PR is reviewed before merge.** Today operator is the only reviewer; once AI aspects have GitHub identities (NEX-120), they review per their lane.
4. **CI must pass before merge.** Status checks (`build + test` matrix across OSes) are required by branch protection. Red CI blocks merge — fix in the same PR, don't bypass.
5. **Squash or rebase merge only.** Linear history is required; merge commits are blocked.
6. **Releases tag from main only.** Push a `vX.Y.Z` tag at the merge commit; goreleaser cuts the release. Pre-release identifiers (`v0.2.0-rc1`) are recognized by the auto-prerelease flag.

## Branch naming

`<aspect>/<short-summary>` or `<NEX-NN>-<short-summary>`. Examples:

- `anvil/nex-83-acp-client-retry`
- `nex-104-agora-scrollback`
- `plumb/probe-09-tool-use-replay`

Names are mnemonic, not load-bearing. The Jira key in the title is a clear signal that work ties to a tracked ticket.

## Use `git worktree`, not `git checkout`

Aspects should create a **new worktree** per feature branch rather than switching branches in the main clone. The main clone stays on `main`, always clean; each in-flight feature lives in its own sibling directory.

**Why this discipline:**

- Aspect personas anchor to the cwd they were spawned in (CLAUDE.md, .mcp.json, agent dirs are cwd-scoped). Switching branches in-place can swap these files mid-session and confuse the aspect about what reality it's looking at.
- Multiple aspects (or the same aspect on multiple tasks) can work in parallel against the same repo without locking or thrashing each other's index.
- Stale feature branches don't pollute the main checkout — `git worktree list` shows everything in flight, and `git worktree remove` cleans cleanly once a PR is merged.

**Standard flow:**

```sh
# Operator (or aspect) creates a worktree off the main clone:
cd ~/Source/nexus
git fetch origin
git worktree add ../nexus-anvil-nex-83 -b anvil/nex-83-retry origin/main

# Aspect cd's into the worktree and works there:
cd ../nexus-anvil-nex-83
# … edits, commits …
git push -u origin anvil/nex-83-retry
gh pr create --fill
```

**Cleanup after merge:**

```sh
cd ~/Source/nexus
git worktree remove ../nexus-anvil-nex-83
git fetch origin --prune   # drops the merged remote branch ref
```

**Naming:** the worktree directory follows `<repo>-<aspect>-<short>` (e.g. `nexus-anvil-nex-83`). Lives **beside** the main clone, never inside it (`../nexus-...`, not `./nex-83/`).

**Common pitfalls:**

- Don't `git checkout <branch>` in the main clone after this pattern is adopted; it defeats the worktree's whole point of keeping main pristine.
- Don't share a single worktree between two aspects working on different branches; each gets its own.
- `git worktree remove` refuses if there are uncommitted changes. Either commit/push first or pass `--force` (lose the work) deliberately.

## PR template

PR descriptions should answer four questions. Keep terse:

1. **What changes** — one or two sentences on the diff.
2. **Why** — link the Jira ticket or chat msg_id that motivated it. If neither exists, file a ticket first; one-shot ad-hoc work is rare.
3. **Test plan** — what you ran locally to verify; what CI proves; what's left for human verification (e.g. dashboard visuals).
4. **Risk / rollback** — surface anything destructive, schema-affecting, or non-trivial to revert.

Commits inside the PR don't need ceremony; the squash commit on merge is the load-bearing artifact.

## Per-aspect responsibilities

**Coding aspects** (shadow, keel, anvil, plumb, forge, wren):
- Open PRs for every code change, no matter how small.
- Pass CI before requesting review.
- Respond to review comments in the PR thread, not chat. Chat is for triage, not code discussion once a PR exists.

**Reviewers** (today: operator; tomorrow per NEX-120: AI aspects with GitHub identities):
- Approve only after reading the diff, not just the summary.
- Comments inline on specific lines. General comments at the PR-thread level.
- Decline (or request-changes) is a normal outcome. Aspects shouldn't take a request-changes personally — the substrate doesn't have ego, it's a discussion.

**Operator** can:
- Override CI requirement in a genuine emergency (admin bypass), but should note in the merge message *why*.
- Force-push if an aspect's PR branch is wedged. Don't do this casually; prefer fresh PR over force-push.

## Repos in scope

All CarriedWorldUniverse public repos with branch protection enabled:

- `nexus`, `agora`, `bridle`, `acp-claude-pty`, `interchange`, `casket-go`, `casket-ts`, `casket-dotnet`

Private repos (vessel, cairn) follow the same discipline as a habit but lack branch-protection enforcement until they go public or get a paid GitHub plan.

## Common scenarios

**Tiny fix (typo, comment edit):** still goes through PR. Mark with `chore:` prefix; reviewer can approve in seconds.

**Aspect-on-aspect collaboration (anvil hands plumb a patch):** PR opened by the implementing aspect. The handing aspect can be requested for review explicitly. Conversation lives in PR comments + chat references both sides.

**Hot-fix on main (broken in production):** open PR with `fix:` prefix and the broken-state evidence. CI gates apply unless operator overrides; document the override in the PR description if used.

**Rollback:** open a PR that reverts the offending commit (use `git revert <sha>`). Standard CI + review path. No force-push to main to "undo" — that breaks every clone.

## What this replaces

Pre-policy state: aspects (and shadow) pushed directly to main on every repo, including nexus and agora before they were public. That worked because the audience was internal-only and the operator vetted in real-time via dashboard / chat.

Now that repos are public + branch-protected, direct push fails by design. This policy is the new normal — codifying the discipline so aspects don't accidentally try to bypass it on every change.

## Open questions / future amendments

- **AI reviewer accounts** (NEX-120): when AI aspects can GitHub-authenticate, the policy gains: "review approval from a non-author aspect required before merge." Until then, operator is sole reviewer.
- **CODEOWNERS** files per repo: optional once AI reviewers land — auto-request the right aspect based on touched files (anvil reviews `runtime/`, keel reviews `nexus/cmd/nexus/`, etc).
- **Branch protection on private repos** (vessel, cairn): blocked on GitHub plan or making repos public. NEX-118 region.

---

*Companion docs: [`code-standards.md`](code-standards.md), [`work-routing.md`](work-routing.md).*
