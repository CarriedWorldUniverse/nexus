# M2 — Builder-Agent Worker Runtime — Design

**Status:** Approved design (2026-06-05) · **Story:** NEX-436 · **Epic:** NEX-434 (k3s on-demand work dispatch)
**Depends on:** NEX-435 (M1 custodian seam — done + live-proven) · **Feeds:** NEX-437 (M3 dispatch controller)
**Related:** `docs/2026-06-05-k3s-work-dispatch-design.md`, `docs/2026-06-05-m1-custodian-auth-foundation-design.md`

## Model

The Claude-subagent / Task-workflow pattern, but the subagents are **first-class networked agents**. A dispatch is a spawn-with-brief; "result to thread" is the subagent's return. The named agents (anvil, plumb, …) stop being always-online and become **on-demand "builder" agents** — spawned per dispatch, run as their own herald identity, do the work, report, and exit.

Deltas from in-process Claude subagents, which shape the design:
- builders are **real accountable identities** — git records and comms carry the actual agent (anvil's commits are anvil's);
- they're **out-of-process k8s pods on the cluster** — heavy work is offloaded off the orchestrator's machine;
- "return" is a **durable comms-thread post**, not a function return — any orchestrator (shadow/wren) sees it, async;
- the same named agents can be dispatched directly — they're just on-demand now.

M2 is the **leaf builder runtime**: one builder, spawn → run → report → exit. (Fan-out / nested dispatch is captured for M3 — see Non-goals.)

## Decisions (from brainstorming, 2026-06-05)

1. **Identity = the named agent dispatched to.** Dispatch to anvil → runs as anvil; commits/comms attributed to anvil. Reuses existing agent herald identities — no new identity scheme.
2. **Inbox-driven brief.** The brief arrives the way dispatch works today — a chat message in the agent's inbox carrying context (ticket, repo, DoD).
3. **Not a live listener.** A builder is not always-on. It spawns, drains its pending inbox **once**, runs the task (as many internal turns as needed), posts result(s) to the thread, and exits.
4. **Explicit done-signal terminates.** The builder emits an explicit completion signal when the brief is done; the entrypoint exits on it. Deterministic — sidesteps the empty-inbox / goal-loop flakiness.
5. **Leaf-only.** Builders don't orchestrate sub-workers in M2; fan-out is M3.

## Components

### 1. Worker image
One **shared** image — identity and provider differ per-run via keyfile + binding, not per-image:
- base + coding toolchain (go, git, gh, build tools)
- provider CLIs (claude-code, codex)
- `agentfunnel` (builder mode), the nexus MCP servers (`nexus-*-mcp`), and `cw`
- **no baked-in secrets** — the M1 custodian seam supplies git/provider creds at runtime.

Built and pushed to a **local in-cluster registry on dMon** (keeps the cluster self-contained; no ghcr dependency).

### 2. Shared storage
One host-backed volume on single-node dMon (RWX via hostPath):
- `/work/<task-id>/` — per-run workspace (clone/worktree; git auth via the cw credential helper)
- `/cache/go`, `/cache/git-mirror` — shared, for fast, disk-cheap clones
Per-run subdir isolation; workspaces cleaned after the Job completes, with brief retention for debugging. Multi-node later → per-agent PVCs + a read-through cache mirror.

### 3. Builder entrypoint
`agentfunnel` in **builder mode** — same deliberation/comms/tool path as the always-on aspect, different start and end:
1. **validate as the named agent** (its keyfile → session JWT);
2. **drain the pending inbox once** — the dispatched brief;
3. **run the deliberation loop to completion** — clone → code → test → push (via cw) → open PR, as that agent;
4. **post result(s) to the thread**;
5. **exit on the done-signal** (Job → Completed).

Two pieces are net-new vs the always-on loop:
- the **drain-once / run-to-done / exit** lifecycle (vs poll-forever);
- the **done affordance** — a small comms/funnel action the builder calls when the brief is complete, which the entrypoint watches for and then exits 0. The dispatch contract instructs the builder to call it when done.

### 4. Brief delivery
Reuse the existing inbox path: the dispatch writes the brief to the agent's broker inbox (pending); the spawned pod drains it at startup. No new delivery channel. (M3's controller ensures the brief is pending and spawns the pod; M2 is exercised by **manually dispatching to a manually-applied Job**.)

### 5. Done-signal
A minimal funnel/comms action (e.g. a `done`/`stand-down` tool, or a sentinel the builder emits) marking brief completion. The entrypoint treats it as the exit trigger. It must be reliable enough that the dispatch contract can say "call this when finished" — which named-agent aspects already follow for the dispatch reply contract.

## Data flow — one dispatch (M2, manual)

1. Orchestrator (or operator) dispatches a brief to a named agent → it lands in that agent's inbox (pending).
2. A worker Job is applied with the worker image + that agent's keyfile (M2: manual `kubectl apply`; M3: controller).
3. The pod boots `agentfunnel` in builder mode → validates **as the agent** → drains the pending brief.
4. It runs the task — clones into `/work/<task-id>` (warm from `/cache/git-mirror`), codes, tests; git auth + provider keys flow through the M1 seam (cw credential helper / `kind=provider`), so no raw secret is in the pod env.
5. On success it pushes a branch and opens the PR **as that agent**, posts a structured result to the thread, and emits the **done-signal**.
6. The entrypoint exits 0; the Job is Completed; `/work/<task-id>` is cleaned. The orchestrator reads the result over comms.

## Identity & security

- **Who** = the named agent's herald identity (its keyfile/session JWT) — the same identity that authenticates, posts to comms, and (via cw `issue-git-permission`) holds a scoped git credential. Git records carry the real agent.
- **What it may use** = the M1 custodian seam — scoped, audited git + provider credentials, **never raw in the pod env**.
- codex builders run with codex's internal sandbox disabled (bridle default, 2026-06-05) — the pod is the trust boundary.

## Testing

- **Entrypoint unit:** builder-mode lifecycle — drains a seeded inbox once, runs to a stubbed done-signal, exits 0; does not re-poll after draining.
- **Done-signal unit:** the affordance triggers exit; absence keeps running (bounded).
- **Live (the DoD):** a manually-applied k8s Job on dMon — worker image + a named agent identity (e.g. anvil) + M1 auth — drains a real coding brief (a NEX-433-style fix), does the work, pushes + opens the PR **as that agent**, posts the result to the thread, and exits cleanly; assert the pod held no raw secret and the Job reached Completed.

## Non-goals (deferred)

- **The dispatch controller** that auto-spawns the Job per dispatch — **M3 (NEX-437)**. M2 is runnable by manual dispatch + manual Job.
- **Fan-out / nested dispatch / duplicate-workflows** (a builder dispatching N children, parent awaiting + aggregating up the tree) — **M3**. M2 keeps builders leaf-only but ensures a builder can *emit* a dispatch action so the capability is reachable. The parent-await fork (parent-stays-alive vs controller-continuation) is recorded on NEX-437.
- Migrating the always-on aspects wholesale — the builders become on-demand *as a consequence* of this work, but the broader migration/orchestrator-hierarchy is its own epic.

## Open questions

- **Registry** — confirm local in-cluster registry vs ghcr at plan time (leaning local for self-containment).
- **Done-affordance shape** — a dedicated `done` tool vs a reply sentinel vs reusing a stand-down action; resolve at plan time against the funnel's comms-action surface.
- **Brief delivery** — pending-inbox-drain (reuse, recommended) vs controller-injected-at-spawn; M2 assumes inbox-drain, M3 may add injection.
- **Worker identity provisioning** — the agent keyfile must be available to the pod (a k8s Secret per agent, or derived); finalize how keyfiles reach builder pods.
