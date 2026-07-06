# Network Rebuild — Migration Plan

*From the audited current state to the ROLE-MODEL/PHASE2 target. v1 — 2026-07-05. Grounded in AUDIT.md (5-way Opus audit); companion to PHASE2-DESIGN.md (audit-corrected).*

**Target state, one paragraph:** shadow (croft) plans with the operator and enqueues; an event-triggered stateless orchestrator drains a work graph on **ledger**; a pool of 3 role-stamped workers (Ornith builders/testers via bridle native-API, frontier gate roles on `claude-api`) executes on **cairn lines**; specs/plans live in a **document register** the operator approves through **console v0**; secrets from **almanac**, external creds from **custodian**, knowledge in **commonplace**, backups via **porter**; every pod heartbeats one status shape; the named fleet is retired to labels; nexus's internal duplicate stores are deleted.

---

## M0 — Safety + foundations *(days; blocks everything destructive)*

| # | Item | Why / detail |
|---|---|---|
| 0.1 | **Commit the live monoliths to git** | `cwb-core-step3-embed-sidecar.yaml`, `nexus-control.yaml`, transcript-ingest cronjob → `carriedworld-cloud`. The live estate is currently unreproducible (croft-home-only YAML). 🔴 gate |
| 0.2 | **Bring sqld up; export persona rows** | `aspect_personalities` + `nexus_settings.nexus_md` exist ONLY behind the down sqld pod. Export to files + commonplace before any aspect deletion. 🔴 gate |
| 0.3 | **Frontier auth** | operator runs `claude setup-token` → almanac `SecureParameter` → k8s secret delivery. (Still parked on operator.) |
| 0.4 | **Reap zombies + GPU-plugin flap** | 4 UnexpectedAdmissionError pods; device plugin flaps on every reboot — investigate restart ordering. Non-blocking hygiene. |
| 0.5 | **Re-sync cwb-proto** | it lags cairn's change/line service; building the workspace integration against a stale contract wastes a build cycle. |
| 0.6 | **Cairn server → robo-dog `/data`** (task #10, recon done) | the VCS home the pool builds on: hostNetwork deploy, `/data/cairn` hostPath, swap `cairn-ssh` LB, selectorless-endpoint repoint, GitHub mirror CronJob. Replaces the near-empty cwb-core cairn (1 repo, 4.1M). |

## M1 — Build the pool machinery *(the 8 units, audit-calibrated)*

Parallel-safe first (well-tested code): **3a** Brief extension (M) · **3b** funnel role-overlay/policy/skill-gating (M–L; mind the `loadToolPolicy` load-once invariant + refresh loop) · **4** pool leasing + cap (M; don't break `!dispatch` per-name semantics; `IsDerivedName` blocks sub-of-sub).

Load-bearing L-units (weakest scaffolding/tests — gates matter most): **1** work-graph CRUD + cancel/requeue on the ledger adapter (requeue-onto-same-line is greenfield; add `handoff`/`result`/`stream_id` via schema or convention) · **5** worker-status frames + table + `/api/admin/workers` (heartbeat greenfield; `dispatch_status.go` has no tests; frames unversioned → field-additive only) · **6** orchestrator graph-drain + OnJobDone wake + auth preflight + auto-reap (drain today is a one-shot CronJob `claude -p`; the wake hook is new).

Then: **2** document register on ledger+cairn (greenfield; croft-reachable; verdicts `requireAdmin`) · **7** activate the dark CWB code (almanac/custodian addrs), CLI version knob, CI image pipeline (PullNever → node distribution) · **8** console v0 (after 2+5).

Bridle-side (small, from the harness audit): funnel-owned ticker for wall-clock heartbeats; `WritePathAllow`/`ReadOnly` in toolrunner (or BeforeToolCall deny) for §3 write scopes; role→provider map pins gate roles to `claude-api` (claude-code can't enforce tool deny); verify Ornith's reasoning-field name against bridle's `reasoning_content` handling.

**Exit criterion:** one synthetic ticket flows enqueue → decompose → builder → tester → reviewer → security → fold, with heartbeats visible in console v0 and a forced terminate/requeue exercised.

## M2 — Dogfood cutover *(pillar by pillar, each behind its seam)*

| Internal duplicate | → Pillar | Move |
|---|---|---|
| local FTS5 "knowledge" store | commonplace | finish the in-flight `migrate-knowledge`; delete local store after verify |
| embedded `ledger.Service` on local ledger.db | ledger pillar | work-graph adapter (unit 1) points at the pillar via cwbproxy; internal DB becomes runtime cache only |
| `nexus/credentials` AES-GCM table | custodian (external) + almanac (internal) | activate dark clients; local table demotes to delivery cache |
| broker-minted identities | herald+casket (v2) | only after herald revival; same `MintDerivedCredential` seam |
| **lynxai** (service, not aspect) | **core CWB** | relocate into cwb-core as a supporting service BEFORE nexus-ns cleanup (else agent web access silently dies); retire orphaned `lynxai-env` |

porter backup scope check: work-graph + register + cairn-server `/data` all in the snapshot set. **vessel-voxcpm: parked WIP — untouched.**

## M3 — Retire the named fleet *(only after M1 exit + M0 gates)*

Ordered: (1) port drain → standing orchestrator proven, THEN delete shadow-aspect + shadow-drain CronJob + SA (never before — pipeline heartbeat). (2) persona exports → commonplace base-knowledge (lane knowledge: anvil=OSS/OpenAI-native, plumb=ornith binding, harrow=ollama-local, keel=Frame/broker-side) + thin personality labels for attribution. (3) preserve maren's provider binding (Gemini/Antigravity login) as a routing entry. (4) delete: aspect deployments, keyfiles (post-cutover), homes/PVCs (snapshot first), old `nexus-broker` deploy, `nexus-bringup/` (dead, wrong roster), dead code (`handqueue`, `handexec`, `cmd/agent`, `autospawn` + its main.go call, `outpost`/`relay`, `shadowrunner.JiraGate`, `classification` triage). (5) audit comms callers addressing literal aspect names → roles; names stay as display labels. (6) cluster hygiene: dormant decomposed cwb deploys + dead NodePorts/LB + ~180GiB orphan PVCs (voxcpm-cache stays — WIP), orphan secrets/CMs after mount-audit.

## M4 — Prove + cut over *(the finish line)*

2–3 real tickets end-to-end through the pipeline; non-code day-to-day lanes flip to Ornith; scheduled work enters via CronJob → orchestrator intake; carried-world work resumes **through** the network (the point of all of this). Then **Phase 5**: the real UI, designed against the running system.

---

## Dependency spine

```
M0.1 git-commit ─┐
M0.2 persona-export ─┤→ (gates M3 deletions)
M0.3 setup-token ────┤→ M1.6 preflight, M1.7 auth
M0.6 cairn server ───┤→ M1 workspace, M2 porter scope
M1 (3a/3b/4 ∥) → 1/5/6 → 2/7/8 → M1-exit ─→ M2 (pillar by pillar) ─→ M3 (retire) ─→ M4 (prove)
```

**Standing risks:** frames are unversioned (field-additive discipline only) · most broker config is restart-to-apply · `b.auth` vs `requireAdmin` on every new endpoint (verdicts + workers endpoint = requireAdmin) · README drift is systemic (trust code) · herald revival explicitly deferred behind its seam.
