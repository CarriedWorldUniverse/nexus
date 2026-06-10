# Nexus

[![CI](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/CarriedWorldUniverse/nexus/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/CarriedWorldUniverse/nexus?include_prereleases&sort=semver&display_name=tag)](https://github.com/CarriedWorldUniverse/nexus/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/CarriedWorldUniverse/nexus.svg)](https://pkg.go.dev/github.com/CarriedWorldUniverse/nexus)
[![License](https://img.shields.io/github/license/CarriedWorldUniverse/nexus)](LICENSE)

A coordination layer for running multiple AI agents as a named team.

Nexus turns one developer into the manager of a team. The team members
("aspects") are named, lore-rooted minds — each with its own scope, persona,
and signing identity. The operator addresses an aspect by name; the aspect does
the work *in its scope*, *under its identity*, and reports back into an audited
chat thread. A central **broker** carries all the chat, observability, and
admin; the work itself runs as on-demand Kubernetes Jobs that boot **as** the
addressed agent.

This is a single-operator R&D system, run for real on a small k3s cluster. It
is operational and moves fast — it is not a product, and this README tracks the
live shape rather than a finished spec.

## Status

In active use. The broker, the aspect runtime (funnel + bridle harness), and
the dispatch fabric are deployed on k3s and carrying real work. What is built
versus still in flight is called out per-section below; the design record lives
under [`docs/`](docs/) (the dated files are the decision trail).

## Shape

### Broker

A single long-running process (`nexus`) that is the cluster's nervous system:

- **Chat over WebSocket** — every interaction is a message on the bus. Aspects,
  the dashboard, and operator clients (`agora`) all connect over the same
  socket; nothing is reachable off the bus, so every exchange is auditable.
- **Dashboard + admin** — roster, per-aspect config (persona, model/provider
  binding, MCP profile, credential grants), and operator surfaces.
- **Observability** — aspect turns stream as `TurnFrame`s through an observe
  hub. Each frame now carries per-turn `Timing` (round- and tool-level wall
  clock, forwarded from the bridle harness), so a turn's time can be read back
  rather than guessed at.
- **Credentials (custodian)** — brokered, identity-scoped access to our own
  services (jira / ledger / cairn) resolved lazily on first use; no raw secrets
  handed to aspects.

### Aspect runtime — funnel + bridle

An aspect runs as `agentfunnel`. The **funnel** owns the agent's session JSONL,
comms dispatch, and context lifecycle; it imports **[bridle](https://github.com/CarriedWorldUniverse/bridle)**,
the provider-agnostic harness that drives one model turn with one tool surface.
Aspects are comms-first: no terminal attach, no direct user-to-agent bypass.

Providers live under `runtime/providers/` — `claude-api`, `claude-code`,
`openai-api`, and a native `ollama-local` lane for cheap local models on the
cluster GPU. (Bridle carries the broader provider set the funnel can select.)

### Dispatch fabric

The heart of the platform. Work is handed to a **named agent**, not a faceless
worker:

- **Address-and-report.** `@plumb …` (natural) and `!dispatch plumb …`
  (explicit) are the same primitive. The post is stored as the thread root
  (the audit anchor); the broker resolves the label `plumb` to its profile and
  spawns a worker.
- **Run-as the named agent.** The spawned Job mounts `aspect-keyfile-<agent>`,
  so the worker validates **as** that agent → loads its persona (scope/lens) and
  signs commits and reviews under the agent's identity. Attribution is real, not
  cosmetic; the persona is a load-bearing attention prior, so the same diff
  surfaces different concerns under different named reviewers.
- **Identity is herald-rooted.** Agent keys derive from an owner seed via
  `DeriveAgentKey(seed, slug)`; the worker registers over the WS with a signed
  herald register-handshake assertion — boot-by-name when the owner is
  authorized.
- **Audited threads.** The worker posts results back into the originating
  thread and exits. It can `@`/`!dispatch` onward; the parent is inferred from
  the sender, so the recursion tree and audit chain stay linked.
- **Per-agent serialism, cross-agent parallelism.** One run per agent name at a
  time (one-session-per-name, NEX-464); different agents run concurrently; a
  second task for a busy agent queues and drains when it frees.

### Napping presence

Aspects are **addressable-but-napping**: identity and inbox persist while the
pod is scaled to zero. A wake-on-mention controller scales the Deployment 0→1
on a chat delivery addressed to a napping aspect, and an idle reaper scales it
back to zero after a quiet window. Presence is the name answering, not a pod
burning watts. *(Built: NEX-568 P1. The mediated multi-aspect roundtable,
non-blocking turns, and aspect-owned audited fan-out — workers carrying the
parent aspect's persona under derived kindred-word identities, e.g.
`shadow.umbra` — extend the [named-agent dispatch model](docs/2026-06-08-named-agent-dispatch-model.md)
and are in flight.)*

## Design principles

- **Comms-first.** Every interaction is a message on the bus — auditable,
  broker-logged, operator-visible. No agent is reachable outside it.
- **The label is the contract.** An agent's name is the single routing key; it
  resolves to one profile (identity + credentials + pod image). One lookup →
  scope, access, and toolchain.
- **Context is Nexus's responsibility.** The funnel owns session JSONL, not the
  provider, which keeps providers swappable and makes compaction, rewind, and
  branch-summary first-class.
- **Dispatch-backed async over held sessions.** Spawn → post the brief → agent
  reports back → exit is deterministic and needs no fragile always-on session.
  The audit thread doubles as the result inbox.

## Cross-platform

Windows, Linux, and macOS. Nexus and the runtime build as Go single-static
binaries — `nexus`, `aspect`, and `agentfunnel`.

## Repository layout

```
nexus/       central process: broker (chat/dashboard/observe/admin), orchestrator,
             roster, custodian, dispatch interception, observability hub
runtime/     aspect/agentfunnel: providers, context persistence, comms + Hand dispatch,
             dispatch runner (broker-embedded), herald register, obs forwarding
deploy/      k3s manifests (broker, worker, per-aspect pods)
agents/      per-aspect home folders
shared/      path resolution, auth, schemas
cmd/         standalone command-line tools
relay/       relay transport
docs/        design specs and dated decision records
scripts/     build and launch helpers
tests/       end-to-end harness
```

See the README in each subdirectory for detail. This repository is public.

## License

See [`LICENSE`](LICENSE).
