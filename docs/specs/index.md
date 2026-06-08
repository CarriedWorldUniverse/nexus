# Specs

Design corpus. Every meaningful design decision lives as a dated spec in `docs/2026-*.md`. Reading order isn't strictly chronological — pick by topic.

## Foundational

- [Frame role spec](../archive/2026-04-28-frame-role-spec.md) — what a Frame is and why it exists
- [Aspect-funnel architecture](../archive/2026-05-02-aspect-funnel-architecture.md) — the load-bearing runtime architecture doc
- [Nexus transport spec](../archive/2026-04-25-nexus-transport-spec.md) — WS frame schema
- [Nexus registration spec](../archive/2026-04-22-nexus-registration-spec.md) — how aspects join

## Provider / deliberation

- [Provider adapter spec](../archive/2026-04-24-provider-adapter-spec.md) — bridle's Provider interface
- [Funnel/bridle caching spec](../archive/2026-05-03-funnel-bridle-caching-spec.md) — prompt-cache strategy
- [Funnel compaction design](../archive/2026-05-01-funnel-compaction-design.md)
- [JSONL rewriter spec](../archive/2026-05-10-jsonl-rewriter-spec.md) — per-turn session jsonl rewriting
- [Pi extract / bridle gaps](../archive/2026-05-13-pi-extract-bridle-gaps.md) — design audit against Pi Code Agent

## Hands + work-routing

- [Hand dispatch v0.1](../archive/2026-04-30-hand-dispatch-v0_1.md) — workers as one-shot subprocesses
- [Commonplace build graphs + Graphify](../2026-06-07-commonplace-build-graphs-and-graphify-recommendation.md) — Graphify as extractor reference, Commonplace as agent build-knowledge graph

## Observability + ops

- [Observability operator runbook](../archive/2026-05-12-observability-operator-runbook.md)
- [Observability core + nexus-watch](../archive/2026-05-12-nexus-watch-and-observability-core.md)
- [Funnel observability audit](../archive/2026-05-12-funnel-observability-audit.md)
- [One-to-one observability plan](../archive/2026-05-11-one-to-one-observability-plan.md)
- [Diligence pass results](../archive/2026-05-11-diligence-pass-results.md)
- [Crossing cutover checklist](../archive/2026-05-09-crossing-cutover-checklist.md)
- [Cutover runbook](../cutover-runbook.md)

## Dashboard + UI

- [Dashboard WS port spec](../archive/2026-05-09-dashboard-ws-port-spec.md)
- [Operator as aspect (WS extension)](../archive/2026-05-04-operator-as-aspect-ws-extension.md)
- [Unify Frame + aspect chat path](../archive/2026-05-04-unify-frame-aspect-chat-path.md)
- [Avatar interface spec](../archive/2026-04-29-avatar-interface-spec.md)

## Storage + auth

- [Storage abstraction spec](../archive/2026-05-05-storage-abstraction-spec.md)
- [Storage abstraction review (plumb)](../archive/2026-05-05-storage-abstraction-spec-review-plumb.md)
- [Files subsystem spec](../2026-05-04-files-subsystem-spec.md)
- [Credential custodian design](../archive/2026-06-04-credential-custodian-design.md) — AI-usable password, 2FA, and third-party credential custody
- [Nexus token custodian design](../archive/2026-06-03-nexus-token-custodian-design.md) — per-aspect Herald/CWB token custody
- [AWS bootstrap spec](../archive/2026-05-01-aws-bootstrap-spec.md) — production deployment

## Skills + identity

- [Skill delivery design](../archive/2026-05-15-skill-delivery-design.md)
- [Code standards doc](../2026-05-15-code-standards.md) — also see [policies/code-standards.md](../policies/code-standards.md)
- [Work routing policy](../2026-05-15-work-routing-policy.md) — also see [policies/work-routing.md](../policies/work-routing.md)

## Build planning + audits

- [Frame 65 build plan](../archive/2026-05-01-frame-65-build-plan.md)
- [Frame stop decisions](../archive/2026-05-01-frame-stop-decisions.md)
- [Issue triage analysis](../archive/2026-05-05-issue-triage-analysis.md)
