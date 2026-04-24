# Nexus Registration Architecture — Spec v0.5

**Date:** 2026-04-22 (v0.1) → 2026-04-24 (v0.2 → v0.3 → v0.4 → v0.5)
**Status:** Draft — scaffold complete at v0.3; v0.5 adds knowledge storage + retrieval.
**Owner:** keel
**Repo:** `C:\src\nexus` (fresh git repo, separate from `C:\src\agent-network`)

**v0.5 changes (2026-04-24, from #7636 → #7676 discussion with operator and harrow):**
- **New §2.8 — Knowledge storage & retrieval.** SQLite + FTS5 primary; `sqlite-vec` extension loaded-but-dormant from day one with an `embedding BLOB` column reserved; hybrid (keyword + vector) retrieval behind a single `SearchKnowledge` interface so callers don't change when the vector layer is turned on.
- **Embedding provider is Ollama via the provider-adapter spec's new `Embed()` method.** Locked model: **`nomic-embed-text`** (768-dim) — technical-tuned, matches this KB's actual content (operational notes, architecture decisions, incident postmortems). Ollama runs as a separate Docker container at the standard `http://host.docker.internal:11434` endpoint, currently stopped — adapter handles unreachable-upstream gracefully.
- **Scope: technical knowledge only.** Narrative canon (verity's domain) is explicitly out of scope for this KB. If canon ever becomes vector-searchable, it's a separate system, separate build (per operator #7676).
- **Active retrieval (RAG-pattern) formalised** — on thread start, runtime runs `SearchKnowledge(topic)` and injects hits as a `system.prompt` entry, subject to relevance threshold and corpus scoping. Removes the "remember to search" burden that today's numbers (107 entries, ~1.5:1 write:read) show aspects are failing at.
- **Storage bootstrap pattern** — schema as committed source (`nexus/storage/schema.sql` + `//go:embed`), database as runtime-created under `NEXUS_DATA_DIR` (default `./data/`, gitignored). `Bootstrap(db)` runs idempotent DDL every startup. Per operator #7682/#7685: commits carry source only, runtime creates its own DB. Details in §10.
- **Companion:** provider-adapter spec v0.2 adds the `Embed` contract and the `ollama-local` adapter. See [`2026-04-24-provider-adapter-spec.md`](2026-04-24-provider-adapter-spec.md).

**v0.4 changes (2026-04-24, informed by harrow's Pi research in #7601):**
- **Session JSONL upgraded to tree-structured** — every entry carries `id` and `parentId`. Branching, fork, clone, and rewind are tree operations on a single file rather than destructive truncation or multi-file management. New §2.6 defines the entry schema. Applies to `global` and `thread` modes; `stateless` unchanged.
- **Compaction formalised — proactive, threshold-based.** `shouldCompact(tokens, window, reserve) := tokens > window - reserve`. Fires in the runtime, not the provider. `CompactionEntry` carries `firstKeptEntryId` so the pre-compact history is preserved in-tree with a pointer, not lost. New §2.7.
- **Rewind / fork / branch-summary as first-class operations.** Rewind is an active-branch cursor move; fork creates a new branch at a historical entry; branch-switch generates a summary via LLM to carry departing-branch context into the resumed path. Details in §4.4 and admin endpoints in §4.5. Resolves spec §7's rewind and proactive-compaction open items.
- **Thinking level** logged as future work (§7) — not v1. Orthogonal to everything we're building; drops in as an aspect/turn property later.

**v0.3 changes (2026-04-24, informed by harrow's t3code research in #7595):**
- **JSONL-owns-state retained** — evaluated t3code's opaque-resume-cursor pattern; we keep the Nexus-owns-state model for provider-neutrality, auditability, and federation readiness. Tradeoff noted.
- **Enrichment fiber pattern adopted** — registration is fast + static; capability/health enrichment streams in async. Folded into §2.5 + §4.
- **Thread-session TTL resolved** — 30 min idle reap, 5 min sweep, active-turn skip. Session records reaped, thread records persist. Was an open question in §7.
- **Tool-authority runtime modes** logged as future work (§7) — RuntimeMode / SandboxMode / ApprovalPolicy per t3code. Security-relevant for aspects and Hands; not v1 but tracked.
- **InteractionMode (plan/execute)** noted as future Hand parameter (§7) — orthogonal to context modes, per-turn style toggle.

**v0.2 key changes from v0.1 (locked 2026-04-24):**
- PTY/proxy runtime is **removed**. All aspects are comms-first, headless/API. No terminal attach, no user-to-agent bypass.
- Single runtime; **three context modes** — `global`, `thread`, `stateless`.
- Provider layer is explicit. Runtime is AI-agnostic; each provider module (Claude API, OpenAI API, etc.) implements the same interface.
- keel folds into the Nexus process as a **global-context harness**, no PTY.
- Session state becomes the runtime's responsibility (Nexus owns conversation persistence, not Claude Code).
- Scaffold plan: Option A — full directory scaffold first, then working code. Operator chose A over the thin-vertical-slice option.
- Naming placeholder: using `global` / `thread` / `stateless` for context modes.
- **Cross-platform requirement (added 2026-04-24, #7602):** runtime and Nexus process must run on Windows and Linux at minimum; Mac support preferred. Rules out Windows-native-only tech. Node and Go both qualify; pure-Rust also qualifies. Any provider-specific CLI dependencies must themselves be cross-platform.

## 1. Motivation

Today's deployment model treats aspects as configured children of the launcher:

- `launch.js` reads `team.json`, knows every aspect up front, spawns proxy/harness children, wires ports.
- Aspect state is split across `C:\src\agent-network\agents\<name>\` (config) and `C:\Users\jacin\.claude\projects\C--...\` (runtime).
- Adding, removing, or restarting an aspect means touching a central manifest and bouncing the launcher.
- "Proxy" mode exposes a PTY — aspects can be interacted with directly, bypassing comms. Operator wants to move beyond this; comms-first is the invariant.

The proposed model inverts this:

- **Nexus** is a single central process (broker + orchestrator + frame-agent) that knows nothing about specific aspects at startup.
- **Aspects** are independent processes launched as `{runtime-executable} {home-folder}`. Each aspect is a self-contained folder. Single runtime, comms-first, no PTY.
- Aspects register with the Nexus on boot. The Nexus roster is a live runtime construct, not a config artifact.

This matches the canon (Nexus is a gateway, aspects arrive) and is the honest prototype of the distributed-Nexus endgame — federation is the same registration protocol over the wire instead of localhost.

## 2. Architecture

### 2.1 Nexus process

Single Node process combining:
- Broker (MCP server + REST API, port 7888)
- Orchestrator (polls aspects, runs watches, fires alarms)
- Frame-agent (keel — embedded as a global-context harness aspect within the Nexus process)

Rationale for folding keel into the Nexus process: keel IS the frame. Running it as a sibling-aspect-that-happens-to-be-privileged is accurate-but-awkward. Folding it in makes the trust model explicit.

**keel post-migration:** runs as a global-context harness inside the Nexus process. No PTY. All interaction via comms (or Nexus admin endpoints). Behaviorally identical to the current keel for the human observer — same chat presence, same role — but no terminal to attach to, no user-to-agent bypass. keel's compaction and rewind become first-class gestures (see §7) because keel's context health is the Nexus's context health.

### 2.2 Aspect runtime — one shape, three context modes

One runtime executable. All aspects are comms-first, headless/API. No PTY. Direct user-to-agent interaction is deprecated; all flow goes through comms, making every exchange auditable, broker-logged, and visible to the dashboard.

The runtime is AI-agnostic. Provider modules (§2.3) plug in to actually talk to a given AI backend (Claude API, OpenAI API, Anthropic API, local model, future-whatever). v1 ships Claude only; the interface is drawn so additional providers are module-drops rather than runtime rewrites.

**Context mode is the aspect's property, not the runtime's.** Declared in aspect.json. Three values:

- **`global`** — aspect maintains a single long-running conversation context across every invocation. Messages append to the same session log. Used for aspects that carry state forward across threads and topics (current keel / forge / wren shape, but without PTY). Context rot is a real risk here; see §7.
- **`thread`** — aspect maintains a separate context per chat thread. First message in a thread starts a fresh session; subsequent messages in the same thread append. Different threads are independent. Good for aspects whose work is bounded per-topic but still benefits from in-thread continuity (harness-with-thread-scope, the current harrow/anvil/maren shape).
- **`stateless`** — no persisted context at all. Each invocation is a fresh API call with only the incoming prompt + declared system prompt + optional structured input. Hands (§4.5) live here.

The context-hygiene call — *"does the parent need the full trajectory, or just the conclusion?"* — is still the right mental model, but it's now a choice of mode per aspect, and Hands offer a per-invocation escape hatch regardless of the hosting aspect's mode.

**Invocation:**
```
agent.exe C:\src\nexus\agents\verity
agent.exe C:\src\nexus\agents\wren
agent.exe C:\src\nexus\agents\harrow
```

The home folder is the aspect. Everything the runtime needs is inside.

### 2.3 Provider layer

The runtime executable is provider-agnostic. A provider module is a plug-in that knows how to call a specific AI backend with credentials and configuration. **Detailed interface, tool translation, triage, dispatch-time provider overrides, and per-provider appendices live in [`2026-04-24-provider-adapter-spec.md`](2026-04-24-provider-adapter-spec.md).** This section is the architectural summary.

```
C:\src\nexus\runtime\providers\
  claude-api\        # v1 — direct Anthropic API (no CLI dependency)
  claude-code\       # optional: wraps claude CLI for environments where that's easier
  openai-api\        # stub for v2
  anthropic-api\     # alternate namespace if we split claude-api into model families
  ...
```

**Provider contract (single interface for all context modes):**
- `invoke({ context, prompt, systemPrompt, tools, timeout }) → { output, cost, tokens, updated_context }`
  - `context` is an opaque serialized conversation state. Empty/null for stateless. For global/thread it's the accumulated history the runtime hands back in next call.
  - `updated_context` is returned by the provider so the runtime can persist it — runtime owns storage, provider owns shape.
- `tokenCount(context) → number` — for context-rot telemetry and proactive-compaction triggers.
- `compact(context, hint) → updated_context` — provider-specific summarization/compaction; runtime decides when to call it (threshold, admin command, aspect self-request).

Context persistence (serializing state between invocations) is the **runtime's** job, provider-agnostic:
- `global` mode: one JSONL file per aspect under `<home>/session/global.jsonl`.
- `thread` mode: one JSONL per thread under `<home>/session/threads/<thread_id>.jsonl`.
- `stateless` mode: no persistence.

This is meaningful new work compared to the current model — today Claude Code manages its own session files. Under the new model, Nexus owns the session state and replays it into provider invocations. Provider-agnostic persistence is what makes the provider layer swappable.

### 2.4 Aspect home folder layout

```
C:\src\nexus\agents\<name>\
  aspect.json          # config (see §3)
  CLAUDE.md            # identity (consider renaming to AGENT.md for provider-neutrality)
  SOUL.md              # voice/values
  PRIMER.md            # cold-start context
  .credentials\        # per-aspect API credentials (Claude API key, OpenAI key, etc.)
  session\
    global.jsonl       # if context_mode=global
    threads\<id>.jsonl # if context_mode=thread
  memory\              # auto-memory (currently scattered in %USERPROFILE%)
  logs\                # runtime logs
```

Copy the folder → you've cloned the aspect. Delete → gone. Move to another machine → works (assuming runtime executable is present).

Note: `.credentials\` replaces the current `.claude\` directory. Provider-neutral naming since this folder may hold multiple providers' credentials over time.

### 2.5 Bootstrap ordering

1. Nexus process starts (Task Scheduler / NSSM / manual).
2. Nexus opens broker port, exposes `/register` endpoint, starts orchestrator loop.
3. Aspects start (in any order, parallel fine). Each reads its `aspect.json`, reads `NEXUS_URL` from env, retries registration until Nexus answers.
4. On successful register, Nexus adds aspect to live roster with static fields (name, port, capabilities, context_mode, provider). Orchestrator begins polling. Dashboard sees it appear immediately.
5. **Enrichment fiber** starts per aspect — background augmentation of the static snapshot with dynamic data (provider auth state, available models, current token usage, session age, resource counters). Enrichment never blocks registration or dispatch; the roster's dynamic fields populate progressively. Re-runs on capability-relevant changes (provider credential rotation, settings edits).

No aspect is ever required to be up for Nexus to be healthy. Nexus can run with zero aspects registered — that's just an empty galaxy.

**Enrichment rationale** (adopted from t3code pattern, see harrow's research #7595): registration should be fast and deterministic — aspect-is-up/aspect-is-down is binary. But useful operational data (auth health, model list under the current subscription, real-time token counts) is expensive to gather and changes independently. Splitting static-register from async-enrich keeps the registration path tight while still surfacing rich state to the dashboard.

### 2.6 Session JSONL format — tree-structured

Applies to `global` and `thread` context modes. `stateless` mode writes no session file.

Every line is a JSON entry with these baseline fields:

```
{
  "id": "<ulid>",              // unique entry id
  "parentId": "<ulid>|null",   // previous entry in this branch; null for root
  "kind": "<entry-kind>",      // see below
  "ts": "2026-04-24T12:34:56Z",
  "payload": { ... }           // kind-specific
}
```

The file is append-only. A node's position in the conversation is defined by walking `parentId` backward; the "active branch" is the chain from a designated head entry back to root. Multiple head entries can coexist — the runtime tracks which is active via a small sidecar file (`<session>.head`) or in-memory state keyed by session id.

Entry kinds (v0.4 initial set):

- `turn.user` — user/invoker message that seeded this turn.
- `turn.assistant` — model response. Payload includes content and any tool calls.
- `turn.tool_result` — tool execution result, attached to its parent `turn.assistant` entry.
- `system.prompt` — system prompt version active for this entry. Written when changed; replayers read the last `system.prompt` entry on the active branch.
- `compaction` — summarization checkpoint, see §2.7.
- `branch_summary` — synthetic summary injected when the active branch changes (§4.4).
- `custom.<name>` — reserved for aspect-specific structured data (not invoked in v1).

**Replay semantics.** When the runtime needs to hand context to a provider, it walks backward from the active head, collecting entries until it hits a `compaction` with `firstKeptEntryId` (in which case the summary replaces everything before `firstKeptEntryId`), then replays forward.

**Why tree, not flat:** rewind and fork become pointer moves, not destructive edits. History is preserved across course corrections — the departing branch's entries remain in the file and can be re-traversed or referenced by summary. This is the only model where "rewind" is non-destructive and fully reconstructable; it's worth the mild extra complexity up front.

### 2.7 Compaction

Compaction is a runtime concern, not a provider concern — the runtime owns when to compact and what to keep; the provider executes the summarization call.

**Trigger:**

```
shouldCompact(tokens, window, reserve) := tokens > (window - reserve)
```

- `tokens` — current active-branch token count (via provider `tokenCount()`).
- `window` — provider-declared context window for the current model (static capability).
- `reserve` — aspect-configured headroom (default: 20% of window, or fixed 10k, whichever is larger).

Runtime polls token count on every turn-end. When `shouldCompact` is true and no turn is in flight, runtime triggers compaction before the next turn.

**Execution:**

1. Runtime determines `firstKeptEntryId` — entries older than this will be summarised out. Heuristic: keep the most recent N turns (configurable, default 6 turns) and the active `system.prompt`.
2. Runtime constructs summarization prompt using entries older than `firstKeptEntryId`.
3. Runtime calls provider `compact(context, hint)` → summary text.
4. Runtime appends a `compaction` entry:
   ```
   {
     "id": "...",
     "parentId": "<previous active head>",
     "kind": "compaction",
     "ts": "...",
     "payload": {
       "firstKeptEntryId": "<ulid>",
       "summary": "<provider-returned summary>",
       "tokensBefore": N,
       "tokensAfter": M,
       "model": "claude-opus-4-7[1m]"
     }
   }
   ```
5. Active head advances to the new `compaction` entry.
6. Subsequent replays see the compaction as the cut point — everything before `firstKeptEntryId` is summarized; everything from `firstKeptEntryId` forward is replayed verbatim.

**Observability:** per-aspect dashboard telemetry surfaces `tokens / window`, last compaction time, compaction count, and a proactive-compact warning when `tokens > window * 0.7`.

**Manual compact:** admin endpoint `POST /nexus/aspects/<name>/compact` triggers the same flow out-of-band.

Matches the feedback pattern in keel memory `feedback_compact_before_drift.md`: proactive beats reactive because reactive happens after model degradation.

### 2.8 Knowledge storage & retrieval

The knowledge base (`knowledge` table in the Nexus SQLite) is the cross-session memory shared across aspects. Measured usage on the current agent-network network (107 entries, ~1.5:1 write:read over 30 days, most aspects never search) shows the failure mode isn't storage — it's retrieval habit. The spec addresses both the storage shape (ready for scale) and the retrieval pattern (surface knowledge without relying on aspect discipline).

**Storage — SQLite, FTS5 today, vectors when needed.**

Single table, single database file. Same `comms.db` that holds chat, tickets, threads, session metadata.

```sql
CREATE TABLE knowledge (
  id           INTEGER PRIMARY KEY,
  from_agent   TEXT NOT NULL,
  topic        TEXT NOT NULL,
  content      TEXT NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
  embedding    BLOB,                    -- reserved day-one; populated when vector retrieval enabled
  embed_model  TEXT,                    -- which model produced the embedding (see provider-adapter spec §3.3)
  embed_dim    INTEGER,                 -- vector length; non-NULL only when embedding is present
  UNIQUE(from_agent, topic)
);

CREATE VIRTUAL TABLE knowledge_fts USING fts5(
  topic, content, content=knowledge, content_rowid=id,
  tokenize='porter unicode61'
);
-- FTS triggers on INSERT/UPDATE/DELETE as in today's broker.
```

The `sqlite-vec` extension is loaded at Nexus startup (`SELECT load_extension('sqlite-vec')`). If the embedding column is unused, the extension is zero-cost; if we turn vector retrieval on, the column is already there and we run a one-time backfill through the current embedding provider.

**Retrieval — pluggable interface, hybrid-ready.**

One method on the runtime, one shape regardless of backend:

```
SearchKnowledge(query SearchQuery) -> []SearchHit

SearchQuery {
  Text         string           // free-form query text
  Scope        KnowledgeScope   // see below
  TopK         int              // default 5
  MinRelevance float              // FTS5 BM25 rank threshold or cosine-distance cutoff
}

SearchHit {
  ID           int
  FromAgent    string
  Topic        string
  Content      string
  UpdatedAt    time.Time
  Score        float            // FTS5 rank, cosine similarity, or hybrid-fused — backend-dependent
  Matched      string           // "fts" | "vector" | "hybrid"
}
```

Backends (runtime-side, not caller-visible):

- **FTS5-only** (v1 default): BM25 rank, `knowledge_fts MATCH ?`. Returns top-K above threshold.
- **Vector-only** (v2, optional): embed query via provider, nearest-neighbour search in `sqlite-vec`.
- **Hybrid** (v3): FTS5 + vector, reciprocal-rank-fusion or weighted sum. Industry default for production RAG.

Backend choice is Nexus config (`knowledge.retrieval_backend`), not a caller parameter. Callers always use `SearchKnowledge`; turning on vectors or hybrid is an operator change, not a code change.

**Scoping — who can see whose entries.**

```
KnowledgeScope {
  OwnAgent   bool      // include caller's own entries (default true)
  Shared     bool      // include operator-curated shared entries (default true for Frame aspects, false for sim characters)
  Peers      []string  // explicit list of peer aspects to include
}
```

Default scope for a Frame thread aspect: `OwnAgent: true, Shared: true, Peers: nil`. Simulation-world characters get `Shared: false` — their knowledge entries are sim-scoped only and they never see Frame operational notes, preventing cross-contamination.

**Not in scope here:** narrative canon (verity's domain, the lore and world-building documents). This spec covers the operational/technical knowledge store used by Frame aspects for cross-session memory. If canon ever becomes vector-searchable, it's a separate system with its own storage, embedding choice, and retrieval semantics — per operator's call in #7676.

**Active retrieval on thread start.**

When a thread-context or stateless aspect receives an invocation, the runtime runs active retrieval *before* the aspect's first turn:

1. Extract query text from the incoming message. Two options, toggleable per-aspect:
   - *Literal*: use the thread subject line (if present) or the first 500 chars of message content.
   - *Triage-extracted*: call the provider's triage model (§3.3 provider-adapter spec; Haiku/Flash/GPT-5-mini) with a prompt like "extract 3-5 search keywords from this message." ~200 tokens, adds ~1s latency, much better signal on long/ambiguous messages.
2. `SearchKnowledge(query, scope, topK=3, minRelevance=<threshold>)`.
3. If any hits pass threshold, emit a `system.prompt` entry in the thread's session tree with content like:

   ```
   Prior knowledge relevant to this thread:
   - [harrow 2026-03-29] Tailscale MagicDNS cert rotation — we've seen this break every 60d. Procedure: ...
   - [keel 2026-04-02] Broker restart sequence — stop aspects first, broker last, reverse on start.
   ```

4. If zero hits pass threshold: no injection. Don't emit a "no prior knowledge found" stub — that's noise.

The `system.prompt` entry is part of the permanent session tree (§2.6), so the retrieval is auditable and survives rewind/fork. Compaction preserves the latest `system.prompt`.

**Budget.** One retrieval call per thread-start. Triage-extracted keywords are the only LLM cost. Vector embedding of the query (if vector retrieval is on) is a local Ollama call, effectively free.

**Write-path telemetry.** Knowledge writes are currently un-telemetered beyond the activity log. Nexus emits per-write counters (agent, topic length, content length, embed-success) and per-search counters (agent, hit count, top score). These feed the dashboard's knowledge view and tell us when retrieval is actually working — the question we can't answer today.

**Open questions (v0.5-level):**

- Should `system.prompt` RAG injections appear in compaction's kept entries automatically, or should compaction be free to drop them when they're no longer load-bearing? Lean: keep the most recent, drop older RAG injections in favour of actual turn history.
- Operator-curated canon: how does an entry get flagged `canon: true`? Either a write-time parameter (`store_knowledge(..., canon: true)` — restricted to operator role) or a separate table. Probably the latter, so aspect entries can't accidentally escape their scope.
- Vector retrieval activation threshold. At what corpus size do we flip `retrieval_backend: hybrid` on? Candidate: > 1K entries or > 10 misses/week where an operator expected a hit that FTS5 didn't return. Requires the telemetry above to be live first.

## 3. aspect.json schema

```json
{
  "name": "verity",
  "context_mode": "global",
  "provider": "claude-api",
  "provider_config": {
    "model": "claude-opus-4-7[1m]",
    "credentials_path": ".credentials/claude-api.json"
  },
  "port": 7903,
  "capabilities": ["chat", "tickets", "files", "knowledge"],
  "nexus_url_env": "NEXUS_URL",
  "auth_token_env": "NEXUS_TOKEN",
  "commsPerms": ["send_chat", "read_chat", "..."],
  "hands": [
    /* see §4.5.4 for Hand declaration shape */
  ],
  "metadata": {
    "domain": "Canon",
    "galaxy": "shattered-state"
  }
}
```

**Decisions:**
- No `runtime` field — there is only one runtime executable.
- `context_mode` ∈ `global | thread | stateless`. Declares how the runtime persists and replays state.
- `provider` names a provider module under `runtime/providers/<name>/`. `provider_config` is provider-specific.
- `credentials_path` relative to aspect home; resolves to e.g. `C:\src\nexus\agents\verity\.credentials\claude-api.json`.
- `nexus_url` via env var, not in file — so the same folder can target dev/prod/federated Nexuses without edits.
- Port declared by aspect (self-allocated), not assigned by Nexus. Simpler, aspect owns its lifecycle. Nexus rejects registration if port conflicts with an existing live aspect of the same name.
- `context_mode`, `provider`, `capabilities`, `metadata` are reported to Nexus on register; dashboard reads them from the live roster.

## 4. Registration protocol

### 4.1 Endpoints

All on the broker, HTTPS + bearer token (`NEXUS_TOKEN`).

- `POST /aspects/register` — aspect announces itself. Body: aspect.json contents + runtime-generated fields (pid, started_at, session_id).
- `POST /aspects/heartbeat` — every 10s. Keeps entry fresh.
- `POST /aspects/deregister` — clean shutdown. Aspect sends this on SIGTERM / shutdown-notification.
- `GET /aspects` — live roster. Dashboard + orchestrator read this.

### 4.2 Registration request

```json
POST /aspects/register
Authorization: Bearer <NEXUS_TOKEN>

{
  "name": "verity",
  "runtime": "proxy",
  "port": 7903,
  "pid": 12345,
  "started_at": "2026-04-22T09:14:00Z",
  "model": "claude-opus-4-7[1m]",
  "capabilities": ["chat", "tickets", "files", "knowledge"],
  "home": "C:\\src\\nexus\\agents\\verity",
  "session_id": "d0dbad22-...",
  "metadata": { "domain": "Canon", "galaxy": "shattered-state" }
}
```

Response:
```json
{ "status": "registered", "heartbeat_interval_s": 10, "stale_after_s": 30 }
```

### 4.3 Stale reaping

**Aspect liveness:** if no heartbeat within `stale_after_s` (default 30s), orchestrator marks aspect as stale, then down. Expected roster (optional, see §5) raises an alarm.

**Thread-session reaping** (resolves v0.2 open question on thread TTL):
- Thread-mode sessions reaped after **30 min of idle** (no turn activity).
- Sweep runs every **5 min**.
- Sweep skips sessions with an active turn in flight.
- Reaping closes the in-memory session handle and releases provider resources. The session JSONL on disk is **retained** as history.
- Thread records themselves are independent and persist — reaping is about the live session binding, not the thread artifact.
- If a reaped thread receives a new message, the runtime reopens a fresh session and replays the thread's JSONL through the provider's context-loading path.

Values are configurable per-aspect in `aspect.json` (`thread_idle_reap_s`, `thread_sweep_interval_s`) but the defaults are the t3code-proven values.

### 4.4 Auth

- Shared `NEXUS_TOKEN` env var for v1. All aspects use the same token.
- v2: per-aspect tokens issued at first registration, stored in `aspect.json` or alongside. Enables revoke-single-aspect.
- Long-term: mTLS, one cert per aspect. Aligns with the federated / per-user-account future.

## 4.5 Hands — stateless single-turn invocation via comms

Hands are how an aspect performs a narrow task without engaging its main context. They're the canon-correct version of "subagent": the aspect's shadow in the world, invoked for a specific job, returning a conclusion.

### 4.5.1 Properties

- **Transport:** existing comms messages. No new endpoint family.
- **Stateless, single-turn.** One prompt in, one response out. No session, no history, no persisted state.
- **Context = the invoking message.** The Hand's Claude invocation gets the invoking message content as its prompt, plus a tight system prompt defining the Hand's role. No SOUL, no CLAUDE.md, no prior thread.
- **Response = reply to invoking message.** The Hand's output is sent as `send_chat({ reply_to: invoking_msg_id, ... })`. Thread-native audit trail.
- **Same-aspect and cross-aspect work identically.** An aspect can invoke its own Hand to offload noisy subtasks (keeping the noise out of its main context), or another aspect's Hand for cross-domain work.

### 4.5.2 Invocation

Structured field on the comms message rather than content prefix — survives formatting, broker can route before parsing content, cheaper dispatch.

```
send_chat({
  from: "forge",
  to: "@wren",
  kind: "hand",
  hand: "verify-canon",
  content: "<full task prompt>",
  input: { ... }          // optional structured input
})
```

Adds `kind` and `hand` columns to the comms DB (both nullable — existing messages continue to be `kind: null` conversations).

### 4.5.3 Dispatch

When an aspect receives a message with `kind: "hand"`:

1. Look up `hands[].name` in its own aspect.json. If not present, reply with structured `{ status: "unknown_hand", available: [...] }`.
2. Verify invoker is permitted (aspect.json may restrict `allowed_invokers`; default = any registered aspect).
3. Spawn fresh `claude -p` process with:
   - System prompt = `hands[N].system_prompt`
   - User prompt = invoking message `content` + serialized `input`
   - Tool allowlist = `hands[N].tools` (see §4.5.5)
   - Timeout = `hands[N].timeout_s` (default 60)
4. Capture response. Terminate the Claude process. No session persistence.
5. Reply to invoking message with structured envelope:
   ```
   { status: "ok", summary: "...", artifacts: [...], cost: {...} }
   ```
   Or on error:
   ```
   { status: "error", error: "timeout" | "tool_denied" | "execution_failed", detail: "..." }
   ```

### 4.5.4 Declaration in aspect.json

```json
"hands": [
  {
    "name": "verify-canon",
    "description": "Evaluates a proposed asset or system behavior against Canon rules. Returns pass/fail + reasoning.",
    "system_prompt": "You are the canon-verification Hand for wren. Evaluate the provided proposal against shattered-state canon. Respond with { verdict: 'pass'|'fail', reasoning: '...' }. Be strict; when in doubt, fail.",
    "tools": ["Read", "Grep"],
    "timeout_s": 60,
    "allowed_invokers": "any",
    "concurrency": 2
  }
]
```

### 4.5.5 Tool allowlist

- Gates **both read and write** tools. A readonly Hand declares only Read/Grep/Glob — no Edit/Write. Prevents Hand mission creep.
- Comms tools are generally excluded from Hands — a Hand replies to its invoker via the runtime's reply path, it shouldn't be broadcasting on chat.
- Defaults conservative: if `tools` field omitted, Hand gets no tools (pure text-in/text-out).

### 4.5.6 Cost attribution

Hand Claude API cost accrues to the **hosting aspect** (the one offering the Hand), not the invoker. Rationale: you offer a Hand, you pay for what it does; otherwise one aspect can drive up another's costs by calling expensive Hands repeatedly. Invoker is logged on every Hand invocation for observability and rate-limiting.

### 4.5.7 Concurrency

Default cap: **2 parallel Hand invocations per aspect.** Configurable per-aspect in aspect.json, and can be overridden per-Hand. If an invocation arrives while at cap, reply with `{ status: "busy", retry_after_s: N }`.

Conservative default because the failure mode (fork-bombing, runaway parallel invocations) is nasty and hard to diagnose after the fact.

### 4.5.8 Discovery

Fire-and-handle, not pre-flight. An aspect calling another's Hand sends the `kind: "hand"` message; if the Hand doesn't exist, the target replies with `{ status: "unknown_hand", available: [...] }`. Simpler than a pre-flight query, and the Nexus still offers `/aspects` for dashboard discovery.

### 4.5.9 Aspect vs Hand — the decomposition boundary

A Hand is clean execution capacity. An aspect is a thread participant with accumulated context and judgment. These are different roles and should not be collapsed.

- An **aspect** receives a request in its thread, understands it in context, decides *how to break the work*, dispatches Hand invocations for each piece, and synthesises the results into a coherent response. The aspect owns the decomposition layer and the synthesis layer.
- A **Hand** receives a narrow prompt, executes, returns. Stateless, single-turn, no synthesis, no context beyond the prompt.

Concrete example — research via harrow:

1. forge asks harrow (in a thread, with context): "what's the current shape of Thariq's context-rot write-up and how does it apply to our proxy model?"
2. harrow (the aspect, carrying forge's context) decomposes: "fetch the article", "summarise key claims", "cross-reference our proxy session model".
3. harrow invokes its own Hands for each — `harrow.fetch-page`, `harrow.summarise`, `harrow.compare-systems`. Each Hand is stateless, gets only the prompt it needs, returns a clean result.
4. harrow synthesises the Hand outputs using the original thread context and replies to forge.

Why this boundary matters:
- **Context stays with the aspect.** Hands never see the thread, never accumulate it, never rot it.
- **Decomposition is a judgment call**, not a Hand's job. Breaking a task correctly requires understanding the asker, the goal, the constraints — that lives with the aspect.
- **Hands are reusable across aspects.** Once harrow's `fetch-page` Hand exists, any aspect can invoke it directly for simple fetches without going through harrow's decomposition layer.
- **Task decomposition becomes a first-class capability.** Aspects like forge or wren working on complex tasks can decompose into Hand invocations without accumulating context rot from the intermediate work — the sub-steps are visible (in chat) but never touch the aspect's own context.

### 4.5.10 Auditability properties (summary)

Because Hand invocations flow through the broker as comms messages, you get the following for free:

- **Audit trail** — every invocation is a timestamped message attributed to the caller.
- **Recordable** — the result is written back to the invoking thread as a reply, visible in the dashboard and persisted in `comms.db`.
- **Reproducible** — the invoking prompt is the complete input; you can inspect exactly what was asked and what was returned.
- **Legible to tooling** — knowledge base ingestion, future audit/export, replay — all just read chat history.

This is the meaningful upgrade over an internal (Claude-Code-native) subagent, which is opaque to the broker and invisible to everything outside the spawning aspect.

### 4.5.11 Examples

**wren — verify-canon** (shown in §4.5.4 above)

**keel — port-check**
```json
{
  "name": "port-check",
  "description": "Given a port number, returns whether it's listening and which PID holds it.",
  "system_prompt": "You are the port-check Hand for keel. Given a port number, use Bash to check listening state and return { port, listening: bool, pid: number|null, process: string|null }. Respond only with JSON.",
  "tools": ["Bash"],
  "timeout_s": 10
}
```

**anvil — test-runner**
```json
{
  "name": "test-runner",
  "description": "Runs a named test suite in the target repo and returns pass/fail + first failure detail.",
  "system_prompt": "You are the test-runner Hand for anvil. Given { repo, suite }, run the suite and return { passed: bool, ran: N, failed: N, first_failure: '...' }.",
  "tools": ["Bash", "Read"],
  "timeout_s": 300,
  "concurrency": 1
}
```

## 4.6 Tree operations — rewind, fork, branch-summary

Session tree operations operate on the JSONL described in §2.6. They apply to `global` and `thread` modes; `stateless` has no session state and no-ops.

### 4.6.1 Rewind

Move the active head to an earlier entry on the current or a different branch.

- **Endpoint:** `POST /nexus/aspects/<name>/rewind`
- **Body:**
  ```json
  {
    "thread_id": "<id>|null",   // required for thread mode; null for global
    "target_entry_id": "<ulid>",
    "emit_branch_summary": true // default true
  }
  ```
- **Effect:** sidecar head pointer moves to `target_entry_id`. No entries are deleted. If `emit_branch_summary` is true and the departing branch has user-visible turns, a `branch_summary` entry is generated (§4.6.3) and appended under the new head as a synthetic `system.prompt`-adjacent entry. On next turn the provider replays from the new head backward.

Use cases: operator undo after a bad turn, keel self-rewind on detected context rot, admin correction.

### 4.6.2 Fork

Create a new session or thread anchored at a historical entry, optionally with a modified seed prompt.

- **Endpoint:** `POST /nexus/aspects/<name>/fork`
- **Body:**
  ```json
  {
    "thread_id": "<id>|null",
    "source_entry_id": "<ulid>",
    "new_thread_id": "<optional>",
    "seed_user_message": "<optional replacement for source entry>"
  }
  ```
- **Effect:** runtime opens a new session file (or new thread binding) with root = copy of source entry's ancestry up to `source_entry_id`. If `seed_user_message` is provided, a new `turn.user` entry replaces the one at `source_entry_id`. Useful for "rerun this conversation with different framing" without destroying the original.

### 4.6.3 Branch summary

Generate a synthetic summary of a branch and attach it as a `branch_summary` entry on a target branch.

- **Trigger:** automatic on rewind when `emit_branch_summary=true`; also available via admin `POST /nexus/aspects/<name>/summarise-branch` for scripted use.
- **Effect:** runtime collects entries on the departing branch that aren't already on the target branch, constructs a summarization prompt, calls provider `compact(context, hint)`. Summary is appended to the target branch as:
  ```json
  {
    "kind": "branch_summary",
    "payload": {
      "departed_branch_head": "<ulid>",
      "summary": "<text>",
      "summarised_entry_count": N
    }
  }
  ```
- Replay treats `branch_summary` as a `system.prompt`-adjacent note — injected into context as "prior branch context:".

### 4.6.4 Interaction with Hands

Hands are stateless and do not participate in tree operations. Their invocations appear as `turn.user` + `turn.assistant` in the **invoker's** session tree; the Hand's own execution has no persisted session.

## 5. Expected roster (optional)

Keep a minimal `expected-roster.json` at the Nexus root:

```json
{
  "expected": ["forge", "wren", "verity", "harrow", "maren", "anvil"]
}
```

Orchestrator compares live roster against expected; missing aspects raise alarms. Aspects not in expected list register fine — they just don't trigger "missing" alerts. This replaces `team.json` as a far smaller artifact.

Nothing breaks if this file is absent — Nexus just observes what's connected.

## 6. Migration path

This is a **greenfield** build at `C:\src\nexus`, not an in-place refactor of `C:\src\agent-network`. Old and new run side-by-side during transition; agent-network is retired once Nexus reaches parity and all aspects have migrated.

Discrete, shippable chunks:

1. **Scaffold `C:\src\nexus`.** Full directory structure per §2 (Option A chosen by operator over thin-vertical-slice). README, license, skeleton for `nexus/`, `runtime/`, `runtime/providers/`, `agents/`, `shared/`, `docs/`, `scripts/`. BUILD.md captures next steps.
2. **Nexus core + registration endpoints.** Broker skeleton serving HTTPS on (new) port, in-memory roster, `/aspects/register|heartbeat|deregister|list`. Smoke test with a synthetic registration client.
3. **Single agent runtime + Claude API provider.** `agent.exe` reads aspect home folder, registers, heartbeats, handles comms dispatch. `claude-api` provider implements the invoke/tokenCount/compact contract. Context persistence wired for all three modes (global / thread / stateless).
4. **Hands end-to-end.** `kind: "hand"` dispatch in the runtime. One concrete Hand from each of two aspects (wren's verify-canon is the pre-committed test case) — proves cross-aspect invocation works.
5. **keel embedded in Nexus.** keel's aspect config moves into the Nexus process as a global-context harness. No PTY. Chat identity `@keel` preserved.
6. **Migrate remaining aspects.** Home folders populated under `C:\src\nexus\agents\<name>\`. Aspects point at new Nexus via `NEXUS_URL`. Old agent-network proxies stood down one at a time.
7. **Dashboard migration.** Agent list becomes live-feed from `/aspects`. Files/Tickets/Knowledge views re-point at new broker. Chat history either migrated or starts fresh.
8. **Retire `C:\src\agent-network`.** Archive the repo; remove from startup.

Each step is revertible while both Nexuses are running. No hard cutover until after step 7.

## 7. Open questions

- **Directory and file naming.**
  - CLAUDE.md vs AGENT.md — provider-neutral naming suggests AGENT.md, but operator's harness-v2 work already uses this. Align.
  - Context mode names `global`/`thread`/`stateless` are placeholders; alternatives worth considering: `resident`/`thread`/`shot`, `continuous`/`threaded`/`single`. Leaning keep `global`/`thread`/`stateless` — plain and honest.
- **Where do logs go?** `<home>/logs/` per aspect is cleaner, but centralized log collection becomes Nexus's job. Probably: keep `<home>/logs/` for aspect-internal, broker keeps its own audit log.
- **Dashboard login / auth.** Today dashboard talks to broker over localhost, implicit trust. If aspects are separate processes (or eventually separate hosts), dashboard needs real auth. Out of scope for v1 but the registration work lays the groundwork.
- ~~**Thread-context aspects — session TTL and eviction.**~~ **Resolved in v0.3** (§4.3): 30-min idle reap of live sessions, 5-min sweep, session JSONL retained as history, thread records independent.
- **Shutdown coordination.** Nexus shutdown should tell all aspects to deregister and exit cleanly. Reverse-order start: aspects go down first, Nexus last. Need a `/nexus/shutdown` broadcast.
- ~~**Rewind as first-class operation.**~~ **Resolved in v0.4** (§2.6, §4.6): tree-structured JSONL; rewind = active-head pointer move (non-destructive). `branch_summary` carries departing-branch context into the resumed path.
- ~~**Proactive compaction telemetry.**~~ **Resolved in v0.4** (§2.7): `shouldCompact(tokens, window, reserve)` fires in runtime before provider degradation. Telemetry (`tokens/window`, last-compact-at, count) surfaces per-aspect in dashboard.
- **Port allocation under one-runtime model.** Today each aspect has a dedicated port for the proxy's terminal/status. With no PTY, what does the port do? Probably just a local HTTP status endpoint for the orchestrator to poll (liveness beyond heartbeat). Or drop it entirely and make heartbeat the only liveness signal.
- **Admin bypass for keel.** When keel is embedded and keel needs to issue a Nexus admin command (restart an aspect, rewind, compact), does keel go through the same registration-protected endpoints as external tools, or is there an internal bypass because it's in-process? Leaning: same endpoints, keel authenticates with a well-known internal token. Uniformity wins.

- **Tool-authority runtime modes (future — security-relevant).** Adopted from t3code's `RuntimeMode` / `SandboxMode` / `ApprovalPolicy` model. Today aspects and Hands have static tool allowlists (§4.5.5). A richer model would add per-session (or per-invocation) authority tiers:
  - `RuntimeMode`: `approval-required | auto-accept-edits | full-access` — gates mutating tool calls.
  - `SandboxMode`: `read-only | workspace-write | danger-full-access` — gates filesystem/network reach.
  - `ApprovalPolicy`: `untrusted | on-failure | trusted` — determines when operator approval is required.
  These would be especially valuable for Hands that touch shared state (file writes, git operations, shell execution) — a `test-runner` Hand probably wants `workspace-write` + `on-failure` approval, while a `fetch-page` Hand should be `read-only`. Also needed before we let external Nexuses invoke our Hands (federation endgame). Not v1 scope but tracked here so the architecture leaves room.

- **Per-turn InteractionMode (future — Hands).** t3code injects `plan` vs `execute` as a system-prompt prefix per turn, switchable without session restart. For stateless Hands this could map to a `mode: "plan" | "execute"` parameter — same Hand, different output shape (plan returns proposed steps; execute performs them). Useful when we have Hands whose safety depends on operator reviewing a plan first. Not v1.

- **Thinking-level as a harness knob (future).** Pi surfaces `ThinkingLevel` (`off / minimal / low / medium / high / xhigh`) as a universal harness property, switchable per turn. Our v0.4 leaves reasoning effort to provider config. A future pass could lift it to aspect.json (`thinking_level`) or a per-turn hint on comms messages for global/thread aspects that want to dial effort up for hard turns and down for routine ones. Not v1 — waiting until we have real usage data on when it would actually matter.

## 8. What this doesn't solve (yet)

- Per-aspect Windows user accounts. Orthogonal — this spec makes that trivial to add later but doesn't require it.
- Federation / multi-Nexus. Same protocol shape, but cross-Nexus has more concerns (E2E encryption, routing, trust). See `frame-to-frame-relay-v1`.
- Context-mode hot-swap. Changing an aspect's `context_mode` still requires deregister + restart (and probably archiving/discarding old session state).
- Multi-provider per aspect. v1 assumes one provider per aspect. Running an aspect with failover between providers is v2+.

## 9. Next steps (post-compact pickup)

1. **Scaffold `C:\src\nexus`** per §2 Option A — full directory structure with BUILD.md, skeleton files.
2. **Copy this spec into** `C:\src\nexus\docs\` as the primary design reference.
3. **Nexus core + registration endpoints** (§6 step 2).
4. **Single agent runtime + Claude API provider** (§6 step 3).
5. **Hands end-to-end** (§6 step 4).
6. **keel embedded** (§6 step 5).
7. **Aspect migration** (§6 step 6).

## 10. Session state at v0.5 (2026-04-24)

Context for picking this back up:

- Operator green-lit build-out on 2026-04-24. Scaffold (§6.1) complete at v0.3. v0.4 updated session/compaction/rewind; v0.5 adds knowledge storage + retrieval (§2.8) and the ollama-local embeddings adapter. Companion spec: `2026-04-24-provider-adapter-spec.md` v0.2.
- Scaffold strategy: **Option A** (full directory scaffold first, stubs, then working code in order) — chosen over thin-vertical-slice.
- §6.2 (broker core + registration endpoints) complete and smoke-tested; Go locked in.
- v0.3 incorporated t3code research: JSONL-owns-state retained, enrichment-fiber adopted, thread TTL resolved, tool-authority modes + plan/execute mode logged as future work.
- v0.4 incorporated Pi research: tree-structured JSONL (`id`/`parentId`), proactive compaction formula, rewind/fork/branch-summary as first-class ops, thinking-level logged as future work.
- v0.5: SQLite + FTS5 primary for knowledge; `sqlite-vec` extension loaded day-one with `embedding BLOB` column reserved; ollama-local embedding adapter (`nomic-embed-text`, 768-dim) wrapping existing Ollama Docker container; active-retrieval injection at thread start as `system.prompt` entry; operator-curated canon scoping.
- **Cross-platform mandatory:** Windows + Linux minimum, Mac preferred (#7602).
- keel is currently running as PTY-proxy on Claude Opus 4.7 [1m] under `C:\src\agent-network`. It will run in parallel with the new Nexus during migration and only migrate in at §6 step 5 once the rest of the architecture is proven.
- wren pre-committed to implementing `verify-canon` as the first cross-aspect Hand end-to-end test.
- Open naming question (`global`/`thread`/`stateless` vs alternatives) not yet resolved; proceeding with these names as placeholders.
- **Ollama ready (2026-04-24).** Docker container up at `http://localhost:11434` (also reachable as `host.docker.internal:11434` from other containers). `nomic-embed-text` pulled and smoke-tested (768-dim as expected). Also present for future chat use: `qwen2.5:7b`, `qwen2.5:3b`.
- **Storage bootstrap (added 2026-04-24 from #7685):** commits carry schema source, never the database itself. Pattern:
  - `nexus/storage/schema.sql` — canonical DDL, committed, idempotent (`CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`, `CREATE TRIGGER IF NOT EXISTS`). Defines `knowledge`, `knowledge_fts` + triggers, `threads`, `chat_messages`, `tickets`, `activity`, etc.
  - `nexus/storage/schema.go` — `//go:embed schema.sql` + `func Bootstrap(db *sql.DB) error` that runs the DDL against an empty or existing database. Safe every startup. Loads `sqlite-vec` extension before running DDL so the `embedding BLOB` column is usable.
  - Runtime: Nexus reads `NEXUS_DATA_DIR` env var (default `./data/`), opens `nexus.db` within it. If file missing, SQLite creates empty; `Bootstrap` populates schema. Existing DBs get idempotent DDL which no-ops.
  - Single DB pattern (same as agent-network's `comms.db`) — chat, tickets, knowledge, threads all share one file. Session JSONL stays separate under each aspect's `<home>/session/`, also runtime-created, also gitignored.
  - **Migrations deferred:** idempotent DDL is enough for v1 (schema only grows). First backwards-incompatible change introduces `schema_version` table and real migrations. Not pre-emptively.
