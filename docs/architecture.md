# Architecture

A bird's-eye view of how the pieces fit. Each section links into the detailed spec where one exists.

## The shape

```
                   ┌───────────────────────────────────────────┐
                   │              nexus (broker)                │
                   │   chat router · routing rules · dashboard  │
                   │   credential store · observability hub     │
                   └───────────────┬───────────────────────────┘
                                   │ WS
        ┌──────────────────────────┼──────────────────────────┐
        ▼                          ▼                          ▼
   ┌─────────┐               ┌──────────┐              ┌──────────┐
   │  Frame  │               │  Aspect  │              │  Aspect  │
   │ (keel)  │               │  anvil   │              │  plumb   │
   └─────────┘               └──────────┘              └──────────┘
        │                          │                          │
        ▼                          ▼                          ▼
   ┌─────────┐               ┌──────────┐              ┌──────────┐
   │ funnel  │               │  funnel  │              │  funnel  │
   │ bridle  │               │  bridle  │              │  bridle  │
   │provider │               │ provider │              │ provider │
   └─────────┘               └──────────┘              └──────────┘
        │                          │                          │
        ▼                          ▼                          ▼
   model API                  model API                  model API
   (claude / api /             (claude-code /              (claude-pty
    deepseek / ...)             api / ...)                  via ACP)
```

## Major pieces

### The broker — `nexus`

Single Go process. Owns:

- **Chat router.** Routes every chat message between aspects based on `role_hint` (planner-dispatch, worker-execution, operator-drive, casual), `@mentions`, threading.
- **Dashboard.** Operator's web view: chat, observability, knowledge store, tickets, files. Served on the broker's HTTP port.
- **Credential store.** Per-aspect default credentials (Anthropic/OpenAI/etc) overlaid as ProviderEnv at turn-spawn time (internal ticket family NEX-74).
- **Observability hub.** Aggregates turn-level events (provider start/end, tool calls, filter decisions) across aspects, exposes a live stream + persisted jsonl.
- **Embedded Frame.** The Frame (`keel`) runs in-process for proximity to broker state.

### Aspects

An aspect is a personality + a runtime process. Personalities live in `agents/<name>/{SOUL.md, PRIMER.md, NEXUS.md, aspect.json}`. The runtime is one of:

- **Embedded** (the Frame, currently `keel`) — runs inside the broker process via `frame.EmbeddedFrame`.
- **Out-of-process via `agentfunnel`** — separate binary, connects to broker over WS.
- **Out-of-process via `aspect`** — older pre-funnel scaffold; still used for some aspects.

### The funnel — `nexus/frame/funnel`

The deliberation loop. Wraps bridle's per-turn library with:

- **FIFO inbox** with idempotency (NEX-96): broker delivers at-least-once; the funnel deduplicates by persisted seen-msg-id set.
- **Session resolver**: Global / Thread / Stateless modes determine how the underlying provider session is reused.
- **Output filter** (cheap-judge): per-aspect post-hoc gate on whether a turn's natural reply gets posted to chat. Hard rules + optional model-judge.
- **Return handler**: how turn output reaches chat (post via send_chat, suppress, fold to scratch, etc).
- **Observability hook**: emits turn lifecycle frames to the broker's observability hub.

### Bridle — separate repo

[`github.com/CarriedWorldUniverse/bridle`](https://github.com/CarriedWorldUniverse/bridle). One stable provider interface, N implementations. Drives a single deliberation turn:

- `claudecode` — subprocess CLI wrapper around `claude -p`
- `claude` — direct Anthropic Messages API
- `ollama` — local model via Ollama
- `claudepty` — PTY-driven `claude` REPL, exposed as bridle.Provider via ACP (see [acp-claude-pty repo](https://github.com/CarriedWorldUniverse/acp-claude-pty))

Bridle is a library imported by funnel. Aspects do not import bridle directly.

### Agora — the operator TUI

[`github.com/CarriedWorldUniverse/agora`](https://github.com/CarriedWorldUniverse/agora). The operator's terminal-resident presence on the bus. Persistent WS connection, real-time chat panel, multi-line input, code-fence buffered streaming render. Built on bridle's claudecode provider; sits on top of the funnel like any other aspect.

### Hands — stateless capability invocation

A "hand" is a one-shot subprocess that inherits its parent aspect's persona + a task-specific specialization. The dispatcher aspect spawns a hand, the hand runs to completion, results return via chat. No peer interaction during the hand's lifetime — it's a pure compute pulse.

See [hand-dispatch v0.1 spec](2026-04-30-hand-dispatch-v0_1.md).

### Interchange — frame-to-frame relay

[`github.com/CarriedWorldUniverse/interchange`](https://github.com/CarriedWorldUniverse/interchange). End-to-end encrypted relay between paired Nexus instances. Topology-opaque (only routes ciphertext between mailbox pairs), operator-approval-gated for pair establishment, evicts envelopes after a retention window.

### Casket — channel identity

Cross-language libraries for AEAD encryption + Ed25519 channel identity. Three implementations:

- [`casket-ts`](https://github.com/CarriedWorldUniverse/casket-ts) — Node.js + Cloudflare Workers
- [`casket-go`](https://github.com/CarriedWorldUniverse/casket-go) — Go port
- [`casket-dotnet`](https://github.com/CarriedWorldUniverse/casket-dotnet) — .NET

Wire-compatible across all three. Used by interchange for the relay's E2E layer.

## Conventions

### Always work via PR

Every change goes through a feature branch + PR + reviewed merge. Main is branch-protected on every public repo. See [git workflow policy](policies/git-workflow.md).

### Work-routing

Each chat message carries a `role_hint` (planner-dispatch / worker-execution / operator-drive / casual) signaling where it sits in the lane structure. Aspects classify their own outgoing messages. See [work-routing policy](policies/work-routing.md).

### Code standards

11 disciplines from errors-as-data through closed enums and config-vs-secrets split. See [code standards policy](policies/code-standards.md).

## Where to look next

- The [aspect-funnel architecture spec](2026-05-02-aspect-funnel-architecture.md) is the longest-form internal architecture doc.
- [Provider adapter spec](2026-04-24-provider-adapter-spec.md) covers bridle's interface contract.
- [Storage abstraction spec](2026-05-05-storage-abstraction-spec.md) describes how nexus persists state.
- [Hand dispatch v0.1](2026-04-30-hand-dispatch-v0_1.md) covers the workers-as-subprocesses model.
