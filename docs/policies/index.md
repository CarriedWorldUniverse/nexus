# Policies

Governance documents that aspects (and operator) follow. Versioned in Drive; copied into this site on every doc build.

| Policy | Purpose | Status |
|---|---|---|
| [Code standards](code-standards.md) | 11 disciplines: errors-as-data, idempotency, closed enums, wire-order, config/secrets split, more | v0 |
| [Work routing](work-routing.md) | How chat messages classify into lanes (planner-dispatch / worker-execution / operator-drive / casual) and how aspects route between them | v1.2 |
| [Git workflow](git-workflow.md) | Trunk-based + worktree-anchored + review-gated. Every change goes through PR | v0 |

These compose. Code standards governs *how* code is written; work routing governs *which aspect picks up which task*; git workflow governs *how the work merges*.

## Convention

Policies are amended via PR (per git-workflow.md, dogfooded). Updates flow Drive → docs/policies/ via copy. A small follow-up workflow can automate that sync once the cadence justifies it.
