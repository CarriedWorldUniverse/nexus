---
name: lean-design-standard
description: Use when designing or cutting a module, system, or seam — or reviewing a change to one — and deciding where to draw a boundary, what to expose vs hide, whether to add an abstraction, whether to delete vs freeze, or where a correctness check belongs. Applies to the game engine, the nexus, or any project. Symptoms: a change keeps rippling into unrelated callers; tempted to add a "just-in-case" interface or unifying framework; a tool/platform promises a "10x"; unsure if a check belongs on the hot path.
---

# Lean Design Standard

Five verified design rules distilled from Parnas '72, Liskov & Zilles '74, Dijkstra '68, Hoare '69, Brooks '86.

**Core idea:** draw boundaries around what is *likely to change*; define them by their operations, not their representation; keep control flow and contracts checkable; spend scarce effort on *essence* (the game) not *accident* (plumbing). These are **heuristics, not gates** — advisory, and always subordinate to the game and the perf doctrine (GPU-first / CPU-off-frame).

**Honest scope.** These rules address **leaks/coupling, comprehensibility, contract-correctness, and complexity-triage**. They do **not** fix **latency** (the perf doctrine owns that — measure on real hardware) or **bloat** (only *deletion* fixes that; more abstraction never does). The source video's "latency/bloat solved decades ago" framing is a hook — keep only what the papers actually earn.

## When to use
- Designing/cutting a module, system, or seam — or reviewing a change to one.
- Symptoms: a change keeps rippling into unrelated callers; tempted to add a "just-in-case" interface or unifying framework; a tool/platform promises a "10x"; unsure whether a check belongs on the hot path; deciding delete vs freeze.
- **Not for:** perf/latency decisions (use the perf doctrine + measure), or pure YAGNI deletion (just delete).

## The five rules

| # | Rule | Check (the test) | Anti-rule — don't over-apply |
|---|------|------------------|------------------------------|
| **P1** Parnas | Cut by **secrets, not steps** — a boundary hides one decision *likely to change*. | If changing a hidden decision (storage layout, schema, mesher, chunk size) forces edits in **unrelated callers**, the seam is wrong. | Only wrap decisions that will *actually* change. Speculative seams are bloat and cost frame time. |
| **P2** Liskov | A type **is its operations**, not its representation. Keep the rep private; make the wrong shortcut *unrepresentable* at the call site. | Can a caller work using only the operations? Is the cheap-wrong path (analytic height) even reachable here? | Encapsulate at the **system edge, not the hot inner loop**. Keep perf cost visible. Leave the CLU type-ceremony. |
| **P3** Dijkstra | Control flow you can **map to execution** — lifecycles as explicit state enums; kernels straight-line. | If it stalls, can I name *which state* it's in from the text? | A guard-clause early return can be clearer *and* faster. Branch-minimal helps **GPU warps** (a hardware fact) — it is **not** a Dijkstra speed proof; branchy CPU code is often fine. |
| **P4** Hoare | State the **invariant**; assert it in **debug** (pre/post + invariants). | High-value seams: CA conservation (sum buffer before/after a tick); producer-post == consumer-pre at the GPU fence. | Debug-flag guarded, never on the shipping hot path. A bug-**detector**, not a guarantee (float drift). **Never fold latency into a correctness assert** — perf is a separate empirical budget on the real target GPU. No theorem provers. |
| **P5** Brooks | Spend on **essence**; build **accident** once, then stop. | Essential (CA rules, water, belief, NPC, economy, world model) → your hours. Accidental (GPU plumbing, scheduling, sampling, serialization) → solve once, freeze. | No tool/framework/platform 10x's the essence — no silver bullet. Sunk cost is about the past; authority must be checked against local constraints. Ignore Brooks's large-team apparatus. |

**M0 · Subordinate to the game and the perf doctrine.** Heuristics, not gates. Never override GPU-first/CPU-off-frame. Don't stop game work for theoretical purity — fix a seam where a real change *keeps rippling*, not as a standing audit. **The cure for bloat is deletion, not more abstraction.**

## Design mode — about to cut/add a boundary
Walk these briefly:
1. **P1** — What one decision does this hide? Is it *actually* likely to change? (Can't name it → don't add the seam.)
2. **P2** — Define it by operations. Is the wrong shortcut unreachable from callers?
3. **P3** — Is the control flow / lifecycle nameable as states?
4. **P4** — What invariant must hold at this seam? (assert in debug)
5. **P5** — Essential or accidental? If accidental and it works, stop.
6. **M0** — Serves the game without overriding the perf doctrine?

## Review mode — advisory, auditing a diff/file
For each of P1–P5, mark `✓ / ✗ / ⚠` with the **specific seam** + one line of rationale. Then an **over-application** pass: speculative seams, sealed reps in hot loops, proof ceremony, tool-shopping, framework-building. Output is advisory — the operator decides.

```
REVIEW: economy.gd
  ✗ P1: economy reads settlement._stockpiles/_census (private) — leak; go via accessors
  ✗ P2: analytic surface_height used — wrong path reachable; sample the real voxel
  ✓ P4: prices guarded for grain == 0
  ⚠ over-app: none
  → advisory; 2 seam leaks to fix before land
```

State the rule by **name**, not just the instance fix: "this is a P1 boundary leak" beats "add an accessor" — the named test transfers; the instance fix doesn't.

## Non-obvious calls (sharpenings)
- **Delete vs freeze (P2):** "dead surface" = called by **no client anywhere**. Survey *all* consumers first — game **and** other systems/pillars (a grep of only the game tree is wrong; e.g. one pillar reads another's config). Then **delete** the dead operations and freeze the minimal remainder. Reversibility is a reason to delete *after* the survey, not to hoard unused surface "just in case."
- **Branch-minimal (P3):** the correct lever for **GPU warp coherence** (divergent lanes serialize), not a Dijkstra speed proof. Don't rewrite branchy CPU code on borrowed authority — measure if perf is the question.
- **Correctness ≠ perf (P4):** the conservation assert is deterministic and runs anywhere (even CI); the tick-time budget is statistical and must be measured on the real target GPU after warmup. **Never one gate** — a flaky perf check poisons the trustworthy correctness check and gets bypassed (`--no-verify`).

## Provenance & examples
- `references/papers.md` — verified citations + what each paper actually established + scope caveats (this is the ADT/CLU paper, **not** the Liskov Substitution Principle; Hoare is *partial* correctness only; Brooks is about productivity, not runtime).
- `references/examples.md` — the strongest engine + nexus applications, adversarially checked.

Source: Macro Lens, *"Bloat, Leaks, Latency — All Solved Decades Ago. We Just Forgot"* (youtu.be/qbn2oX3Bds0).
