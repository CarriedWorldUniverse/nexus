---
name: remote-ops
description: 'Use when constructing ssh/kubectl commands to dMon or robo-dog, or when waiting on long/background jobs — quoting patterns that survive the remote shell, the zsh traps, remote python-heredoc, background-wait discipline, and token sanitizing.'
when_to_use: 'When constructing ssh/kubectl commands to dMon or robo-dog, or waiting on long/background jobs — quoting patterns, zsh traps, remote heredocs, background-wait discipline, token sanitizing.'
---

# Remote ops — commands that survive the wire

Transcript audit (2026-07-08) found ~25 shell failures and ~12 wait-frictions with recurring signatures. Every rule below is one of those signatures, inverted.

## Quoting (the failure class)

1. **One quote layer only.** Wrap the whole remote script in single quotes; **never nest single quotes inside** (`unmatched '` / `unexpected EOF` were repeat offenders). Need quotes inside? Use double quotes inside the single-quoted wrapper, or go to rule 2.
2. **Anything non-trivial → heredoc, not quote-juggling.** Two blessed forms:
   - Remote python: `ssh host 'python3 - <<'"'"'EOF'"'"' … EOF'` — or simpler, pipe it: `cat script.py | ssh host python3 -`.
   - Remote multi-command: write the script locally, `scp` or pipe it: `ssh host 'bash -s' < script.sh`. Past ~5 lines of remote logic, a piped script beats inline quoting every time.
3. **Section separators: never `echo ===`.** zsh (old Mac sessions) and even bash-with-eval choke on bare `==` (`== not found`). Use `echo "== label"` (quoted, single pair) — the convention that works everywhere.
4. **Don't use zsh-reserved variable names** in anything that might run under zsh: `status` is read-only there (3 failures). Prefer `st`, `rc`.
5. **Sanitize token-bearing output**: pipe through `sed -E "s/gh[a-z]_[A-Za-z0-9_]+//g"` on any push/auth/log output (cairn skill rule, generalized).
6. **Message/args with parens or shell-specials over ssh** break the remote parse — keep commit messages and quoted args to plain prose (cairn gotcha, applies to all remote commands).

## Waits and background jobs (the friction class)

7. **Never `sleep N; check`** — the harness blocks sleep-chains (4 hits). To wait on a condition: `Bash(run_in_background: true)` with `until <check>; do sleep 5; done` — one completion notification. For recurring events: Monitor.
8. **Polling loops get a bounded lifetime**: `for i in $(seq 1 N)` + break-on-done, never `while true` in a foreground call — 8 timeout kills in the logs were unbounded or over-long polls. Size the timeout to the real operation (`timeout:` param up to 600000).
9. **Long remote jobs**: `nohup … > log 2>&1 &` on the remote, return immediately, then wait with rule 7 against the log/pidfile. Don't hold the ssh session open for the job's duration.

## Path + existence discipline

10. **Don't guess paths on remote or local — verify first** (`ls`/`test -d`) before `cd`/Read: 18 file-does-not-exist failures were guessed paths (`~/shadow/src/cairn` vs `~/src/cairn`). One `ls` is cheaper than one failed round-trip.
11. **kubectl on dMon needs sudo from croft's non-interactive ssh**: `sudo k3s kubectl …` (jacinta's interactive shell has sans-sudo config; ssh sessions don't).

## Completion criterion

A remote command block is ready to run when: single quote-layer (or piped script), no bare `==`, no zsh-reserved names, token-sanitized if auth-adjacent, bounded wait, and any assumed path verified in the same block or a prior one.
