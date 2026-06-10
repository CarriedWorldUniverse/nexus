# M3 — Dispatch Controller · Implementation Spec (NEX-437)

> **Status (as of 2026-06-11):** historical. The standalone dispatch-controller aspect specced here was retired in favour of the **broker-inline recursive Runner**, which is the live dispatch path. Current identity model: `../2026-06-08-named-agent-dispatch-model.md`.

**Status:** Design (2026-06-06) · **Epic:** NEX-434 · **Milestone:** M3 (NEX-437)
**Depends on:** NEX-435 (M1 custodian seam — done + live-proven), NEX-436 (M2 worker runtime — built + lifecycle-proven)
**Feeds:** M3b fan-out (separate follow-on spec)
**Related:** `docs/2026-06-05-k3s-work-dispatch-design.md` (epic contract), `docs/2026-06-05-m2-builder-worker-runtime-design.md`

## Goal

Make builder dispatch **self-service**: an orchestrator (shadow/wren) dispatches a work brief and a k8s Job spawns on dMon's k3s, runs the brief as the named builder-agent, returns the PR/answer over comms, and exits — **no manual `kubectl`**. This is the k3s backend for the existing `!dispatch` verb (`[[project_dispatch_purpose]]`: "subagent with comms"), spawning a *pod* instead of a local subagent.

## Scope

**This spec: the single-dispatch controller only.** One brief → one Job → one result. Fan-out (a builder spawning sub-workers, a parent staying alive to aggregate results) is explicitly deferred to its own spec (M3b) — see Non-goals. The controller delivers the M3 DoD and unblocks real builder runs without it.

## Decisions (from brainstorming, 2026-06-06)

1. **Controller interface = its own connected inbox.** The controller is a long-lived broker client (`@dispatch-controller`, its own herald identity), always connected like an orchestrator. **Dispatch = a message to its inbox** (normal comms, no special channel). The controller drains its inbox; **each message → one Job, with the message as the seed brief.**
   - *Why an inbox works here:* the inbox-drop failure (a disconnected client loses pending messages — found in the M2 live run) only hits **late-connecting** clients. The controller is always connected, so its inbox is reliable. We use the inbox at the layer where it works (always-on controller) and inject-at-spawn at the layer where it doesn't (the late-joining pod).
2. **Brief delivery to the worker = inject at spawn.** The controller writes the brief into the Job as a mounted file; `agentfunnel -builder -brief-file …` reads it as the seed work item at startup. This replaces the **broken** "pod drains its pending broker inbox" assumption from the epic/M2 docs (the broker does not persist a disconnected aspect's inbox).
3. **Worker identity = the named agent, on-demand.** Dispatch to anvil → runs as anvil; commits + comms attributed to anvil (the agent-attributed-git-records model). Builder-agents stop being always-on systemd services (so no broker WS-slot contention); orchestrators (shadow/wren) stay always-on. **M3 retires the always-on builder aspects** on dMon.
4. **Dispatch durability = ACK + ticket-keyed idempotency.** The controller's inbox is non-durable across *its own* restarts. So: the controller ACKs each dispatch in-thread (a dropped dispatch is detectable), and a dispatch is keyed by its ticket — if a Job for that ticket is already running, re-dispatch adopts/no-ops rather than double-spawning. "Re-send if no ack" is therefore safe.
5. **Failure/retry = no silent retry** (`backoffLimit 0`). On failure the controller posts the failure + a log pointer to the ticket thread; the orchestrator decides (re-dispatch or intervene).
6. **Concurrency = controller-enforced caps** (cluster-wide + per-orchestrator); beyond the cap, queue (FIFO) rather than reject.

## Architecture

An in-cluster **controller service** (one k3s Deployment) that:
- connects to the broker as `@dispatch-controller` and watches its inbox for dispatch briefs;
- for each brief, **provisions** the run (keyfile Secret, M1 git-cred grant, brief file) and **creates** a k8s Job from the M2 worker template, running as the named agent;
- **tracks** the Job lifecycle (watch) and **reports** status back into the originating ticket thread;
- enforces concurrency caps and ticket-keyed idempotency.

It holds a Kubernetes ServiceAccount with RBAC scoped to `Job` + the Job's `Secret`/pod-log reads **create/get/list/watch/delete in the `nexus` namespace only** — no cluster-wide privileges. The broker stays unprivileged with respect to k8s.

```
orchestrator ──!dispatch(k3s, agent, brief)──▶ @dispatch-controller inbox
                                                      │  (drain; one msg = one brief)
                                                      ▼
                                          provision: keyfile Secret + M1 git-cred + brief file
                                                      │
                                                      ▼
                                          create Job (M2 image, named-agent identity,
                                                      injected brief, M1 seam auth, /work)
                                                      │  watch lifecycle
   ticket thread ◀── status (spawned/running/done/failed) + worker's PR-link/result ──┘
```

## Components

### 1. Controller service (`runtime/cmd/dispatch-controller`, new)
- Broker client (reuse the `wsclient`/keyfile validate path) as `@dispatch-controller`.
- Inbox drain loop: for each unprocessed dispatch message → parse the brief → idempotency check → provision → create Job → register a lifecycle watcher. ACK the message (react/reply in-thread) on accept.
- Kubernetes client (in-cluster config + the scoped ServiceAccount).
- In-memory map of active Jobs keyed by ticket (for idempotency + concurrency accounting); rebuilt from a Job `list` (label selector) on controller restart so it recovers in-flight Jobs.

### 2. `!dispatch` k3s routing (broker side)
- The existing `!dispatch` parser gains a **k3s target** (e.g. `!dispatch k3s @anvil NEX-XXX …` or a `target=k3s` field). A k3s-target dispatch is delivered as a message to `@dispatch-controller`'s inbox carrying the structured brief.
- Brief schema (the message body / a fenced block): `{ agent, repo, ticket, branch?, brief (task + DoD), thread }`.

### 3. Job provisioning (controller)
Per brief, before Job create:
- **Keyfile Secret** — ensure `aspect-keyfile-<agent>` exists in `nexus` (mint via the broker/herald path; created once, reused).
- **Git-cred grant** — `cw issue-git-permission` (M1) scoped to the brief's repo for that agent, so the worker's `git push` works through the custodian seam with no raw secret.
- **Brief file** — the dispatch message written to a per-Job `ConfigMap`/`Secret` mounted at `/etc/nexus/brief.md`.
- **Provider auth** — provider keys (codex/claude) brokered via the M1 seam (`kind=provider`), not hand-mounted. (The M2 live run mounted codex auth manually; M3 automates this through M1.)

### 4. Brief injection (M2-runtime addition — small)
- `agentfunnel -builder` gains `-brief-file <path>`: at startup, read the file as the seed work item and run it (instead of waiting on the inbox). The existing `<<TASK_COMPLETE>>` sentinel (NEX-440) remains the completion signal.

### 5. Lifecycle + status (controller)
- Watch the Job → post concise status to the ticket thread on transitions (spawned / running / completed / failed). The **worker** posts its own substantive result (PR link) + the sentinel; the controller posts the terminal Job state and cleans `/work/<task-id>`.
- Surface active Jobs in roster/dashboard (list by label). Custodian + Ledger carry the credential-use audit trail.

## Data flow — one dispatch
1. Orchestrator issues `!dispatch` targeting k3s for `@anvil` on `NEX-XXX` (brief + DoD), in the ticket thread.
2. Broker routes it as a message to `@dispatch-controller`'s inbox.
3. Controller drains it, ACKs in-thread, checks idempotency (no live Job for NEX-XXX) and the concurrency cap, then provisions (keyfile Secret + git-cred + brief file).
4. Controller creates the Job: M2 image, `agentfunnel -builder -brief-file /etc/nexus/brief.md`, named-agent keyfile, hostAliases, `/work` + cache, M1-seam auth.
5. Pod boots, validates **as anvil**, reads the injected brief, clones into `/work/<task-id>`, codes/tests; git + provider auth flow through the M1 seam.
6. On success it pushes a branch and opens the PR **as anvil**, posts a structured result to the ticket thread, emits `<<TASK_COMPLETE>>`.
7. Job exits 0; controller records Completed, cleans `/work`, posts terminal status. Orchestrator reads the PR over comms and proceeds (shadow reviews).

## Identity & security
- **Attribution:** named-agent identity end to end (commits, PR, comms).
- **No raw secrets in the pod:** git + provider auth via the M1 custodian seam; the brief file holds task text only.
- **Least privilege:** controller RBAC scoped to Jobs/their Secrets/pod-logs in `nexus` only; per-dispatch git-cred scoped to the brief's repo.
- **Audit:** custodian + Ledger record every credential issuance/use.

## Dependencies / coordination
- **M2-runtime add:** `agentfunnel -builder -brief-file` (§4) — a small change in `runtime/cmd/agentfunnel`.
- **M1 seam:** provider-key brokering for the pod (so codex/claude auth isn't hand-mounted) + `cw issue-git-permission` for the per-dispatch git grant.
- **Retire always-on builders:** stop `aspect@{anvil,forge,harrow,maren,verity}` systemd services on dMon as they move to on-demand; orchestrators (shadow/wren) unaffected.

## Testing / DoD
- **Unit:** brief parsing; idempotency (re-dispatch of a live ticket no-ops); concurrency cap (queue beyond N); Job-spec construction (golden); controller restart rebuilds active-Job state from a Job `list`.
- **Integration (live, dMon):** shadow `!dispatch`es a **real coding brief** at k3s for a named agent → Job spawns as that agent → it clones, codes, tests, pushes, opens a **real PR**, returns it over comms → exits, **no manual kubectl**. This simultaneously closes M2's remaining full-DoD (a builder shipping a real PR). Thin-device offload demonstrated (dispatch from a device that can't run the toolchain).

## Non-goals (this spec)
- **Fan-out / nested dispatch** (a builder dispatching sub-workers; parent-stays-alive result aggregation) → M3b, its own spec.
- **Multi-node** scheduling / per-aspect PVCs → future (single-node dMon for now).
- **Autoscaling / bin-packing** beyond the simple concurrency cap.

## Open questions
- **Brief file as ConfigMap vs Secret** — the brief is task text (not secret), so ConfigMap is the default; revisit if briefs ever carry sensitive context. (Lean ConfigMap.)
- **Queue persistence** — the FIFO queue beyond the concurrency cap lives in controller memory; a controller restart drops queued-but-unstarted dispatches (running Jobs recover from the label `list`). Mitigated by ACK + re-dispatch; a durable queue is out of scope unless it bites.
