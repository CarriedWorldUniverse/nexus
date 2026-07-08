---
name: ticket-pipeline
description: Use to run a whole ticket/prompt end-to-end, solo, as the orchestrator — intake+clarify with the operator, decompose and fan out to builder subagents, then reviewer → security-validator → assemble a PR → a free local-AI (Ornith) review pass with comments → out for external human review. The full ticket-to-PR lifecycle in one shadow session, operator kept in the loop only for questions. Builds on the `orchestrator` skill + the builder/reviewer/security-validator agent defs.
when_to_use: 'When running a whole ticket/prompt end-to-end solo as the orchestrator — intake+clarify, fan out builders, review, security-validate, assemble a PR, Ornith comment pass, out for human review.'
---

# Ticket pipeline — Opus orchestrates ticket → PR, solo

You (Opus) drive the whole lifecycle. Workers are tiered subagents (`.claude/agents/`): **builder** (sonnet/medium), **reviewer** (sonnet/high), **security-validator** (sonnet/high). The operator is pulled in only to answer questions. This is the standalone-session version of the nexus dispatch pipeline — not a nexus run.

## Input
A ticket or raw prompt. If it's a **ledger ticket**, read it (and close/update it at the end). Otherwise treat the prompt as the ticket.

## The stages

**1 — Intake + clarify (you/Opus) — the operator gate.**
Read the ticket + the relevant code. Form the plan. Then split what's unresolved into **facts vs decisions**: a *fact* is answerable from the codebase/cluster/docs — look it up yourself, never ask the operator something grep can answer. Only *decisions* (trade-offs, scope edges, taste, spend) go to the operator. Surface all independent decisions **at once** via `AskUserQuestion`, each with your **recommended answer first** — operator answers once → you proceed uninterrupted. Exception — **dependent decisions don't batch**: when B only makes sense after A is answered (a design-tree walk), grill one question at a time down the tree, still recommending at each step. Only re-interrupt later if a builder hits a genuine blocker you can't resolve. Don't start building with unresolved must-know decisions.

**2 — Decompose → build (builder subagents, parallel).**
Split into independent, tightly-specced units (see `orchestrator` skill for the split rule + spec quality). Fan out **builder** subagents — multiple `Agent(subagent_type="builder", …)` calls **in one message** for parallelism. Each spec: inputs, expected output/artifact, acceptance criteria, conventions. Use **worktree isolation** if builders touch overlapping files (`isolation:"worktree"`).

**3 — Review (reviewer subagent).**
Feed the combined change to a **reviewer** subagent. Must-fix findings → loop back to a builder with a sharpened spec, then re-review. Don't advance with open must-fix items.

**4 — Security validate (security-validator subagent).**
Once review is clean, run a **security-validator** subagent over the change. Real vulnerabilities → back to build → re-review + re-validate. Don't advance with unresolved high/critical.

**5 — Assemble the PR (you/Opus).**
Integrate the threads, resolve cross-unit conflicts, run the full build/test suite yourself. Branch off main, commit (per the repo's commit convention), push, open a **GitHub PR** via `gh` with a clear summary + what was verified. (cairn later — for now GitHub.)

**6 — Local-AI review + comments (Ornith — a CALL-OUT, not a subagent).**
Ornith can't be an Agent subagent (Anthropic-only). So: send the PR diff to **Ornith** — `gh pr diff <n>` piped to `claude-ornith -p "review this diff, list concrete issues"` (or the litellm API). Take its comments and **post them on the PR** (`gh pr comment`). This is the free sovereign first pass before a human spends time — flag Ornith's comments as such.

**7 — External review (human).**
Leave the PR **open for human review**, with the AI comments attached. Report to the operator: PR link, what shipped, review+security summary, Ornith's notes. Done — the human decides merge.

## Loop control
Findings flow back to you; you re-spec and re-delegate (build → re-review → re-validate) until review + security are clean, then assemble. You own the plan, the verification, and the integration; subagents own bounded execution.

## Anti-patterns
- **Skipping the intake gate** → building on wrong assumptions. Ask everything first.
- **Advancing past a red review/security stage** → the gates are the point.
- **Trust-merging subagent output** unread → you verify; that's the orchestrator half.
- **Serial builders** for independent units → batch the Agent calls.
- **Over-decomposing** a small ticket → if it's one clear change, just build it (skip the fan-out), but still review + security + PR.
