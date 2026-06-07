# Hooks survey — Claude Code + Codex + Gemini CLI → funnel recommendation

**Date:** 2026-06-08
**Status:** research (NEX-501) — feeds the funnel hook system (NEX-500)
**Context:** `2026-06-08-dispatch-pod-and-home-model.md` §3.

## TL;DR / recommendation

**All three — Claude Code, Codex, and Gemini CLI — have converged on the same hook model** (stdin-JSON in, JSON-out + exit-code decisions, event matchers, layered config). And **Codex literally adopted Claude Code's schema** — `hooks.json`, the same event names, the same input/output fields, even `CLAUDE_PLUGIN_ROOT` compatibility aliases. The convergence is now overwhelming: the CC `hooks.json` shape is the de-facto standard. **Adopt it.** For the funnel specifically:

1. **Build the hook layer at the funnel seam, provider-agnostic** — *not* relying on the underlying CLI's hooks. This isn't just because gemma has none: **codex (which our builders run) has hooks, but they're command-only and have *incomplete tool interception*** — PreToolUse/PostToolUse miss some shell calls, the `unified_exec` path, and WebSearch. So codex's native hooks **cannot be trusted as the funnel's enforcement point** (custodian gating, audit). The canonical hook layer must live in the funnel, where memory/custodian/observability work uniformly across claude-code/codex/gemma. (Where a CLI has native hooks, optionally bridge — never depend on it.)
2. **Adopt Claude Code's richer handler types** — `command` + **`http`** + **`mcp_tool`** + **`prompt`/`agent`** — not just shell commands (Gemini is command-only). These map directly onto nexus's world: an `http` hook calls the **broker/custodian**; an `mcp_tool` hook invokes an **MCP**; a `prompt`/`agent` hook is a **cheap-AI micro-decision**.
3. **The funnel's judge + rewriter + observability ARE hooks** — formalize them as the first ones, don't bolt a parallel system beside them. The judge = an `AfterAgent`/`Stop` prompt-hook (should-post + class); the rewriter = an `AfterModel`/`MessageDisplay` transform; observability = `PostToolUse`.
4. **Priority events to wire** (see §4 table): `SessionStart` (inject memory), `Stop`/`SessionEnd` (capture memory + commit the per-agent home, NEX-499), `PreToolUse` (custodian credential/scope gate), `PostToolUse` (audit/trace), `PreCompact` (snapshot before lossy compaction).

## 1. Claude Code hooks

**~28 events, by cadence:**
- **Session:** `SessionStart` (`startup`/`resume`/`clear`/`compact`), `Setup`, `SessionEnd`.
- **Per-turn:** `UserPromptSubmit`, `UserPromptExpansion`, `Stop`, `StopFailure`.
- **Agentic loop / tools:** `PreToolUse`, `PermissionRequest`, `PermissionDenied`, `PostToolUse`, `PostToolUseFailure`, `PostToolBatch`.
- **Async/reactive:** `Notification`, `MessageDisplay` (replace streamed text, display-only), `SubagentStart`/`Stop`, `TaskCreated`/`TaskCompleted`, `TeammateIdle`, `CwdChanged`, `FileChanged`, `ConfigChange`, `PreCompact`/`PostCompact`, `InstructionsLoaded`, `WorktreeCreate`/`Remove`, `Elicitation`/`ElicitationResult`.

**Handler types (5):** `command`, `http` (POST JSON to a URL), `mcp_tool` (invoke a connected MCP), `prompt` (fast-model micro-decision), `agent`. With `async`/`asyncRewake` for background hooks.

**Config:** `settings.json` (`~/.claude`, project, local, managed policy, plugin `hooks.json`, skill/agent frontmatter). Matchers: exact / `|`-list / regex for tool names; enum for lifecycle events.

**Decision model:** stdin JSON in (`session_id`, `cwd`, `hook_event_name`, `tool_name`, `tool_input`, …). Out: exit `0` (parse stdout JSON) / `2` (block, stderr = reason) / other (non-blocking warn). JSON fields: `continue`, `decision`/`permissionDecision` (`allow`/`deny`/`ask`/`defer`), `hookSpecificOutput.additionalContext` (inject), `systemMessage`, `suppressOutput`.

## 2. Codex CLI hooks

**~11 events** (added v0.114→0.117, early 2026): `SessionStart`, `SubagentStart` (thread/subagent-start scope); `UserPromptSubmit`, `PreToolUse`, `PermissionRequest`, `PostToolUse`, `PreCompact`, `PostCompact`, `SubagentStop`, `Stop` (turn scope). Note: **no `SessionEnd`** (end-of-turn is `Stop`).

**Codex deliberately adopted Claude Code's schema:** `hooks.json` (+ inline `[hooks]` in `config.toml`), the same three-level hierarchy (event > matcher group > handlers), the same stdin fields (`session_id`, `cwd`, `hook_event_name`, `tool_name`, `tool_input`, `tool_response`, …), and the same decision model (`permissionDecision` `deny`/`allow` + `updatedInput`, `decision: "block"`, `additionalContext`, `continue`, exit `0`/`2`). It even ships `CLAUDE_PLUGIN_ROOT`/`CLAUDE_PLUGIN_DATA` compat aliases.

**Handler types:** `command` only in practice (`prompt`/`agent` are parsed but skipped — the schema *reserves* them, matching CC).

**Two findings that matter for us:**
- **Trust + managed model** — non-managed hooks are trusted by SHA and reviewed before first run (`/hooks` to manage); enterprise `requirements.toml` can force `allow_managed_hooks_only` (managed hooks can't be disabled). This is a ready-made **guardrail pattern for the privileged / agentharness lane** (gated, audited, admin-pinned hooks).
- **Incomplete tool interception** — PreToolUse/PostToolUse don't catch all shell calls / `unified_exec` / WebSearch, with no parallel-prevention or undo. Concrete reason codex's own hooks can't be the funnel's enforcement point.

## 3. Gemini CLI hooks

**12 events:**
- **Tool:** `BeforeTool`, `AfterTool`.
- **Agent:** `BeforeAgent` (after submit, before planning — inject context), `AfterAgent` (after final response — validate/retry).
- **Model:** `BeforeModel` (modify request or inject *synthetic response*, skipping the LLM), `AfterModel` (per-chunk redaction/PII filter), `BeforeToolSelection` (filter the offered toolset / force tool mode).
- **Lifecycle:** `SessionStart` (`startup`/`resume`/`clear`), `SessionEnd`, `Notification`, `PreCompress`.

**Handler types:** `command` only.

**Config:** `settings.json` (project `.gemini/`, user `~/.gemini/`, extensions), `matcher` (regex for tools, exact for lifecycle), `sequential` flag. Hooks bundle into extensions.

**Decision model:** essentially identical to CC — stdin JSON in (`session_id`, `cwd`, `hook_event_name`, event-specific), out: exit `0`/`2`/other, JSON fields `decision` (`allow`/`deny`/`block`), `continue`, `hookSpecificOutput.additionalContext`, plus a **stable SDK-agnostic LLMRequest/LLMResponse** shape for the model hooks.

## 4. Side-by-side

| Capability | Claude Code | Codex CLI | Gemini CLI |
|---|---|---|---|
| Gate/modify a tool call | `PreToolUse` (`deny`/`ask`/`defer` + modify input) | `PreToolUse` (`deny` + `updatedInput`; no `ask`) | `BeforeTool` (`deny`, rewrite) |
| Audit/transform tool result | `PostToolUse` (+`Failure`/`Batch`) | `PostToolUse` (`block`/feedback + context) | `AfterTool` (hide/replace/append/chain) |
| Permission decision | `PermissionRequest`/`Denied` | `PermissionRequest` (allow/deny) | — |
| Inject context pre-turn | `UserPromptSubmit` | `UserPromptSubmit` | `BeforeAgent` |
| Continue/validate end-of-turn | `Stop` | `Stop` (block→continuation), `SubagentStop` | `AfterAgent` (deny→retry) |
| Filter offered toolset | — | — | **`BeforeToolSelection`** |
| Modify LLM request / synthesize | — | — | **`BeforeModel`** |
| Transform model output | `MessageDisplay` (display-only) | — | **`AfterModel`** (per-chunk) |
| Session start / end | `SessionStart` / `SessionEnd` | `SessionStart` (no `SessionEnd`) | `SessionStart` / `SessionEnd` |
| Compaction | `PreCompact` / `PostCompact` | `PreCompact` / `PostCompact` | `PreCompress` |
| Notifications | `Notification` | (via `Stop`/messages) | `Notification` |
| Handler types | command / **http** / **mcp_tool** / **prompt** / **agent** | command (prompt/agent reserved) | command |
| Trust / managed model | managed policy settings | **SHA-trust + managed `requirements.toml`** | extensions |
| Tool interception | full | **incomplete** (misses some shell / `unified_exec`) | full |

**Convergence:** the core is near-identical across all three — gate tools, inject context, session lifecycle, compaction, decisions via JSON-out + exit `0`/`2`. **Codex and CC share literally the same schema**; Gemini is compatible in spirit.

**Where each leads:**
- **Claude Code** — most events + **5 handler types** (the differentiator for nexus: `http`/`mcp_tool`/`prompt`).
- **Codex** — CC's schema, command-only in practice, plus a **SHA-trust + managed-hooks** model worth stealing for the privileged lane; *but* incomplete tool interception.
- **Gemini CLI** — first-class **model-level hooks** (`BeforeModel` synthetic-response, `AfterModel` per-chunk redaction, `BeforeToolSelection` toolset filter) the others expose only partially. Steal the *concepts*.

## 5. Recommendation for the funnel — events to wire

| nexus need | Event(s) | Hook does | Handler fit |
|---|---|---|---|
| **Inject memory / persona** | `SessionStart` | read the per-agent home (NEX-499) → `additionalContext` | command / prompt |
| **Capture memory + commit home** | `Stop` / `SessionEnd` | write learned state → commit+merge the bare-git home | command |
| **Custodian credential/scope gate** | `PreToolUse` | check the tool call against the agent's scope; `deny` if out-of-scope | **http → custodian** |
| **Audit / cost-trace** | `PostToolUse` | emit the tool trace to observability | http / command |
| **Snapshot before lossy compaction** | `PreCompact` | persist key facts to the home before summarisation (addresses the known lossy-compaction problem) | command / prompt |
| **Post-or-not decision (the judge)** | `Stop` / `AfterAgent` | should-post + class — *this is the existing judge* | **prompt** (cheap AI) |
| **Output rewrite (the rewriter)** | `AfterModel` / `MessageDisplay` | the existing rewriter transform | command / prompt |
| **Toolset scoping** | (borrow `BeforeToolSelection`) | restrict the offered tools to the agent's `mcp_profile`/scope | command |

**Design stance:** one funnel-level hook engine (events + matchers + stdin-JSON/JSON-out/exit-code) **using the CC/codex `hooks.json` schema** (now the de-facto standard), provider-agnostic across claude-code/codex/gemma, with CC's handler-type abstraction (`command`/`http`/`mcp_tool`/`prompt`). Re-express judge + rewriter + observability as hooks on this engine rather than as bespoke code paths. All five priority events exist in codex too, so the engine maps cleanly onto codex-backed agents — we just don't rely on codex's (incomplete) interception. Borrow **Codex's SHA-trust + managed-hooks model** for the privileged/agentharness lane (gated, audited, admin-pinned), and **Gemini's model-level hook concepts** (`BeforeToolSelection`, `AfterModel`) where they serve scoping/redaction.

## Sources
- Claude Code hooks reference — https://code.claude.com/docs/en/hooks
- Codex hooks — https://developers.openai.com/codex/hooks ; advanced config — https://developers.openai.com/codex/config-advanced
- Gemini CLI hooks reference — https://geminicli.com/docs/hooks/reference/ ; overview — https://geminicli.com/docs/hooks/ ; Google Developers Blog — https://developers.googleblog.com/tailor-gemini-cli-to-your-workflow-with-hooks/
