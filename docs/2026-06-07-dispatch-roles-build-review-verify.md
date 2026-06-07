# Dispatch roles — build / review / verify (design note)

**Date:** 2026-06-07
**Status:** design note / recommendation
**Relates:** the Recursive Cost-Routed Dispatch design (`2026-06-07-recursive-dispatch-routing-design.md`), NEX-434, NEX-481, NEX-489.

## Question

Should the dispatch system encode roles — builder, reviewer, verifier — the way the Claude subagent-driven workflow does (implementer → spec-reviewer → code-reviewer)?

## Recommendation

**Yes to the pattern. No to encoding it as dispatch infrastructure.**

Support build → review → verify, but express roles as **briefs/skills**, keeping the dispatch mechanism and the builder pool **role-agnostic**. A "reviewer" is just a dispatch whose brief says *"review this PR against this DoD and return a verdict"* — not a new Job type.

## Why do the pattern

- **Quality.** Two-stage review demonstrably catches plausible-but-wrong work. (Concretely: the review pass on the Runner-hardening PR caught a mutex-held-across-I/O bug and a squash-merge regression that would otherwise have shipped.)
- **Throughput / bottleneck.** Review today is either the builder's own loop-termination judge (a self-check, not a review of the output) or **shadow reviewing the PR**. The moment dispatch fans out to N parallel builders, a single human-ish reviewer is the serialization point that defeats the parallelism. Review must fan out too.
- **Cost fit.** Review is cheap relative to building, so it is exactly the kind of work the cost-router wants to run on a cheap tier. "Verify-cheap" is good spend.

## Why NOT typed roles in the dispatch layer

In the Claude workflow, "implementer" / "spec-reviewer" / "code-reviewer" are **the same agent primitive given different prompts** — the role lives in the prompt, not the infrastructure. Baking a role enum (`builder`/`reviewer`/`spec-reviewer`/`verifier`/…) into dispatch would be:

- **over-engineering** — a rigid enum we'd keep extending, and
- **the wrong seam** — pool slots should stay generic agents that run whatever brief, exactly as they are now.

So dispatch stays role-agnostic; roles are a brief/skill convention on top.

## What this actually needs (mostly already in the design)

1. **Briefs that reference prior work** — a PR URL / a parent run's output — so a reviewer has something to review. Small addition; the brief already carries repo/ticket/thread.
2. **Sequencing + aggregation** — build → review ordering and verdict collection. This *is* the recursive design's decomposition DAG (dependent chunks sequence) plus aggregation/synthesis (piece 4). A "verify" node sits alongside execute / decompose.
3. **Role-scoped credentials (optional)** — the one place a notion of role earns real infra: a reviewer gets read-only git, a builder gets write. Even this can be brief-requested at the custodian layer rather than a first-class type.

## Near-term payoff (no full tree required)

On the dispatch we are standing up now (NEX-489): after a builder opens a PR, the orchestrator dispatches a **review brief** to another pool slot → it reviews against the DoD → posts a verdict. That alone takes shadow out of the critical path and is buildable on the generic, role-agnostic dispatch.

## Bottom line

Do build/review/verify via **prompts + the decomposition DAG + a "review brief" convention**. Keep dispatch role-agnostic. Let "role" touch infrastructure only where it gates **credential scope**.
