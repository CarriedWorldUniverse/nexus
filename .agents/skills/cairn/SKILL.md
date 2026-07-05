---
name: cairn
description: Use when committing, pushing, branching, or managing Carried World code with the cairn VCS — the go-git-backed dogfood version control on dMon whose origin IS the carried-world-godot GitHub repo. Covers the working-change / line / express / fold model, the daily commit→push loop, the autosync + push-auto-reconcile behaviour, the full command surface, and the hard-won gotchas (commit ≠ push, message quoting over ssh, token-free push, protected-branch rejection, validating against the live tree not a stale clone).
when_to_use: 'When committing, pushing, branching, or managing Carried World code with the cairn VCS (the go-git dogfood VCS on dMon).'
---

# cairn — the Carried World VCS

cairn is a **go-git-backed** version-control system (org `CarriedWorldUniverse`). Two halves:
- **Working-copy CLI** — `github.com/CarriedWorldUniverse/cairn`, source `cmd/cairn/main.go` (a thin dispatcher over `internal/worktree.Repo`; the engine is `internal/{worktree,change,release,version,credstore,userconfig}`). The binary is `/usr/local/bin/cairn`. **This is what you run.** (Note: the local checkout at dMon `~/src/cairn` is a *stale, server-only* clone — `cmd/cairn-server` only; read the CLI from the GitHub repo's `main`.)
- **Server** — `cmd/cairn-server`: a go-git host (SSH casket-key → herald agent; HTTP via mTLS gateway, `X-CWB-*` identity), per-agent push attribution, `repo:read/write` scopes, branch protection (no force-push on the default branch), PRs-as-ledger-issues, **fast-forward-only server-side merge**.

Carried World is cairn-managed on dMon at **`~/Projects/carried-world-cairn/main`**, and **cairn's origin IS the GitHub repo `CarriedWorldUniverse/carried-world-godot`** — `cairn commit` writes local history; `cairn push` lands it on GitHub (a `git pull` then refreshes any backup clone). Identity = `nexus-cw` / `nexus@darksoft.co.nz` (`cairn config user.name|email`); the `github.com` token is in the credstore (`~/.config/cairn/credentials`), so pushes need no PAT on the command line.

## The mental model (the cool part — it's Jujutsu-like, not git-like)
- **The working change is always open.** Every line has a live, unsealed change at its tip — your on-disk edits ARE that change (no staging area; `log`/`blame` show it as `(working)`). `cairn commit <branch> -m "msg"` **seals** the open change (stamps the message) and **opens a fresh one**. So you never "create" a commit from nothing — you name the one you're already in.
- **Lines, not branches.** A repo is a TREE of lines (`cairn tree`); `main` is the structural root. `cairn status [branch]` reports a line's working change vs its parent (`branch / lineage / ahead / conflicts / expressed / changes`). `cairn ls` lists expressed lines with their `ChangeID`.
- **express = a line as a folder on disk.** `cairn express <branch> [--from <parent>]` materializes `<repo>/<branch>/` to edit. **Run any command from inside a branch folder and it acts on that line** (like git's current branch) — `commit`/`push`/`status` with no branch arg use it. `unexpress <branch>` removes the folder (`--force` to discard unsealed work).
- **commit reconciles against the parent.** Because it's git-backed, sealing reconciles the line against the latest parent — **you are always writing against the latest committed code**, branch or not. No stale-branch drift; conflicts surface early (`cairn resolve <branch> <path>`) instead of as a big-bang merge. Commit returns **exit 2** (not 1) when it recorded conflicts, so `cairn commit && cairn push` is script-safe.
- **fold = merge a line into its parent.** `cairn fold <branch>` (must be conflict-free; the server permits only ff on the default branch). Clean because the line never diverged.
- **Two remote fidelities.** A plain **git remote gets a projection** (ordinary git history — what GitHub sees). A **`--cairn` remote gets full fidelity** (the line tree + change-ids + open conflicts). `cairn remote add <name> <url> [--cairn]`.

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

## Sync behaviour (commit / push / pull)
- **`autosync` is the switch on commit.** With `autosync` set, `cairn commit` auto-syncs with origin (prints `auto-synced with origin` / `auto-sync skipped: …`). **Here it's UNSET**, so a commit is **local only** — you must `cairn push`. (A 22-commit arc once sat `ahead: 49` local-only until a push.)
- **`push` auto-reconciles divergence.** Bare `cairn push` (from the root) publishes **all lines + tags** and, if the remote diverged, pulls + 3-way-merges + retries once so "push just works" (silent on success; a merge conflict surfaces "resolve, then push"). A **single-line** push — `cairn push [remote] [branch]`, or a bare push from *inside* a branch folder — pushes just that line and does **not** auto-retry.
- **`pull`** = fetch + reconcile each local line against its remote, re-materializing expressed folders (conflicts reported, non-fatal). **`fetch`** = tracking refs only.

## Gotchas (hard-won)
1. **commit ≠ push (autosync unset).** Always `cairn push` to back up to GitHub. Verify on GitHub, not the local counter: `gh api repos/CarriedWorldUniverse/carried-world-godot/branches/main --jq .commit.sha` should equal the latest cairn sha. (Observed: the local `ahead` counter looked **sticky** post-push — the remote HEAD is ground truth.)
2. **Message quoting over ssh.** Use `-m`. Through `ssh 'cd … && cairn commit main -m "…"'`, **keep the message free of parentheses + shell-special chars** (they break the remote quote parse) — plain prose, `Co-Authored-By:` on its own line. **Sanitize push/log output**: `sed -E "s/gh[a-z]_[A-Za-z0-9_]+//g"`.
3. **Token-free push, with a precedence.** Auth resolves **`CAIRN_TOKEN` > `GITHUB_TOKEN` > `GITLAB_TOKEN` > credstore** (`cairn auth` lists hosts, never tokens; set via `echo $TOK | cairn login github.com`). So a plain `cairn push` uses the stored credential — no PAT on the command line. A bad credential maps to: *"authentication failed — set $CAIRN_TOKEN …"*.
4. **Protected-branch rejection ⇒ the PR workflow.** If origin's `main` is protected (PR-required), a direct push is rejected and cairn says: *"the branch is likely protected … if you folded or committed into this branch locally, `cairn undo` rewinds it; then push your own line and open a PR."* (CW's main isn't protected today, so direct pushes work — but this is why `express → fold` + a pushed line is the durable pattern.)
5. **Identity, or you get a placeholder.** Commits with no identity are stamped `…@users.noreply.cairn`; fix the whole history with `cairn reauthor --old-email '*@users.noreply.cairn' --name nexus-cw --email nexus@darksoft.co.nz`. cairn owns its identity (repo→global→`CAIRN_AUTHOR` env); never silently from git.
6. **`--repo <dir>`** for any subcommand if not at the repo root (default `.`; cairn walks up to `.cairn` like git). Run from `~/Projects/carried-world-cairn/main`.
7. **Validate against the cairn tree, NOT a stale clone.** A separate clone (`~/Projects/carried-world`, a shadow copy) goes stale until `git pull` — review/verify agents reading it see OLD code and raise false "won't compile / too many arguments" alarms. The live build **is** the cairn tree; audit there.

## Carried World deploy loop
- Edit working copies (`/tmp/bush/*.gd`, scratchpad `layout/*.gd`) → `scp` to `…/carried-world-cairn/main/stream/` → `cairn commit main -m` → `cairn push`.
- The running console builds from this tree and **auto-refreshes from source on every change** (a stale build is impossible).
- **Before relaunch, headless-validate the real compile**: `voxelgodot.bin --headless --path ./stream 2>&1 | grep -iE "SCRIPT ERROR|Parse Error|Compilation failed|Nonexistent|infer the type"` — `--check-only` MISSES GDScript type-inference errors (see `feedback_godot_validate_headless_compile`). Relaunch with `~/cw_console_up.sh`.

## Full command surface (grouped)
- **Working copy:** `init [dir]`, `clone <url> [dir]`, `express <branch> [--from p]`, `unexpress [--force]`, `commit <branch> -m`, `fold [--force]`, `reparent <branch> <parent>`, `abandon [--force]`, `status [branch]`, `diff [branch] | <a> <b>`, `tree`, `ls`, `resolve <branch> <path>`.
- **Remotes:** `remote [add <name> <url> [--cairn]]`, `push [remote] [branch] [--all] [--force]`, `fetch [remote]`, `pull [remote]`.
- **History (read):** `log [branch] [-n N]`, `show <commit>`, `blame <path> [branch]` (per-line change-id), `undo` (revert last op), `oplog`.
- **History (edit — rebases, can conflict → exit 2):** `reword <commit> <msg>`, `squash <commit>`, `drop <commit>`, `cherry-pick <commit> [branch]`, `reauthor --old-email <glob> --name <n> --email <e> [--dry-run]`.
- **Stash:** `stash [-m] [branch]`, `stash pop|list|drop [id]`.
- **Identity/auth:** `setup`, `config [--global] <key> [val]` (keys: `user.name`, `user.email`, `autosync`), `login <host>` (token on stdin), `logout <host>`, `auth`.
- **Versioning:** `tag <name> [branch]`, `version [--target npm|nuget|pypi|oci|go] [--release]`, `version bump <major|minor|patch>`, `release --target <eco> [--dry-run]`.
- **Privacy/embargo:** `private <path> [--shape-only]` / `private ls` (withhold a path from every push — omit, or placeholder bytes), `embargo <commit>` / `embargo ls` (hold a commit + descendants out of the *public projection* — gated, distinct from private), `disclose <path|commit>` (lift either).
- **Bisect:** `bisect start --good <c> --bad <c> [branch]`, `bisect good|bad|skip|status|reset`, `bisect run -- <cmd>` (0=good, 125=skip, else=bad).

Common flags: `--repo <dir>` (default `.`), `--author <name>` (else `$CAIRN_AUTHOR`).
