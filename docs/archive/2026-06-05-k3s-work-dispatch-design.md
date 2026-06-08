# k3s On-Demand Work Dispatch — Design

**Status:** Approved design (2026-06-05) · **Epic:** NEX-434 · **Milestones:** NEX-435 (M1), NEX-436 (M2), NEX-437 (M3)
**Related:** NEX-433 (case-in-point), NEX-376 (herald / unified identity), `docs/2026-06-04-credential-custodian-design.md`

## Problem

The aspects (anvil, forge, harrow, …) run on dMon as **per-user systemd units** — `aspect@<name>.service`, each a separate Linux user with its own home. That model has no shared writable workspace and no shared git/GitHub auth, so an aspect **cannot ship code**: it has nowhere to create a worktree it owns, no `gh`/SSH credential, and no sudo to acquire one. NEX-433 is the case-in-point — anvil produced a complete, verified fix but could not write a branch or push, and its blocked reply was suppressed by the judge, so it never even surfaced.

Patching this per-user (ACLs, per-aspect tokens) spreads credentials across Linux accounts and scales badly. The structural fix is to stop running heavy/code work as a pinned Linux user and instead run it as an **on-demand container with a writable volume and brokered auth** — on the k3s cluster that already hosts CWB on dMon.

This also unlocks **compute offload**: thin devices (little-blue and other low-spec machines) can dispatch heavy work (clones, builds, worktrees) to dMon and get the answer back over comms, instead of hogging local CPU and disk.

## Goals

- An orchestrator (shadow → dev, wren → Unity) dispatches a unit of work; it runs as an **on-demand k8s Job** on dMon's k3s and returns its result over comms.
- A worker can **clone, build, test, push a branch, and open a PR** — with a writable workspace and git/provider auth that the agent **never sees the raw secrets of**.
- Idle cost is zero (no always-on worker pods); thin devices offload transparently.

## Non-goals (deferred to a later epic)

- Migrating the existing **always-on** aspects (and the broker) off systemd onto k3s.
- The orchestrator hierarchy itself (few high-powered always-on orchestrators; shadow overrides wren). This design assumes today's setup and only adds the on-demand dispatch capability.

## Architecture

Three milestones, built in order. M1 is load-bearing — everything consumes its auth.

```
orchestrator (shadow)                dMon k3s
  │  !dispatch (k3s backend)            │
  ▼                                     ▼
dispatch controller ──creates──▶  Worker Job (pod)
  (M3)                                 │  image: toolchain + provider CLIs
  │                                    │         + agentfunnel + MCP + cw  (M2)
  │                                    │  mounts: /work/<task-id> + shared caches
  │                                    │  identity: herald (DeriveAgentKey)
  │                                    │  auth: cw ⇄ custodian (no raw secrets) (M1)
  │                                    │
  │            comms thread  ◀──posts result (PR link / answer)──┘
  ▼
orchestrator reads result over comms; Job exits (idle cost zero)
```

### M1 — Auth foundation (custodian-first) · NEX-435

The invariant: **no raw secret ever reaches the agent.** custodian is the broker; `cw` is the execution arm (the git-side parallel to lynxai on the web side).

- **Custodian credential broker** — the operator stocks it; it issues *scoped, short-lived* credentials on request and audits every issuance/use via Ledger. This extends the existing `nexus/cwb/custodian` package and the credential-custodian design (`docs/2026-06-04-credential-custodian-design.md`) — it is **not** greenfield.
- **`cw` git credential helper** — registered as git's `credential.helper`. On push/clone, `cw` requests a scoped ephemeral token from custodian for that one operation and hands it to git; the token never lands in env, shell history, or repo config. The agent runs plain `git push` and it works.
- **`cw issue-git-permission`** — mints and registers a worker's least-privilege git permission (e.g. "push to `CarriedWorldUniverse/nexus`") in custodian, so each worker gets only what its brief needs.
- **Provider keys** (claude-code / codex / direct-API) are brokered the same way — supplied at call-time from custodian, never mounted into the pod.

### M2 — Worker runtime · NEX-436

Consumes M1. Depends on M1.

- **Worker image** — coding toolchain (go, git, gh, build tools), provider CLIs (claude-code, codex), `agentfunnel`, the nexus MCP servers (`nexus-*-mcp`), and `cw`. No baked-in secrets. Built and pushed to a registry (local k3s registry or ghcr).
- **Shared storage** — one host-backed volume on single-node dMon (RWX). Layout: `/work/<task-id>/` per-task workspace (clone/worktree) and shared `/cache/go` + `/cache/git-mirror` so clones are fast and disk-cheap. Per-task subdirectory isolation; workspaces are cleaned after the Job completes (with brief retention for debugging). If dMon ever goes multi-node, split `/work` to per-aspect PVCs; the cache can become a read-through mirror.
- **One-shot aspect entrypoint** — `agentfunnel` in a task/one-shot mode (distinct from the always-on deliberation loop): validate against the broker with a **herald identity** (`DeriveAgentKey(owner_seed, slug)` — a worker-identity pool or a per-dispatch ephemeral identity), run the brief to completion (clone → code → test → push via cw → open PR), post the result to the comms thread, and exit.

### M3 — Dispatch controller · NEX-437

The seam that makes it self-service. Depends on M2.

- An **in-cluster controller service**: receives a dispatch request (work brief — task, repo, ticket, DoD, identity), creates a k8s **Job** (the M2 worker), tracks its lifecycle, and surfaces status. Runs with a ServiceAccount scoped to Job create/watch/delete in its namespace.
- It is a new **k3s backend for the existing `!dispatch`** mechanism ("subagent with comms") — spawning a *pod* rather than a local subagent. Orchestrators and thin devices use the same dispatch verb; the backend decides local vs k3s.
- **Observability**: Job logs + state surfaced (kubectl / roster / dashboard); custodian + Ledger provide the credential-use audit trail.

## Data flow — one dispatch

1. Orchestrator issues `!dispatch` with a work brief, backend = k3s.
2. Dispatch controller (M3) validates the brief and creates a Job with: the worker image (M2), the brief as input, a herald identity reference, and the `/work` + cache volume mounts.
3. The worker pod starts, validates against the broker as its herald identity, and joins the brief's comms thread.
4. It clones into `/work/<task-id>` (warm from `/cache/git-mirror`), does the work, runs tests. Git auth flows through `cw` ⇄ custodian (M1); provider calls draw keys the same way.
5. On success it pushes a branch and opens the PR; on completion (or failure) it posts a structured result to the comms thread.
6. The Job exits; the controller records terminal status; the orchestrator reads the result over comms and proceeds (e.g. shadow reviews the PR).

## Identity & security model

- **Identity to act** comes from herald (`DeriveAgentKey`) — every dispatched unit of work is an accountable, attributable participant, not an anonymous runner.
- **Credentials to push / call models** come from custodian via `cw`, scoped and short-lived, **never exposed to the agent**. Ledger audits who pushed/called what.
- These two are deliberately separate: identity is *who*, custodian is *what they're allowed to use right now*.

## Testing strategy

- **M1**: unit tests for the broker scoping/audit; one live round-trip — a process with no secrets in its env performs a real `git push` and obtains a provider key, both through custodian, with the Ledger audit row asserted.
- **M2**: a manually-applied k8s Job runs a real coding brief (NEX-433 is the natural first subject) end-to-end and posts its result; assert branch/PR exist and the pod held no raw secret.
- **M3**: shadow issues a k3s-backed dispatch; assert a Job spawns, returns the PR/answer over comms, and exits — no manual kubectl. Thin-device offload demonstrated.

## Decomposition

This epic is intentionally three specs. Each milestone (M1/M2/M3) gets its own implementation spec and plan before building — **M1 first**, as it is both load-bearing here and the first concrete slice of custodian. This document is the architectural contract they share.

## Open questions

- **Worker identity granularity** — a small pool of reusable worker identities vs a per-dispatch ephemeral identity derived from the dispatching orchestrator. (Leaning per-dispatch ephemeral for clean attribution; pool for simplicity. Resolve in M2.)
- **Registry** — local in-cluster registry vs ghcr for the worker image. (Resolve in M2; local keeps dMon self-contained.)
- **Failure/retry semantics** — Job backoff, partial-work cleanup, and how a failed brief reports back. (Resolve in M3.)
- **Concurrency caps** — per-orchestrator / cluster-wide limits on simultaneous worker Jobs. (Resolve in M3.)
