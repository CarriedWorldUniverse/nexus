# Specs

Design corpus. Every meaningful design decision lives as a dated spec in `docs/2026-*.md`. This index leads with the **current** corpus (dispatch-native, herald-rooted identity, the roundtable, agent-skills, the work UI) and keeps the foundational early specs in an **archive** section below. Reading order isn't strictly chronological — pick by topic.

## Current — dispatch-native runtime

The live shape of the runtime: aspects as on-demand cloud pods, named-agent dispatch, recursive fan-out.

- [Dispatch-native platform architecture](../2026-06-08-dispatch-native-platform-architecture.md) — the lean dispatch-hub model
- [Dispatch pod + home model](../2026-06-08-dispatch-pod-and-home-model.md) — napping presence + the pod home
- [Named-agent dispatch model](../2026-06-08-named-agent-dispatch-model.md) — work runs as a real named team member
- [Recursive dispatch routing](../2026-06-07-recursive-dispatch-routing-design.md) · [mechanism plan](../2026-06-07-recursive-dispatch-mechanism-plan.md)
- [Dispatch roles — build / review / verify](../2026-06-07-dispatch-roles-build-review-verify.md)
- [Funnel hooks + bridle passing](../2026-06-08-funnel-hooks-and-bridle-passing-design.md) · [hooks survey (CC + Gemini)](../2026-06-08-hooks-survey-cc-gemini.md)

## Current — identity + CWB platform

How agents get a herald-rooted identity and reach the gRPC pillars behind the gateway.

- [Herald-rooted agent bootstrap](../2026-06-03-herald-rooted-agent-bootstrap-design.md) — boot by name from an owner-seed-derived key
- [Nexus↔CWB gateway design](../archive/2026-06-03-nexus-cwb-gateway-design.md) · [plan](../archive/2026-06-03-nexus-cwb-gateway-plan.md)
- [Herald auth/register handshake](../archive/2026-06-03-herald-auth-register-handshake-design.md)
- [Aspect-side bootstrap + enroll](../archive/2026-06-03-aspect-side-bootstrap-and-enroll-design.md)
- [Credential custodian design](../archive/2026-06-04-credential-custodian-design.md) — AI-usable credential custody
- [Nexus token custodian design](../archive/2026-06-03-nexus-token-custodian-design.md) — per-aspect herald/CWB token custody
- [K3s work dispatch design](../archive/2026-06-05-k3s-work-dispatch-design.md) — orchestrator + on-demand workers as k8s Jobs
- [Nexus hosting on dMon](../archive/2026-05-29-nexus-hosting-dmon-design.md) · [bringup plan](../archive/2026-05-30-dmon-nexus-bringup-plan.md)

## Current — roundtable + skills

- [Roundtable design](../2026-06-11-roundtable-design.md) — multi-agent deliberation in a thread ([p1](../2026-06-11-roundtable-p1-plan.md) · [p2](../2026-06-11-roundtable-p2-plan.md) · [p3](../2026-06-11-roundtable-p3-plan.md) · [p4](../2026-06-11-roundtable-p4-plan.md) plans)
- [Agent-skills system design](../2026-06-09-agent-skills-system-design.md) · [MCP plan](../2026-06-09-agent-skills-mcp-plan.md) · [wiring plan](../2026-06-09-agent-skills-wiring-plan.md)
- [Commonplace build graphs + Graphify](../2026-06-07-commonplace-build-graphs-and-graphify-recommendation.md)

## Current — work UI

The operator-facing surface for watching and steering the agent team, phased.

- [Phase 1 — watch](../2026-06-09-work-ui-phase1-watch-design.md) ([plan](../2026-06-09-work-ui-phase1-watch-plan.md))
- [Phase 2 — control](../2026-06-09-work-ui-phase2-control-design.md) ([plan](../2026-06-09-work-ui-phase2-control-plan.md))
- [Phase 3 — converse](../2026-06-09-work-ui-phase3-converse-design.md) ([plan](../2026-06-09-work-ui-phase3-converse-plan.md))
- [Phase 4 — configure](../2026-06-09-work-ui-phase4-configure-design.md) ([plan](../2026-06-09-work-ui-phase4-configure-plan.md))
- [Phase 5 — mobile](../2026-06-09-work-ui-phase5-mobile-design.md) ([plan](../2026-06-09-work-ui-phase5-mobile-plan.md))

## Current — policies + standards

- [Code standards](../2026-05-15-code-standards.md) — also see [policies/code-standards.md](../policies/code-standards.md)
- [Developer standards](../2026-05-17-developer-standards.md)
- [Work routing policy](../2026-05-15-work-routing-policy.md) — also see [policies/work-routing.md](../policies/work-routing.md)
- [Files subsystem spec](../2026-05-04-files-subsystem-spec.md)
- [Orchestration redesign spec](../2026-05-24-orchestration-redesign-spec.md)
- [Cutover runbook](../cutover-runbook.md)

---

## Archive — foundational

The load-bearing early specs. Still accurate on the deliberation loop and provider contract; superseded on the *runtime topology* (host processes → dispatch pods) and on CWB shape.

### Runtime + deliberation

- [Frame role spec](../archive/2026-04-28-frame-role-spec.md) — what a Frame is and why it exists
- [Aspect-funnel architecture](../archive/2026-05-02-aspect-funnel-architecture.md) — the longest-form runtime architecture doc
- [Provider adapter spec](../archive/2026-04-24-provider-adapter-spec.md) — bridle's Provider interface
- [Funnel compaction design](../archive/2026-05-01-funnel-compaction-design.md) · [funnel/bridle caching](../archive/2026-05-03-funnel-bridle-caching-spec.md)
- [JSONL rewriter spec](../archive/2026-05-10-jsonl-rewriter-spec.md) · [Pi extract / bridle gaps](../archive/2026-05-13-pi-extract-bridle-gaps.md)
- [Nexus transport spec](../archive/2026-04-25-nexus-transport-spec.md) · [registration spec](../archive/2026-04-22-nexus-registration-spec.md)

### Hands + work-routing

- [Hand dispatch v0.1](../archive/2026-04-30-hand-dispatch-v0_1.md) — workers as one-shot subprocesses
- [Comms-protocol actions](../archive/2026-05-17-comms-protocol-actions-spec.md) ([plan](../archive/2026-05-17-comms-protocol-actions-plan.md))
- [Native issue tracker spec](../archive/2026-05-17-native-issue-tracker-spec.md) ([foundation plan](../archive/2026-05-17-native-issue-tracker-foundation-plan.md)) — the ledger lineage

### Observability + ops

- [Observability operator runbook](../archive/2026-05-12-observability-operator-runbook.md)
- [Observability core + nexus-watch](../archive/2026-05-12-nexus-watch-and-observability-core.md) · [funnel observability audit](../archive/2026-05-12-funnel-observability-audit.md)
- [One-to-one observability plan](../archive/2026-05-11-one-to-one-observability-plan.md) · [diligence pass results](../archive/2026-05-11-diligence-pass-results.md)
- [Crossing cutover checklist](../archive/2026-05-09-crossing-cutover-checklist.md)

### Dashboard + UI

- [Dashboard WS port spec](../archive/2026-05-09-dashboard-ws-port-spec.md)
- [Operator as aspect (WS extension)](../archive/2026-05-04-operator-as-aspect-ws-extension.md) · [unify Frame + aspect chat path](../archive/2026-05-04-unify-frame-aspect-chat-path.md)
- [Avatar interface spec](../archive/2026-04-29-avatar-interface-spec.md) — the vessel lineage

### Storage + auth

- [Storage abstraction spec](../archive/2026-05-05-storage-abstraction-spec.md) ([plumb review](../archive/2026-05-05-storage-abstraction-spec-review-plumb.md))
- [AWS bootstrap spec](../archive/2026-05-01-aws-bootstrap-spec.md) — earlier production deployment (since superseded by k3s-on-dMon)

### Config + stability

- [Aspect dynamic config](../archive/2026-05-28-aspect-dynamic-config-design.md)
- [Agentfunnel connection stability](../archive/2026-05-23-agentfunnel-connection-stability-spec.md)
- [Feed trust surface](../archive/2026-05-23-feed-trust-surface-spec.md)
- [Skill delivery design](../archive/2026-05-15-skill-delivery-design.md)
- [JWT refresh over WS](../2026-05-23-jwt-refresh-over-ws-spec.md) ([plan](../2026-05-23-jwt-refresh-over-ws-plan.md))

### Build planning + audits

- [Frame 65 build plan](../archive/2026-05-01-frame-65-build-plan.md) · [frame stop decisions](../archive/2026-05-01-frame-stop-decisions.md)
- [Issue triage analysis](../archive/2026-05-05-issue-triage-analysis.md)
