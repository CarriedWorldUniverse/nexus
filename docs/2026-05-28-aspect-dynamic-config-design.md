# Aspect dynamic provider+model configuration (brainstorm draft)

**Status:** Brainstorm v0.4, 2026-05-28. Filed with [NEX-332](https://carriedworlduniverse.atlassian.net/browse/NEX-332). Iterating in chat between operator and shadow. v0.4 adds the verified factual content of the keyfile (signed-payload contents + canonical-vs-in-the-wild schema gap) so the schema-doc work has source-of-truth to build from.

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

## Resolved questions

### Session continuity on provider-type swap — RESOLVED

**Bridle owns the canonical jsonl across providers.** Provider-internal session IDs (claude-code's `--resume`, etc.) are implementation details we don't trust as authoritative. Bridle's session log + rewrite/compaction layer is the source of truth.

Implication: swapping providers mid-conversation is safe by construction — bridle re-lowers its own session into whatever format the new provider expects on the next turn. No translation step needed; no thread-pinning required.

Claude-code's own jsonl rewrite (size-control) keeps the per-provider state manageable but is downstream of bridle's authoritative log.

### Mid-turn refresh semantics — RESOLVED

**Mid-turn changes don't happen.** A `config.refresh` event that arrives during an in-flight turn is buffered; the in-flight turn completes on the old config; the new config is applied at the next bridle turn boundary.

Single semantic, no operator-facing "should we cancel?" prompt. If the operator wants an in-flight turn killed and redispatched on the new config, that's a separate operator action (existing turn-cancel surface), not entangled with refresh.

### Resolver location + cold-start persistence — RESOLVED

**Resolver is funnel-side** (locked in v0). On config change, the new config is **persisted locally** so a cold start with broker unreachable boots on last-known-good config and waits for the broker to come back online before applying any newer state.

Persistence target: **operator says "write to the keyfile"**, but see the design wrinkle below — likely a sidecar `current-config.json` next to the keyfile is cleaner. Decision is one for round 3.

#### Keyfile co-location — RESOLVED (with caveat)

I initially argued for a sidecar because I had the wrong mental model of the keyfile — assumed it was a pure signed identity envelope. Actual structure is (verified by reading `nexus/aspects/keyfile.go` + a live `plumb.keyfile.json`):

**Documented canonical keyfile (`aspects.Keyfile` struct):**

```
{
  "version": 1,                              // schema version (currently 1)
  "format": "nexus-keyfile-v1",              // schema identifier
  "envelope": {                              // plaintext routing — broker-issued
    "nexus_url": "wss://...",                // dial target
    "nexus_id":  "...",                      // mutual-auth check
    "issued_at": "2026-... RFC3339"          // audit
  },
  "encrypted_payload": "<base64 sealed blob>"// X25519 sealed; contents below
}
```

**What's actually inside `encrypted_payload`** (`aspects.Payload` struct — the bit that's signed/encrypted via nacl/box against the server pubkey):

```
{
  "aspect_name":     "plumb",                // identity
  "aspect_privkey":  "<base64 Ed25519 seed>",// session-auth credential
  "keyfile_version": 17,                     // anti-replay; broker tracks current
  "minted_at":       "2026-... RFC3339",     // audit
  "nexus_id":        "..."                   // mutual-auth: must equal envelope.nexus_id
}
```

**Off-spec extra in the wild (the `jira` block):**

```
"jira": {
  "site":        "...",
  "email":       "...",
  "api_token":   "...",                      // ! credential
  "project_key": "NEX"
}
```

This block is NOT in the canonical `aspects.Keyfile` struct. Some other code path (nexus-jira-mcp probably) reads it via a separate JSON parse. It survives because nothing actively scrubs unknown fields on rotation — but that's incidental, not designed.

**Implication:** the codebase has two partial schemas — the canonical struct that only knows about `envelope`/`encrypted_payload`, and the operator/tooling-side reality where additional sections (`jira`) coexist. Adding `config` the same way works in practice but cements the ad-hoc shape. The schema-doc work (below) makes this intentional and bounded.

**Rotation caveat:** keyfile rotation rewrites `envelope` + `encrypted_payload`. Operator-managed sections (`jira`, future `config`) currently survive only if the rotation tool merges rather than replaces — needs verification.

### Keyfile schema prerequisite — NEW

Operator call: rather than letting the keyfile accumulate ad-hoc top-level sections (`jira` today; `config` tomorrow; whatever-next next month), define a **versioned keyfile schema** as a prerequisite to landing `config`.

Scope of the schema work:
- Document every existing top-level key (`version`, `format`, `envelope`, `encrypted_payload`, `jira`) — what it carries, who writes it, who reads it, is it signed
- Define the policy for new sections: who can add, naming convention, signed-or-unsigned default, mandatory-or-optional
- Define the unknown-section policy: preserved-on-rotation but not interpreted, OR rejected at parse
- Define when the `version` / `format` field changes (additions don't; breaking changes do)
- Document rotation behaviour: which sections are preserved across `encrypted_payload`/`envelope` refresh

This belongs as a child story of NEX-332 — landing it first means the `config` addition slots into a defined slot rather than appearing as the next ad-hoc expansion.

Suggested filename: `docs/2026-05-XX-keyfile-schema.md` (separate doc — keyfile schema is cross-cutting infra, not config-system internal).

## Still-open questions

### 4. Wire format for config + refresh events

Two sub-decisions:

**Config shape** — JSON document with fields: `provider`, `model`, `effort?`, `sampling{temperature, top_p, top_k, max_output_tokens, stop_sequences, seed}`, `env{}` (env vars to pass to subprocess providers), `extra_args[]` (CLI args for subprocess providers).

**Refresh event shape** — frame on the existing aspect WS with a new payload type, carrying the full new config (not a diff — avoids reconcile complexity). Aspect compares to its local cache and applies if different.

### 5. Scope of what flips dynamically

- **Definitely dynamic**: provider, model, effort, sampling params, provider-type swap (made safe by the bridle-owns-jsonl resolution above).
- **Probably not dynamic**: aspect identity, keyfile, broker URL, system prompt body (personality.refresh covers that separately).
- **TBD**: tools, MCP config — these are aspect-capability-level, may belong with the system prompt rather than the provider config.

### 6. Dashboard UX

Per-aspect settings panel already exists (NEX-307 line of work). Extend with:
- Provider dropdown (claudecode | claude | openai | bedrock | gemini | ollama | …)
- Model field (validated against provider)
- Effort dropdown (low | medium | high | xhigh | max) — provider-specific availability
- Sampling sub-panel (collapsible advanced section)
- Env/extra-args (advanced, hidden by default)
- "Apply" button → broker stores → emits refresh → aspect picks up

Show currently-applied config vs pending edits so operator can see drift.

### 7. Interaction with NEX-300 configurability arc

NEX-300 shipped per-aspect knobs (judge/summarizer/main-turn AI choice) across Frame + agentfunnel. This epic builds on that:
- Reuse the per-aspect config storage layer (don't invent new storage).
- Generalise the knobs (NEX-300 covered specific axes; this epic makes the surface uniform — any wireable bridle/funnel param can flip).
- Add the push protocol (NEX-300 changes apply on next aspect connect; this epic adds live push).

### 8. Versioning + audit

Every config change should be auditable. Broker stores `(aspect, version, config_json, changed_at, changed_by)`. Refresh event carries the new version number; aspect logs it. Useful for "shadow started giving weird answers after 14:32" debugging.

---

## Phasing sketch (refine post-brainstorm)

Not a plan yet; just an order-of-operations gut check to keep the epic tractable.

0. **Keyfile schema doc + rotation-preserving harden** — prerequisite. Defines the slot `config` lands in and ensures rotation doesn't rewind operator sections. Filed as separate doc/story per "Keyfile schema prerequisite" above.
1. **Funnel-side config cache + ProviderResolver** — pure refactor of the existing single-provider construction path. No new wire protocol. Aspects boot from `aspect.json` as today but route through the resolver. Zero-impact intermediate state.
2. **Bridle openai-as-DeepSeek validation** — confirm `OPENAI_BASE_URL=https://api.deepseek.com/v1` + DeepSeek key actually works end-to-end via the openai provider. This is the immediate-bug-fix slice: dMon agents can switch off the claude-code+shim hack as soon as it lands.
3. **Broker-side config storage + manual edit API** — extend NEX-300 storage to cover the full config shape. REST + dashboard surface for read/write. No push yet; aspects pick up on reconnect.
4. **`config.refresh` push protocol** — broker emits frame on change; aspect handles + applies + writes to local keyfile `config` section. End-to-end live reconfigure.
5. **Dashboard UI completion** — full edit surface with provider/model/effort/sampling controls; "currently applied vs pending" display.

Session continuity policy (the original phase 6) is no longer needed since Q1 resolved in favour of bridle-owns-jsonl — no thread-pinning or translation layer required.

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
1. File the keyfile-schema child story (phase 0 — prerequisite).
2. Lock the answer to Q5 (scope — tools/MCP in or out).
3. Lock the answer to Q4 (wire format details: full-config push vs diff; refresh ACK semantics).
4. Sketch the broker storage schema (extension of NEX-300 storage).
5. Decide whether phases 3-5 stay together as one arc or split further.
