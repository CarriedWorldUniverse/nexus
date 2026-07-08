# Auto-routing — sovereign classifier + escalate-on-block

**Status:** spec, 2026-07-06 · **Author:** shadow · **Depends on:** `MODEL-SELECTOR.md` (the grid-filled selector table — the classifier's rubric; not written until the brain grid completes).

## Why

The role-brain tier + effort knob let us *set* a builder's brain per dispatch, but the *choice* is static today (`ORCHESTRATOR_ROLE_BRAINS` config). We want the choice made **automatically at runtime, per ticket**, without ceding it to a third-party router (OpenRouter etc. — a for-profit middleman on the load-bearing path; sovereignty regression). litellm is already our gateway; the *decision* belongs in our orchestrator.

The operator's design: **a cheap local model reads the ticket and returns the tier.** Ornith (thinking off) does the meta-decision; the expensive brains only ever touch the actual work — the "orchestrate with cheap models" pattern applied to routing itself. Classification is Ornith's proven sweet spot (the grid shows it clears bounded, single-turn, structured-output tasks reliably; it fails at *building* hard things, not at *reading* them).

Two units, in order. Unit 1 picks; Unit 2 catches a wrong pick. **Advisory + escalation:** the classifier's pick is a hypothesis, never final — a cheap router is *allowed* to guess and be corrected, which is what keeps it cheap.

---

## Unit 1 — Ticket classifier (the router)

### Shape
A one-shot cheap-model call returning structured JSON — a direct clone of the acceptance verifier (`nexus/frame/funnel/acceptance.go`: `AcceptanceVerifier.Verify` → `json.Unmarshal` of a `{...}` object). New: `nexus/orchestrator/classifier.go` `TicketClassifier`.

```
Input:  ticket (TaskSpec + AcceptanceCriteria + Repo?) + the selector rubric (MODEL-SELECTOR.md, embedded/loaded)
Model:  Ornith via litellm, --effort/thinking OFF, temperature 0 (deterministic-as-possible)
Output: {tier, brain, effort, confidence, reason}
          tier    ∈ {simple, complex}
          brain   ∈ the role-brain string it maps to (e.g. "openai:deepseek-reasoner", "claude-code:claude-sonnet-5:low")
          effort  ∈ {"", low, medium, high}   (never past high)
          confidence ∈ {low, medium, high}
          reason  free text (audit trail)
```

### Rubric = the selector table
The classifier's system prompt IS `MODEL-SELECTOR.md`'s scoring rubric + the grid numbers: the need-axes (task-shape / correctness-stakes / cost / sovereignty), the per-brain scores, and the two-step rule (floor by capability, then cheapest-that-clears on the run's priorities). One pipeline: **grid → MODEL-SELECTOR.md → classifier prompt.** When the grid refines a cell, the rubric updates; no code change.

### Wiring (orchestrator, `drain.go` `dispatchOne`, ~line 80)
Today: `rolePrompt, skills, policy, brainProvider, brainModel, brainEffort := o.resolve(role)` — the role's *static* brain. Change: when a classifier is configured AND the work item did not already pin a brain (respect an explicit `--role builder-complex` / `WorkItem.Personality` — human override wins), call `o.classify(ctx, wi)` first; its `{brain, effort}` becomes the PoolItem's Provider/Model/Effort, overriding the static role default. Precedence, tightest-first:
```
explicit work-item brain (--role tier, --personality) > classifier pick > static role-brain default > launch default
```

### Fail-open, always
The classifier is an *optimization*, never a gate. Any failure — Ornith unreachable, unparseable JSON, low confidence below a threshold — falls through to the **static role-brain default** (today's behaviour), logged one line. A router that can hang the pipeline is worse than no router. (Same posture as the acceptance verifier's fail-open.)

### Cost of the router itself
One short Ornith completion per ticket, thinking off — cents-of-a-cent / free (local), sub-second. Negligible against the work it routes. Log its output-token count anyway (the fleet already tracks this) so router overhead is visible.

### Tests
- classify returns structured pick; maps tier→brain per rubric.
- fail-open: Ornith error / bad JSON / confidence<threshold → static default, no dispatch failure.
- explicit work-item brain is NOT overridden by the classifier (human override wins).
- deterministic-ish: same ticket → same pick (temp 0), asserted with a fake model.

---

## Unit 2 — Escalate-on-block (the backstop)

### Why advisory needs a net
A cheap router *will* sometimes under-tier a hard ticket. That's fine **iff** an honest block bumps it up. Classifier + escalation together are robust even when the classifier is imperfect — cheap-fast routing with a correctness floor.

### Trigger (already exists)
The worker ends `blocked` → the acceptance gate / no-PR path → the orchestrator already emits `orchestrator-work-item-blocked` (`nexus/orchestrator/result.go:53`) and the ledger has a real `StatusBlocked` (`workgraph/types.go:26`, `adapter.go:352`). The escalation hook is that block signal — no new detection needed.

### The loop
On a work item transitioning to `blocked`:
1. Read an **escalation ladder** (config, ordered cheapest→dearest, e.g. `ornith → deepseek-reasoner → sonnet-5:low → sonnet-5:high → opus-4.8:medium`). The ladder is derived from `MODEL-SELECTOR.md`'s capability ordering.
2. Look up the brain the just-blocked run used; pick the **next rung up**.
3. **Requeue** the work item (the existing `Cancel(requeue=true)` / reset-to-queued path — same mechanism the reap-loop fix uses) with the escalated brain pinned (stamp it on the work item so `dispatchOne` uses it, bypassing the classifier — this is now an escalation, not a fresh guess).
4. **Bound it.** A per-item escalation counter (stored on the work item / a small orchestrator map). At the top of the ladder → stop, leave `blocked`, alert. No infinite climb, no runaway spend. Cap default 2 rungs.
5. **Idempotency** vs the existing reap/requeue: an escalation requeue must be distinguishable from a stale-worker reap requeue (which re-runs the *same* brain). Tag the requeue reason so reap and escalation don't fight (mirrors the ledger-status recheck the reap-loop fix added).

### Interaction with existing verification
Escalation only fires on **honest block** (gate said not-met / no-PR after budget) — NOT on a *verified pass* and NOT on a stale-worker reap. So a confabulated pass (caught by the gate → block) escalates correctly; a genuine "impossible task" (NET-30 style) climbs the ladder then stops at the top with an alert (a human sees it, as it should).

### Tests
- blocked item at rung N → requeued at rung N+1 with that brain pinned, escalation count++.
- at top rung → stays blocked, alert, no further requeue.
- escalation requeue ≠ reap requeue (tag/branch); the two don't double-dispatch.
- a verified pass never escalates; a stale-worker reap re-runs same brain, not escalated.

---

## Sequencing

1. **Grid finishes** → real numbers per brain/effort cell.
2. **`MODEL-SELECTOR.md`** written (rubric + numbers + capability ordering + escalation ladder).
3. **Unit 1 (classifier)** — rubric = the doc.
4. **Unit 2 (escalation)** — ladder = the doc's ordering.

Units 1+2 are both env-gated and fail-open/bounded — dark unless configured, reproducing today's static routing exactly when off. Same rollout posture as every unit in this line. The result: **ticket in → Ornith reads it, picks a brain → pool worker runs it → honest block climbs the ladder → verified pass ships** — the whole loop sovereign, the decision local, the expensive brains touched only when the work (or a failed cheaper attempt) demands them.
