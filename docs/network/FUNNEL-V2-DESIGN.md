# Funnel v2 — the context engine

**Status:** draft spec, 2026-07-06 · **Author:** shadow · **Supersedes:** the implicit "accumulate-and-resend" behaviour of the current agentfunnel/bridle loop.

## Why

The pool pipeline works end to end (verified completion, ledger dispatch, heartbeats, PR gates). The **cost/quality of a single worker run** does not. Live measurement, ticket NET-36 (a real repo PR built by `anvil-builder` on Ornith):

- **94 tool-call steps, 3.1M input tokens, ~33k avg input/step, 18.5 min.**
- Root shape: the loop is an **append-only transcript replayed in full every step**. Input grows ~linearly per step → total prefill cost is ~O(n²) over a run. A 30B local model with a bounded window also **degrades as the window saturates** (stale tool output crowds out the goal).

This is not a model-capability problem — NET-36 *did* produce a correct, merged-quality PR. It is a **context-engineering** problem. Funnel v2 reframes the funnel from a *transcript accumulator* into a **context engine**: each step it *composes a view* rather than *appends and resends*.

Validated against the deepagents research (`~/shadow/research/deepagents-2026-07-06.md`): the mechanisms below are that library's load-bearing ideas, ported as small Go changes rather than adopting the Python framework (sovereignty north-star: minimise external load-bearing deps). The single biggest lever — prefix caching — is **not** a deepagents feature at all; it lives in the vLLM server we already run.

## Non-goals

- Not touching the verification layer (acceptance gates, cheap-judge, bounded re-prompts), the ledger/dispatch, heartbeats, or PR gates. Those are nexus differentiators deepagents lacks; they sit *outside* the turn loop and survive this rework untouched.
- Not adopting LangChain/LangGraph. Patterns, not the library.
- Not a rewrite of `bridle`'s provider adapters. v2 defines a **contract** bridle must satisfy; where it already does, we use it; where it doesn't, that's a scoped bridle change called out below.

## The architecture split (the load-bearing boundary)

```
ledger → dispatch → agentfunnel ──[composes context]──▶ bridle ──▶ vLLM(Ornith)
                        │                                  │            │
                    funnel v2                         turn loop     prefix cache
                 (this spec owns)                   (message accum) (serving layer)
```

- **agentfunnel / `nexus/frame/funnel`** — nexus's wrapper. Owns prompt composition (`composeSystemPrompt`, `main.go:1112`), the goal loop (`goal_loop.go` `Pursue`), tool-result handling, and what gets handed to bridle each turn. **Most of v2 lives here.**
- **`bridle`** (external, `CarriedWorldUniverse/bridle`) — owns the message list and the provider call. v2 needs two things from it (§4).
- **vLLM** (Ornith serving, `100.92.111.3:30801`) — owns the KV/prefix cache. v2 needs one flag + a discipline (§1).

## The four mechanisms

### 1. Prefix-cache-safe transcript + vLLM prefix caching — *the dominant lever*

**Problem:** vLLM re-prefills the entire prompt every step because the prefix isn't stable (or caching is off). This is where the 3.1M tokens are spent.

**Change (serving):** enable **automatic prefix caching** on the Ornith vLLM deployment (`--enable-prefix-caching`; confirm the model-stack manifest and re-roll).

**Change (funnel — the precondition):** the prompt prefix must be **byte-identical across steps** so the cache actually hits. Concretely, the assembled prompt must be **append-only**:
- **Stable prefix zone** (never re-rendered mid-run): system prompt = personality + resolved role prompt + tool schemas + workspace preamble. `composeSystemPrompt` output must be deterministic and frozen at turn 1. **No timestamps, no re-ordered tool lists, no re-sorted maps, no per-turn "current time" injection** in the prefix.
- **Append-only history:** new turns append; existing turns are **never edited in place** (edits invalidate every cached token after the edit point). The one operation that *must* mutate history — eviction (§2) — is **batched** and treated as an explicit cache-reset checkpoint, not a per-step trickle.

**Expected effect:** the dominant fix. Per-step prefill drops from full-context to delta-only — order-of-magnitude input-compute reduction on long runs. *Precondition for the other two: any unbatched edit/eviction silently defeats it.*

**Audit needed:** grep the funnel + bridle prompt path for non-determinism in the prefix (map iteration order, `time.Now()` in system text, re-rendered tool declarations). List every source and fix or move it out of the prefix.

**Audit results (2026-07-06):**

| Source | Verdict | Note |
| --- | --- | --- |
| `composeSystemPrompt` (`runtime/cmd/agentfunnel/main.go:1112`) | deterministic | no timestamps, no map iteration in the rendered text |
| openai provider request assembly (`bridle` `provider/openai/openai.go:361`) | deterministic | serializes the already-built message/tool list; introduces no ordering of its own |
| MCP tool listing → `mergeToolSurface` (`bridle` `run.go:834`, pre-fix) | **the one default-on breaker** | MCP servers reconnect + re-`ListTools` every `RunTurn` (`run.go:32-52`); each server's list order is server-dependent, so the merged surface reordered on every turn and busted the cached prefix from token 0. **Fixed** in bridle by sorting the merged surface by tool `Name` — see `fix/deterministic-tool-order`, merged as `e2c52f8` ([bridle#74](https://github.com/CarriedWorldUniverse/bridle/pull/74)). |
| `hookAdditionalContext` (SessionStart/AutoRecall hooks) (`nexus/frame/funnel/funnel.go:1387-1394`, pre-fix) | breaks-cache-always when enabled | previously off by default (env-gated); concatenated onto the SYSTEM prompt every turn a hook fires — a single recall-firing turn invalidated the KV cache for the whole conversation (measured 22.9s→0.2s TTFT swing). **Fixed unconditionally** — tail-injection into the trailing per-turn user/delta zone (same zone as Inbox/continuation-brief, bridle `lowerRequest` `run.go:635-653`) is now the only path, no env gate; the rendered recall entries also drop their volatile `updated:` timestamp so the delta-zone diff itself stays minimal (`nexus/frame/funnel/commonplace.go:67`). |

### 2. Tool-result eviction to a workspace

**Problem:** big tool results (file reads, command output, grep dumps) sit in-window forever and get replayed every step. On repo work the worker re-reads the same file contents constantly.

**Change (funnel):** a **workspace** (the builder already has a per-run home repo + `/work` PVC — reuse it) plus two thresholds ported verbatim from deepagents' Context-Management design:
- **Large result → file.** A single tool result **> ~20k tokens** is written to a workspace file; the in-window message becomes `«result written to <path> (N lines); first 10 lines: …»`.
- **Context-pressure sweep.** At **~85% of the model's context window**, older tool results are rewritten to file pointers (oldest first) until back under threshold.
- **Re-read on demand:** the worker's existing `read_file`/`grep` tools already let it pull a body back when it actually needs it — so eviction is lossless, just deferred.

**Cache interaction (critical):** eviction *edits* history → invalidates the prefix cache from the edit point. So sweeps are **batched and rare** (a checkpoint), never per-step. Do the sweep, accept one cache-cold step, then run append-only again.

**The 85% pressure signal already exists, for free.** bridle's `ContextPolicy.PromptBudget` + `ContextBudgetWarning` (bridle `context.go:32,53`) is a warn-only, engine-agnostic check on the assembled prompt that fires today when the estimate meets/exceeds the configured budget (bridle `run.go:108-115`) — v1 just needs to set the budget to 85% of the model's window and listen for the warning as the sweep trigger, no new signal to build. The token estimate to reuse for both that budget check and the eviction thresholds is the existing char-based `estimateContextTokens` (`nexus/frame/funnel/funnel.go:2420`) — no new tokeniser dependency needed (answers the "tokeniser for thresholds" open question below).

**Expected effect:** caps steady-state window on file-heavy runs; plausibly 2–5× fewer tokens/step late in a run; and directly guards the 30B model against the quality collapse that comes with a saturated window.

### 3. `write_todos` — a no-op recitation tool

**Problem:** over 94 steps the model loses the plot (NET-36's first run degenerated into a literal repetition loop; the re-run needed anti-repetition sampling just to stay coherent).

**Change (funnel):** register a **no-op tool** `write_todos(items: [{text, status: pending|in_progress|completed}])`. It stores the list in funnel state and re-emits it into recent context each turn (Manus-style recitation — the deepagents pattern). Costs tens of tokens/step. The system prompt instructs: plan first, keep the list current.

**Bonus — it strengthens the verification layer for free:** the todo list is a **machine-readable plan** the acceptance judge (§ verification, untouched) can check against the ticket's acceptance criteria — a better signal than judging prose. Wire the current todo snapshot into the acceptance verifier's input.

**Expected effect:** minimal token cost; measurably fewer lost-the-plot failures on 50+ step runs; a new signal for the existing gates.

### 4. (Deferred) subagent quarantine

deepagents' fourth pillar — spawn a child agent for a sub-task, return only its final result, discard its transcript. nexus **already has this at the fleet level** (the orchestrator dispatches separate worker Jobs). In-worker quarantine (explore→build→verify as child turn-loops) is a real future lever but has a sharp interaction with §1: each child **breaks prefix-cache continuity**, so it only wins when a child's transcript is *large*. **Deferred** until §1–§3 are measured; revisit if single-worker runs still blow context after eviction.

## Measurement — baseline first, prove the reduction

We already emit per-turn `input_tokens` / `output_tokens` / `steps` via `ObservabilityHook` (funnel logs `funnel: turn complete … input_tokens=…`). The bar for v2 is **measured, not asserted**:

1. **Baseline (now, before any change):** re-run a NET-36-class repo ticket on current `main`; capture total input tokens, tokens/step curve, step count, wall-clock, outcome. (We have NET-36's real numbers: 3.1M / 94 / 18.5min — use as the reference point.)
2. **After §1 (prefix caching + stable prefix):** same ticket; expect the tokens/step curve to flatten (cache hits) — the headline number.
3. **After §2 (eviction):** expect the late-run tokens/step to drop and the curve to plateau instead of climb.
4. **After §3 (todos):** expect fewer steps / no repetition-loop failures, tracked as run-outcome not tokens.

Add a one-line per-run summary to the worker-status store / recent-runs panel: `input_tokens`, `steps`, `evictions`, outcome — so the fleet UI shows the efficiency trend across real tickets, not just a one-off benchmark.

## Rollout

Env-gated, additive, reversible — same posture as every unit this session:
- Tail-injection of hook-additional-context is unconditional (no env gate — see §1 audit table); vLLM `--enable-prefix-caching` (serving).
- `FUNNEL_WORKSPACE_EVICT=1` + `FUNNEL_EVICT_RESULT_TOKENS` (default 20000) + `FUNNEL_CTX_SWEEP_PCT` (default 85).
- `FUNNEL_TODOS=1`.
Each independently togglable so the measurement steps above isolate each mechanism's effect. Ship as separate units (one line each), each with the seal-then-verify + `-race` gate and a live re-run of the NET-36-class ticket as its acceptance test.

## Open questions

- **Does bridle expose an eviction seam?** — **RESOLVED.** bridle's `Harness` is stateless (bridle `harness.go:284`); funnel owns `sessionTail` itself (`nexus/frame/funnel/funnel.go:607`), so eviction is **funnel-local**: a sweep in `commitTurnState` (`funnel.go:1628`) that rewrites session-tail entries to file pointers in place before the next turn is assembled. bridle needs **no** `RewriteMessage`/`CompactHook` — funnel never needs bridle to mutate anything it's holding. Within-turn stubs (e.g. announcing "result written to file" mid-turn) can use bridle's existing `RegisterBeforeModelCall` hook (bridle `hooks.go:130`), which hands back a mutable `ProviderRequest` before the provider call.
- **Tokeniser for thresholds.** "20k tokens" needs a cheap token count in Go without a full tokeniser dep — a chars/4 estimate is fine for a threshold (over-evicting slightly is harmless); confirm acceptable. **Answered above (§2):** reuse the existing char-based `estimateContextTokens` (`funnel.go:2420`) rather than adding a tokeniser.
- **Prefix stability in bridle's own framing.** — **RESOLVED.** The distill/rewriter path (`buildAgentFunnelRewriter`, `runtime/cmd/agentfunnel/main.go:2326-2330`) is a **no-op on the openai/Ornith path** — it only activates for claude-code-flavored providers (which have a session jsonl to compress); it never touches history on the path this spec targets. The only history rewrite in play is funnel's own `compact()` at the 125k-token threshold (`funnel.go:1811`, `DefaultCompactionPolicy` at `funnel.go:79`). Consequence for §2: eviction sweeps and compaction both mutate `sessionTail`, so they **must be ordered** relative to each other — a compaction rotate must not run mid-sweep and discard freshly-written eviction pointers (and vice versa); serialize the two under the same lock funnel already holds in `commitTurnState`.
