---
name: cairn
description: Use when committing, pushing, branching, or managing Carried World code with the cairn VCS — the go-git-backed dogfood version control on dMon whose origin IS the carried-world-godot GitHub repo. Covers the working-change / line / express / fold model, the git-reflex→cairn translation table (no staging, express not branch, fold not merge), the daily commit→push loop, the autosync + push-auto-reconcile behaviour, the full command surface, and the hard-won gotchas (commit ≠ push, message quoting over ssh, token-free push, protected-branch rejection, validating against the live tree not a stale clone, and checking WHICH cairn binary is on PATH before trusting odd behaviour).
when_to_use: 'When committing, pushing, branching, or managing Carried World code with the cairn VCS (the go-git dogfood VCS on dMon).'
---

# cairn — the Carried World VCS

cairn is a **go-git-backed** version-control system (org `CarriedWorldUniverse`). Two halves:
- **Working-copy CLI** — `github.com/CarriedWorldUniverse/cairn`, source `cmd/cairn/main.go` (a thin dispatcher over `internal/worktree.Repo`; the engine is `internal/{worktree,change,release,version,credstore,userconfig}`). The binary is `/usr/local/bin/cairn`; since v0.1.20 it self-updates — `sudo cairn update` installs the latest GitHub release (checksum-verified; `cairn update --check` just reports), so **never rebuild or hand-install the binary you run** (gotcha 8). **This is what you run.** (A source checkout — dMon `~/src/cairn`, croft likewise — is a FULL clone of the repo and will happily build a working `cairn`: it is for READING code, never for producing the binary on your PATH.)
- **Server** — `cmd/cairn-server`: a go-git host (SSH casket-key → herald agent; HTTP via mTLS gateway, `X-CWB-*` identity), per-agent push attribution, `repo:read/write` scopes, branch protection (no force-push on the default branch), PRs-as-ledger-issues, **fast-forward-only server-side merge**.

Carried World is cairn-managed on dMon at **`~/Projects/carried-world-cairn/main`**, and **cairn's origin IS the GitHub repo `CarriedWorldUniverse/carried-world-godot`** — `cairn commit` writes local history; `cairn push` lands it on GitHub (a `git pull` then refreshes any backup clone). Identity = `nexus-cw` / `nexus@darksoft.co.nz` (`cairn config user.name|email`); the `github.com` token is in the credstore (`~/.config/cairn/credentials`), so pushes need no PAT on the command line.

## The mental model (the cool part — it's Jujutsu-like, not git-like)
- **The working change is always open.** Every line has a live, unsealed change at its tip — your on-disk edits ARE that change (no staging area; `log`/`blame` show it as `(working)`). `cairn commit <branch> -m "msg"` **seals** the open change (stamps the message) and **opens a fresh one**. So you never "create" a commit from nothing — you name the one you're already in.
- **Lines, not branches.** A repo is a TREE of lines (`cairn tree`); `main` is the structural root. Since v0.1.38 a clone infers each imported branch's real parent from topology (a feature forked from develop sits under develop, `ahead` counts only its own commits); clones made earlier keep every branch flat under the root — `cairn reparent --infer` (v0.1.39; `--dry-run` first) re-derives them in place, or re-clone. `cairn status [branch]` reports a line's working change vs its parent (`branch / lineage / ahead / remote / conflicts / expressed / changes`). `cairn ls` lists expressed lines with their `ChangeID`.
- **`tree` draws the tree by name and shows where each line stands on the remote** (v0.1.40 / v0.1.42): `name  ahead=N  [pushed | ahead N | behind N | diverged +a/-b | unpushed | gone]`. `ahead` = sealed commits the PARENT line lacks (`git rev-list parent..line`), not "since clone" — so a PR that merged main in counts only its own commits. The `[state]` is as of the last fetch/push: `cairn tree --fetch` refreshes first, `--gone` lists only lines whose branch the remote deleted, `--flat` keeps the old id listing. `status` prints the same state on its `remote:` line.
- **History queries are cairn's own, not go-git's** (v0.1.45). go-git only stores objects and talks to remotes; merge bases, ancestry, `ahead`/`behind` and the clone-time fork point are generation-ordered and diff-tested against real `git`, so same-second or clock-skewed commits no longer skew them. The first command on an existing clone numbers every commit once (≈1.3s per 25k commits), then it is cached in `.cairn/cairn.db`.
- **express = a line as a folder on disk.** `cairn express <branch> [--from <parent>]` materializes `<repo>/<branch>/` to edit. **Run any command from inside a branch folder and it acts on that line** (like git's current branch) — `commit`/`push`/`status` with no branch arg use it. `unexpress <branch>` removes the folder (`--force` to discard unsealed work).
- **commit reconciles against the parent.** Because it's git-backed, sealing reconciles the line against the latest parent — **you are always writing against the latest committed code**, branch or not. No stale-branch drift; conflicts surface early (`cairn resolve <branch> <path>`) instead of as a big-bang merge. Commit returns **exit 2** (not 1) when it recorded conflicts, so `cairn commit && cairn push` is script-safe.
- **fold = merge a line into its parent.** `cairn fold <branch>` (must be conflict-free; the server permits only ff on the default branch). Clean because the line never diverged.
- **Two remote fidelities.** A plain **git remote gets a projection** (ordinary git history — what GitHub sees). A **`--cairn` remote gets full fidelity** (the line tree + change-ids + open conflicts). `cairn remote add <name> <url> [--cairn]`.

## Git reflex → cairn translation (corpus-gradient corrections)
Your training pulls toward git spellings. Where semantics match, cairn is already git-shaped (`status`, `log`, `diff`, `push`, `pull`, `stash`, `cherry-pick`, `tag`, `bisect` — use them as normal). Elsewhere, type the cairn verb from this table:

| Git reflex | cairn reality |
|---|---|
| `git add` / staging | **Does not exist.** On-disk edits ARE the open working change; go straight to `cairn commit <branch> -m`. |
| `git branch <n>` / `checkout -b <n>` / `switch -c <n>` | `cairn express <n>` (materializes the line as a folder). **Aliased.** Since v0.1.38 it forks from the line whose folder you are standing in — git's current-branch semantics — and from the root only at the repo root; `--from <parent>` overrides. |
| `git branch` (bare, to LIST) | `cairn tree` (the line tree) / `cairn ls` (expressed folders). |
| `git checkout <branch>` / `switch <branch>` | `cd <repo>/<branch>/` — lines are folders; being inside one selects it. |
| `git merge <branch>` | `cairn fold <branch>` (from the parent; must be conflict-free). **Aliased.** |
| `git rebase` | Automatic — every `commit` reconciles against the latest parent. Never needed. |
| `git commit -a` / `--amend` | Plain `commit` covers `-a` (no staging). Amend-a-message = `cairn reword <commit> <msg>`. |
| `git reset` / `git revert` | `cairn undo` (op-level) + `cairn oplog` to see what to undo; `cairn drop <commit>` removes a sealed commit. |
| `git rm` / `mv` | Just delete/move files on disk — the working change tracks it. |
| `git worktree` | Every expressed line already IS a folder — `cairn express` / `cairn ls`. |

**Since v0.1.24 typing the git verb is no longer a dead end** (cairn #139 / PR #144). The two rows marked **Aliased** (four spellings) are real synonyms that just run — `cairn merge x` IS `cairn fold x`, flags and all. Every other git verb above answers with its one-line translation *instead of* the 60-line usage wall, and exits 1. So a reflex miss now self-corrects in one shot — but the cairn verb is still the one to reach for, and the table is what you reach with.

Two edges worth knowing:
- **`checkout -b <name> <start-point>` is deliberately NOT aliased.** The start point names a parent, which express spells `--from`; cairn refuses and prints `cairn express <name> --from <start-point>` rather than silently forking off the wrong line.
- **Aliases need v0.1.24+.** On an older binary every git spelling is still a bare `unknown subcommand` + usage dump. `cairn --version` to check, `sudo cairn update` to fix (gotcha 8).

## Two workflows
**Daily (small change):**
```
edit → cairn commit main -m "what + why" → cairn push
```
**Feature arc (multi-commit) — the intended pattern (dogfoods branch/merge + survives a protected main):**
```
cairn express village-life                      # working line off main (a folder)
… edit → cairn commit village-life -m "…"  (repeat; reconciles vs main each time)
cairn fold village-life                         # merge the line into main (clean ff)
cairn push                                      # back up to GitHub origin
```

## Pool builder use (`CW_VCS=cairn`) — clone-per-run
When a dispatched builder runs with `CW_VCS=cairn`, the harness has **already** provisioned an isolated cairn working copy for the ticket before your turn: it `cairn clone`d a per-run copy, set the `nexus-cw` identity, pointed `origin` at the GitHub repo, and `cairn express`ed your line — and dropped you **inside that line's folder**. So you do NOT clone, express, or configure anything. You just:
```
… edit the files in this folder (your line) …
cairn commit <branch> -m "<what + why>"  &&  cairn push origin <branch>
```
- **`commit && push`, exit-checked** — the `&&` is mandatory. `cairn commit` exit **0** = sealed clean (push it); exit **2** = it recorded conflicts against a `main` that moved under you (run `cairn resolve <branch> <path>`, re-commit, THEN push — never push a conflicted line); exit 1 = error (surface it). Never push unconditionally: a failed commit followed by a push ships an empty/broken branch.
- **The branch name is fixed** — use the exact `builder/<ticket>` line the harness expressed; the acceptance gate finds your PR by it.
- Open the PR with `gh` (git projection) exactly as the git path does. The clone is disposed on despawn — nothing to clean up.
- This is *clone-per-run*: your copy is yours alone, so there is no cross-builder contention to think about. (Origin here is a **git remote** — GitHub — so `cairn push` publishes an ordinary-git projection that `gh` PRs against; nothing about the server/full-fidelity path applies.)

## Sync behaviour (commit / push / pull)
- **`autosync` is the switch on commit.** With `autosync` set, `cairn commit` auto-syncs with origin (prints `auto-synced with origin` / `auto-sync skipped: …`). **Here it's UNSET**, so a commit is **local only** — you must `cairn push`. (A 22-commit arc once sat `ahead: 49` local-only until a push.)
- **`push` auto-reconciles divergence.** Bare `cairn push` (from the root) publishes **all lines + tags** and, if the remote diverged, pulls + 3-way-merges + retries once so "push just works" (silent on success; a merge conflict surfaces "resolve, then push"). A **single-line** push — `cairn push [remote] [branch]`, or a bare push from *inside* a branch folder — pushes just that line and does **not** auto-retry.
- **`pull`** = fetch + reconcile each local line against its remote, re-materializing expressed folders (conflicts reported, non-fatal). **`fetch`** = tracking refs only.
- **`fetch`/`pull` prune, and pull tidies gone lines** (v0.1.42). Tracking refs for branches deleted on the remote are dropped, so `tree` stops calling them `pushed`. `pull` then **abandons every line whose branch is gone from the remote UNLESS it is expressed** (a folder means it is yours — it is listed as *kept*, not touched) and prints what it pruned; `cairn undo` brings them back, `pull --keep-gone` skips the pruning.
- **A conflicted pull** (a local commit and the remote changed the same lines) records a merge on the working change. `status` lists the conflict plus your own work — not the files the pull brought in (v0.1.44). Edit the markers out, `cairn resolve <branch> <path>` (v0.1.41 refuses ANY leftover `<<<<<<<`/`|||||||`/`=======`/`>>>>>>>` line, not just a complete block; `--force` if intentional), `cairn commit`, `cairn push`.

## Gotchas (hard-won)
1. **commit ≠ push (autosync unset).** Always `cairn push` to back up to GitHub. Check the push landed: `cairn tree --fetch` (or `status`'s `remote:` line) must say `[pushed]`; the remote HEAD is ground truth — `gh api repos/CarriedWorldUniverse/carried-world-godot/branches/main --jq .commit.sha`. `ahead` is NOT a push counter: it counts commits the parent line lacks, pushed or not. (Before v0.1.42 it walked first parents to the root and read as a sticky, ever-growing number — e.g. `ahead=19756` behind a merge.)
2. **Message quoting over ssh.** Use `-m`. Through `ssh 'cd … && cairn commit main -m "…"'`, **keep the message free of parentheses + shell-special chars** (they break the remote quote parse) — plain prose, `Co-Authored-By:` on its own line. **Sanitize push/log output**: `sed -E "s/gh[a-z]_[A-Za-z0-9_]+//g"`.
3. **Token-free push, with a precedence.** Auth resolves **`CAIRN_TOKEN` > `GITHUB_TOKEN` > `GITLAB_TOKEN` > credstore** (`cairn auth` lists hosts, never tokens; set via `echo $TOK | cairn login github.com`). So a plain `cairn push` uses the stored credential — no PAT on the command line. A bad credential maps to: *"authentication failed — set $CAIRN_TOKEN …"*.
4. **Protected-branch rejection ⇒ the PR workflow.** If origin's `main` is protected (PR-required), a direct push is rejected and cairn says: *"the branch is likely protected … if you folded or committed into this branch locally, `cairn undo` rewinds it; then push your own line and open a PR."* (CW's main isn't protected today, so direct pushes work — but this is why `express → fold` + a pushed line is the durable pattern.)
5. **Identity, or you get a placeholder.** Commits with no identity are stamped `…@users.noreply.cairn`; fix the whole history with `cairn reauthor --old-email '*@users.noreply.cairn' --name nexus-cw --email nexus@darksoft.co.nz`. cairn owns its identity (repo→global→`CAIRN_AUTHOR` env); never silently from git.
6. **`--repo <dir>`** for any subcommand if not at the repo root (default `.`; cairn walks up to `.cairn` like git). Run from `~/Projects/carried-world-cairn/main`.
7. **Validate against the cairn tree, NOT a stale clone.** A separate clone (`~/Projects/carried-world`, a shadow copy) goes stale until `git pull` — review/verify agents reading it see OLD code and raise false "won't compile / too many arguments" alarms. The live build **is** the cairn tree; audit there.
8. **Check WHICH cairn you run before debugging any cairn behaviour.** `~/.local/bin` precedes `/usr/local/bin` on PATH, so a hand-built binary there silently wins every invocation. That shadowed the real install for three weeks and produced a bogus "deletions don't survive commit OR pull" report (cairn #134) against a build 9 days older than the fix that closed it. Two cheap checks:
   - `which -a cairn` — more than one hit is a shadow; rename/remove everything that is not `/usr/local/bin/cairn`.
   - `cairn --version` — a number (`cairn 0.1.22`) is a release build. **`cairn dev` is a source build**: no version to compare, so `cairn update` refuses it without `--force`. It cannot self-heal out of this state, and updating `/usr/local/bin` does nothing while the shadow stands.
   Below v0.1.20 there is no `update` subcommand, so bootstrap once by hand: download `cairn_<ver>_linux_amd64.tar.gz` + `checksums.txt` from the release, `sha256sum -c`, then `sudo install -o root -g root -m 0755 cairn /usr/local/bin/cairn`. `cairn update` carries it from there. **Also check it is CURRENT: `cairn update --check`.** On 2026-09-04 croft's binary was found at 0.1.24 with 0.1.36 released — twelve releases of fixes missing, including the pull data-loss below — after a whole session of using it to push cairn's own PRs.
9. **A squash-merged PR leaves empty duplicates on local `main` that you CANNOT drop.** After GitHub squash-merges your line, `cairn pull` rebases your original commits on top of the squashed ones. Their diffs are empty (`cairn show <c>` prints the message and no hunks), the tree is byte-identical to origin (`cairn diff <local-tip> <origin-sha>` is empty) — but `ahead` inflates and `cairn drop` refuses them, because `main` is the root line. Harmless: pushes to a protected `main` are rejected anyway and a line expressed off `main` still diffs clean. Do NOT go walking `cairn undo` back through the pull to tidy it — that reverts express/unexpress bookkeeping too. Live with it, or re-clone the working copy. (Observed 2026-09-01 landing cairn #142/#139.)
10. **`abandon` unexpresses for you, and an abandoned name is dead.** `cairn abandon <b> --force` removes the folder itself — running `unexpress` first makes abandon fail with *"not expressed"*. Since v0.1.37 `express <b>` on an abandoned line is refused (*line is abandoned*): express under a NEW name with `--from` the old parent. Since v0.1.33 `push origin <b>` refuses it too (before that it printed *pushed* and pushed nothing). Only `status`/`commit`/`push` infer the branch from the folder you are in; `fold`/`unexpress`/`abandon` always take `<branch>`.
11. **Below v0.1.37, `cairn pull` DISCARDS un-sealed edits in every expressed folder** (cairn #182: reverted edits, deleted new files, silently). On an older binary run `cairn status <line>` — which snapshots — before ANY pull. And before opening a PR from a pushed line, assert the pushed diff is non-empty: `gh api repos/<o>/<r>/compare/main...<line> --jq '.files|length'`. A stale binary once pushed a tip identical to main and the PR merged +0 −0; nothing in the push output said so.
12. **Below v0.1.44, editing during a conflicted pull silently dropped the remote side** (cairn #195). Any sync while the conflict was open (an edit + `status`) rebuilt the merge on its local parent only; `commit` then re-marked the conflict you had just resolved, and `push` was rejected as non-fast-forward. `status` also listed every file the pull brought in as `M`. Recovery on 0.1.44+: `cairn pull` again, resolve, commit. Relatedly, below v0.1.43 a PR that merged main in could show an inflated `ahead` (seen: 28) — update, don't chase it.

## Big repos: what is slow, and how to SEE it (measured 2026-09-02)
Do not guess which phase is slow — cairn now tells you. `clone` prints a timed line per phase and a total the phases must add up to (v0.1.25 announced them, v0.1.29 timed them); `express`/`unexpress`/`fold`/`pull` print theirs too, including the working-copy sync. Quick read-only verbs stay silent and scriptable.

```
cairn: fetched objects in 2.8s
cairn: resolving the remote's default branch … 1ms
cairn: mapping branches onto the line tree … 999/999 1.2s
cairn: materializing … 40000/40000 files 11.4s
cairn: clone total 14.9s
```

- **`mapping branches` was O(branches × history)** until v0.1.26 — one full history walk per branch. On a repo with many branches and deep history that phase alone dominated the clone (52x faster after cairn #148). If an OLD binary is slow there, update before investigating anything else.
- **`materializing` scales with FILE COUNT, not bytes.** Reference points: 40k files / 259MB is ~11s on Linux/btrfs, but ~5ms/file on **NTFS with Defender** (≈3m16s for 40k) because every file creation is AV-scanned. `CAIRN_MATERIALIZE_WORKERS=<n>` overrides the pool (default 4, chosen from Linux where go-git's object store caps the gain); on a latency-bound filesystem try 16 or 32. A Defender exclusion on the repo directory is the other lever, and it is the operator's call.
- **`synced working copy` is paid before nearly EVERY command**, not just clone — it scans every expressed folder. v0.1.30 skips re-snapshotting a branch whose scan is unchanged AND whose working head has not moved, which took a 40k-file `status` from 3.5s to ~1.1s. If it is still slow, the branch genuinely changed or an old wc-cache is being upgraded (the first run after v0.1.30 re-records and is slower once).

## Carried World deploy loop
- Edit working copies (`/tmp/bush/*.gd`, scratchpad `layout/*.gd`) → `scp` to `…/carried-world-cairn/main/stream/` → `cairn commit main -m` → `cairn push`.
- The running console builds from this tree and **auto-refreshes from source on every change** (a stale build is impossible).
- **Before relaunch, headless-validate the real compile**: `voxelgodot.bin --headless --path ./stream 2>&1 | grep -iE "SCRIPT ERROR|Parse Error|Compilation failed|Nonexistent|infer the type"` — `--check-only` MISSES GDScript type-inference errors (see `feedback_godot_validate_headless_compile`). Relaunch with `~/cw_console_up.sh`.

## Full command surface (grouped)
- **Working copy:** `init [dir]`, `clone <url> [dir]`, `express <branch> [--from p]`, `unexpress <branch> [--force]`, `commit [branch] -m` (branch inferred inside a folder), `fold <branch> [--force]`, `reparent <branch> <parent>` | `reparent --infer [--dry-run]`, `abandon <branch> [--force]`, `status [branch]`, `diff [branch] | <a> <b>`, `tree [--fetch] [--gone] [--flat]`, `ls`, `resolve <branch> <path> [--force]`.
- **Remotes:** `remote [add <name> <url> [--cairn]]`, `push [remote] [branch] [--all] [--force]`, `fetch [remote]` (prunes), `pull [remote] [--keep-gone]`.
- **History (read):** `log [branch] [-n N]`, `show <commit>`, `blame <path> [branch]` (per-line change-id), `undo` (revert last op), `oplog`.
- **History (edit — rebases, can conflict → exit 2):** `reword <commit> <msg>`, `squash <commit>`, `drop <commit>`, `cherry-pick <commit> [branch]`, `reauthor --old-email <glob> --name <n> --email <e> [--dry-run]`. **`reword`/`squash`/`drop` are REFUSED on the root line** — `cairn: cannot edit history on the root line`. So a stray commit on `main` cannot be dropped; edit history on a child line, or live with it (see gotcha 9).
- **Stash:** `stash [-m] [branch]`, `stash pop|list|drop [id]`.
- **Identity/auth:** `setup`, `config [--global] <key> [val]` (keys: `user.name`, `user.email`, `autosync`), `login <host>` (token on stdin), `logout <host>`, `auth`.
- **Versioning:** `tag <name> [branch]`, `version [--target npm|nuget|pypi|oci|go] [--release]`, `version bump <major|minor|patch>`, `release --target <eco> [--dry-run]`, `update [--check|--force]` (self-update the binary from the latest GitHub release).
- **Privacy/embargo:** `private <path> [--shape-only]` / `private ls` (withhold a path from every push — omit, or placeholder bytes), `embargo <commit>` / `embargo ls` (hold a commit + descendants out of the *public projection* — gated, distinct from private), `disclose <path|commit>` (lift either).
- **Bisect:** `bisect start --good <c> --bad <c> [branch]`, `bisect good|bad|skip|status|reset`, `bisect run -- <cmd>` (0=good, 125=skip, else=bad).

Common flags: `--repo <dir>` (default `.`), `--author <name>` (else `$CAIRN_AUTHOR`).
