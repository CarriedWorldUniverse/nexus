---
name: orchestrator
description: Use when a task is big enough to split into pieces — plan and decompose on Opus (you), then hand the bounded/mechanical/parallelizable pieces to SONNET subagents (Agent tool, model:"sonnet") and verify + synthesize the results yourself. The always-on, permission-mode-free version of opusplan: Opus does the thinking, Sonnet does the doing, and because it's a MODEL choice (not Plan Mode) it composes with auto/bypass — no mode switch, no flow break. Saves Opus quota on execution-heavy work.
---

# Orchestrator — Opus plans, Sonnet subagents execute

## Why this exists
`opusplan` only routes to Opus while you're literally in **Plan Mode** — which forfeits auto/bypass and breaks flow, so in practice Opus rarely fires. This gets the same frontier-thinks / cheap-builds split **without a mode switch**: you (Opus) stay in control and delegate bounded work to **Sonnet subagents** via the Agent tool's per-call `model` override. Model choice, not permission mode → it just works inside auto/bypass.

## When to use
- A task decomposes into ≥2 independent or mechanical chunks: multi-file edits, write-N-tests, apply-a-pattern-across-files, parallel research/recon, migrations, sweeps, reviews-by-dimension.
- The **plan/judgment is the hard part**; execution is bounded once specified.

## When NOT to
- Small single-step tasks — dispatch+verify overhead exceeds just doing it.
- Work whose judgment can't be pre-specified (architecture, ambiguous tradeoffs, final verification) — that stays on Opus (you).

## The loop
1. **Plan (you/Opus).** Understand the task; decompose into units. For each: **Opus-keep** (judgment, cross-cutting, verify/synthesize) vs **Sonnet-delegate** (bounded, spec-able).
2. **Spec each delegated unit** so a Sonnet subagent succeeds without Opus-level judgment: exact **inputs**, the expected **output/artifact**, **acceptance criteria**, and any constraints/conventions to match. Vague spec → wrong result.
3. **Delegate.** Spawn Sonnet subagents: `Agent(prompt=<tight spec>, model="sonnet", subagent_type=<fit>)`. Run independent units **in parallel** — multiple Agent calls in ONE message. Use `Explore` for read/search units, `general-purpose`/`claude` for build units, `code-reviewer` for review units.
4. **Verify + synthesize (you/Opus).** Read each result **adversarially** — don't trust-and-merge (subagents are confident when wrong). Catch errors, integrate, resolve cross-unit conflicts. Re-delegate fixes with a sharpened spec.

## The split rule
| Opus keeps (you) | Sonnet subagents |
|---|---|
| architecture, decomposition, the plan itself | implement this function/module to spec |
| ambiguous tradeoffs, cross-cutting decisions | apply this change across N files |
| verification, synthesis, final judgment | write tests for X; run + report results |
| anything where a wrong call cascades | narrow research; mechanical refactor; recon |

Even-cheaper tier: `model="haiku"` for trivial mechanical bits (rename, reformat, extract, tabulate). Reserve Opus subagents (`model="opus"`) only for a delegated sub-problem that genuinely needs frontier reasoning.

## Scaling up
- **A handful of units** → the Agent tool (this skill). Lightest; composes with the current turn.
- **Large deterministic fan-out** (dozens of units, pipeline stages, adversarial multi-vote verify) → the **Workflow tool** instead — but it needs explicit user opt-in ("use a workflow" / ultracode). Same Opus-plans/Sonnet-executes idea, scripted: `agent(prompt, {model:"sonnet"})`, `parallel()`, `pipeline()`.

## Anti-patterns
- **Over-delegating** — a subagent for a 2-line edit. Dispatch+verify cost > doing it. Delegate chunks, not keystrokes.
- **Vague specs** — "improve the module" → the subagent guesses. Always: inputs, output, acceptance.
- **Delegating the judgment** — send the *bounded execution of a decision you made*, not the *decision*.
- **Trust-merge** — pasting subagent output unread. Verify every result; that's the Opus half of the job.
- **Serial when parallel** — independent units in separate messages waste wall-clock. Batch the Agent calls.
