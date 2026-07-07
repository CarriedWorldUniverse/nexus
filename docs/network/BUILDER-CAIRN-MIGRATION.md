# Builder VCS migration: git → cairn (clone-per-run)

**Status:** spec, 2026-07-08 · **Author:** shadow · **Depends on:** the measured cairn concurrency boundary (below) · **Related:** cairn issue #81.

## Why

cairn is the sovereign git replacement — the intended VCS for all work. Its per-commit reconciliation gives a strong property for a **pool of parallel builders**: every `cairn commit` reconciles the line against the latest `main`, so conflicts surface early and per-commit and the branch stays **fast-forwardable** — no big-bang merge collision at PR time (the pain that today forces the "close one, cheap re-land on fresh main" workaround on concurrent same-file git tickets). Moving the builder onto cairn also dogfoods the VCS on the highest-volume path.

cairn's CLI can front a **git remote** as well as a cairn server, so this migration changes the *workflow* without moving the server: GitHub stays the origin, `gh` PRs still work, and a later step can move to a cairn server (full-fidelity lines, PRs-as-ledger-issues).

## The hard constraint (measured, not assumed)

A pool runs **concurrent** builder processes. Test (dMon, cairn fronting a local bare-git server):

| model | result |
|---|---|
| N concurrent builders, **one shared clone** | ✗ concurrent `express` clobbers the shared expressed-lines registry (no cross-process lock) → `commit` fails `branch not expressed` → **empty branch pushed, silent work-loss** (cairn #81) |
| N sequential, shared clone | ✓ (proves it's concurrency, not usage) |
| N concurrent, **each its own clone** | ✓ all land, real content, all ff-able |

So the git shared-mirror + `worktree add` model (which isolates per worktree and is concurrency-safe) does **not** translate to a shared cairn clone — cairn's `express` shares mutable clone metadata. **Until cairn #81 is fixed, the builder must use CLONE-PER-RUN.** This is the decisive design input.

## The model

**Clone-per-run, isolated per dispatch.**

- **Persistent `shared-repos` PVC keeps a bare mirror** per repo (as today), kept fresh with a periodic/pre-run `git fetch` (or `cairn fetch`). It is the fast local source — NOT a shared working copy.
- **Each dispatch clones its own working copy** from that local mirror into its per-run dir (`/src/<repo>/<aspect>-<runID>/`), works, and disposes it on despawn. A local `cairn clone <mirror>` copies objects locally (no network) — fast for a moderate repo; a huge-repo reference/shared-objects clone is an optimization to request from cairn if clone cost bites.

### The builder loop (replaces the git mirror+worktree in `builder_repos.go`)

```
cairn clone  <local-mirror-or-origin>  <per-run-dir>      # own clone (isolated)
cd <per-run-dir>
cairn config user.name nexus-cw ; user.email nexus@darksoft.co.nz   # or CAIRN_AUTHOR env
cairn express builder/<ticket> --from main                # the run's line
… agent does the work in builder/<ticket>/ …
cairn commit builder/<ticket> -m "<msg>"  &&  cairn push origin builder/<ticket>
# open PR via gh (git projection), as today
# despawn: rm -rf <per-run-dir>   (the whole clone is per-run)
```

### Exit-code discipline (mandatory)
- **`commit && push`** — never push unconditionally. A non-zero `commit` MUST block the push, or a failed commit ships an empty branch (the #81 failure mode, and general hygiene).
- **`cairn commit` exit codes:** `0` sealed clean; **`2` = recorded conflicts** (the run's line diverged from a `main` that moved mid-run — reconcile via `cairn resolve <branch> <path>` then re-commit, do NOT push a conflicted line); `1` = error (with clone-per-run this should not be the #81 race, but treat as fail-and-surface). Mirror the retry posture the git path already uses (`gitWithRetry`/`gitLockContention`) for transient errors.

### Auth & identity
- Push auth: cairn resolves `CAIRN_TOKEN > GITHUB_TOKEN > GITLAB_TOKEN > credstore`. Set `CAIRN_TOKEN`/`GITHUB_TOKEN` from the builder's existing git credential (the same token the git path uses) — no PAT on the command line.
- Identity: `nexus-cw` / `nexus@darksoft.co.nz` via `cairn config` (repo-local, in the per-run clone) or `CAIRN_AUTHOR` env — else commits get a `…@users.noreply.cairn` placeholder.

## Code changes
- **`runtime/cmd/agentfunnel/builder_repos.go`** — replace `ensureMirror`/`spawn` (git `clone --mirror` + `worktree add`) and `cleanDespawn` (`worktree remove`) with: refresh the shared mirror, `cairn clone` per run into the per-run dir, `express`, and `rm -rf` the per-run clone on despawn. Keep the `shared-repos` PVC (now: bare mirror source, not a shared working copy). Preserve the transient-error retry wrapper.
- **Env gate** `CW_VCS=cairn|git` (default `git`) so this is dark until proven — same rollout posture as every other unit. Flip to `cairn` per dispatch, verify on a live pool ticket, then default.
- **Skills** — generalize `cairn` (drop carried-world-specific origin/paths to examples; it's the VCS how-to) and update `development`'s VCS section to the cairn loop when `CW_VCS=cairn` (today it says git branch/push/PR). Per the skill audit, `cairn` is KEEP-class for the pool.

## Sequencing
1. `builder_repos.go` cairn clone-per-run behind `CW_VCS=cairn` (git stays default).
2. Live-verify: one pool ticket end-to-end (clone → express → commit → push → PR → gate), and a **concurrent** pair to confirm isolation holds in-cluster.
3. Generalize the `cairn` skill + update `development`.
4. Flip default to `cairn` for the nexus repo; keep GitHub as the remote.
5. (Later, independent) cairn server for full-fidelity lines + PRs-as-ledger-issues; and if cairn #81 lands, revisit shared-clone to reclaim the object-store efficiency.

## Non-goals
Moving the *server* off GitHub (this keeps the git remote). Changing the acceptance gate, PR, or dispatch mechanics — the builder still opens a PR on `builder/<ticket>` and is judged the same way; only the local VCS mechanics change.
