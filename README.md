# Nexus

A coordination layer for running multiple AI agents as a coherent team.

Nexus is a single central process (broker + orchestrator) plus a lightweight per-agent runtime. Agents register on boot, communicate through a shared message bus, and can invoke each other's stateless capabilities ("Hands") over that same bus. All context is owned by Nexus, not by the underlying AI provider — sessions live as tree-structured JSONL files, compaction is proactive, and rewind is a non-destructive tree operation.

## Status

Early scaffold. The design is drafted in [`docs/2026-04-22-nexus-registration-spec.md`](docs/2026-04-22-nexus-registration-spec.md) (v0.4). No runtime code yet. See [`BUILD.md`](BUILD.md) for the build plan and current step.

## Shape

- **Nexus process** — central broker (messaging + REST), orchestrator (polling, watches, alarms), and an embedded frame-agent. Single long-running process.
- **Agent runtime** — one executable (`agent`) that reads an agent's home folder and runs it. Comms-first; no terminal attach, no direct user-to-agent bypass.
- **Three context modes** per agent: `global` (one long-running session), `thread` (one session per chat thread), `stateless` (no persistence; the mode Hands live in).
- **Provider layer** — the runtime is AI-agnostic. A provider module implements `invoke / tokenCount / compact` against a specific backend. v1 ships `claude-api` only; OpenAI and others are module-drops.
- **Hands** — stateless, single-turn capabilities invoked via comms messages. An agent can call its own Hands to offload noisy subtasks, or another agent's Hands for cross-domain work. Audit trail is automatic because invocations flow through the same message bus as chat.

## Design principles

- **Comms-first.** Every interaction is a message on the bus. No agent is reachable outside the bus, which means every exchange is auditable, broker-logged, and visible to operators.
- **Context is Nexus's responsibility.** The runtime owns session JSONL, not the provider. This keeps providers swappable and makes rewind, fork, branch-summary, and proactive compaction first-class operations rather than provider-specific features.
- **Registration is dynamic.** Nexus doesn't know what agents exist at startup. Agents register with `POST /aspects/register` and announce their capabilities; the roster is a live runtime construct, not a config artifact. This is the honest prototype of federation: cross-Nexus is the same protocol over the wire.
- **Proactive over reactive.** Compaction fires before the model degrades (`tokens > window - reserve`), not after. Rewind preserves history in-tree rather than truncating.

## Cross-platform

Windows, Linux, and macOS. The runtime and Nexus process are built as Go single-static-binary executables — `nexus` and `agent`.

## Repository layout

```
nexus/       central process (broker, orchestrator, embedded frame-agent, admin)
runtime/     agent.exe: providers, context persistence, comms dispatch, Hand dispatch
agents/      per-agent home folders (populated at migration time)
shared/      path resolution, auth, schemas
docs/        design specs and decision records
scripts/     build and launch helpers
tests/       end-to-end harness
```

See the README in each subdirectory for detail.

## License

See [`LICENSE`](LICENSE).

## Status note

This repository is currently private. It will open once there's something usable to share — probably once the end-to-end Hand invocation path works and at least two agents can register and coordinate. See [`BUILD.md`](BUILD.md) for where we are in that plan.
