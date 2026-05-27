# Aspect dynamic provider+model configuration (brainstorm draft)

**Status:** Brainstorm v0, 2026-05-28. Filed with [NEX-332](https://carriedworlduniverse.atlassian.net/browse/NEX-332). Iterating in chat between operator and shadow. Captures decisions taken so far + the open questions still on the table; will graduate to a spec once those resolve.

---

## Problem

Aspect provider/model/effort/sampling are set once at process start from `aspect.json` + env vars, then frozen for the lifetime of the aspect. Changing any of it requires editing the start script + restarting the aspect process.

This bit us on 2026-05-28: dMon agents (anvil, forge, harrow, keel, maren, verity, wren) all run claude-code subprocess with `ANTHROPIC_BASE_URL` hijacked to point at DeepSeek's anthropic-compat shim. Extended thinking is enabled in claude-code by default for the model in use, but DeepSeek's shim doesn't round-trip thinking blocks correctly → 400 on every multi-turn conversation.

The bridle-side patch for the cross-turn thinking-block path ([PR #40](https://github.com/CarriedWorldUniverse/bridle/pull/40)) addresses the direct-Anthropic path only. The structural fix is to:

1. Drop the claude-code+shim hack and talk to DeepSeek directly via bridle's `openai` provider (DeepSeek's `/v1` is OpenAI-compatible).
2. Make that switch — and any future provider/model/effort change — operator-controlled without aspect restarts.

This generalises beyond the immediate bug: any per-aspect cost optimisation, model A/B, or backend swap (DeepSeek ↔ Anthropic ↔ local Ollama) should be a dashboard click, not a config-file-and-restart.

---

## Decisions taken in chat

These are settled enough to write up; flag in PRs if any need revisiting.

### Funnel-side `ProviderResolver` + local config cache

Funnel holds the canonical aspect config in-process (mirrored to a local `current-config.json` on disk so a crash-restart picks up the last-applied state). Each turn calls a `ProviderResolver` function — `resolver(currentConfig) → Provider` — and wraps the returned provider in a fresh Harness for that turn's `RunTurn`.

Per-turn provider construction is cheap for every bridle provider type, including `claudecode`: `claude -p` already spawns fresh per turn, so there's no persistent subprocess to tear down. The "subprocess swap cost" concern raised early in the chat was overblown.

Location: funnel-side, not broker-side. Reasons:
- Aspect autonomy — survives broker disconnect; the cached config is the authoritative local view.
- Matches the deferred `personality.refresh` push protocol (flagged in agentfunnel/main.go header as "Part 7"). Both should land the same pattern at the same time.
- Centralising in the broker would couple every turn to a broker round-trip; not worth the latency hit.

### Broker pushes `config.refresh` events

Broker stores per-aspect provider/model/effort/sampling/env config (likely an extension of the existing configurability arc storage from NEX-300). On any change, broker emits a `config.refresh` frame to the affected aspect. Aspect updates its local cache, applies before the next turn.

No per-turn round-trip; only on-change traffic. Mid-conversation reconfigure is hot.

### Cheap design wins

- Provider construction is per-turn; no shared mutable state on the provider object across turns.
- Refresh events are fire-and-forget from the broker's perspective; aspect ACKs (or doesn't); next config push supersedes.
- Initial provider load: aspect.json on first boot, then broker push at validate-time can override. Broker is authoritative if both are present.

---

## Open questions

### 1. Session continuity on provider-type swap mid-conversation

Claude-code carries session state via `--resume <session-id>` against a local jsonl file. Bridle direct-API providers carry state via `SessionTail` in-memory + funnel's persistence layer. If an aspect is mid-thread on `claudecode` and you swap to `openai`, the thread's history-shape changes hands.

Options:
- **Translation step**: convert the claude-code jsonl into bridle SessionEvents (and vice versa) when the provider type changes. Lossy in general (thinking blocks, tool internals, etc.).
- **Context reset on type swap**: accept that swapping provider type loses thread context. New thread starts fresh. Operator gets a warning in the dashboard before applying.
- **Provider-type-pinned threads**: thread metadata records the provider type at creation; refresh only applies to NEW threads created after the change. Existing threads finish on the old provider.

Recommend option 3 by default (pinned threads), with option 2 as an explicit "reset and apply" affordance.

### 2. Mid-turn refresh semantics

If `config.refresh` arrives while a turn is in flight:
- **Finish current turn with old config, apply on next dispatch.** Simpler; preserves the in-flight semantic. Slight delay before change is visible (the duration of one turn). Likely the right default.
- **Cancel + restart with new config.** Faster to apply but throws away token spend on the cancelled turn + can lose partial outputs.

Recommend "finish current, apply next." Operator can manually cancel + redispatch if they want the change applied to the in-flight turn.

### 3. Wire format for config + refresh events

Two sub-decisions:

**Config shape** — JSON document with fields: `provider`, `model`, `effort?`, `sampling{temperature, top_p, top_k, max_output_tokens, stop_sequences, seed}`, `env{}` (env vars to pass to subprocess providers), `extra_args[]` (CLI args for subprocess providers).

**Refresh event shape** — frame on the existing aspect WS with a new payload type, carrying the full new config (not a diff — avoids reconcile complexity). Aspect compares to its local cache and applies if different.

### 4. Scope of what flips dynamically

- **Definitely dynamic**: provider, model, effort, sampling params.
- **Dynamic but with caveats** (per Q1): provider-type swap.
- **Probably not dynamic**: aspect identity, keyfile, broker URL, system prompt body (personality.refresh covers that separately).
- **TBD**: tools, MCP config — these are aspect-capability-level, may belong with the system prompt rather than the provider config.

### 5. Dashboard UX

Per-aspect settings panel already exists (NEX-307 line of work). Extend with:
- Provider dropdown (claudecode | claude | openai | bedrock | gemini | ollama | …)
- Model field (validated against provider)
- Effort dropdown (low | medium | high | xhigh | max) — provider-specific availability
- Sampling sub-panel (collapsible advanced section)
- Env/extra-args (advanced, hidden by default)
- "Apply" button → broker stores → emits refresh → aspect picks up

Show currently-applied config vs pending edits so operator can see drift.

### 6. Interaction with NEX-300 configurability arc

NEX-300 shipped per-aspect knobs (judge/summarizer/main-turn AI choice) across Frame + agentfunnel. This epic builds on that:
- Reuse the per-aspect config storage layer (don't invent new storage).
- Generalise the knobs (NEX-300 covered specific axes; this epic makes the surface uniform — any wireable bridle/funnel param can flip).
- Add the push protocol (NEX-300 changes apply on next aspect connect; this epic adds live push).

### 7. Versioning + audit

Every config change should be auditable. Broker stores `(aspect, version, config_json, changed_at, changed_by)`. Refresh event carries the new version number; aspect logs it. Useful for "shadow started giving weird answers after 14:32" debugging.

---

## Phasing sketch (refine post-brainstorm)

Not a plan yet; just an order-of-operations gut check to keep the epic tractable.

1. **Funnel-side config cache + ProviderResolver** — pure refactor of the existing single-provider construction path. No new wire protocol. Aspects boot from `aspect.json` as today but route through the resolver. Zero-impact intermediate state.
2. **Bridle openai-as-DeepSeek validation** — confirm `OPENAI_BASE_URL=https://api.deepseek.com/v1` + DeepSeek key actually works end-to-end via the openai provider. This is the immediate-bug-fix slice: dMon agents can switch off the claude-code+shim hack as soon as it lands.
3. **Broker-side config storage + manual edit API** — extend NEX-300 storage to cover the full config shape. REST + dashboard surface for read/write. No push yet; aspects pick up on reconnect.
4. **`config.refresh` push protocol** — broker emits frame on change; aspect handles + applies. End-to-end live reconfigure.
5. **Dashboard UI completion** — full edit surface with provider/model/effort/sampling controls; "currently applied vs pending" display.
6. **Session continuity policy** — implement provider-type-pinned threads (Q1 default) + the explicit "reset and apply" affordance.

Phase 1+2 are independently useful (un-jam dMon) and don't need the full architecture. Phases 3-6 are the operator-experience win.

---

## Out of scope (for this epic)

- Personality.refresh push protocol — separate work, but should share the wire-protocol shape with `config.refresh`. Coordinate naming.
- Multi-provider parallelism (running one turn on two providers and comparing). Future epic if needed.
- Cost-based auto-routing (rules like "use cheap model when prompt < 1k tokens"). Future epic.
- Per-thread config (vs per-aspect). Possible later; per-aspect is plenty for v1.

---

## Open for the next round of brainstorm

When we pick this back up, the prioritised list is:
1. Lock the answer to Q1 (session continuity on provider-type swap) — drives whether we need a translation layer or not.
2. Lock the answer to Q4 (scope — tools/MCP in or out).
3. Sketch the broker storage schema (extension of NEX-300 storage).
4. Decide whether phases 3-6 stay together as one arc or split further.
