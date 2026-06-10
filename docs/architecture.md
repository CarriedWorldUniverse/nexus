# Architecture

A bird's-eye view of how the pieces fit. The stack is two cooperating halves: an **agent runtime** (the team of AI aspects and the machinery that runs them) and the **CWB platform** (the identity and authority plane they stand on). Each section links into the detailed spec where one exists.

## The shape

```
                    ┌─────────────────────────────────────────┐
                    │            nexus (broker)               │
                    │  chat router · routing rules · dashboard │
                    │  observability hub · dispatch fabric    │
                    └──────────────┬──────────────────────────┘
                                   │  WS
        ┌──────────────────────────┼──────────────────────────┐
        ▼                          ▼                          ▼
   ┌─────────┐               ┌──────────┐              ┌──────────┐
   │ aspect  │               │  agora   │              │ aspect   │
   │ (pod)   │               │(operator │              │ (pod)    │
   │ funnel  │               │  TUI)    │              │ funnel   │
   │ bridle  │               │ bridle   │              │ bridle   │
   └────┬────┘               └────┬─────┘              └────┬─────┘
        ▼                         ▼                         ▼
    model API                model API                  model API
  (claude-code /          (claudecode)              (claude / api /
   api / codex /                                     gemini / ollama /
   gemini / ...)                                     antigravity / ...)

                    CWB platform (identity + authority)
        ┌──────────────────── interchange ───────────────────┐
        │            public boundary gateway (mTLS)           │
        ├──────────┬───────────┬────────────┬─────────────────┤
        ▼          ▼           ▼            ▼
     herald     ledger    commonplace     cairn      (+ custodian)
   (identity)  (work/    (knowledge)     (git)      (secret broker)
               audit)
```

## The agent runtime

### The broker — `nexus`

A single Go process. Owns:

- **Chat router.** Routes every chat message between aspects based on routing rules, `@mentions`, and threading. The operator is one peer on the bus, not above it.
- **Dashboard.** Operator's web view: chat, observability, knowledge, work, files. Served on the broker's HTTP port.
- **Observability hub.** Aggregates turn-level events (provider start/end, tool calls, filter decisions) across aspects; exposes a live stream + persisted JSONL.
- **Dispatch fabric.** Dispatches work to aspects running as on-demand cloud pods on a single-node k3s cluster (see [Dispatch](#dispatch-aspects-as-cloud-pods)).

### Aspects

An aspect is a named personality plus a runtime. Personalities live in `agents/<name>/{SOUL.md, PRIMER.md, NEXUS.md, aspect.json}`. The runtime wraps `bridle` (one deliberation turn against a provider) inside a `funnel` (which owns the inbox, compaction, output filter, and observability) and connects to the broker over WebSocket. Aspects run as **dispatch pods**, not host processes.

### The funnel

The deliberation loop. Wraps bridle's per-turn library with:

- **FIFO inbox** with idempotency: the broker delivers at-least-once; the funnel deduplicates by a persisted seen-msg-id set.
- **Session resolver** — Global / Thread / Stateless modes determine how the underlying provider session is reused.
- **Output filter** (cheap-judge) — a per-aspect post-hoc gate on whether a turn's natural reply gets posted to chat.
- **Return handler** — how turn output reaches chat (post, suppress, fold to scratch).
- **Observability hook** — emits turn-lifecycle frames to the broker's observability hub.

### Bridle — separate repo

[`bridle`](repos/bridle.md) is one stable provider interface with N implementations, driving a single deliberation turn. It spans **direct model APIs** (Claude/OpenAI/Bedrock/Gemini) and **headless CLI streams** (Claude Code, Codex, Gemini, Antigravity, local Ollama), with per-turn timing instrumentation. The funnel imports bridle; aspects do not import it directly.

### Agora — the operator TUI

[`agora`](repos/agora.md) is the operator's terminal-resident seat at the same table — a persistent-WS chat panel built on bridle's claudecode engine. It sits on the bus like any other aspect.

### Dispatch — aspects as cloud pods

Aspects run as on-demand pods on a single-node k3s cluster rather than host processes. Dispatched work runs as a **named agent in its own pod** and reports back into an audited chat thread. In flight:

- **Addressable-but-napping presence** — a mention wakes a sleeping aspect's pod in seconds; it naps again when quiet.
- **Aspect-owned worker "hands"** — fresh-context workers that carry the parent's persona under a derived identity, fully audit-threaded, so an aspect can fan work out without blocking the conversation.

See [dispatch-native platform architecture](2026-06-08-dispatch-native-platform-architecture.md), [dispatch pod + home model](2026-06-08-dispatch-pod-and-home-model.md), and [named-agent dispatch model](2026-06-08-named-agent-dispatch-model.md).

### Roundtable — multi-agent deliberation

Convening several named aspects into a thread to reach consensus, with the operator mediated (digests and batched decision-points) rather than firehosed. See [roundtable design](2026-06-11-roundtable-design.md).

## The CWB platform

The identity and authority plane. The pillars run as **standalone gRPC services over mTLS**, fronted by [`interchange`](repos/interchange.md) as the **public boundary gateway**. Interchange runs herald verification at the edge and injects the verified caller as `cwb-*` gRPC metadata that the pillars trust over the mTLS hop; any HTTP/JSON view is synthesized at the gateway. (Interchange also still carries the original topology-opaque E2E relay for Frame-to-Frame communication.)

### herald — identity

[`herald`](repos/herald.md) attests *who you are* (humans **and** agents) and proclaims *what authority you hold*. Agents are first-class identities — own keypair, own audit trail, own scopes — linked to a responsible human but never equal to them; accountability and capability are orthogonal. Built on `zitadel/oidc`, crypto-rooted via casket. Admin authority is identity-derived (a JWT carrying `herald:platform-admin` / `herald:org-admin` scopes), not a static token.

### ledger — work + audit

[`ledger`](repos/ledger.md) is an aspect-first issue tracker: markdown-canonical storage, append-only event timeline, immutable comments, per-issue-type workflow validation with a required Definition-of-Done. Designed for aspects to consume and emit natively; built to replace Jira as the canonical tracker for the stack.

### commonplace — knowledge

[`commonplace`](repos/commonplace.md) is the knowledge pillar — store and **semantically search** knowledge (query by concept, get similar-in-meaning entries back). Embeddings (local ollama / `nomic-embed-text` by default) fused with FTS5 keyword search. The first deliberate layer of a learning-memory substrate for AI.

### cairn — git

[`cairn`](repos/cairn.md) is an agent-native git platform with a **native go-git core**. The Forgejo lineage is preserved on archived branches as historical context; the live platform is our own.

### custodian — credential broker

[`custodian`](repos/custodian.md) is an external-credential vault: herald-keyed, per-org crypto isolation, brokering the *use* of secrets to verified identities without placing raw credentials in model context. Reference design today; lives as a seam in the broker (`nexus/broker/custodian.go`) until it graduates to its own service.

### casket — crypto root

Cross-language libraries for AEAD encryption + Ed25519 channel identity, wire-compatible across [`casket-go`](repos/casket-go.md), [`casket-ts`](repos/casket-ts.md), and [`casket-dotnet`](repos/casket-dotnet.md). The crypto root for herald's agent keys and interchange's E2E relay.

### Clients + protocol

[`cw`](repos/cw.md) is the platform CLI for humans and agents (anchored on the interchange edge, auth via herald); [`cwb-client`](repos/cwb-client.md) is the reusable Go client extracted from it; [`cwb-proto`](repos/cwb-proto.md) holds the shared protobuf/gRPC wire contracts; [`cwb-conformance`](repos/cwb-conformance.md) is the external end-to-end suite that exercises the pillars through their real public boundary.

## Conventions

- **Always work via PR.** Every change goes through a feature branch + reviewed merge; main is branch-protected on every public repo. See [git workflow policy](policies/git-workflow.md).
- **Work-routing.** Chat messages carry routing signals for where they sit in the lane structure; aspects classify their own outgoing messages. See [work-routing policy](policies/work-routing.md).
- **Code standards.** Disciplines from errors-as-data through closed enums and config-vs-secrets split. See [code standards policy](policies/code-standards.md).

## Where to look next

- [Dispatch-native platform architecture](2026-06-08-dispatch-native-platform-architecture.md) — the current runtime shape.
- [Herald-rooted agent bootstrap](2026-06-03-herald-rooted-agent-bootstrap-design.md) — how an agent boots by name from a herald-rooted identity.
- [Nexus↔CWB gateway](archive/2026-06-03-nexus-cwb-gateway-design.md) — the broker's path onto the CWB pillars.
- [Aspect-funnel architecture spec](archive/2026-05-02-aspect-funnel-architecture.md) — the longest-form runtime architecture doc (foundational).
