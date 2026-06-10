# Recursive Cost-Routed Dispatch — Design

> **Status (as of 2026-06-11):** the recursive broker-inline Runner shipped and is the live dispatch path (dispatch-controller retired), but the *faceless builder-pool / per-run identity* model below was superseded by **named-agent dispatch** (`2026-06-08-named-agent-dispatch-model.md`) — work runs as a real named team member, not an anonymous pool slot. The recursion + cost-routing framing here remains the reference; the identity model does not.

> **Status:** design (brainstormed with the operator, 2026-06-07). Supersedes the flat `dispatch-controller` model. Precedes per-piece specs + implementation plans. The gemma tier is now validated empirically (2026-06-07; see §3 and *Local tier — GPU budget*); the rest of the ladder (Gemini slotting) and the routing judge are tuned from logged costs.

## One-liner

**The Claude-workflow / subagent orchestration model, lifted onto the nexus network — recursive, observable, and cost-routed by scoping complexity down each layer, decomposing only when the cost tables say it pays.**

## Problem (current state)

- **Noise + mis-identity.** Dispatch rides *visible chat messages* — a JSON brief to `@dispatch-controller`, plus the controller's "spawned/completed" status posts. That's network noise, and it makes the controller look like a chatting aspect when it's really plumbing. The original `!dispatch` intent was a **broker-intercepted skill/process trigger** (the `!` runs a process; it is *not* delivered as chat), which the current flow lost.
- **Flat, not recursive.** One controller spawns one builder per agent (per-agent serialize, NEX-464). No decomposition, no fan-out, no nesting.
- **No cost/complexity awareness.** A task goes to one builder on its (historically pinned) model regardless of how well-scoped it is or how much reasoning it needs.
- **Doesn't scale.** Can't decompose a large task into parallel slices, and can't route spend to fit the work.

## Vision

The operator talks to **shadow** — one conversation. shadow is the root orchestrator: decomposition, dispatch, and execution happen **behind the scenes**, and shadow reports back **only as needed**. The full flow is **traced** (recreatable) so that *when something breaks* it can be investigated — but it is not something to watch live.

Work flows as a **recursive tree**: shadow decomposes into chunks (parallel where independent) → dispatches them → each worker **either executes or decomposes-and-dispatches further** → recursing until each slice is small enough to just do. The "agent" is no longer a pinned personality+model; the orchestrator **routes each chunk to the cheapest capable model**, and **decomposition is the lever that lowers each slice's required tier** — pushing spend down the ladder. But decomposition is not free, so it happens **only when the cost math says it pays**.

## Architecture

### 1. Recursive dispatch tree
Every node — shadow at the root, then each worker, then their sub-workers — runs the same loop:
1. Receive a chunk (task + context).
2. **Execute or decompose?** (the routing judge, §2). Small + well-scoped enough to just do, or too big/complex?
3. If executable → run it on the routed model → return result.
4. If not → decompose into smaller chunks **as a small DAG** (mark independent chunks → run in parallel; dependent chunks → sequence) → dispatch each recursively → collect → synthesize → return.

Divide-and-conquer. Base case = "small enough to execute cheaply." **Bounded recursion depth** and a **concurrency cap** prevent runaway trees.

### 2. The routing judge (four factors)
A judge runs **on chunk receipt** and decides **execute vs decompose**, then selects the model. It is itself cheap (Tier-3 triage) and weighs four factors:

- **Capability (tier):** how much reasoning does this need → which tier (§3).
- **Cost:** real per-model token price, from cost tables (ccusage-style accounting — measured, not vibes), plus **scarcity/budget** (premium tiers are rate-capped, not just pricey).
- **Availability:** live rate-limit headroom per model/provider.
- **Latency/throughput:** how fast tokens generate (matters for interactive paths; hidden by parallelism for background fan-out).

**Character** (conversational vs tool-heavy) is the within-tier tiebreaker.

**Execute-vs-decompose is a cost decision**, not "complex → always decompose": it compares `direct-execute cost (cheapest capable tier)` against `decompose cost (the split + N cheap slices + aggregation)`. Decomposition pays off for large/complex work and loses for small/clear work. **When it decides to decompose, the split itself is assigned a capable (Tier-1) model** — decomposing well (clean slices, correct dependency DAG) is reasoning-heavy, done once, and amortized over the slices: spend *up* on the split, run the slices *cheap*.

The judge estimates pre-hoc, so it will sometimes be wrong → **adaptive escalation**: execute on a cheap tier, and if it struggles/fails, escalate or decompose. Logged costs (§5) tune the judge offline.

### 3. Capability / provider / character map (three tiers)
Capability is **three tiers**; cost, availability, latency, and character vary *within* a tier.

| Tier | Models | Notes |
|---|---|---|
| **1 — high reasoning** | Opus ≈ GPT-5.5 (med effort); Gemini Pro (~here) | ~equal capability. **claude = personal/conversational, gpt = tool-heavy.** Scarce/rate-capped — reserve. |
| **2 — workhorse** | deepseek ≈ sonnet ≈ GPT-5.5-fast; **gemma (reasoning-on)** ≈ here; Gemini Flash | most execution lands here |
| **3 — cheap/fast** | gemma (reasoning-off), haiku (tentative) | triage, judging, simple well-scoped slices |

Key model profiles:
- **gemma (local, on the 5090): validated as a real Tier-2 workhorse (2026-06-07).** A genuinely usable LLM — **not frontier, but most work doesn't require frontier**, and decomposition lowers each slice to exactly this tier. Measured ~40–52 tok/s warm: correct Go (`MergeIntervals`), a clean decomposition with a correct dependency DAG, reliable JSON judging, and it caught a subtle empty-slice panic in a bug-find. **Free, quota-free**; its only real constraint is **slower token generation**, which **parallel decomposition hides** (N independent slices → wall-clock ≈ one slice). So gemma is the **default workhorse for background fan-out**, not merely the floor (see *Local tier — GPU budget* below). Reserve fast cloud models for the **latency-sensitive conversational seat** (shadow ↔ operator).
- **Gemini:** mostly a **provider-diversity** play — a separate (Google) quota pool → more failover headroom per tier. Family spans Tier 1–2/3; the operator's small sub is ~Flash tier.
- **claude vs gpt at Tier 1:** equal capability; route by character — claude holds the conversation/orchestration, gpt does tool-heavy execution (and tools are scoped around conversations).

> **Validated (2026-06-07):** gemma-12B = solid Tier-2 (profile above + GPU-budget below). **Still to validate:** the Gemini family's exact slotting.

### Local tier — GPU budget & elasticity (dMon, measured 2026-06-07)
The local tier runs on dMon's **discrete RTX 5090 (24 GB)**. dMon has two GPUs — the discrete card and an **integrated (CPU-based) GPU** that drives the display — so the discrete's full 24 GB is compute/render, with no desktop overhead. Measured footprint:

- **gemma-12B (QAT q4):** **2 parallel × 128k context ≈ 10.6 GB** loaded (model 7.2 + ~3.4 GB KV). Gemma's **sliding-window attention** keeps long-context KV modest (~1.4 GB per 128k slot), so a generous real-work context is affordable. 2×128k was chosen for fewer threads + bigger context; 4×256k was rejected (left only ~1.6 GB free).
- **voxcpm (voice):** ~6.4 GB, single (one wav per request); a per-request VRAM leak (crept to ~16 GB/day) was fixed (`inference_mode` + `empty_cache`).
- **Headroom:** **~7.2 GB free** on the discrete for Unity rendering — dMon is a dev machine first.

**Elasticity is the key property:** gemma's keep-alive unloads it after ~5 min idle, returning its ~10.6 GB — so the local tier **yields to Unity** when the operator is rendering, and **reclaims the GPU for parallel dispatch** when it's idle. The "parallel local slices" claim above and the "gemma = always-available floor" below both rest on this measured budget: 2 concurrent 128k slices, free and quota-free, deferring to interactive work.

### 4. Route-around hierarchy (rate-limit resilience)
A 429 / quota-exhaustion is a **routing signal, not a failure**. Because within-tier models are ~equivalent, the preference order is:
1. **Within-tier failover (free, lossless):** swap to an equal-capability peer (Opus↔GPT-5.5↔Gemini-Pro at Tier 1; deepseek↔sonnet↔gpt-fast↔gemma at Tier 2).
2. **Defer:** backoff-and-queue until the window resets — when the whole tier is capped *and* quality matters.
3. **Drop a tier / decompose to fit:** the only *lossy* move, taken only when quality can absorb it. Decomposition doubles as evasion — slice the work down to tiers that are available; **gemma (local, quota-free) is the always-available floor**, so decomposed-small-enough work can always run.

### 5. Cost-logged recreatable trace (observability)
Every node records: its chunk, the execute-vs-decompose decision, the chosen model, the **priming** it handed each sub-chunk, the **result**, and the **actual token cost**. This is the orchestration tree — queryable/replayable for post-mortem (lands in the feed/ledger). It serves double duty: the **failure-investigation** tool *and* the **feedback that tunes the routing judge** (did decomposing that task actually beat direct-execute? — measured, not guessed). Behind the scenes by default; shadow surfaces only progress/blockers/results.

### 6. Interface
Operator ↔ shadow, one conversation. shadow is the root orchestrator; everything below is automatic and quiet until it needs the operator or it's done.

## Decomposition pulls triple duty
The same decomposition step buys: **(a) lower cost** (slices run on cheaper tiers), **(b) parallelism** (independent slices fan out), and **(c) rate-limit resilience** (slices fit available tiers; gemma's slowness hidden). It is applied **only when the cost tables say it pays**, and the split is done by a capable model.

## Build order (five pieces)
1. **Recursive dispatch mechanism** — execute-or-decompose-and-dispatch, broker-intercepted `!dispatch` (no chat noise, controller de-aspected), recursive, parallel, **per-run identity** (replaces NEX-464 per-agent serialize). *The spine; supersedes the flat dispatch-controller.* **Build first.**
2. **Cost-logged trace** — land early so every run is measured from day 1 (feeds the judge tuning).
3. **Routing judge** — cheap triage; the four-factor policy; cost table + live availability; capable-decompose. Tuned on the trace data.
4. **Aggregation/synthesis** — collect a DAG of results up the tree.
5. **Tuning** — validate the tier map (gemma capability test), tune the judge from real logged costs.

## Relationship to existing infra
- **bridle / the frame provider seam** is the model-switch substrate (keel already runs gemma-turn / deepseek-judge through it). Rate-limit failover and tier routing ride this seam — adding the *availability signal* + *fallback policy*, not a new provider layer.
- **Supersedes** the flat `dispatch-controller` + NEX-464 per-agent serialize (→ per-run identity). Folds in the NEX-480 hardening items.
- **Relates to** NEX-434 (k3s dispatch epic), NEX-451 (thread-based builder context), NEX-478 (judges→gemma — already partly true: builder judges run deepseek).
- **The Claude Workflow tool** is the in-process reference for the decomposition primitives (parallel / pipeline / nested) — and a **bootstrap**: shadow can orchestrate via in-process workflows now while the network-native version is built.

## Open questions / to validate
- **gemma's true tier** — ✅ validated 2026-06-07: solid Tier-2 (see §3 + *Local tier — GPU budget*).
- **Gemini family slotting** — measure.
- **Judge estimate-uncertainty** — the adaptive-escalation policy + how aggressively to decompose by default.
- **Representation** — deliberately deferred. The requirement is a recreatable *trace*, not a live UI; how (and whether) the tree surfaces in threads/feed is a later, lower-priority call.
- **Limits** — recursion depth cap, concurrency cap, and how budget/scarcity is enforced (hard ceiling vs soft preference).
