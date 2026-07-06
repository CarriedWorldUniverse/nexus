# Nexus Network — Role Model & Operating Spec

*Base knowledge the rebuilt agent network boots from. v1 — 2026-07-04.*

The previous fleet's failure was **role sprawl** — every new capability became a new named aspect. This spec exists to keep the role set small **on purpose**.

## 1. Core principles (the invariants)

1. **Roles are cut by *kind of job*** — never by technology or attribute. "Backend vs frontend", "Go vs React" are **not** roles.
2. **Unbundle role / identity / personality / model.** Fusing these into named aspects is what sprawled. They are four independent axes (§4).
3. **Stack/tech differences → base knowledge** (+ maybe a routing hint). **Verification differences → which gates fire.** Never a new role.
4. **Least privilege.** Each role's agent gets *only* the skills/tools its job needs — not the whole set. Hygiene **and** a security boundary (separation of duties).
5. **All work enters through the orchestrator.** Operator, event, scheduled — one router, many trigger sources.
6. **cairn is the VCS** (lines / express / commit / fold), not git, going forward.
7. **Small on purpose.** A new role must clear "different *job*, or a different *verification modality*" — not "different tech".

## 2. The roles

### Code family — LLM-brained, autonomous gates

| Role | Job | Brain / tier | Scoped skills (allow-list) | Writes? |
|---|---|---|---|---|
| **orchestrator** | Plan, decompose, dispatch, route the flow, hold judgment, run gates | Frontier | dispatch, decompose (orchestrator/ticket-pipeline), lean-design, cairn (read) | no |
| **builder** | Spec → code → hand to gates | Ornith (gated) | read/write/edit, bash, glob/grep, cairn (express/commit), verify | yes (own line) |
| **tester** | Write + run tests, exercise the change | Ornith | test-run, bash, read, cairn (read) | tests only |
| **reviewer** | Correctness / quality / convention read | Swing (frontier → prove Ornith) | code-review, read, cairn (diff/blame) | no |
| **security-reviewer** | Adversarial: injection / authz / secrets / SSRF / deps | Frontier (always) | security-review, dep/secret scan, read, cairn (read) | no |

### Art family — generative-brained, operator-judged

| Role | Job | Brain | Scoped skills | Verification |
|---|---|---|---|---|
| **painter** | Textures / 2D / concept art | Diffusion (Flux/SDXL-class, GPU) | image-gen pipeline, read (brief) | **operator-visual** (+ vision-review stills check) |
| **modeller** | Asset meshes (creatures / props / NPCs) | Generative-3D (GPU) | 3d-gen pipeline, read (brief) | **operator-visual** |

> Procedural/voxel **world** geometry (`godot_voxel`, the CA kernel) is **code → builder**, judged by render. "Modeller" is only for genuine mesh assets — never route voxel work to it.

## 3. Agents, identities, personalities

- **Agents = an interchangeable pool** of accountable slots (agent-01..N), each with a real cairn/nexus identity (keyfile, per-agent token). Accountability, cairn authorship, and permissions ride on these.
- **Roles are assigned per work-item.** Orchestrator dispatches to a *role*; the first free agent is spawned as {personality + role} for that task, does the bounded unit, hands back, returns to the pool.
- **Personality is thin — decoration, not capability.** Name + voice + chat attribution, carrying **zero** load-bearing knowledge. Capability = role (skills) + task spec + base knowledge (commonplace). A personality that "knows the game engine" = re-bundled; that knowledge belongs in commonplace.
- **Four axes, never fused:** identity = accountable (pool slot) · role = the job · personality = cosmetic label · model = routing config.

## 4. The flow

- **Orchestrator-as-hub.** Every work-item enters the orchestrator's intake and returns to it between hops. On a gate reject it decides rework vs escalate vs ship.
- **Gates are conditional, not a fixed march.** The orchestrator invokes only the gates an item needs:
  - security-reviewer → security surface (endpoints, auth, input, deps, secrets)
  - tester → runtime surface
  - operator-visual → frontend or art output
  - a docs tweak may be builder → reviewer → ship
- **Direct role-to-role handoff is a later optimization** — start hub-and-spoke; add pipelining only if orchestrator routing proves a bottleneck.

## 5. The handoff contract (the load-bearing seam)

One fixed shape per hop. Vague handoffs = failure at every seam.

- **Receives:** `{ task_spec, acceptance_criteria, prior_output/artifacts, cairn_line_ref, base_knowledge_pointers }`
- **Returns:** `{ result, verdict(pass|reject + reasons), artifacts(diff/commit/asset), next_role_hint }`

Verdicts are explicit and machine-routable — a reject carries reasons the orchestrator acts on.

## 6. Verification spine

The axis that actually varies is **how work is verified**, not its tech:

- **Autonomous** — backend/logic: tester runs it, reviewer reads it, security-reviewer attacks it. Loop closes without the operator.
- **Operator-visual** — frontend UI + all art: the operator is the judge; vision-review (gemma @ dMon `:30804`) is a stills sanity-check only. These items always end at the operator.
- **Frontend is the hybrid** — autonomous code gates **plus** an operator-visual gate.

## 7. Brain / model routing

- **Frontier (Claude/Opus):** orchestrator, security-reviewer (always), reviewer (default, swing).
- **Ornith (owned, gated):** builder, tester, reviewer (as it proves out).
- **Generative art models (GPU):** painter (diffusion), modeller (gen-3D).
- **Ornith runs OpenAI-native** via bridle's native-API ToolRunner (the harness we own + proved with DeepSeek), pointed at `vllm-ornith:8000/v1` — **not** the Anthropic-shaped Claude-CLI shim. `plumb-ornith` = plumb on this path via `provider-binding {openai, ornith}`. Don't build a bespoke CLI/harness.
- **Ornith is a gated builder, never the orchestrator** — its envelope (good scaffold, weak judgment/self-verify) is *why* the gates are external and mandatory.

### Routing a work item to a personality

Pool workers (`{personality}-{role}`) inherit their provider/model from their **personality's** aspects row at validate time — not from the role. To route a hard ticket to a stronger brain (e.g. flip `keel` to `claude-code` while the rest of the roster stays on Ornith), two things compose:

1. **Set the personality's brain:** `nexus aspect set <personality> --provider --model` (e.g. `nexus aspect set keel --provider claude-code`). This is the only thing that actually changes what the personality *is* — routing a work item to it just picks which existing brain answers.
2. **Request that personality for a work item:** `nexus workitem create --role builder --task ... --criteria ... --personality keel`. The lease is strict: `keel` free → leased to `keel-builder` exactly; `keel` busy → the item **queues** (never substituted to a different free personality — the request is about the brain, and substitution would defeat it). It drains onto `keel` on the next completion that frees it. Omit `--personality` for the default: any free personality, unchanged.

`--personality` only *targets* a lease; it never sets a personality's provider/model itself, and this command never touches cluster state or aspects rows.

### Role tiers (2026-07-06)

Complexity tier is a **role property, not a personality property.** `builder` and `builder-complex` are the same job (spec → code → hand to gates, same verification spine, same skills allow-list) at two different BRAINS:

- **`builder` (simple tier)** — the default everything already ran before this split: Ornith, bounded single-file/single-concern tickets.
- **`builder-complex` (heavy tier)** — a heavier provider/model (operator-configured, e.g. Claude/Opus-class) for work where a lighter brain's scaffold-but-weak-judgment envelope isn't enough: real concurrency, multi-file refactors, or subtle-correctness changes — the live, motivating case is NET-49 (a race-condition fix Ornith kept getting subtly wrong across several attempts; routing the retry to `builder-complex` closed it in one pass).

Any personality may take either role — tier is orthogonal to which personality (name/voice) is running it, exactly like the existing personality-vs-role split in §3. **Verification is identical across tiers**: the same gates (tester/reviewer/security-reviewer as the item needs), the same handoff contract (§5), the same DoD/acceptance-criteria check — a heavier brain earns no shortcut past the verification spine.

**Config:** the orchestrator's role→brain mapping is `ORCHESTRATOR_ROLE_BRAINS`, a comma-separated `role=provider:model` list, e.g.:

```
ORCHESTRATOR_ROLE_BRAINS="builder-complex=claude-code:claude-sonnet-4-6"
```

Unset (or a role absent from the list) means no override for that role — it dispatches with whatever the leased personality's own aspects row already resolves to (§7's existing per-personality routing), unchanged. There is no hardcoded default role→brain mapping; the example model id above is illustrative, not a pin — pick whatever brain fits the operator's actual provider/model roster.

**Precedence** (highest wins): the role's configured brain (`ORCHESTRATOR_ROLE_BRAINS`) > the leased personality's own aspects-row provider/model (§7's `nexus aspect set`) > the dispatch launch default. A role brain is threaded orchestrator → `dispatch.PoolItem` → `dispatch.Brief` → the Job's `CW_PROVIDER`/`CW_MODEL` env, which the worker (agentfunnel) prefers over the broker validate/resolve response's provider/model at boot.

`nexus workitem create --role <role>` validates `--role` against the registered pool-role vocabulary (`security-reviewer`, `builder-complex`, `builder`, `tester`, `reviewer`, `painter`, `modeller`) and fails fast with a helpful error on an unregistered role, rather than filing a work item nothing will ever lease.

## 8. Scheduling

Scheduling is a **trigger — not a role, not an agent.** A thin timer (k8s CronJob — proven by transcript-ingest; the old "must be in-nexus, Windows has no cron" rationale is stale on k3s) fires → builds a work-item (role + spec) → injects it into the orchestrator's intake → routed like any work. If named "hermes": **hermes delivers, never performs.** The schedule list (what/cadence/enabled) can be operator-editable config in nexus; execution is the dumbest reliable timer.

## 9. Skills scoping (least privilege)

Each role's agent is granted **only** the skills/tools in its §2 allow-list, enforced at spawn:

- reviewer + security-reviewer are **read-only** — cannot write or commit, so cannot tamper with what they gate (separation of duties = the structural form of the `b.auth`-vs-`requireAdmin` lesson).
- builder writes only within its task's cairn line / workdir.
- orchestrator dispatches + reads; never directly edits.
- art roles get art pipelines, not code tools.

Skill-set is a property of the **role**, injected at spawn alongside the (thin) personality.

## 10. VCS: cairn (default, not git)

- Builder work happens on a **cairn line** (`express` → edit → `commit` → hand back).
- Orchestrator **folds** the line into main when the pipeline passes (clean ff; the line never diverged).
- Reviewer / security-reviewer read via `cairn diff` / `blame`.
- Identity = the agent's per-slot cairn author, so attribution is real.
- Protected-main → express-line-then-fold is the default pattern anyway.

---

### Resolved decisions (operator, 2026-07-04)

1. **Topology: shadow = the operator's planning interface, not the pipeline orchestrator.** shadow (croft session) plans with the operator and **enqueues work-items to the orchestration queue**; a **standing orchestrator aspect** (frontier-brained, in-cluster) drains the queue, dispatches to the pool, runs gates. Scheduled triggers feed the same queue. shadow never executes the pipeline.
2. **All named aspects retire** (plumb, anvil, keel, …). Names survive only as thin personality labels the pool stamps at spawn. No exceptions.
3. **Pool = 3 slots** (agent-01..03) to start — enough for builder+tester+reviewer concurrent on one ticket; grow when a real queue forms.
4. **Base knowledge lives in commonplace** (shared entries; retrieval verified 2026-07-04) + the spec on a cairn docs line.

### Still open

1. **reviewer tier** — starts frontier; flip to Ornith only after it proves it catches semantic/security misses (the `b.auth` class). Gated by a real test.
2. **Exact handoff schema** (§5) — pin concrete types during Phase 1.
