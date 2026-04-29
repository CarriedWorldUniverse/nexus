# Provider Adapter Spec — v0.2

**Date:** 2026-04-24
**Status:** Draft
**Owner:** keel (spec) / forge (per-adapter tuning)
**Companion to:** [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) §2.3

## Changes since v0.1
- Added `Embed(text) -> vector` to the adapter interface (§3) with `ErrUnsupported` fallback for providers without an embeddings endpoint.
- Added `Embeddings` / `EmbeddingModel` / `EmbeddingDim` to `Capabilities` (§10).
- Added `ollama-local` as the initial embeddings adapter (§9.4) wrapping the existing Ollama instance; chat adapters (`claude-api`, `gemini-api`, `openai-api`) declare embeddings support where the upstream provider offers it (OpenAI yes, Anthropic no, Gemini yes).
- Noted that embedding model choice is a one-way door at scale — switching embedding models requires re-embedding the whole corpus (§9.4 notes).

## 1. Scope

The Nexus registration spec defines a provider layer in §2.3 with the interface `invoke / tokenCount / compact`. That sketch is enough for v1 (Claude-only) but doesn't cover what a real multi-provider network needs:

- Tool-call translation between Nexus's tool shape and each provider's function-calling format.
- Streaming and response-shape normalisation.
- Triage — a cheap fast model per provider for low-stakes turns.
- Dispatch-time provider selection — an aspect declares a default, but Hands and individual invocations should be able to override.
- **Embeddings** — fixed-length vector representations of text, used by the knowledge store's retrieval layer (registration spec §5.6). Not every provider offers embeddings; adapters that do declare it via capabilities.

This spec fills those in. It sits beneath the Nexus registration spec: §2.3 names the contract; this document defines it in detail and enumerates the initial adapters.

## 2. Two interfaces

Every adapter sits between two stable contracts.

**Inward (Nexus standard).** The harness talks to the rest of the network in Nexus terms — comms messages, Nexus tool definitions, ticket structure, aspect identity. This is uniform regardless of which model is underneath. The adapter does **not** see comms; the runtime handles comms dispatch and hands the adapter a normalised invocation.

**Outward (provider-native).** The adapter talks to its specific model backend using that backend's SDK, auth, tool format, and streaming protocol.

The harness core is provider-agnostic. It selects an adapter by name (`aspect.json`'s `provider` field, or a dispatch-time override) and calls the contract. Everything above the adapter is Nexus concern; everything below is provider concern.

```
              ┌──────────────────────────────────┐
              │  Nexus runtime (agent executable)│
              │  comms · tickets · session tree  │
              │  SOUL · CLAUDE.md · tool registry│
              └──────────────┬───────────────────┘
                             │  normalised invocation
                             ▼
              ┌──────────────────────────────────┐
              │       provider adapter           │
              │  translate · call SDK · normalise│
              └──────────────┬───────────────────┘
                             │  provider-native
                             ▼
                       ┌──────────┐
                       │  model   │  (Claude / Gemini / GPT / …)
                       └──────────┘
```

## 3. Adapter interface

Extends the minimal §2.3 contract. All methods operate on normalised types defined in §4.

```
type Provider interface {
    Invoke(ctx, InvokeRequest)  InvokeResult
    Stream(ctx, InvokeRequest)  StreamIterator   // optional; fall back to Invoke
    TokenCount(ContextWindow, payload)  int
    Compact(ctx, Context, hint)  Context

    Embed(ctx, EmbedRequest)    EmbedResult       // optional; ErrUnsupported if not supported

    Capabilities()  Capabilities
    Models()        ModelList           // live-fetched when possible
    TriageModel()   string              // cheap/fast model name for triage turns
}
```

An adapter may implement only a subset. A pure-embeddings adapter (`ollama-local`) returns `ErrUnsupported` from `Invoke`/`Stream`; a chat-only provider returns `ErrUnsupported` from `Embed`. The runtime checks `Capabilities()` before dispatch.

Capabilities declare what the adapter supports (streaming, tool use, vision, 1M context variants, in-session model switch). The runtime reads these at registration time to gate features.

### 3.1 InvokeRequest (normalised input)

```
InvokeRequest {
    Context         []Entry          // tree-structured session entries, replayed along the active branch (spec §2.6)
    Prompt          string           // the new user/invoker turn
    SystemPrompt    string           // SOUL + CLAUDE.md + aspect-specific directives, already composed
    Tools           []ToolDefinition // Nexus tool shape (§4.1)
    Model           string           // provider-scoped model id; may be overridden per-invocation
    ThinkingLevel   string           // off/minimal/low/medium/high/xhigh — adapter ignores if unsupported
    Timeout         duration
    MaxTokens       int              // optional cap
}
```

### 3.2 InvokeResult (normalised output)

```
InvokeResult {
    Output          string
    ToolCalls       []ToolCall        // normalised (§4.2)
    StopReason      StopReason        // end_turn | tool_use | max_tokens | timeout | error
    Cost            CostRecord        // tokens in, tokens out, $ (or null if provider doesn't report)
    Tokens          TokenCounts       // for compaction triggers
    UpdatedContext  []Entry           // entries to append to session tree (the new assistant turn at minimum)
    ProviderRaw     any               // opaque — for audit logs and debugging, not for Nexus logic
}
```

### 3.3 EmbedRequest / EmbedResult

```
EmbedRequest {
    Text   string    // single text to embed
    Model  string    // provider-scoped embedding model id (e.g. "nomic-embed-text", "text-embedding-3-small")
}

EmbedResult {
    Vector []float32 // length == Capabilities().EmbeddingDim for the selected model
    Model  string    // echoed back so callers can verify the right model produced the vector
    Dim    int
}
```

Batch embedding (multiple texts in one call) is deferred to v0.3 — initial callers embed one entry at a time, which is what the knowledge-store write path needs. A `BatchEmbed` method gets added when we see enough throughput to justify it.

**Caller responsibility:** the runtime must track which model produced which vector (see registration spec §5.6). Mixing vectors from different embedding models in the same similarity search is silently wrong — the vectors live in different meaning-spaces and cosine distance between them is noise.

### 3.4 Streaming

`Stream` is optional. When present, it returns an iterator yielding deltas (partial content, tool-call fragments, thinking tokens). The runtime composes streaming into an `InvokeResult` equivalent for callers that don't care about streaming (Hands, one-shot comms responses); streaming callers (future dashboard live-view) consume the iterator directly.

Adapters without streaming support return `ErrUnsupported` and the runtime falls back to `Invoke`.

## 4. Normalised types

### 4.1 Tool definition

A Nexus tool is declared once and rendered per provider by the adapter.

```
ToolDefinition {
    Name         string
    Description  string
    InputSchema  JSONSchema          // draft-2020-12
    OutputHint   string              // optional — what the caller expects back
    Readonly     bool                // hint for sandbox/approval layers (future — spec §7)
}
```

The adapter translates each provider's function-calling dialect:

- **Anthropic:** `{ name, description, input_schema }` — near-identical, minimal translation.
- **Google Gemini:** `{ name, description, parameters }` — nested under `function_declarations`, subtle JSON-schema-dialect differences (no `$defs`, some types named differently).
- **OpenAI:** `{ name, description, parameters }` wrapped in `{ type: "function", function: {...} }`.

JSON-schema dialect normalisation is the adapter's responsibility. The Nexus tool schema is the canonical shape; adapters down-convert.

### 4.2 Tool call

When the model requests a tool call, the adapter normalises to:

```
ToolCall {
    ID        string              // provider-assigned or adapter-generated
    Name      string
    Arguments map[string]any      // parsed JSON, not a raw string
}
```

The runtime dispatches the tool call, captures the result, and on the next invocation passes back a `ToolResult` entry in the context tree. The adapter then renders the result in the provider-native shape (Anthropic: `tool_result` content block; Gemini: `function_response` part; OpenAI: `tool` message role).

### 4.3 Context entry

Entries match the tree-structured JSONL schema from registration spec §2.6 (`turn.user`, `turn.assistant`, `turn.tool_result`, `system.prompt`, `compaction`, `branch_summary`). The adapter reads the replayed active branch and composes the provider-native conversation history.

## 5. Triage

Each adapter declares a triage model — a cheaper, faster variant of the primary model used for low-stakes turns (tool-choice selection, classification, intent-detection) where the main model is overkill.

| Provider | Primary (example) | Triage |
|---|---|---|
| claude-api | claude-opus-4-7 | claude-haiku-4-5 |
| gemini-api | gemini-3.1-pro | gemini-3.1-flash |
| openai-api | gpt-5 | gpt-5-mini |

Triage is adjacent to the primary invocation path, not a separate adapter — the runtime asks `provider.TriageModel()` and issues an `InvokeRequest` with that model name. Cost accounting attributes triage calls to the same aspect/Hand budget as primary calls, with a `triage: true` tag.

Triage is **not** a separate provider choice for aspects. An aspect running on Claude uses Claude triage; switching provider switches both primary and triage in lockstep.

## 6. Provider selection

Three levels of specificity, in order of precedence:

1. **Dispatch-time override** — a comms message invoking a Hand can include `provider: "gemini-api"` (and optional `model: "gemini-3.1-pro"`). The runtime routes to that adapter for this invocation only. Useful for multimodal Hands, A/B comparisons, provider outage fallback.
2. **Hand default** — a Hand declaration in `aspect.json` can pin a provider: `{ name: "translate-image", provider: "gemini-api", model: "gemini-3.1-pro" }`. If not specified, inherits the aspect default.
3. **Aspect default** — `aspect.json.provider` (+ `provider_config`) — the baseline for global/thread sessions and any Hand that doesn't override.

The runtime logs which level supplied the provider on every invocation, so debugging "why did this go to Gemini?" is a log query, not a code trace.

**Constraints on override:**

- Dispatch-time override on a **global or thread** aspect is rejected — the session's context is entangled with the provider's tokeniser and tool-format. Switching mid-session would corrupt history. Override only applies to stateless invocations (Hands).
- Providers must declare compatible capabilities for an override to succeed (you can't invoke a vision Hand against a text-only provider). The runtime checks `Capabilities()` before dispatch and rejects with a clear error if incompatible.

## 7. Configuration

### 7.1 aspect.json (already defined in registration spec §3)

```json
{
  "name": "maren",
  "context_mode": "thread",
  "provider": "gemini-api",
  "provider_config": {
    "model": "gemini-3.1-pro",
    "credentials_path": ".credentials/gemini-api.json",
    "thinking_level": "medium"
  },
  "hands": [
    {
      "name": "translate-image",
      "provider": "gemini-api",
      "model": "gemini-3.1-pro",
      "system_prompt": "...",
      "tools": []
    },
    {
      "name": "verify-text",
      "system_prompt": "..."
    }
  ]
}
```

The aspect defaults to Gemini; `translate-image` explicitly pins Gemini (for clarity); `verify-text` inherits the aspect default.

### 7.2 Credentials

Each adapter expects a credentials file at `<home>/.credentials/<provider>.json`:

- `claude-api.json` — `{ api_key: "sk-ant-..." }`
- `gemini-api.json` — `{ api_key: "..." }` or `{ vertex: { project, location, service_account_path } }`
- `openai-api.json` — `{ api_key: "sk-..." }`

Credentials never appear in aspect.json or logs. The path is relative to the aspect home.

## 8. What stays above the adapter

These are Nexus concerns; adapters never see them:

- SOUL.md and CLAUDE.md content (composed into `SystemPrompt` by the runtime).
- Comms protocol (the runtime dispatches messages to tool calls or session turns; the adapter sees only normalised invocations).
- Ticket and knowledge structures.
- Aspect identity, network topology, roster.
- Session tree persistence (runtime writes JSONL; adapter is handed already-replayed entries).
- Compaction policy (runtime decides when; adapter executes summarisation when asked).

## 9. Initial adapters

### 9.1 claude-api (v1 — live)

- **SDK:** official Anthropic Go SDK.
- **Auth:** API key in `.credentials/claude-api.json`.
- **Tool format:** Anthropic `tools` array with `input_schema` — closest to Nexus shape.
- **Streaming:** SSE, well-supported.
- **Triage:** Haiku 4.5.
- **Embeddings:** **not supported** — Anthropic has no embeddings endpoint. `Embed()` returns `ErrUnsupported`; `Capabilities().Embeddings = false`.
- **Notes:** current production target. `[1m]` context variants supported via model suffix; runtime reads the effective context window from `Capabilities()`.

### 9.2 gemini-api (v2 — next)

- **SDK:** Google AI Go SDK or Vertex AI SDK.
- **Auth:** either Google AI Studio API key or Vertex service account.
- **Tool format:** `function_declarations`; JSON-schema dialect slightly narrower than draft-2020-12.
- **Streaming:** supported, different chunk shape from SSE.
- **Triage:** Gemini 3.1 Flash.
- **Embeddings:** supported via `text-embedding-004` (768-dim) or `gemini-embedding-001` — implement when the chat adapter lands; until then, ollama-local carries embedding traffic.
- **Notes:** multimodal dispatch is the motivating use case — image understanding Hands route here. The lack of a CLI-equivalent to `claude -p` is fine under the new runtime because we're API-only anyway (no PTY in the v0.2+ architecture).

### 9.3 openai-api (stub — v3+)

- **SDK:** official OpenAI Go SDK.
- **Auth:** API key.
- **Tool format:** `tools` with `type: "function"` wrapper.
- **Streaming:** SSE.
- **Triage:** GPT-5 Mini or equivalent.
- **Embeddings:** supported via `text-embedding-3-small` (1536-dim) / `text-embedding-3-large` (3072-dim). Stub parity until the chat path is implemented.
- **Notes:** stub in v1 to validate the adapter interface isn't Claude-shaped by accident. Real implementation deferred.

### 9.4 ollama-local (v1 — embeddings only)

Pure-embeddings adapter wrapping a locally-hosted Ollama instance. This is the embeddings path for v1: the `knowledge` store's retrieval layer (registration spec §5.6) calls `provider("ollama-local").Embed(text)` and stores the resulting vector alongside the entry.

- **Transport:** HTTP POST to `<OLLAMA_URL>/api/embeddings` with `{ model, prompt }`; response `{ embedding: [...] }`.
- **Endpoint configuration:** `OLLAMA_URL` env var, default `http://host.docker.internal:11434` (the standard Docker-to-host address used by operator's existing container; `http://localhost:11434` if Nexus runs on the same host without Docker networking in the way). Container being stopped is tolerated — the adapter health-checks on first embed call and surfaces `ErrProvider` with a clear "Ollama unreachable at <url>" message rather than panicking.
- **Locked embedding model:** `nomic-embed-text` (768-dim, ~150MB). Tuned for technical text — operational notes, architecture decisions, incident postmortems. KB scope is technical; narrative canon is out of scope. Swapping the model later requires re-embedding the whole corpus (see one-way-door note below).
- **Capabilities:** `Chat: false`, `ToolUse: false`, `Streaming: false`, `Embeddings: true`, `EmbeddingModel: "nomic-embed-text"`, `EmbeddingDim: 768`. Selecting this adapter for a chat invocation fails at dispatch time.
- **Auth:** none (local endpoint). If Ollama ever runs remote with auth enabled, `OLLAMA_AUTH_TOKEN` env var.
- **Swappable upstream:** the adapter abstracts Ollama's HTTP shape, not the model. Moving to OpenAI embeddings later means registering `openai-api` with embeddings enabled and changing the `knowledge.embedding_provider` config (registration spec §5.6) — no caller changes. But see the one-way-door note below.

**One-way door on embedding-model change.** Vectors from different embedding models are not comparable. If a Nexus has been running with `nomic-embed-text` (768-dim) for six months and accumulated 10K knowledge entries, switching to `text-embedding-3-small` (1536-dim) means:

1. Every existing vector becomes noise — searches mixing old and new vectors return garbage.
2. The whole corpus must be re-embedded through the new model before the switch is live.
3. Schema migration: the `embedding` column's dimension changes, requiring a rebuild of the vector index.

This is tractable at low corpus size and painful at scale. Implication: pick the initial embedding model deliberately, document the switching procedure, and expose corpus-re-embed as an explicit operator command rather than a silent config change.

**Docker orchestration.** Ollama runs outside the Nexus process. Nexus startup does not start Ollama (out of scope); the adapter assumes the container is managed separately. If the container is down at adapter-init time, the Nexus still starts — embedding calls fail until Ollama is brought up, at which point they start succeeding again without a Nexus restart.

## 10. Capability declaration

Every adapter returns a `Capabilities` struct on demand:

```
Capabilities {
    Streaming           bool
    ToolUse             bool
    Vision              bool
    LongContext         bool             // 1M+ context window available
    InSessionModelSwap  bool             // can change model mid-session
    ThinkingLevels      []string         // supported values or nil
    MaxContextTokens    int              // for current default model
    SupportsTriage      bool             // TriageModel() returns non-empty

    Embeddings          bool             // Embed() is implemented
    EmbeddingModel      string           // default embedding model id
    EmbeddingDim        int              // vector length produced by EmbeddingModel
    Chat                bool             // Invoke() is implemented (false for pure-embedding adapters)
}
```

The runtime uses these to:
- Gate dispatch-time overrides (§6) — reject if target adapter lacks a required capability.
- Populate `/aspects` enrichment (spec §2.5) — dashboard shows per-aspect available models and features.
- Shape the system prompt — if `ThinkingLevels` is empty for this provider, strip thinking-level directives.

## 11. Error handling and retries

Adapters surface a normalised error taxonomy:

- `ErrAuth` — credentials missing/invalid.
- `ErrRateLimit` — with `retry_after` seconds.
- `ErrContextWindow` — request exceeded model's limit even after packing; runtime should compact and retry.
- `ErrUnsupported` — feature not available on this provider/model (streaming, tool-use, etc.).
- `ErrProvider` — anything else from the provider.
- `ErrTimeout` — adapter-side or provider-reported timeout.

Runtime decides retry policy. Default: one retry on `ErrRateLimit` with backoff, one retry on `ErrContextWindow` after a forced compaction, no retry on the rest.

## 12. Open questions

- **Cost attribution when dispatch-time override is used.** If aspect A invokes aspect B's Hand with a provider override, does B still pay (per registration spec §4.5.6), or does A pay because they chose the override? Leaning: B still pays — it's their Hand. A is responsible for "should I call this at all" cost-wise; B is responsible for "what does this Hand cost on each provider it runs on."
- **Provider outage failover.** Not v1. But the adapter interface should make it cheap to add: detect `ErrProvider` with a certain pattern, try a declared fallback adapter. Hand declarations could grow a `fallback_provider` field later.
- **Vertex vs Studio for Gemini.** Vertex gives quota/billing consolidation but requires service-account-based auth; Studio is simpler but has tighter rate limits. Pick one as default when the adapter lands.
- **JSON-schema dialect drift.** Anthropic accepts draft-2020-12-ish; Gemini accepts a subset; OpenAI is closer to draft-07. The adapter normalisation layer needs a test suite that defines what breaks where. Forge territory once we get there.
- **Thinking-level portability.** Claude and some newer Gemini models expose reasoning-effort knobs; OpenAI's `reasoning_effort` is different again. Runtime uses the normalised string; adapter maps or ignores. Per-provider mapping table lives in each adapter's appendix once they're implemented.

## 13. Lane ownership

- **keel** — this spec, the adapter interface, the provider-selection precedence, error taxonomy, configuration surface, capability declaration.
- **forge** — per-adapter implementation tuning, JSON-schema translation, streaming shape details, triage-model selection per adapter, retry policy tuning.
- **wren** — canon-compatibility review. Running maren on Gemini changes the voice the model produces; wren checks whether a non-Claude aspect can hold character, and flags adjustments needed to SOUL.md / CLAUDE.md to travel across providers.
- **Research support** — for new provider adapter work: API surface differences, SDK discovery, model-capability mapping.

## 14. Status

v0.2 draft. Inward/outward boundary, interface shape, normalised types, triage, provider-selection precedence, initial adapter targets, and the embeddings path (adapter interface + ollama-local + capability surface) defined. Implementation sequencing tracked in `BUILD.md`.
