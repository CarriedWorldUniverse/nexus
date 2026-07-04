# Network Rebuild — Consolidated Audit (2026-07-04)

Five parallel Opus-4.8 read-only audits: **nexus core**, **CWB pillars**, **bridle harness**, **live cluster / IaC**, **legacy fleet**. This synthesizes them into: what we have, what the code claims vs does, what the migration actually requires, and the corrections the audit forces on the design. Source-of-truth for detail = the five task transcripts; this is the decision layer.

---

## A. The headline corrections to the design (act on these first)

The audit invalidates or sharpens several PHASE2-DESIGN assumptions:

1. **almanac is NOT a scheduler.** It is the config + internal-secrets pillar (SSM Parameter Store / Secrets Manager model). **No scheduling pillar exists anywhere in CWB.** → Delete the "almanac = scheduled triggers" row (§10). CronJob stays the timer; "hermes" has no pillar and needs none.
2. **Frontier token belongs in almanac, not custodian.** custodian = *external* creds (git tokens, Drive OAuth — "acting as a user on someone else's service"). The Claude OAuth token is *internal platform config* = almanac's `SecureParameter` domain, by both products' own boundaries. → §6/§10 source the frontier token from **almanac**.
3. **The CWB "consumption" is mostly ALREADY WRITTEN, and dark.** nexus has full custodian/almanac/cfgreconcile integration code — compiles, tested, **disabled by default** (gated on unset `*_GRPC_ADDR`, silently falls back to local stores). So unit 7 + the dogfood migration is largely *activate-and-cut-over*, not greenfield. Don't mistake dark-code presence for "it works," but don't rebuild it either.
4. **The document register (§9) IS greenfield** — ledger has no `kind: spec|plan|design`, no `approved/rejected` status, no approvals model. Confirmed net-new on the ledger engine (build unit 2).
5. **lynxai and vessel-voxcpm are NOT aspects — they are services** (operator, confirmed). They were deployed into the nexus ns and mislabeled because everything there looked like an aspect. lynxai = the ToolRunner's `web_fetch`/`web_extract` backend (dropping it silently kills all agent web access); vessel-voxcpm = TTS. No persona, no identity, no roster slot → **out of the aspect/retirement model entirely.** Action: relocate them to a services namespace (model-stack or a new `services` ns), keep them running as plain services. They are not "aspects we keep" — they were never aspects.

---

## B. 🔴 Safety gates — MUST do before Phase 3 deletes anything

These are the "unrecoverable if we get it wrong" findings, from the cluster + fleet audits:

1. **The live system is unreproducible.** `cwb-core` (all live CWB) and `nexus-control` (live agent-net) exist ONLY as uncommitted YAML in croft's home (`~/cwb-core-step3-embed-sidecar.yaml`, `~/nexus-control.yaml`); transcript-ingest cronjob likewise. If croft's PVC is lost, the estate is gone. → **Commit all three into `carriedworld-cloud` before touching anything.**
2. **All persona content lives only in a down database.** `aspect_personalities` (NEXUS/SOUL/PRIMER for 6 aspects) + `nexus_settings.nexus_md` are in the broker's libsql store on the `sqld-data` PVC — and **sqld is currently scaled 0 / no endpoints**. → **Bring sqld up and export the personality rows before deleting any aspect.** Their only copy is behind a dead pod.
3. **Port the drain before deleting shadow-aspect.** shadow-aspect's `-drain` CronJob IS the pipeline heartbeat and is exactly what Phase-2 §2 ports into the standing orchestrator. Delete-before-port = dead pipeline. (Also: its Jira cost-gate expects secret `shadow-jira-gate` which doesn't exist → **fails OPEN**; harmless only while suspended. Don't un-suspend as-is — ungated Opus every 30 min.)

---

## C. Component status — nexus (what's live / dead / dark)

**LIVE (the running system):** `nexus/broker`, `runtime/cmd/agentfunnel` (the worker/drain binary), `runtime/dispatch` (k8s-Job engine — `Runner`, `jobspec`, `brief`, `spawn`), `nexus/runs` (dispatch read-model — Phase-2 work-graph extends this), `nexus/frames` + `nexus/frame/funnel` (wire + bridle deliberation), `cwbproxy` (the seam to CWB edge), the MCP sidecars (comms/jira/issue/skills/github/imap), `loki-alert-bridge`.

**DARK (written, tested, disabled-by-default):** the entire CWB-client integration — `nexus/cwb/custodian`, `cmd/nexus/{almanac,custodian}.go`, `cfgreconcile`. Gated on unset env, degrades to local. *Looks* done; unexercised. This is unit-7's raw material.

**DEAD/superseded (Phase-3 deletion candidates):** `nexus/handqueue` + `runtime/handexec` + `runtime/cmd/agent` (pre-k8s single-host `-hand` stack), `nexus/autospawn` (Windows-era per-home supervisor — remove the `main.go:767` call too), `nexus/outpost`+`relay` (multi-host transport the in-cluster model doesn't use), `shadowrunner.JiraGate` + `nexus/classification` triage (dead once the orchestrator judges the graph not a Jira snapshot).

**DUPLICATE — nexus reimplements a pillar's job (dogfood-or-delete):**
| Nexus internal | → Pillar | State |
|---|---|---|
| embedded `ledger.Service` on local `ledger.db` | **ledger** | library-embedded; want work-items *as ledger issues* on the pillar. `cwbproxy /ledger/` seam exists. |
| `nexus/knowledge` (SQLite FTS5, *named* "Commonplace" but a local table) | **commonplace** | migration in-flight (`scripts/migrate-knowledge`); finish + delete local store |
| `nexus/credentials` (AES-GCM table) | **custodian** | custodian code dark; this is unit-7's swap |
| `MintDerivedCredential` broker-minted JWTs | **herald+casket** | v1 keep (guard); v2 swap behind same seam |

---

## D. Pillar fitness (dogfood readiness)

| Pillar | Verdict | Key finding |
|---|---|---|
| **ledger** | **READY (adapter-shaped)** | `issue_links` type=`blocks` + `IsBlocked`/`ListReady` **already implements** §1 "ready when deps done"; atomic `ClaimIssue` = pool-lease; `skills` tags (v14) = role routing. Gaps: no `handoff`/`result`/`stream_id` columns, doc-register is greenfield. |
| **cairn** | **READY** | line/fold/workspace model built server-side (`internal/change/`) = §8 shared workspace. Confirm line API reachable via gateway (cwb-proto lags cairn's change service). |
| **commonplace** | **READY** | live, porter-backed, hybrid FTS5+vector; only nexus-side "approved→distillate" glue needed. |
| **porter** | **READY (backup half)** | backup pod built + custodian-integrated; README falsely says "not implemented." Verify it snapshots the new work-graph/register data. |
| **casket-go** | **READY** | stable crypto lib; consumed by other pillars, not nexus directly. |
| **interchange** | **READY (enabling infra)** | hard dependency of the whole mapping; already routes to consumed pillars. |
| **herald** | **NEEDS-WORK (revive+deploy)** | agent-issuance code real but **dormant**; §10 guard already defers to v2. |
| **custodian** | **NEEDS-WORK + re-slot** | usable get/put/list but `kind=git` only; frontier token is almanac's job not custodian's (see A2). |
| **almanac** | **KEEP, re-slot to config/secrets** | NOT a scheduler (A1). README says "not begun" but it's built (drift). `Watch`/live-reload claimed but absent. |
| **mason** | **KEEP (not a nexus consumer)** | Strata deploy engine (almanac→k8s reconcile); validates almanac; unmapped in §10, that's correct. |

Cross-cutting: **README drift is systemic** on newer pillars (porter/almanac/custodian status lines all wrong) — trust code, not docs. Checkouts are ~1wk old on an unmerged `feat/multi-arch-image` branch; **cwb-proto lags its consumers** (missing cairn's change service) — re-sync before building against it.

---

## E. bridle harness (worker seat readiness)

- **Provider fit:** builder/tester → `openai-api` against vLLM `/v1` (proven, Phase 0). reviewer/security/orchestrator → **`claude-api`** (native, `BeforeToolCall` deny works) NOT `claude-code` (subprocess — **no before-tool hook**, so P3a deny can't gate it). Encode this in the role→provider map.
- **§3 tool gating:** rides `BeforeToolCall` deny — present. **§3 skill gating & write-scope: genuine gaps** — toolrunner has no write-path allowlist / read-only mode (`config.go`); skill-allowlist filtering of `.agents/skills` is agentfunnel's job (bridle has no skill concept). Both are agentfunnel-side new build, correctly outside bridle.
- **§5 heartbeat: real gap.** No wall-clock mid-turn heartbeat (`OnStepBoundary` is round-driven — a 5-min bash build emits nothing). Recommended seam: funnel-owned `time.Ticker` reading the stamped `EventSink` (zero bridle change). Mid-turn token totals not exposed (finalized only in `TurnResult`).
- **Goal-loop:** bridle is deliberately one-turn/stateless; the work-until-DoD loop is a funnel construct (stubfunnel demonstrates the skeleton). No bridle gap beyond the heartbeat.
- **De-risks Ornith:** tool-call leak repair/retry contract (`run.go:354`) + per-turn `ProviderEnv` (mix vLLM + Anthropic in one funnel) are strengths. Verify Ornith's `/v1` reasoning field name matches the `reasoning_content` bridle expects, or cross-turn replay silently drops it.

---

## F. Phase-2 build sizing (audit-calibrated)

| Unit | Size | Note |
|---|---|---|
| 1. Work graph + cancel/requeue | **L** | requeue-onto-same-line is greenfield; runs.Status≠work_item.status mapping; two DBs (runs.db runtime + ledger graph) |
| 2. Document register | **L** | greenfield lifecycle on ledger; must be croft-reachable; verdicts `requireAdmin` |
| 3a. Brief extension + jobspec | **M** | additive, well-tested code |
| 3b. Funnel role-overlay/policy/skill-gating | **M–L** | `loadToolPolicy` is load-once-thread-through — per-spawn breaks that invariant; skill gating has no hook |
| 4. Pool leasing + cap | **M** | careful: `canRun`'s per-name serialization is what `!dispatch` rides; `IsDerivedName` blocks sub-of-sub |
| 5. Worker status + `/api/admin/workers` | **L** | heartbeat greenfield; `dispatch_status.go` has NO test; frames unversioned (field-additive only) |
| 6. Orchestrator graph-drain + wake + preflight + auto-reap | **L** | biggest conceptual gap: drain is a CronJob one-shot `claude -p` today, not an event service; `OnJobDone` exists but has no orchestrator wake hook |
| 7. Auth (almanac-sourced) + CLI knob + CI | **M** | image is `PullNever` — CI must distribute to nodes, not just push registry; most config is restart-to-apply |
| 8. Console v0 | **M** | depends on 2+5; verdict buttons `requireAdmin` |

**Sequencing:** 1/5/6 are the load-bearing L-build with weakest existing scaffolding + test coverage → most care. 3/4 are safe parallelizable M-evolutions of tested code. 7 + the dogfood cutover is activate-dark-code.

---

## G. Cluster hygiene (do alongside, not blocking)

Top items from the estate audit: GPU-device-plugin flaps 4 zombie pods on every reboot (reap + investigate); ~180 GiB PVCs bound to scaled-0 workloads (voxcpm 80G, gemma-weights 60G, nexus/ollama 40G); dead NodePort/LB exposure pointing at scaled-0 backends; systemic IaC drift (git describes the *old decomposed* arch, live is two monoliths). Full top-10 in the cluster transcript. `nexus-bringup/` is dead (wrong old roster) → delete.

---

## H. Corrected dogfood mapping (supersedes PHASE2-DESIGN §10 table)

| Component | Pillar | Change from original |
|---|---|---|
| Work graph / work-items | **ledger** | unchanged — confirmed strong fit |
| Document register | **ledger** (lifecycle) + **cairn** (content) + **commonplace** (distillate) | unchanged; register is greenfield |
| Artifacts / workspace | **cairn** | unchanged |
| Pool identities | **herald + casket** (v2), broker keyfiles (v1) | unchanged — guard holds |
| **Secrets (frontier token + internal config)** | **almanac** | **CHANGED from custodian** |
| External creds (git, Drive) | **custodian** | clarified — its real domain |
| Scheduled triggers | **k8s CronJob only** | **CHANGED — no pillar; almanac row deleted** |
| Base/fleet knowledge | **commonplace** | unchanged (live) |
| Backup | **porter** | unchanged (verify scope covers new stores) |
| *(deploy/reconcile)* | **mason** | noted — not a nexus consumer, keep for Strata |
