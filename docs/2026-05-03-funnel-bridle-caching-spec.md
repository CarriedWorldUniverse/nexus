# Funnel ↔ Bridle Prompt Caching Spec — v0.1

**Date:** 2026-05-03
**Status:** Draft
**Owner:** keel (spec) / forge (per-adapter mapping)
**Companion to:**
- [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) §2.6 (session tree)
- [`2026-04-24-provider-adapter-spec.md`](2026-04-24-provider-adapter-spec.md) §3 (adapter interface)
- [`2026-05-01-funnel-compaction-design.md`](2026-05-01-funnel-compaction-design.md) (compaction lifecycle)

## 1. Scope

Prompt caching is a per-provider mechanic that, used correctly, drops marginal token cost on stable prefixes by ~10× and meaningfully reduces latency. Used incorrectly — breakpoints on volatile content, missed minimum sizes, repeated cache writes — it silently wastes money.

This spec divides the responsibility:

- **Funnel** owns the *context* — what's in it, what order, which parts are stable, and where the cache breakpoints should fall. This is the only layer with the information needed to make those calls (compaction state, persistent vs volatile entries, turn boundaries).
- **Bridle** (provider adapter) owns the *mechanic* — translating funnel's portable cache hints into the specific provider's caching surface. Each provider's caching model is different; the adapter is the only place where that asymmetry is allowed to exist.

The funnel's cache-hint vocabulary is portable across providers. An adapter that targets a provider with no caching support simply drops the hints; correctness is preserved, the optimisation is lost.

## 2. Trust and threat model

Caching does not weaken the trust boundaries already declared in the registration spec:

- **Cached content stays inside the provider's account scope.** Anthropic prompt caching is per-API-key; Gemini context caches are per-project; OpenAI implicit caching is per-organisation. Keys/credentials are stored per-adapter (provider-adapter spec §7.2). A cached prefix never leaks across credentials.
- **Cache reads are not observable to other tenants.** This is a provider guarantee we rely on; if it ever weakens, the spec is wrong. Document the assumption rather than re-prove it.
- **Cached content is the same content sent in a non-cached request.** Caching does not change what's sent — it changes whether the provider rebuilds attention state for the prefix or replays it. No new exfiltration surface is introduced by enabling caching.
- **Cache identifiers (Gemini context-cache resource IDs) are credentials-shaped.** Treat them like API keys: never log them at info level, never serialise into session JSONL, never include in dashboard payloads. If exposed, they grant read-access to whatever was cached, scoped to the credential that wrote them.

What this spec is **not** doing: it is not designing cross-tenant cache sharing, cross-credential cache reuse, or any mechanism that crosses an aspect's provider-credential boundary. Co-location of identical prefixes across aspects on the *same* credential is allowed (§9); anything beyond that is out of scope.

## 3. Funnel-side: cache hints in the context tree

Each entry the funnel hands the bridle (provider-adapter spec §3.1, `InvokeRequest.Context`) may carry a `cache_hint` annotation:

```
CacheHint {
    Stability   StabilityClass    // see below
    Boundary    bool              // mark this entry as a cache breakpoint
    TTL         "5m" | "1h"       // requested TTL for the breakpoint
    Tag         string            // optional; for observability/debug only
}

StabilityClass = persistent | post-compaction | turn-stable | volatile
```

The funnel sets the hint when composing context from the session tree:

| StabilityClass | Meaning | Typical TTL |
|---|---|---|
| `persistent` | SOUL.md, CLAUDE.md, PRIMER.md, system frame, tool defs — stable for the lifetime of the aspect | `1h` |
| `post-compaction` | Compaction summary + everything before the latest compaction boundary | `1h` |
| `turn-stable` | Closed turns in the current session — already-emitted assistant messages, completed tool results | `5m` |
| `volatile` | The new prompt, in-flight tool calls, anything still being assembled | (no caching) |

**Boundary placement.** A breakpoint is placed on the *last* entry of each stability class that is bounded above the next class's first entry. Concretely, the funnel marks `Boundary: true` on:

1. The last `persistent` entry (after tools + SOUL + CLAUDE.md + PRIMER.md).
2. The latest `compaction` entry (frame from `compaction.complete`, see compaction-design §3.4).
3. The last `turn-stable` entry (last closed turn before the new prompt).

Three breakpoints by default; a fourth slot is reserved for adapter-internal use (§4.2). This stays inside Anthropic's 4-breakpoint budget while leaving providers with weaker breakpoint models room to make their own choices.

**The funnel does not set the TTL based on guesswork.** TTL is a property of stability class, not heuristic. `persistent` and `post-compaction` always request `1h`; `turn-stable` always requests `5m`. The adapter is free to demote (§4.3) but never to upgrade.

## 4. Bridle-side: adapter mapping

### 4.1 Capability declaration

Extend `Capabilities` (provider-adapter spec §10):

```
Capabilities {
    ...existing...

    Caching          CachingModel
    MinCacheTokens   int          // smallest prefix the provider will actually cache
    MaxBreakpoints   int          // max simultaneous breakpoints; 0 if not breakpoint-based
    SupportsTTL      []string     // supported TTL values, e.g. ["5m", "1h"] or nil
}

CachingModel = none | implicit-prefix | explicit-breakpoints | resource-handle
```

| Provider | CachingModel | MinCacheTokens | MaxBreakpoints | SupportsTTL | Cached read price |
|---|---|---|---|---|---|
| claude-api | `explicit-breakpoints` | 4096 (Opus 4.7, Sonnet 4.6, Haiku 4.5) | 4 | `["5m", "1h"]` | 0.1× base (write 1.25× / 2.0×) |
| openai-api | `implicit-prefix` | 1024 (then +128 increments) | 0 | nil (provider-managed) | ~0.5× base on GPT-4o, steeper on newer models, no write fee |
| gemini-api (implicit) | `implicit-prefix` | 1024 (Flash) / 2048 (Pro) | 0 | nil | 0.1× base on Gemini 2.5+ (90% discount), no write fee |
| gemini-api (explicit) | `resource-handle` | **32,768** | n/a | configurable, default 60m | 0.1× base + per-hour storage billing per cached token |
| ollama-local | `none` | 0 | 0 | nil | n/a |
| local subprocess models | `none` | 0 | 0 | nil | n/a |

Notes:

- **OpenAI** does not charge a cache-write premium — first call pays full rate, subsequent calls get the discount. There is no TTL knob; the provider evicts on its own schedule (typically minutes of inactivity, longer during low-load periods).
- **OpenAI prefix-routing constraint.** The provider routes requests to a cache-bearing machine based on a hash of the **first ~256 tokens** of the prompt. If those tokens vary (e.g. timestamp, randomised greeting, per-call user ID injected at the top), every request lands on a different machine and the rest of the prefix never hits cache regardless of length. The funnel's `persistent` ordering — tools first, then SOUL, then static system frame — naturally satisfies this; the rule is "do not put anything volatile in the first 256 tokens."
- **Gemini** offers two caching surfaces. Implicit is automatic, free to opt into, and matches OpenAI's mental model. Explicit (resource-handle) requires a 32,768-token minimum prefix and bills for storage duration regardless of hit rate — only worth it for very large persistent prefixes (long skill libraries, KB summaries, full SOUL+CLAUDE.md+PRIMER.md+tool-set composites that exceed 32k). Below that threshold, prefer implicit.
- **Gemini storage billing.** Explicit cache resources cost per-million-tokens-stored per-hour for the duration of their TTL, irrespective of hits. A 50,000-token prefix cached for 1h that's never read still costs the storage line. The adapter must factor this into the decision to create a resource.

The runtime reads these once at registration and uses them to gate behaviour (e.g. dashboard "caching: explicit / 1h supported" badge per aspect).

### 4.2 Mapping rules

**`explicit-breakpoints` (Anthropic).** For each entry the funnel marks `Boundary: true`:

- Render the entry as the provider-native content block.
- Attach `cache_control: {"type": "ephemeral"}` if `TTL == "5m"`, or `{"type": "ephemeral", "ttl": "1h"}` if `TTL == "1h"`.
- If the prefix up to and including this entry is below `MinCacheTokens`, suppress the breakpoint silently (no error). Caching has no effect below the threshold; we don't burn a breakpoint slot on it.
- If more breakpoints are requested than the budget allows (4 for Anthropic), keep them in this order of priority: persistent > post-compaction > turn-stable. Drop the lowest-priority one. (Adapter's optional 4th slot is for adapter-internal use — currently reserved, not used.)

**`implicit-prefix` (OpenAI).** Drop all explicit breakpoint annotations; OpenAI's caching is automatic on identical prefixes ≥ 1024 tokens, growing in 128-token increments. The adapter must:

- **Keep the first 256 tokens stable.** OpenAI hashes a prefix of (typically) the first 256 tokens to route the request to a cache-bearing machine; volatile content in that range routes every request to a different machine and defeats caching entirely regardless of total prefix size. The funnel's `persistent`-first ordering (tools → SOUL → CLAUDE.md → PRIMER.md → static system frame) gives this for free, but the adapter must not insert any provider-side preamble (system-injected timestamps, request IDs, etc.) in front of it.
- Ensure deterministic message ordering and content (no incidentally-non-deterministic JSON key ordering in tool definitions — sort keys before serialising; arrays preserved as semantic).
- Surface `usage.prompt_tokens_details.cached_tokens` in `cache_stats` (§5).
- Otherwise leave the request unchanged. There is no write premium and no TTL — first call pays full rate, subsequent calls get the discount, eviction is provider-managed.

**`implicit-prefix` (Gemini, default mode).** Gemini 2.5+ models support implicit caching with the same shape as OpenAI: automatic, no opt-in, no write premium, 90% discount on cached tokens. Minimum prefix is **1024 tokens (Flash) / 2048 tokens (Pro)**. The adapter behaviour is identical to OpenAI's — drop hints, ensure deterministic ordering, surface cached-token count in `cache_stats`. This is the default mode for `gemini-api`.

**`resource-handle` (Gemini, explicit mode — opt-in).** Used only when the prefix exceeds the explicit caching minimum of **32,768 tokens** AND a `persistent` or `post-compaction` boundary is marked AND the aspect's expected re-read rate justifies the storage billing. Decision rule (adapter-side):

```
use_explicit_cache = (
    prefix_tokens >= 32768 AND
    boundary.Stability in (persistent, post-compaction) AND
    aspect.expected_turns_per_hour >= 6   // amortise storage over reads
)
```

When triggered, the adapter:

- Maintains a per-aspect cache-resource registry keyed on hash(prefix-content).
- On first call: creates a context cache via the Gemini API with TTL = funnel's requested TTL (60m for `1h`, 5m floor not supported by Gemini explicit — demote to implicit instead), stores the resource ID alongside the aspect's runtime state, sends the request referencing the cached prefix.
- On subsequent calls: looks up the resource, references it by ID. If TTL has expired, falls back to implicit caching for that call and recreates the resource.
- **Storage cost accounting.** Per-hour storage billing per million stored tokens applies for the resource's TTL whether or not it gets read. Adapter records this as a separate line in `cache_stats` (`StorageTokenHours`).
- Resource IDs live in the adapter's runtime state, not in session JSONL or dashboard logs (§2 — credentials-shaped).
- On aspect shutdown / model change / SOUL.md change: explicitly delete the resource via the Gemini API (do not rely on TTL expiry — you're paying for storage until then). Discard the registry entry.

For aspects where the persistent prefix is below 32k, the adapter never goes explicit — implicit caching covers it and avoids storage billing.

**`none`.** Drop hints. No caching surface. Adapter passes the rendered context straight through.

### 4.3 Demotion rules

The adapter may demote a hint when:

- TTL is requested but unsupported → use the longest supported TTL (or default if none).
- Prefix is below `MinCacheTokens` → silently drop the breakpoint.
- Breakpoint budget exceeded → drop in priority order (above).

The adapter must **not** upgrade a hint (e.g. apply `1h` when funnel asked for `5m`). Funnel's TTL is the cap.

### 4.4 Tool-definition stability

Tool definitions are emitted before the system prompt and are part of the `persistent` prefix. The adapter MUST serialise tools with stable JSON key ordering — non-deterministic key order is a silent cache-buster on every provider. Sort keys lexicographically; preserve array order (which is semantic).

## 5. Cache stats — observability

`InvokeResult` (provider-adapter spec §3.2) gains:

```
InvokeResult {
    ...existing...

    CacheStats CacheStats
}

CacheStats {
    Model              CachingModel    // which model the adapter ran
    WrittenTokens      int             // tokens billed at write rate (Anthropic only — 1.25× / 2.0× base)
    ReadTokens         int             // tokens served from cache (Anthropic ~0.1×; OpenAI ~0.5×; Gemini 2.5+ ~0.1×)
    UncachedTokens     int             // tokens billed at full input rate
    BreakpointsUsed    int             // for explicit-breakpoints providers; 0 otherwise
    TTLApplied         string          // "5m" | "1h" | "" | "auto" (implicit) | "resource"
    StorageTokenHours  float64         // Gemini explicit only — tokens × hours stored, for billing reconciliation
}
```

The funnel emits a `cache.stats` frame on the dashboard wire alongside each completed turn (transport spec §5 — new frame type to be defined when the dashboard schema next moves). Operators see per-aspect cache hit ratio, write/read token volume, and an estimated $-saved-vs-uncached.

**Why this matters operationally.** A cache miss rate that quietly climbs from 5% to 50% (because a timestamp drifted into the persistent prefix, or tool key ordering went non-deterministic in a refactor) burns money and won't show up in functional tests. The dashboard surface is the canary.

**Default alert thresholds (recommended, not enforced):**
- Persistent-prefix cache hit ratio < 70% over a 24h window → warn (probable invalidator drifted into persistent context).
- Cache-write token volume on `1h` breakpoints > 5× cache-read volume → warn (1h breakpoint being repeatedly written but not read — likely placement bug or aspect restart loop).

## 6. Compaction interaction

Compaction (per `2026-05-01-funnel-compaction-design.md`) rewrites history:

1. Funnel emits `compaction.begin` (visibility — agent isn't hung, see compaction design §3.3.1).
2. Adapter runs `Compact()` to produce summary entries.
3. Funnel emits `compaction.complete` and replaces pre-compaction entries with the summary in the session tree.

Caching effect:
- The previous post-compaction prefix is invalidated (content before it has been replaced by a new summary).
- The new summary becomes the next call's `post-compaction` boundary.
- The first call after `compaction.complete` will pay the cache-write cost for the new summarised prefix at `1h` TTL. Subsequent calls within the hour read from cache.

The funnel does not need to do anything special — the new boundary annotation flows from the stability classification, which is recomputed each turn. The adapter, on `resource-handle` providers, must invalidate any prior cache resource referencing the old prefix (§4.2).

## 7. Mid-turn injection interaction

Sync `send_comms` (registration spec §2.2; provider-adapter spec §3) appends a `user` message mid-turn and re-invokes. The injected message is the new tail; everything before it is the same context the model just paused on, and remains cacheable. The funnel marks the original prompt + tool calls + paused state as `turn-stable` (the turn isn't *closed*, but it *is stable* — nothing before the injection point will change). The `turn-stable` breakpoint sits just before the injected user message.

Cache outcome: the resumed call reads almost the entire prefix from cache and pays full price only on the injected message + the model's response. This makes sync `send_comms` cheap to use, which is the right incentive.

## 8. Persistent vs ephemeral aspect prefixes

A small operational detail with material cost impact:

- **Persistent aspects** (global, thread) are long-running. The `persistent` prefix (SOUL + CLAUDE.md + tools) is read across many turns; the 1h-TTL write amortises across hundreds of turns. Cache wins large.
- **Hand dispatches** are stateless one-shot invocations. The Hand's system prompt + tools + invocation arguments are sent once and the invocation ends. There's nothing to amortise — caching the prefix costs more than it saves.

The funnel SHOULD NOT emit `persistent`-class breakpoints for Hand invocations. The adapter SHOULD treat a Hand invocation as a single-shot request with no cache annotations. (Anthropic implicit prompt caching may still help at the API edge across Hands that share an identical prefix; that's the provider's free win, not something we're shaping.)

This is a funnel behaviour, not an adapter behaviour: the funnel knows it's dispatching a Hand vs servicing a persistent aspect, and elides hints accordingly.

## 9. Cross-aspect prefix sharing

Two aspects on the same Frame, both running on `claude-api`, both built from the same skill library, will have substantially overlapping `persistent` prefixes (tools + skill content + shared CLAUDE.md). On Anthropic, identical prefix bytes + same API key → cache hit, regardless of which aspect issued the call.

This is **deployment guidance, not protocol**:

- Co-locating aspects with shared skills onto a single Frame (single API key) makes the second aspect's first call a cache read, not a write.
- Splitting aspects with shared prefixes across separate Frames (separate API keys, e.g. work vs home Frame) doubles the cache writes — accept the cost or factor the shared library into a Frame-wide common prefix.

The funnel does not need to know about this. The adapter does not need to know about this. It's a property of how Frames and credentials are structured, surfaced here so operators planning Frame topology have the information.

## 10. Open questions

- **Gemini implicit-vs-explicit threshold tuning.** The 32k-prefix + ≥6 turns/hour rule in §4.2 is a first-pass heuristic. Real number depends on the Gemini explicit storage rate vs the per-token discount differential. Telemetry from the first month of multi-Gemini-aspect deployment should inform the actual cutover point. Until then, default to implicit and only enable explicit when an operator opts in per-aspect.
- **Cache stats granularity for `resource-handle` providers.** Gemini's context-cache API reports usage at the resource level, not per-call. The adapter can derive per-call read tokens but write tokens get attributed only to the call that created the resource. Acceptable distortion; document on the dashboard surface.
- **OpenAI prefix-routing fragility.** A 256-token-prefix routing hash is sensitive to subtle changes (e.g. an SDK upgrade reordering message-content array shape). We need a CI test that fingerprints the rendered first 256 tokens of a known aspect and fails on drift. Track this once the adapter has a real test rig.
- **Breakpoint demotion telemetry.** When the adapter drops a breakpoint due to budget overflow or below-minimum prefix, the funnel should know — both for debugging "why did this prefix not cache" and for evolving the placement strategy. Add a `demoted_breakpoints` field to `CacheStats` once we hit a real case where it matters.
- **1h-TTL pricing realism.** 2× write for 12× longer life is only worth it if the prefix is genuinely re-read more than 6× within the hour. For low-traffic aspects that's not guaranteed. Open: should `persistent` default to `5m` for aspects below a turn-rate threshold? Defer until we have telemetry.
- **Cache invalidation on tool-definition change.** Today every `aspect.json` reload that changes tools invalidates the persistent cache. We currently reload aspect config on every aspect bring-up — restarts produce write-storms. Consider a `tools.hash` check at bring-up: if unchanged from last run, the persistent cache may still be live.
- **Cross-provider failover and cache state.** When provider-outage failover lands (provider-adapter spec §12), the failover provider has no cache state. Acceptable — failover is a degraded mode by definition. Note for the failover spec when it lands.

## 11. Lane ownership

- **keel** — this spec, the cache-hint vocabulary, the funnel-side stability-class rules, the `CacheStats` shape, the credential-scope assumptions in §2.
- **forge** — per-adapter mapping (Anthropic `cache_control` rendering, Gemini context-cache resource lifecycle, OpenAI key-ordering hardening), demotion rule tuning, MinCacheTokens / MaxBreakpoints declarations.
- **scribe** — dashboard surface for `cache.stats`, alert threshold defaults, operator-facing documentation.
- **wren** — n/a (caching does not affect aspect voice/canon).

## 12. Status

v0.1 draft. Hint vocabulary, stability classes, breakpoint placement defaults, adapter capability surface, cache_stats shape, compaction/mid-turn-injection interactions, and Hand/cross-aspect deployment guidance defined. Implementation sequencing follows once the funnel-bridle split is in code.
