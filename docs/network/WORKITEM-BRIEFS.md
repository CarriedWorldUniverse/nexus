# Cutting work into units — the brief authoring standard

**Status:** standard, 2026-07-08 · **Author:** shadow · **Pairs with:** `OBSERVABLE-CRITERIA.md` (that doc governs how a unit's DoD is *phrased*; this one governs how work is *cut* into units) and `ROLE-MODEL.md` (the `work_item` contract the brief fills).
**Provenance:** slicing vocabulary adapted from mattpocock/skills `to-tickets` (MIT), fitted to the pool's contract and our live lessons.

## The principle

**A unit is a tracer bullet: a narrow but complete path, sized to one fresh context window, verifiable on its own.**

The pool's economics enforce this. NET-36 (unshaped brief, whole-feature scope) cost 3.1M input tokens and 94 steps; NET-47/48 (shaped briefs, single slices) were one-turn ~2min verified passes. The gate hardening can verify any honest unit — but only the brief decides whether the unit is *small and complete enough* for an honest pass to exist.

## Slicing rules

1. **Vertical, not horizontal.** Each unit cuts a narrow but COMPLETE path through every layer it touches (schema + logic + CLI/API + tests) — not one layer of many features. A horizontal slice ("all the schemas") is never demoable and forces cross-unit trust.
2. **Verifiable alone.** A completed unit is demoable or gate-checkable on its own: its own PR, its own observable criteria, no "will make sense after unit 4."
3. **One fresh context window.** If the worker needs prior units' conversation to understand the brief, the cut is wrong. Everything the unit needs rides in the brief: task_spec, file excerpts if load-bearing, acceptance criteria, conventions. (Funnel §1 makes input cheap via prefix cache — the constraint is coherence, not tokens.)
4. **Prefactor first.** If a small mechanical change would make the real change easy, that prefactor is its own unit, blocked-on by the rest. "Make the change easy, then make the easy change."
5. **Declare blocking edges.** Each unit names the units that must complete before it can start (ledger link or "Blocked by" in the brief). A unit with no blockers is immediately dispatchable. The orchestrator works the **frontier** — any unit whose blockers are all done; parallelism falls out instead of being planned.

## The exception: wide refactors → expand–contract

A **wide refactor** — one mechanical change (rename a column, retype a shared symbol) whose blast radius fans across the codebase — cannot land as a vertical slice; a single edit breaks every call site at once. Sequence it as **expand–contract**:

- **Expand:** add the new form beside the old; nothing breaks. One unit.
- **Migrate:** move call sites over in blast-radius-sized batches (per package/per directory), each batch its own unit blocked by the expand. CI stays green batch to batch because the old form still exists.
- **Contract:** delete the old form once no caller remains — one unit, blocked by every migrate batch.

This is also the tested answer to concurrent same-file conflicts in the pool (the "close + cheap re-land ticket on fresh main" strategy is a degenerate expand–contract): sequence the batches instead of racing them.

## Anti-staleness rules

- **No file paths or code snippets in briefs** — they go stale between authoring and dispatch. *Exception:* a decision-rich snippet (schema, state machine, type shape — often from a prototype) that encodes a decision more precisely than prose; inline the decision-rich part only.
- **Brief against the live tree, not a clone.** Validate any claim about current code at dispatch time (the stale-clone false-alarm lesson generalizes to briefs).

## Brief contents (fills the `work_item` contract)

- **What it delivers** — end-to-end behaviour from the consumer's perspective, not a layer-by-layer implementation list.
- **Acceptance criteria** — per `OBSERVABLE-CRITERIA.md`, every clause checkable from artifact + evidence. Include the branch convention (`builder/<ticket>`) so `prExists` and provenance ride for free.
- **Blocked by** — unit references, or "none — immediately dispatchable."
- **Base knowledge** — only what this unit needs (per-role skill allowlists keep the rest out of context).

## Checklist (before seeding a unit)

- [ ] Vertical: touches every layer its behaviour needs, and no other features.
- [ ] Verifiable alone: own PR, own observable criteria, demoable without siblings.
- [ ] Self-contained: a fresh context window plus this brief suffices — no conversational carry-over.
- [ ] Prefactoring extracted into its own blocked-on unit.
- [ ] Blocking edges declared; frontier is non-empty.
- [ ] Wide refactor? → expand–contract sequence, not a forced slice.
- [ ] No stale paths/snippets except decision-rich ones.
