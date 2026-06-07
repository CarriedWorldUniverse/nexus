# Dispatch-native platform architecture

**Date:** 2026-06-08
**Status:** architecture spec (approved via operator design dialogue, 2026-06-08)
**Scope:** the umbrella architecture. The dispatch *mechanics* (named-agent identity/threading) and the *runtime* layer (pod images, per-agent home, hooks) are specced in their own notes and referenced here.
**Relates:** `2026-06-08-named-agent-dispatch-model.md` (NEX-492/494, LIVE), `2026-06-08-dispatch-pod-and-home-model.md` (NEX-491: 493/499/500/501), NEX-434 (k3s work dispatch). Memory: nexus-team-manager ideal, dispatch-native architecture, cluster-tailnet environment, interchange-gateway respec.

## North star

nexus turns a solo developer into the **manager of a team**. The platform is **dispatch-native**: there is exactly one always-on conversation — operator ↔ shadow — and everyone else spins up on demand, does the work as a real named team member, reports back to an audited thread, and exits. Structured as **two planes** (control / workload) and **two harnesses** (agentharness / agentfunnel) over a **multi-node** workload cloud.

## 1. Operator surface — 1:1 with an always-on shadow

The operator talks to **one** always-on agent: **shadow**, the root orchestrator.
- **shadow is an always-on resident** (a k3s pod like keel) with its own per-agent home/memory, identity, and MCPs — gaining continuity across operator sessions (same shadow, persistent memory) rather than a fresh CLI each time.
- The **"1:1 chat engine" is thin**: a focused client on the operator↔shadow thread over the existing broker chat — not a new subsystem.
- **Agora is retired** as the *operation* model. It is the live-multi-aspect-presence model, which is exactly what the reliability reasoning argues against (dispatch-backed async beats held live sessions; keep the live team minimal). The one always-on live thing is shadow; everyone else is dispatch.
- **Everything below shadow is dispatch** — spin-up-on-demand, post back to the thread, exit.

## 2. Two planes — control vs workload (network-isolated, one-way trust)

- **Control / management plane (trusted):** shadow + **agentharness** on each box. Its own network/tailnet. Home of privileged operations — host `sudo`, `k3s` admin, node join — and the orchestrator. *Provisions and manages* the workload plane.
- **Workload plane (sandboxed):** the multi-node k3s work cluster running **agentfunnel** dispatch pods. A separate network/tailnet. Where untrusted, LLM-driven work runs.

**One-way trust boundary:**
- **control → workload**: shadow dispatches work in; agentharness provisions/joins nodes.
- **workload → control**: *blocked*. A dispatch pod cannot route to the control plane, agentharness, or the machine-control creds — so a compromised or rogue work pod **cannot escalate to machine control**.

A **network/tailnet** boundary (not in-cluster RBAC alone) is the point: defense in depth — even a k8s privilege-escape stays trapped in the workload network. Matches the established "each cluster is its own tailnet env" direction.

**Bootstrap dependency direction:** the control plane *stands up* the workload cluster (agentharness joins nodes) and so must exist first and independently — agentharness can't live in the cluster it creates. The separation is therefore *necessary*, not merely safe.

**The broker / interchange is the mediated gateway** straddling the boundary: shadow dispatches and agents post back over it, so it is the one controlled channel between planes while the planes are otherwise network-isolated (lines up with the interchange-as-public-boundary-gateway respec).

## 3. Two harnesses

Same agent-runtime shape (headless CLI + broker seam); they differ in *where* they run and their *privilege*:

| | **agentfunnel** | **agentharness** |
|---|---|---|
| Runs | in-cluster pod | on the box (host service) |
| Sandbox | sandboxed, ephemeral | host-privileged, persistent |
| Purpose | dispatch work (code/art/research) | machine control (setup/maintenance/ops) + cluster membership |
| Spawned by | the cluster | the box / control plane |
| Creds | scoped, lazy (custodian) | host-scoped, **audited, human-gated** (blast radius = the machine) |

## 4. Multi-node workload cloud

- **dMon = control-plane node**; other boxes run **agentharness → install + join the k3s agent → become worker nodes** → pods schedulable there. To add hardware, you run agentharness on it. (agentharness does double duty: machine control *and* cluster join.)
- **Substrate = the tailnet** — nodes can be anywhere (LAN or remote), communicating over Tailscale.
- **Schedule by hardware/role:** node labels + affinity (`gpu=true` → GPU work like forge training / maren render lands on a GPU node; CPU work spreads anywhere). This resolves the **single-5090 contention** and spreads parallel builders across CPU nodes.
- **Join token is a sensitive credential** → brokered via custodian, scoped, issued only to agentharness on an *authorized* box. Joining is gated; only trusted hardware is admitted (a joined node runs your workloads).

## 5. Runtime layer (already designed — referenced)

- **Named-agent dispatch** (run-as-named-agent, post-as-thread-root) — NEX-492/494, **LIVE**.
- **Role-generic pod images**, **per-agent bare-git home** (worktree-merge, versioned/auditable memory), **shared repos mirror**, **funnel hooks** — `2026-06-08-dispatch-pod-and-home-model.md`, NEX-491 (493/499/500/501).

## Open questions (to resolve during build)

1. **Control-plane HA** — dMon as the single control-plane node is a SPOF; HA control plane is a later step.
2. **agentharness privilege/gating model** — exactly how machine-control ops are human-gated, audited, and cred-scoped.
3. **Broker/interchange placement** relative to the plane boundary — the gateway design (what crosses, how it's authenticated).
4. **Node admission/trust** — the join-credential flow; only admit hardware you control.

## Build order (rough; runtime layer NEX-491 proceeds in parallel)

1. **shadow as an always-on control-plane resident + the 1:1 operator surface** (retire agora).
2. **agentharness** — the on-box privileged machine-control harness.
3. **Plane separation** — two tailnets, one-way trust, broker as the gateway.
4. **Multi-node k3s** — agentharness join mechanism + role/GPU node-affinity scheduling.
