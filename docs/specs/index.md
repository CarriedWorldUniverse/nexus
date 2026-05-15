# Specs

Design corpus. Every meaningful design decision lives as a dated spec in `docs/2026-*.md`. Reading order isn't strictly chronological — pick by topic.

## Foundational

- [Frame role spec](../2026-04-28-frame-role-spec.md) — what a Frame is and why it exists
- [Aspect-funnel architecture](../2026-05-02-aspect-funnel-architecture.md) — the load-bearing runtime architecture doc
- [Nexus transport spec](../2026-04-25-nexus-transport-spec.md) — WS frame schema
- [Nexus registration spec](../2026-04-22-nexus-registration-spec.md) — how aspects join

## Provider / deliberation

- [Provider adapter spec](../2026-04-24-provider-adapter-spec.md) — bridle's Provider interface
- [Funnel/bridle caching spec](../2026-05-03-funnel-bridle-caching-spec.md) — prompt-cache strategy
- [Funnel compaction design](../2026-05-01-funnel-compaction-design.md)
- [JSONL rewriter spec](../2026-05-10-jsonl-rewriter-spec.md) — per-turn session jsonl rewriting
- [Pi extract / bridle gaps](../2026-05-13-pi-extract-bridle-gaps.md) — design audit against Pi Code Agent

## Hands + work-routing

- [Hand dispatch v0.1](../2026-04-30-hand-dispatch-v0_1.md) — workers as one-shot subprocesses

## Observability + ops

- [Observability operator runbook](../2026-05-12-observability-operator-runbook.md)
- [Observability core + nexus-watch](../2026-05-12-nexus-watch-and-observability-core.md)
- [Funnel observability audit](../2026-05-12-funnel-observability-audit.md)
- [One-to-one observability plan](../2026-05-11-one-to-one-observability-plan.md)
- [Diligence pass results](../2026-05-11-diligence-pass-results.md)
- [Crossing cutover checklist](../2026-05-09-crossing-cutover-checklist.md)
- [Cutover runbook](../cutover-runbook.md)

## Dashboard + UI

- [Dashboard WS port spec](../2026-05-09-dashboard-ws-port-spec.md)
- [Operator as aspect (WS extension)](../2026-05-04-operator-as-aspect-ws-extension.md)
- [Unify Frame + aspect chat path](../2026-05-04-unify-frame-aspect-chat-path.md)
- [Avatar interface spec](../2026-04-29-avatar-interface-spec.md)

## Storage + auth

- [Storage abstraction spec](../2026-05-05-storage-abstraction-spec.md)
- [Storage abstraction review (plumb)](../2026-05-05-storage-abstraction-spec-review-plumb.md)
- [Files subsystem spec](../2026-05-04-files-subsystem-spec.md)
- [AWS bootstrap spec](../2026-05-01-aws-bootstrap-spec.md) — production deployment

## Skills + identity

- [Skill delivery design](../2026-05-15-skill-delivery-design.md)
- [Code standards doc](../2026-05-15-code-standards.md) — also see [policies/code-standards.md](../policies/code-standards.md)
- [Work routing policy](../2026-05-15-work-routing-policy.md) — also see [policies/work-routing.md](../policies/work-routing.md)

## Build planning + audits

- [Frame 65 build plan](../2026-05-01-frame-65-build-plan.md)
- [Frame stop decisions](../2026-05-01-frame-stop-decisions.md)
- [Issue triage analysis](../2026-05-05-issue-triage-analysis.md)
