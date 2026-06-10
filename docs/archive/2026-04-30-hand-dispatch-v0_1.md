# Hand Dispatch — v0.1

> **Status (as of 2026-06-11):** historical. Ticket dispatch is now **named-agent dispatch** (`../2026-06-08-named-agent-dispatch-model.md`) over the broker-inline Runner — work runs as a real named team member. The aspect-owned "hands" fan-out described here is being re-introduced alongside it (roundtable spec), distinct from ticket dispatch.

**Date:** 2026-04-30
**Status:** Spec — operator-resolved (msgs #8366 / #8368 / #8370)
**Supersedes:** `agents/keel/HANDS_ARCHITECTURE.md` (2026-04-13) — named-persistent identity model retired
**Affects:** §6.4 (substrate stays), §6.5 (Frame harness consumes this)

---

## 1. Summary

Hands are **fresh-context instances of the dispatching aspect**, run in interchangeable subprocess slots drawn from a shared pool, fairness-scheduled. The slot is anonymous infrastructure; the persona running in it is inherited from the dispatcher — when maren dispatches, the slot boots with maren's NEXUS.md and SOUL.md and acts as maren on a fresh-context turn. There are no per-aspect *named* hands ("anvil-the-coder", "maren-the-illustrator"), no offline-online lifecycle for named identities, no `create_hand` / `summon_hand` MCP surface. Every dispatch boots a slot with the dispatcher's identity framing; the worker runs the work; the result lands in-thread automatically because dispatches are thread-bound chat posts.

This is a deliberate simplification from the named-persistent retainer model in `HANDS_ARCHITECTURE.md`. The fairness invariant — no aspect starves while others hog the pool — replaces the "everyone gets one guaranteed named hand" framing. Identity flows from dispatcher → hand at boot time; slots themselves carry no identity between uses.

---

## 2. Model

### 2.1 Pool

A single shared pool of ephemeral subprocess slots. Slots are interchangeable from the dispatcher's perspective — there's no per-slot named identity, no per-slot persona retained between dispatches, no per-slot config.

**But each dispatch boots its slot with the dispatching aspect's identity framing.** When maren dispatches, the worker boots loaded with maren's NEXUS.md, SOUL.md, PRIMER — it operates *as maren on a fresh-context turn*. Same voice, same dispositions, same domain knowledge. When anvil dispatches, the same slot (next time it's used) boots as anvil's hand. The slot is anonymous; the persona is inherited from the dispatcher per-dispatch.

This means:
- **Result attribution.** A hand's reply IS the dispatching aspect's reply. The aspect's identity, the aspect's voice, the aspect's authentication — same all the way through. Broker treats hand-posts as aspect-posts (same bearer token at the harness layer).
- **Triage rules inherited.** A hand's triage decisions follow the dispatching aspect's triage configuration (per-aspect disable, hard rules, model selection).
- **Cost attribution natural.** Tokens consumed by the hand bill against the dispatching aspect's account — same identity, same billing path.
- **Domain knowledge preserved.** The hand isn't a generic worker reasoning about an unfamiliar problem; it's the aspect itself doing focused work on a fresh context. Knowledge of the aspect's prior work flows in via the active-context summary handed to the hand at dispatch time.

What "anonymous" means here: dispatcher doesn't address workers by name (no `summon_hand("anvil-the-coder")`). Worker slots are interchangeable infrastructure. Persona is inherited from the dispatcher at boot — a dispatch is *an aspect spinning up a fresh-context shadow of itself to do a side task*.

This kills "workers have no persona" from earlier framings — the persona IS the dispatcher's, just instantiated in a fresh context each time. Workers are interchangeable as *slots*, not as *personalities*.

**Soft cap `N`** = the steady-state max-concurrent worker count.
- Default: `N = 3`.
- Tunes upward as observed-active count rises, bounded by CPU + cost limits per machine.
- Operator-configurable; auto-tuning is a future iteration, not v0.1.

**Hard ceiling `H`** = absolute cap. `H ≥ N` always. Dispatch rejects with `dispatch_rejected: hard_ceiling` past `H`.
- Default: `H = (number of registered aspects) + 1`.
- Rationale: gives every aspect a slot they can spillover into, plus one — enough headroom for a Frame-dispatch on top while every aspect is busy. Tracks the deployment shape, not an arbitrary multiplier. For a 6-aspect deployment, `H = 7`.
- Operator-configurable; default is what the dispatcher computes at startup from the registered roster. Changes to roster size do not auto-retune `H` mid-run; restart picks up the new default.
- Exists to prevent runaway spawning during pathological dispatch storms.

### 2.2 Queue

A single FIFO queue of pending work items. Each item carries:
- `aspect` — the dispatching aspect's id
- `thread` — the comms thread the dispatch came from (where the reply will land)
- `payload` — the task content (prompt, file refs, etc.)
- `submitted_at` — for fairness tiebreak
- `dispatch_id` — opaque id for tracking and abort

Queue depth is observable but not capped at v0.1. (Capping is open — see §7 Future.)

### 2.3 Dispatch flow

```
aspect → dispatcher (dispatch_id, aspect, thread, payload)
          │
          ├── workers free? ─── yes → spawn worker, run, exit
          │
          └── workers all busy?
              ├── this aspect has no active worker?
              │   └── spawn anyway (spillover, even if over N, up to H)
              └── this aspect has active worker?
                  └── enqueue (FIFO)

worker exits → dispatcher reaps
              │
              └── queue non-empty?
                  └── scan queue:
                      prefer item from aspect with no active worker
                      else FIFO head
                      → spawn worker for picked item
```

### 2.4 Result delivery

The dispatcher does **not** route results. Dispatches are chat posts in threads — the harness subprocess is invoked with thread context already bound. Its reply (via standard harness comms) lands in the originating thread automatically.

This is how harness-mode aspects already work in the §6.4 substrate. Dispatcher routes invocations; the comms substrate routes replies. No new result-routing plumbing.

---

## 3. Fairness invariant

**Statement.** No aspect's dispatch waits behind another aspect's dispatches if the waiting aspect has no currently-active worker.

**Enforcement points:**

1. **On dispatch arrival.** If all `N` workers busy AND dispatching aspect has no active worker, spawn anyway (spillover, up to `H`). The aspect never enqueues if they have nothing in flight.

2. **On worker release.** When a worker exits, scan the queue head-first looking for an item from an aspect with no currently-active worker. If found, dispatch that item. Else dispatch FIFO head.

**Why this works.**
- Aspects with no active work are never blocked by other aspects' work. Spillover guarantees instant slot.
- Aspects with active work can be queued behind their own future dispatches (FIFO ensures their second dispatch waits for their first to free a slot, before yielding to other queued items).
- An aspect cannot starve another aspect by submitting many dispatches: the fairness scan on release keeps pulling from idle aspects until the queue is empty of them.

**What it does NOT prevent:**
- An aspect dispatching forever can starve itself within its own backlog. (Self-imposed; outside scope.)
- Two aspects coordinating to keep the pool above `N` indefinitely until `H`. (Soft cap is a soft cap; this is by design. If `H` becomes a problem, lower it or raise observability.)

---

## 4. Substrate carryover from §6.4

Already shipped, retained:

- `nexus/handqueue` — FIFO queue + max-concurrency cap (becomes soft cap `N`).
- `runtime/handexec` + `SpawnExecutor` — harness subprocess in `--hand` mode.
- `nexus/autospawn` — aspect bring-up at startup (unchanged; not part of dispatch).
- `nexus/outpost` — cross-host relay (unchanged; future work).

What v0.1 adds:

- **Per-aspect active-worker tracking** in the dispatcher. Map `aspect_id → set<worker_id>`.
- **Fairness scan** at worker-release: walk the queue once, return the first item whose `aspect_id` has empty active-worker set; else FIFO head.
- **Spillover check** at dispatch arrival: if `len(active_workers) >= N` AND `len(active_workers[aspect_id]) == 0`, spawn anyway up to `H`. Else enqueue.
- **Hard ceiling enforcement** at every spawn attempt. Reject with structured error beyond `H`.

---

## 5. Address space

### 5.1 Generic protocol terms

The protocol layer and code use generic terms — no deployment-specific noun-with-identity baked into the substrate.

- **dispatcher** — the routing component.
- **worker** (or **subagent**) — a subprocess running a dispatch.
- **dispatch** — a unit of work submitted to the dispatcher.

Code below the user-facing edge MUST use these terms. No `Hand` types in storage, no `summonHand` API entrypoints, no `hand_id` fields in protocol payloads. This keeps the substrate reusable across deployments with different terminology preferences.

### 5.2 Terminology layer (deployment-configurable)

A thin label-mapping layer sits at the user-facing edges (chat output, admin endpoints, dashboard text). It maps generic terms to deployment-specific surface vocabulary.

**Default mapping for nexus-cw deployment** (canonical pantheon language):
```
worker     → hand
dispatcher → dispatcher  (no remap)
dispatch   → summon
```

A different operator deploying the same Nexus binary can configure their own mapping (e.g. `worker → servant`, `worker → helper`, `worker → agent`). The mapping is read at startup from a deployment config (placeholder: `terminology` block in `team.json` / future `nexus.json`); code paths that emit user-facing text consult the map when rendering.

**Boundary:** the mapping affects user-visible strings only. Wire-format payloads, log lines, error codes, source identifiers all stay generic. An operator changing terminology cannot change protocol semantics.

This layer is v0.1 in spec, but its implementation can land alongside Frame harness (§6.5) — until it ships, code uses generic terms everywhere and external surfaces fall back to generic too.

### 5.3 Frame as orchestrator (not peer)

Frame is the team's orchestrator. In the generic Nexus model, Frame coordinates the aspects rather than competing with them as a peer dispatcher. The relationship is structural, not just stylistic.

**Default operation: Frame participates as-if-equal.** Frame's regular dispatches go through the same queue, the same fairness scan, the same spillover rules as any aspect. Aspects work as if Frame is one of them. This is the preferred mode of operation — coordination by influence (asking aspects to do work, summarizing their outputs into chat, scheduling future actions) rather than by overriding the substrate.

**Override capability available — for network protection, not coordination convenience.** Frame's overrides exist to protect the network from misbehaving aspects, runaway workers, or operational hazards. They are NOT for jumping queues to get Frame's own work done faster, nor for tuning the substrate at convenience. Coordination happens by influence — asking aspects to do work, summarizing their outputs, scheduling — not by overriding the substrate.

**Authentication.** Override gestures require an explicit `admin` flag on the caller's bearer token. The token-mint pipeline marks Frame's token with `admin: true`; aspect tokens never carry this flag. The dispatcher rejects override requests with `403 admin_required` for non-admin callers. This makes the authority asymmetry checkable, not aspirational.

The dispatcher exposes these protection gestures only to the Frame role:

- **Abort dispatch** by `dispatch_id`. The targeted worker exits; the originating thread receives an `aborted` notice instead of a result. Used when a dispatch is going wrong (runaway tokens, looping, taking too long).
- **Kill worker** by worker id. Stronger than abort — terminates a worker that's not respecting an abort, leaking resources, or otherwise misbehaving at the process level. The dispatch is marked failed; the originating thread sees a kill notice.
- **Force-shutdown aspect.** Disconnects the aspect's harness, marks it offline, drops any pending dispatches it owns from the queue. Used for an aspect that's broken or whose home has been compromised.
- **Force-shutdown network.** Graceful or ungraceful Nexus stop. Coordinated stop signals all aspects, drains the worker pool, exits cleanly; ungraceful is the emergency exit.
- **Take surface offline (defensive).** Selectively disable public-facing surfaces while keeping internal comms running. Examples: turn off pair-flow registration during a request flood; revoke a malicious peer pair and drop its mailbox traffic; disable Funnel ingress while keeping the tailnet listener alive. The attacker's reach shrinks; the network keeps coordinating internally. Reversible from the same surface — Frame can re-enable when the threat passes.

**Persistence: shutdown means stay-down.** Force-shutdown gestures (worker, aspect, surface, network) MUST defeat the supervisor's auto-respawn. A killed worker that auto-restarts immediately is no defense; a force-shutdown aspect that auto-rejoins offers no protection. The supervisor pattern (today: `network.js` auto-respawn for broker + orchestrator + aspects) MUST honor a persistent disable signal — file flag, registry marker, or equivalent — that survives supervisor cycles until the operator explicitly clears it.

This is a real change from the current pattern. Today's supervisor respawns unconditionally; v0.1 dispatcher's protection gestures depend on the supervisor reading and honoring a "stay down" marker per component. Without this, Frame can pull the trigger but the network just bounces back. **Tracked as a follow-on task — see §7 Future and the linked supervisor work.**

**What is NOT in the protection surface:**
- ❌ Priority dispatch / queue-jumping (would break fairness).
- ❌ Force-assign to specific worker (no protection use case).
- ❌ Runtime tuning of `N` or `H` (deployment concern, operator-only).
- ❌ Adding/removing aspects from the roster (registration is its own subsystem).

**Normative guidance.** Override gestures are for **the network is in trouble** moments, not **Frame wants to coordinate** moments. Every override is audit-logged. A Frame that aborts dispatches as a coordination tool is misusing its authority and breaking the fairness invariant aspects rely on.

**Defensive posture: take the surface offline.** The override list above is one consequence of a broader principle — Frame is the network's defensive perimeter. When something goes wrong (active attack, compromised aspect, runaway dispatch storm), Frame's job is to *shrink the attack surface* faster than the attacker can exploit it. Disabling registration endpoints, revoking peer pairs, killing workers, or pulling Funnel ingress are all the same gesture in different contexts: **make the attacker's leverage smaller while the team figures out what to do**. The override surface is the toolkit for that.

This makes Frame's authority operationally load-bearing: an aspect getting abused or a worker leaking is something only Frame can stop quickly. Override exists because coordination-by-influence is too slow when the network is under stress.

This means §6.5's Frame harness gains dispatch capability for free — it uses the dispatcher like any aspect — and gains the override surface as a separate, audit-logged capability available only to the Frame role.

---

### 5.4 Dispatch authentication

The dispatcher MUST verify the caller's identity matches the `aspect` field on the dispatch request. An aspect cannot submit a dispatch claiming to be another aspect — that would let a malicious or buggy aspect manipulate fairness scheduling (e.g. impersonate an idle aspect to trigger spillover).

Mechanism: bearer token at request time, mapped to an `aspect_id` at the broker layer. Dispatcher trusts the broker's auth resolution; mismatch rejects with `403 identity_mismatch`.

This mirrors the broker's existing `enforceIdentity` rule (agents can only post chat as themselves) and extends it to the dispatcher. Same pattern, same threat model.

### 5.5 Worker timeout

Every dispatch carries an implicit `deadline_secs` default (v0.1: 30 minutes). The dispatcher sets a timer at spawn; on expiry, the worker is killed (the same gesture as Frame's `kill worker` override) and the originating thread receives a `timeout` notice.

Why: without a timeout, runaway workers depend on Frame noticing manually. The `kill worker` override is for *unusual* kills; the default timeout is for *routine* protection against looping or stuck dispatches.

Caller-supplied override: a dispatch can specify `deadline_secs: <N>` in its payload up to a hard maximum (v0.1: 2 hours). Beyond the hard maximum, dispatcher caps to the maximum without erroring — the caller's intent is honored "as much as the system allows."

The dispatcher audit-logs both the assigned deadline and the timeout fire, so Frame and operator can see what's happening.

### 5.6 Agent-harness self-monitoring

Distinguish two layers carefully here:

- **harness** — the generic worker subprocess shape. Owns lifecycle (spawn, exit, post-to-thread). Same across deployments and across AI providers. Lives in this codebase as a stable interface.
- **agent harness** — the AI-specific runner *inside* the worker. Knows how to talk to its provider (Claude API, OpenAI API, Ollama, future-whatever), how to execute the tools available to that provider, how to manage that provider's session state. Different per architecture; lives behind the provider-adapter interface.

Self-monitoring is the **agent harness's** responsibility, not the harness's. The reason: what counts as "stuck" is provider-shaped:
- A Claude streaming API call has a known healthy frame cadence; deadlock detection looks at inter-frame gaps.
- An Ollama HTTP call is request/response with no streaming; deadlock detection is total-time-without-response.
- A future provider may have its own signal (heartbeat frames, partial-result push, whatever).
- Tool budgets depend on which tools the provider exposed in this turn.
- "Harness exception" only has meaning relative to the agent harness's own internal control flow.

Putting self-monitoring in the (generic) harness layer would either force a lowest-common-denominator timeout (long, blunt) or pollute the harness with provider knowledge (bad layering). Neither.

**The harness layer offers a stable mechanism; the agent harness implements detection per-provider and uses the mechanism.**

What the harness layer guarantees:
- A way for the agent harness to post a structured result to the originating thread (the same path used for normal results).
- A way for the agent harness to exit the worker subprocess cleanly with that result registered.
- The dispatcher reads the posted result and reaps the slot — non-zero exit looks like a crash, zero exit + posted result is a clean termination regardless of *why* the agent harness chose to terminate.

What the agent harness implements (per provider, varying):
- Watching for whatever "stuck" looks like in *its* protocol (stalled stream, timed-out HTTP, retry-forever loop, etc.).
- Watching its tool executions against per-tool budgets *it* knows about.
- Catching unhandled exceptions in its own control flow.
- Recognizing agent-emitted "I cannot proceed" signals.
- On detection: build a structured `agent_error` (or `harness_error`) result describing the failure mode, post it via the harness mechanism, exit clean.

**Why this matters:** without agent-harness self-monitoring, the dispatcher's 30-min deadline becomes the floor for every stuck turn, even when the actual failure mode is detectable inside seconds. Self-monitoring catches the failure where the knowledge of "stuck" lives; dispatcher timeout is the outer net for failures the agent harness itself can't see (its own subprocess hangs, its own loop deadlocks, harness layer crashes before the agent harness even starts).

**Three layers, belt-and-braces:**
1. Agent-harness self-monitoring (§5.6) — provider-specific, fast-path on common failures.
2. Harness-layer crash recovery — the harness layer always posts a `harness_crash` result and exits if the agent harness throws something the agent harness itself didn't catch.
3. Dispatcher timeout (§5.5) — outer wall-time net for any failure the worker process couldn't surface at all.

v0.1 ships:
- Layer 1 (agent harness): scoped per provider; the Claude API agent harness implements model-stall + tool-budget + exception detection. Other provider agent harnesses implement equivalents when they ship; the contract is "post a structured result + exit clean," not "use this exact algorithm."
- Layer 2 (harness): unhandled-exception trap that posts `harness_crash` and exits.
- Layer 3 (dispatcher): §5.5 worker timeout, already specified.

### 5.7 Triage turn (pre-turn filter)

**Prior art.** This pattern already exists in the agent-network harness (`code/harness/index.js`) under the name **Triage Turn**. The Nexus rebuild adopts the existing pattern, vocabulary, and field shapes; this section formalizes it into spec rather than introducing something new. The existing implementation has been operational and proven against real comms-swarm load.

**Why:** turns are expensive. Many dispatches arrive that don't carry net-new information — a third aspect echoing acknowledgement, a typo correction with no semantic delta, a watch firing on a stable condition, a peer-thread message that's a courtesy reply. Burning a frontier-model turn on those is real money and real latency for no value. A cheap pre-turn classification pays for itself almost immediately.

**Hard rules (no model invocation needed):**

Some events get a fixed verdict without running the cheap model:
- Direct `@<agent>` mention → always engage, tier=3, mode=reply.
- Operator-from message → always engage, tier=3, mode=reply.
- Wake / shutdown notifications → handled before triage entirely (don't enter the queue).

These hard rules cost nothing and prevent the triage model from second-guessing the load-bearing engagement signals.

**Cheap-model classification (when no hard rule fires):**

Structured prompt + JSON-only output:

```
{
  "engage": true | false,
  "reason": "<one short phrase>",
  "tier": 1-5,
  "mode": "react" | "reply" | "act"
}
```

- `engage=false` → informational noise, an ack, or not addressed to this agent. No response needed.
- `engage=true, mode=react` → emoji-only acknowledgment (👀, 👍). No substantive response.
- `engage=true, mode=reply` → text response.
- `engage=true, mode=act` → requires tool use or code work.
- `tier`: compute tier (1=idle, 2=light, 3=standard, 4=heavy, 5=deep). Lets the dispatcher / agent harness scale model selection or token budget.

**Rule:** when in doubt, default to `engage=true`. False negatives are worse than false positives — losing work is more expensive than spending a cheap turn on noise.

**Skip path (engage=false):**

Agent harness posts a structured `skipped_no_signal` result to the originating thread with the triage `reason`, exits clean. No frontier-model tokens burned. The thread sees the skip explicitly.

**React path (engage=true, mode=react):**

The agent harness can emit the emoji directly (no full turn needed) or run a minimal turn to produce one. Either way, the path bypasses the heavy turn machinery. Saves nearly as much as a hard skip; communicates "I see this" upward.

**Implementation:**
- Cheap model (Haiku for the Claude-API agent harness; qwen2.5:3b or equivalent for local Ollama). The triage's value collapses if it costs more than the savings.
- Structured-output enforcement (Claude tool-call, OpenAI structured output, Ollama grammar). Returns must be parseable; parse failure is treated as `engage: true` (fail-open).
- Configurable per-aspect: an aspect can disable triage for workloads that want every dispatch processed.

**Risks acknowledged:**
- **False negatives** (triage skips when the input mattered) lose work. Mitigations: fail-open on ambiguity, fail-open on hard rules (mention/operator can never be filtered out), every skip logged with `reason` so wrong skips become observable.
- **Adversarial low-signal input.** Gate is per-dispatch scope; can't permanently silence a peer. Watch the skip-rate per (source, aspect) pair; if it climbs anomalously, surface to operator.

**Layer position:** runs inside the agent harness, before the main turn invocation. NOT a separate layer in the dispatcher / harness / agent-harness scheme — it's agent-harness-internal. Different agent harnesses (per provider) implement triage appropriate to their model + cost profile. Local-Ollama agent harnesses may skip triage entirely if the provider is already cheap.

**Critical: triage MUST NOT touch the main session context.**

Observed bug in the agent-network reference implementation (operator #8393): triage-turn JSON output has been seen leaking into chat. Root cause is session contamination — the triage prompt is appended into the same session JSONL that the main-turn model later reads, so the main turn sees the triage instruction in its history and either echoes JSON output as a reply or model-shapes its reply around the triage format.

**Required behavior for the Nexus rebuild:**
- Triage runs against a **sidecar session** (separate JSONL file or in-memory only). Its prompt + response are isolated from the main agent session.
- After the verdict, the triage sidecar is discarded. The main turn (when engage=true and not mode=react) sees no triage turn in its history.
- The agent-network reference implementation has a `triageSessionId` distinct from the main session — confirm the sidecar is actually being used and not contaminating; if there's contamination, that's a bug in the reference impl that the Nexus rebuild MUST NOT replicate.
- v0.1 verification adds: a triage-engage=false path produces ZERO chat output (not even the JSON verdict), and a subsequent main turn shows no awareness of the triage having run.

**v0.1 status:** the existing harness implementation in agent-network is the reference for the *pattern* (vocabulary, hard rules, fail-open semantics). The Nexus rebuild's Claude-API agent harness ports the pattern but enforces strict session isolation as called out above. Other agent harnesses (OpenAI, Ollama, future) implement triage at their discretion when they ship; the session-isolation requirement is universal regardless of provider.

The agent-network bug should also be filed against agent-network for fixing in the existing system; the spec language here ensures the rebuild starts clean.

---

## 6. Failure modes

### 6.1 Worker crash

Worker subprocess exits non-zero before submitting a result. Dispatcher posts an error message back to the thread (via the harness's standard error path — the harness already does this for runtime errors). Dispatcher reaps the worker slot, runs the fairness scan, advances queue.

### 6.2 Dispatcher restart with workers in flight

Dispatcher tracks active workers in memory. On restart, in-flight workers are orphaned: their PIDs persist (until they exit on their own), but the dispatcher's view is empty.

**v0.1 behavior:** orphaned workers run to completion against the broker (they post results via comms; the broker records the chat post). The dispatcher does not adopt them. If the dispatcher spawns NEW workers thinking the pool is empty, the system will briefly have more workers than `N` but not more than `H` + orphaned-count.

**Future (not blocking v0.1):** persist active-worker map to disk on each spawn/reap, recover on restart. Or have workers self-register with the dispatcher on startup. Both are post-v0.1.

### 6.3 Hard ceiling reached

Dispatch rejects with `{"error": "dispatch_rejected", "reason": "hard_ceiling", "active": <H>, "soft_cap": <N>, "limit": <H>}`. Caller decides whether to retry, abort, or surface.

### 6.4 Aspect submits when their own past dispatch is already in flight

Their second item enqueues normally (FIFO). They do not get spillover for their second dispatch because they already have an active worker; spillover only applies to aspects with zero active workers.

---

## 7. Future / out of scope for v0.1

- **Supervisor: shutdown-means-stay-down.** `network.js` (or its successor) MUST honor a persistent disable marker per component so Frame's force-shutdown gestures actually persist across supervisor cycles. Without this, every protection op is undone instantly by auto-respawn. Real change from current pattern. **Filed as a separate task — landing this is a precondition for §5.3 protection ops doing what the spec says they do.**
- **Auto-tuning `N`.** v0.1 is operator-config; future versions can adjust `N` based on observed-active rolling average + cost rate-limit signals.
- **Queue-depth cap.** Reject when queue grows past some threshold rather than letting it grow unboundedly (nexus-work's #8329 warning). Worth adding when we see real load.
- **Persisted active-worker tracking.** For dispatcher-restart recovery — see §6.2.
- **Outpost-side dispatch queue.** Cross-host worker spawning. v0.1 only Nexus dispatches.
- **Per-aspect cost attribution.** Workers all bill the dispatching aspect; a cost-per-aspect aggregator is a separate feature on top.
- **Skill modules.** A skill is a system-prompt-fragment + tool-allowlist that can be requested at dispatch time. Existing in `HANDS_ARCHITECTURE.md` §7; remains a useful concept but is its own feature, not blocking v0.1. Tracked separately as #69.
- **Worker reconnect / aspect WS drop mid-dispatch.** Aspect's WS connection drops while their dispatch is in flight — what happens to active workers spawned for them, and to results that try to land in a now-disconnected thread? v0.1: workers run to completion, replies post to thread regardless of aspect connection state (broker stores them; aspect sees them on reconnect). Future: smarter handling.
- **Frame dispatch with no thread.** Bootstrap tasks, scheduled summaries — v0.1 default is post-to-Frame's-own-default-channel if no thread specified. Better answer is its own design pass.

---

## 8. Removed from `HANDS_ARCHITECTURE.md`

For the record, what does NOT carry forward:

- `create_hand` / `summon_hand` / `list_hands` / `hand_status` / `abort_hand` / `revise_hand` / `retire_hand` MCP surface — replaced by single `dispatch` op.
- Per-hand `SOUL.md` / scaffold under `agents/<owner>/hands/<name>/` — hands inherit their dispatching aspect's identity (NEXUS.md/SOUL.md/PRIMER) at boot, no per-hand files.
- Hand naming, hand quotas per owner, hand lifecycle states — N/A. (Hands inherit their dispatcher's identity; they don't have their own.)
- `requires_review` field on issue spec — handled by the aspect synthesizing its result before posting back, not by the dispatcher.
- `output_channel` field — N/A, dispatch is a thread post; reply uses the thread.
- `personality_seed`, `description`, hand revision — N/A.

The named-persistent model's *aesthetic* (hands as personified retainers) was right for a different problem shape. For the actual dispatch problem — interchangeable subprocess workers doing thread-scoped tasks — anonymous fairness-scheduled is simpler and closer to how nexus-work's `@dispatcher` works in practice.

---

## 9. Implementation parts

**v0.1 implementation sub-parts** (decompose-then-cycle, ~10 parts estimated):

1. Active-worker tracking map in `nexus/handqueue` (per-aspect set<worker_id>).
2. Fairness scan helper at queue release.
3. Spillover check at dispatch arrival.
4. Hard ceiling enforcement + structured rejection error. `H = roster_size + 1` computed at startup.
5. **Dispatch authentication** (§5.4): identity verification at entry; broker-mediated bearer-token resolution; `403 identity_mismatch` on caller-vs-aspect drift.
6. **Admin flag on Frame's bearer token** (§5.3): broker mints Frame token with `admin: true`; aspect tokens never carry it; dispatcher rejects override calls with `403 admin_required` for non-admin.
7. **Worker timeout** (§5.5): default 30min, caller-overridable up to 2hr cap, dispatcher kills on expiry with audit log + thread `timeout` notice.
8. **Agent-harness self-monitoring** (§5.6): mechanism in harness layer (post-result + clean-exit); detection in the Claude-API agent harness specifically (model-stall, tool-budget, exception). Other agent harnesses implement when they ship — the contract is the mechanism, not the algorithm.
9. **Triage turn** (§5.7): port the existing agent-network harness pattern (Triage Turn) into the Nexus Claude-API agent harness. Hard rules (mention/operator), cheap-model classification with `{engage, reason, tier, mode}` JSON output, fail-open on parse error, per-aspect disable config. Reference implementation: `code/harness/index.js` triageHardRules + buildTriagePrompt.
9. Tests: fairness invariant (multi-aspect with one hogger), spillover (idle aspect over soft cap), hard ceiling rejection, identity-mismatch rejection, non-admin override rejection, dispatcher timeout fires, harness self-monitor fires (model-stall, tool-hang, harness-exception scenarios).
10. Terminology layer (per §5.2): config struct, render helper at user-facing edges, default `worker → hand` mapping for nexus-cw. Generic terms remain in code + protocol.
11. BUILD.md update + spec cross-reference.

(Pre-turn gate test cases roll into part 9 alongside the gate implementation.)

The current `nexus/handqueue` package name pre-dates this spec. Renaming the directory is out of scope for v0.1 (touches imports across the tree); leave the package name alone, treat it as legacy. New code uses generic vocabulary in identifiers and comments.

Each part: branch → code → test → review → merge. Workflow per `agent-network/CLAUDE.md`.

---

## 10. Verification

Once implemented, the following must hold:

1. **Fairness:** with `N=3` and 3 aspects each looping a dispatch, all three aspects make progress; no single aspect's queue grows unboundedly.
2. **Spillover:** with all 3 workers busy on aspect A's work, an arrival from aspect B (no active worker) immediately spawns a 4th worker (under H).
3. **Hard ceiling:** with `N=3, H=5`, the 6th simultaneous spawn attempt rejects with `hard_ceiling`.
4. **Thread routing:** a dispatch posted in thread T results in the worker's reply landing in thread T, no dispatcher-side routing.
4b. **Identity inheritance:** maren dispatches → the worker subprocess boots loaded with maren's NEXUS.md/SOUL.md/PRIMER → its reply posts as `from: maren` (not as a generic worker, not as `from: orchestrator`). Same chat presence, same voice, same auth identity as if maren replied directly.
5. **Substrate compat:** existing §6.4 e2e smoke test (cross-aspect verify-canon) still passes unchanged.
6. **Identity enforcement:** an aspect submitting a dispatch with `aspect: <other-id>` is rejected with `403 identity_mismatch`. Cannot spoof another aspect's id to game fairness.
7. **Admin enforcement:** an aspect (non-admin token) calling any override op (abort/kill/force-shutdown/take-surface-offline) is rejected with `403 admin_required`. Frame's admin token is the only caller honored.
8. **Dispatcher timeout:** a dispatch with no `deadline_secs` runs until the default (30min) and is then killed by the dispatcher; thread receives `timeout` notice. A dispatch with `deadline_secs: 60` is killed at 60s. A dispatch with `deadline_secs: 99999` is capped to the maximum (2hr) without erroring.
9. **Agent-harness self-monitor (model stall):** a dispatch whose Claude API call hangs (simulated 60s+ no response) is terminated by the *agent harness* inside seconds with an `agent_error` result, NOT by the dispatcher's 30-min deadline.
10. **Agent-harness self-monitor (tool hang):** a dispatch where a tool execution hangs past its per-tool budget is cancelled by the agent harness with a `tool_timeout` result; the turn continues or terminates per the agent harness's own rules.
11. **Harness-layer crash trap:** an unhandled exception in the agent harness that the agent harness itself doesn't catch falls through to the harness layer, which posts a `harness_crash` result and exits cleanly. (Layer 2 of the three-layer scheme.)
12. **Triage hard rule (mention):** dispatch tagged with direct `@<agent>` mention bypasses the triage model entirely → engage=true, tier=3, mode=reply.
13. **Triage hard rule (operator):** dispatch from operator bypasses the triage model entirely → engage=true, tier=3, mode=reply.
14. **Triage skip:** dispatch with no-net-new-info → triage model returns `engage=false` → agent harness skips, posts `skipped_no_signal` with triage reason, exits clean. No frontier-model tokens burned.
15. **Triage react:** dispatch warranting only acknowledgment → triage returns `engage=true, mode=react` → agent harness emits emoji and exits without running a full turn.
16. **Triage parse failure:** triage model returns malformed JSON or times out → fails open (engage=true) → agent harness runs full turn. Skipping on triage error is forbidden.
17. **Triage session isolation:** an engage=false dispatch produces zero chat output (no JSON leak, no skipped_no_signal echo of the verdict). A subsequent main turn for the same agent shows no awareness that triage ran. Triage prompt + response live in a sidecar that does not touch the main session JSONL.
