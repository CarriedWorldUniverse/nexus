---
name: writing-skills
description: 'Use when writing, editing, reviewing, or pruning a skill (SKILL.md) — the craft reference for making skills predictable: invocation choice, description writing, information hierarchy, leading words, and the named failure modes (premature completion, duplication, sediment). Also use for the periodic pruning pass over ~/.claude/skills/.'
when_to_use: 'When writing, editing, reviewing, or pruning a skill (SKILL.md) — invocation choice, description writing, information hierarchy, leading words, failure modes, and the pruning pass.'
---

# Writing skills well

A skill exists to wrangle **determinism out of a stochastic system**. The root virtue is **predictability** — the agent taking the same *process* every run (not producing the same output). Every rule below serves it.

(Adapted from mattpocock/skills `writing-great-skills`, fitted to our conventions. Our prior rule stands: **skills = procedures + the tools to run them; facts/state/decisions → memory; needed-every-turn → CLAUDE.md.** A skill that is mostly facts is a memory file wearing the wrong hat.)

## Invocation: two costs, pick one

- **Model-invoked** (has a rich `description`): the agent can fire it autonomously and other skills can reach it. Costs **context load** — the description sits in the window every turn, forever.
- **User-invoked** (`disable-model-invocation: true`, human-facing one-line description): zero context load, but costs **cognitive load** — the operator is the index that must remember it exists.

Pick model-invocation only when the agent must reach it on its own or another skill must reach it. Our per-role `SkillAllowlist` work sharpens the stakes: every model-invoked description a role can see is context that role pays for on every turn — scope tightly.

## The description

Two jobs: state what the skill is, and list the **branches** that trigger it. It earns the hardest pruning in the file:
- Put the skill's **leading word** first.
- **One trigger per branch** — synonyms restating one branch are duplication; collapse them.
- Cut identity that's already in the body; keep triggers + any "when another skill needs…" reach clause.

## Information hierarchy

Two content types — **steps** (ordered actions) and **reference** (rules/definitions consulted on demand) — mix freely. Place each piece on the ladder by how immediately the agent needs it:

1. **In-skill step** — ends on a **completion criterion** that is *checkable* (done vs not-done decidable) and, where it matters, *exhaustive* ("every modified file accounted for", not "produce a list"). A vague criterion invites premature completion. This is the same rule as the broker's observable-acceptance-criteria doctrine — one discipline, two homes.
2. **In-skill reference** — a flat peer-set of rules on one rung is fine, not a smell.
3. **External reference** — pushed to a sibling file behind a **context pointer**, loaded only when it fires. The pointer's *wording* decides whether the agent reaches it.

**Progressive disclosure** = moving material down the ladder so the top stays legible. The branching test: inline what every branch needs; push behind a pointer what only some branches reach. **Co-location**: keep a concept's definition, rules, and caveats under one heading — reading one part brings its neighbours.

## Leading words

A **leading word** is a compact concept already in the model's pretraining that anchors a region of behaviour in one token (*tracer bullet*, *fog of war*, *red*, *seal*, *fold*). It serves predictability twice: in the body it anchors execution; in the description it anchors invocation. Hunt for restatements begging to collapse: "fast, deterministic, low-overhead" → *tight*; "a loop you believe in" → *red*. Fewer tokens AND a sharper hook. (Our own examples already work this way: cairn's *express/fold/seal*, the gate work's *judge-the-diff*, the cognitive-core's *pool/belief*.)

## When to split

Each cut spends one of the two loads — split only when the cut earns it:
- **By invocation**: a distinct leading word should trigger independently, or another skill must reach it. Pays context load.
- **By sequence**: the steps still ahead tempt the agent to rush the current one — hide the post-completion steps. Use only when the completion criterion is irreducibly fuzzy AND the rush is observed; sharpening the criterion is cheaper and comes first.

## Failure modes (diagnose by name)

- **Premature completion** — ending a step before it's genuinely done. Defence in order: sharpen the completion criterion (cheap, local); only then split the sequence.
- **Duplication** — one meaning in two places. Costs maintenance + tokens, and inflates the meaning's rank on the ladder. Keep a **single source of truth** per meaning.
- **Sediment** — stale layers that settle because adding feels safe and removing feels risky. *The default fate of any skill without a pruning discipline.* Our verify-the-deployed-artifact lesson generalizes: a skill's text drifts from practice exactly like a manifest drifts from a deploy.

## The pruning pass (run this over a skill, or the whole library)

For each skill:
1. **Hat check** — is any section facts/state/decisions? Move it to memory; leave a pointer if a branch needs it.
2. **Description audit** — one trigger per branch, leading word front-loaded, no body-identity restated.
3. **No-op hunt** — test each *sentence* in isolation; a sentence that changes no behaviour is deleted whole, not trimmed.
4. **Duplication sweep** — every meaning has one home; collapse restatements into the leading word.
5. **Ladder check** — anything inline that only one branch reaches → push behind a pointer; anything behind a pointer that every run needs → pull up.
6. **Criterion check** — every step ends on a checkable, exhaustive-where-it-matters completion criterion.
7. **Sediment date** — if a line describes a practice we no longer follow, delete it now; "might need it" is what memory and git history are for.

Completion criterion for the pass itself: every skill in scope has had all seven checks applied and each resulting edit either made or explicitly declined with a reason.
