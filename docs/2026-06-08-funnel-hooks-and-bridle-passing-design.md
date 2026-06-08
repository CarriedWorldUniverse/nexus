# Funnel hooks + bridle event passing — design spec

**Date:** 2026-06-08
**Status:** design spec (approved via operator dialogue, 2026-06-08)
**Relates:** NEX-500 (funnel hook system), NEX-501 (hooks survey — `docs/2026-06-08-hooks-survey-cc-gemini.md`), NEX-499 (per-agent home), NEX-502 (platform architecture). bridle = the Frame/provider seam (own repo, NEX-188).

## Goal

A provider-agnostic **hook system in the agentfunnel**, on the Claude-Code/Codex `hooks.json` schema (the de-facto standard — see the survey). The funnel holds the canonical hook engine; bridle is the *conduit* that carries tool-level events up (and, later, decisions down). **Phase 1 priority: memory hooks that read and write Commonplace** — fetch relevant memory into the turn, capture decisions/learnings back. Tool-use approvals (gating) and the bridle decision-passing come later.

## Why memory-first is also the simplest first slice

Memory read/write happen at **turn boundaries** — pre-turn fetch, post-turn capture — which are already **clean funnel-side seams** (`Deliberate`'s phases). They need **no bridle change**: the funnel doesn't have to intercept anything mid-turn inside bridle/the CLI. The bridle event-passing (events up + decisions down) is only required for *tool-level* gating, which is deferred. So Phase 1 is funnel-only and low-risk.

## Architecture

**`HookEngine`** (new `nexus/frame/funnel/hooks` package): a registry of `event → matcher groups → handlers`, loaded from `hooks.json` / inline config. One method:

```
Dispatch(ctx, event, payload) → Decision
```

It runs the handlers matching `event` (+ matcher), merges their results (**deny wins**; `additionalContext` concatenates), and returns a `Decision`. Handlers are typed:

- **`mcp_tool`** — invoke a connected MCP tool (this is how memory hooks reach Commonplace: `search_knowledge` / `store_knowledge` on the nexus-comms MCP). **In Phase 1.**
- **`http`** — POST the payload to a URL (broker/custodian callbacks). **Phase 1.**
- **`command`** — run a script (stdin-JSON in, JSON-out + exit `0`/`2`). **Phase 1.**
- **`prompt`** — a fast-model micro-decision (this is what the judge becomes). Phase 2.

**Decision model** (CC/codex schema): exit `0` (parse JSON) / `2` (block, stderr = reason); JSON fields `decision`/`permissionDecision`, `additionalContext`, `continue`, `systemMessage`. Multiple handlers → deny wins; contexts concatenate.

**Event taxonomy** mapped to existing `funnel.Deliberate` call sites (from the code map):

| Event | Funnel seam (file) | Phase |
|---|---|---|
| `SessionStart` | `Deliberate` entry / `popHeadForTurn` (funnel.go:934/1176) | 1 |
| `UserPromptSubmit` / pre-turn | `buildTurnRequest` (funnel.go:1332) — where auto-recall already injects | 1 |
| `Stop` / post-turn | `judgeTurn` / `commitTurnState` / `dispatchReturn` (funnel.go:1565–1740) | 1 |
| `PreCompact` | `maybeCompact` (funnel.go:1255) | 2 |
| `PostToolUse` (observe/audit) | bridle `ToolCallResult` events via the existing `EventSink` | 2 |
| `PreToolUse` (gate) | in-process via bridle `HookSink`; CLI-native via the bridge | 3 |

## Phase 1 — engine + memory hooks (Commonplace) — PRIORITY

**Build:** the `HookEngine` + config loader (`hooks.json` + inline, layered: network / aspect / per-run, loaded at `SessionStart`); the `mcp_tool`, `http`, `command` handler types; and the two memory hooks.

**`read-memory` hook** (`SessionStart` + pre-turn): query Commonplace for memory relevant to the trigger/task and inject it as `additionalContext` at the top of the turn. This **formalizes the existing inline auto-recall** (`RenderRecalledKnowledge` / `CommonplaceGuard` in `comms.go`, today hard-wired in `buildTurnRequest`) into a configurable hook backed by `search_knowledge` (the nexus-comms `Knowledge` interface). It may also read the agent's per-agent home (NEX-499) for private memory. Recalled content stays *reference data, not instructions* (preserve the existing `CommonplaceGuard`).

**`write-memory` hook** (`Stop` / post-turn): capture decisions / learnings / handoffs to Commonplace via `store_knowledge` (keyed by aspect id + topic, replacing same-topic entries — the existing semantics). What to capture is hook-configured (e.g. a `prompt`-derived distillation in P2; a simpler rule or explicit-marker capture in P1). Optionally also commits to the per-agent home.

**No bridle change in Phase 1.** Both hooks fire at funnel turn-boundaries through the existing `Knowledge` seam.

**Output:** an agent that, on every turn, has relevant Commonplace memory pulled in automatically (formalized + configurable) and writes worth-keeping memory back automatically — the read/write-memory loop the operator asked for.

## Phase 2 — consolidate existing hooks

Re-express the **judge** (`OutputFilter.Judge`, a `Stop` decision), the **rewriter** (`PostTurnHook.AfterTurn`), and **observability** (the `ObservabilityHook` event consumer + Lock-5 `Events`) as engine-registered handlers, so there is **one** dispatch mechanism rather than three bespoke `Config` fields running in parallel. Add the `prompt` handler (the judge is its first user) and the `PreCompact` + `PostToolUse`(observe/audit) events. Parity-tested against current behaviour; the `Config` fields keep working, now routed through the engine.

## Phase 3 — tool-use gating + bridle passing (later)

This is where **"passing hooks from bridle back up"** fully lands.
- **bridle `HookSink`** — upgrade the `EventSink` the funnel passes into `RunTurn` so that for **in-process** tool calls (the bridle `ToolRunner`) bridle asks the sink and honours a returned `Decision` (deny → tool returns an error to the model; `updatedInput` rewrites args). Events-up + decision-down. (bridle change, own repo.)
- **CLI-native bridge** — bridle configures the underlying CLI's native hooks (claude-code / codex `hooks.json`) to POST to a funnel hook endpoint (`http`), so CLI-*internal* `Bash`/`Edit` `PreToolUse` reaches the same engine for a decision. This closes the gap the survey flagged (codex's own interception is incomplete) and gives full cross-provider tool gating.
- **Trust model** — borrow Codex's SHA-trust + managed-hooks model for the privileged / agentharness lane (cross-ref NEX-502/504): gated, audited, admin-pinned hooks.

## Cross-cutting

- **Config & layering:** `hooks.json` + inline, merged across network → aspect → per-run; carried in aspect config / `mcp_profile`; loaded at `SessionStart`.
- **Error handling:** panic-safe per handler (matches the existing hook wrapping in funnel.go), per-handler timeout, **fail-open for observe/memory hooks** (a Commonplace miss must never break the turn), **fail-closed for security gates** (Phase 3) — configurable per hook.
- **Provider-agnostic:** the engine is funnel-level, so it works identically for claude-code / codex / gemma backends. The CLI-native bridge (P3) is an *optional enhancement* per provider, never a dependency.

## Components / files

- **New:** `nexus/frame/funnel/hooks/` — `engine.go` (registry + `Dispatch`), `config.go` (`hooks.json`/inline loader), `handlers.go` (`mcp_tool`/`http`/`command`/`prompt`), `decision.go` (merge + schema).
- **Modified:** `funnel.go` — fire `HookEngine.Dispatch` at the mapped call sites; `buildTurnRequest` read-memory hook supersedes the inline auto-recall; `commitTurnState`/`dispatchReturn` write-memory hook. `comms.go` `Knowledge` seam reused by the memory handlers.
- **Later (P3):** bridle `HookSink` (bridle repo) + the CLI-native-hook bridge.

## Testing

- Engine units: matcher resolution, decision-merge (deny-wins, context-concat), exit-code/JSON parsing, per-handler timeout/panic isolation.
- Memory-hook integration: `SessionStart` read-memory injects Commonplace hits as `additionalContext`; `Stop` write-memory calls `store_knowledge`; fail-open on Commonplace error (turn still completes).
- Parity (P2): judge / rewriter / observability behave identically routed through the engine.
- Gating (P3): a `deny` from the bridle `HookSink` blocks an in-process tool; the CLI-native bridge denies a `Bash` call end-to-end.

## Ticketing

- **NEX-500** — the funnel hook engine + the Phase 1 **memory hooks (Commonplace read/write)**, and Phase 2 consolidation.
- A **bridle-change story** (Phase 3): the `HookSink` (events up + decision down) + the CLI-native-hook bridge for tool gating — filed against bridle, related to NEX-500 and the agentharness gating (NEX-504).
