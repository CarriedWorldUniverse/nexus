# Model selector — the brain grid

**Status:** measured, 2026-07-07 · **Author:** shadow · **Feeds:** `AUTO-ROUTING-DESIGN.md` (this doc IS the classifier's rubric + the escalation ladder).

## What this is

A numeric, **lower-is-better** table for picking a builder's brain per run. Not a leaderboard — a *decision aid*. Two-step rule:

1. **Floor by capability.** Drop every brain that can't clear the run's tier at all (a stalled/blocked brain is disqualified regardless of price).
2. **Cheapest-that-clears on the run's priority columns.** Among survivors, score only the columns this run actually cares about (a sovereignty-critical run weights `Sov`; a burn-conscious run weights `$`), and take the lowest sum.

The point the grid proves: **capability ≠ sticker price, and "newer/bigger" ≠ better.** Effort is a real cost dial; the least token-efficient brain that clears is the newest Sonnet at high reasoning; the cheapest-that-clears is a *low-effort* Claude.

## The selector (numeric, 1 = best on that axis)

Scales are 1–5, lower = better. `Cap` = can it clear **complex**. `Tok` = output-token cost bucket (the real cost driver — the fleet is output-bound). `$` = marginal dollar cost. `Sov` = sovereignty (local/owned). `Eff` = effort-dial control. `Lat` = wall-clock. `Wire` = wired & proven.

| brain | Cap | Tok | $ | Sov | Eff | Lat | Wire | cost class |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|---|
| **ornith** (local GB10) | 5* | 1 | 1 | 1 | 2 | 1 | 1 | FREE / sovereign |
| **sonnet-4.6 @low** | 1 | 1 | 2 | 3 | 1 | 2 | 1 | subscription |
| **sonnet-5 @low** | 1 | 1 | 2 | 3 | 1 | 1 | 1 | subscription |
| **sonnet-5 @medium** | 1 | 2 | 2 | 3 | 1 | 2 | 1 | subscription |
| **opus-4.8** (default) | 1 | 2 | 2 | 3 | 1 | 2 | 1 | subscription |
| **opus-4.8 @low** | 1 | 3 | 2 | 3 | 1 | 2 | 1 | subscription |
| **sonnet-5 @high** | 1 | 3 | 2 | 3 | 1 | 3 | 1 | subscription |
| **sonnet-4.6** (default) | 1 | 4 | 2 | 3 | 1 | 3 | 1 | subscription |
| **deepseek-reasoner** (v3) | 2 | 5 | 3 | 3 | 3 | 4 | 1 | metered-prepaid |
| **deepseek-v4-pro** | 4 | 5 | 3 | 3 | 3 | 4 | 2 | metered-prepaid |
| **deepseek-v4-flash** | 2? | 4? | 3 | 3 | 3 | 4 | 3 | metered-prepaid |
| **deepseek-chat** | 4 | 4 | 3 | 3 | 3 | 2 | 1 | metered-prepaid |
| **glm-4.6** | 5 | — | 2 | 3 | 3 | 4 | 2 | subscription |

\* `ornith` Cap=5 is **for the complex tier only** — locally it fails at *building* hard things. It is Cap=1 for classification / judge / simple: bounded, single-turn, structured-output work is its sweet spot (drives the classifier + the acceptance judge). Don't read Cap=5 as "weak model" — read it as "wrong tool for complex builds."

### Cost classes (the `$` column decoded)
Three kinds of "cost", not one:
- **FREE / sovereign** — ornith on the local GB10. No marginal cost, no external dependency. Always prefer where capability allows.
- **subscription / $0-marginal** — Claude (code CLI + API), GLM. No per-token bill, but tokens **burn quota / rate-limit headroom** — token-efficiency still matters, it just isn't dollars.
- **metered-prepaid** — DeepSeek **only**. Real dollars off a prepaid balance; depletes. Cheap per token, but the *only* brain that spends actual money, so its high token counts convert to real (small) spend.

## The evidence (raw grid, n=1 per cell)

Task E1 across every cell: *implement funnel-v2 §2 workspace eviction* (a real, scoped complex ticket). `met` = acceptance judge (ornith-judge) confirmed a PR with the required implementation.

| cell | met | out_tokens | wall_s | note |
|---|:--:|--:|--:|---|
| sonnet-4.6 @low | ✓ | **11,407** | 411 | cheapest-that-clears |
| sonnet-5 @low | ✓ | **11,686** | 261 | ~tied cheapest, fastest |
| sonnet-5 @medium | ✓ | 18,658 | 381 | |
| opus-4.8 (default) | ✓ | 19,922 | 411 | |
| opus-4.8 @low | ✓ | 28,432 | 531 | > opus-default → see variance caveat |
| sonnet-5 @high | ✓ | **36,659** | 652 | least token-efficient that clears |
| sonnet-4.6 (default) | ✓ | 54,747 | 1103 | |
| deepseek-reasoner (v3) | ✓ | 153,307 | 1284 | clears at ~13× the low-Claude token cost |
| deepseek-v4-pro | ✗ blocked | 155,110 | 1494 | newer ≠ better: did NOT clear |
| deepseek-v4-flash | ~ | (uncaptured) | 1825 | opened mergeable PR #430; verdict not scraped |
| deepseek-chat | ✗ blocked | 60,800 | 532 | |
| glm-4.6 | ✗ stalled | 4,772 (partial) | 1464 | thin PR #436 then idle-timeout; not a complex brain |

### What the numbers say
1. **Effort is a real, monotonic cost dial.** sonnet-5: 11.7k → 18.7k → 36.7k for low → medium → high. The `--effort` knob (#425) works and *is* the primary cost lever within a brain.
2. **Operator hypothesis confirmed locally:** sonnet-5 @high (36.7k) costs **more** output than opus-4.8 @low (28.4k). High-reasoning Sonnet is not the cheap option people assume.
3. **Cheapest-that-clears complex = a low-effort Claude** (sonnet-4.6 @low ≈ sonnet-5 @low, ~11.5k) — not the biggest model, not the metered one.
4. **DeepSeek reasoners clear but at ~8–13× the tokens**; metered-pennies makes that *affordable* but slow (1284s) and un-sovereign. v4-pro (newest) did **not** clear — "newer" bought nothing here.
5. **GLM-4.6 can't hold the complex tier** (stalled). Keep it for simple/cheap roles, not complex builds.

### Caveats (read before trusting a single cell)
- **n=1 per cell.** The `opus-4.8 @low (28.4k) > opus-4.8 default (19.9k)` inversion is almost certainly run-to-run variance, not "low effort costs more." Trust **buckets and within-brain trends** (the clean sonnet-5 effort ladder), not exact single values.
- **Task-shape confound.** E1 implements a section that *already exists on main* (#422). A low-effort model can verify + open a thin PR and pass the gate cheaply, which flatters low-effort token counts. The transferable signal is the **relative ordering**, not the absolute floor.
- The two deepseek-v4 cells and glm are messy (scraper misses, stalls) — treat their rows as directional, not precise.

## Capability ordering → the escalation ladder

For `AUTO-ROUTING-DESIGN.md` Unit 2 (escalate-on-block), cheapest→dearest among brains that *can* clear complex:

```
ornith(simple only) → sonnet-4.6:low → sonnet-5:low → sonnet-5:medium → opus-4.8:low → sonnet-5:high → opus-4.8:medium → deepseek-reasoner(metered fallback)
```

- **Default complex brain:** `sonnet-5:low` (or `sonnet-4.6:low`) — cheapest-that-clears, fast.
- **On honest block:** climb the ladder (bounded, cap 2 rungs per `AUTO-ROUTING-DESIGN.md`).
- **Sovereignty-first runs:** try `ornith` for anything simple/bounded; only leave the GB10 when capability demands.
- **Excluded from the complex ladder:** glm-4.6 (stalls), deepseek-v4-pro (blocked), deepseek-chat (blocked). Keep GLM/deepseek-chat for the *simple* tier where cheap-and-good-enough wins.

## Pipeline
grid → **this doc** → classifier system-prompt (Unit 1 rubric) + escalation ladder (Unit 2). When a cell is re-measured, update the table here; the router picks it up with no code change.
