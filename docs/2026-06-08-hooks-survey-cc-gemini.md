# Hooks survey — Claude Code + Gemini CLI → funnel recommendation

**Date:** 2026-06-08
**Status:** research (NEX-501) — feeds the funnel hook system (NEX-500)
**Context:** `2026-06-08-dispatch-pod-and-home-model.md` §3.

## TL;DR / recommendation

Claude Code and Gemini CLI have **independently converged on the same hook model** — stdin-JSON in, JSON-out + exit-code decisions, event matchers, layered config. That convergence is a strong signal: **adopt the shared shape.** For the funnel specifically:

1. **Build the hook layer at the funnel seam, provider-agnostic** — *not* relying on the underlying CLI's hooks. claude-code has a rich hook system; codex and gemma do not. The funnel's platform hooks (memory, custodian, observability) must work regardless of which CLI an agent runs, so the canonical hook layer lives in the funnel. (Where the CLI has native hooks, optionally bridge — but don't depend on it.)
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

## 2. Gemini CLI hooks

**12 events:**
- **Tool:** `BeforeTool`, `AfterTool`.
- **Agent:** `BeforeAgent` (after submit, before planning — inject context), `AfterAgent` (after final response — validate/retry).
- **Model:** `BeforeModel` (modify request or inject *synthetic response*, skipping the LLM), `AfterModel` (per-chunk redaction/PII filter), `BeforeToolSelection` (filter the offered toolset / force tool mode).
- **Lifecycle:** `SessionStart` (`startup`/`resume`/`clear`), `SessionEnd`, `Notification`, `PreCompress`.

**Handler types:** `command` only.

**Config:** `settings.json` (project `.gemini/`, user `~/.gemini/`, extensions), `matcher` (regex for tools, exact for lifecycle), `sequential` flag. Hooks bundle into extensions.

**Decision model:** essentially identical to CC — stdin JSON in (`session_id`, `cwd`, `hook_event_name`, event-specific), out: exit `0`/`2`/other, JSON fields `decision` (`allow`/`deny`/`block`), `continue`, `hookSpecificOutput.additionalContext`, plus a **stable SDK-agnostic LLMRequest/LLMResponse** shape for the model hooks.

## 3. Side-by-side

| Capability | Claude Code | Gemini CLI |
|---|---|---|
| Gate/modify a tool call | `PreToolUse` (`deny`/`ask`/`defer`, modify input) | `BeforeTool` (`deny`, rewrite `tool_input`) |
| Audit/transform tool result | `PostToolUse` (+ `PostToolUseFailure`, `PostToolBatch`) | `AfterTool` (hide/replace result, append context, chain a tool) |
| Inject context pre-turn | `UserPromptSubmit` (`additionalContext`) | `BeforeAgent` (`additionalContext`) |
| Validate/retry final response | `Stop` (block to continue) | `AfterAgent` (`deny` → retry with correction) |
| Filter the offered toolset | — (only per-call `PreToolUse`) | **`BeforeToolSelection`** (mode NONE/ANY, allow-list) |
| Modify LLM request / synthesize response | — (closest: `UserPromptSubmit`) | **`BeforeModel`** (override request, inject synthetic response) |
| Transform model output text | `MessageDisplay` (display-only) | **`AfterModel`** (per-chunk, affects content) |
| Session start/end | `SessionStart`/`SessionEnd` | `SessionStart`/`SessionEnd` |
| Pre-compaction | `PreCompact` (can block) | `PreCompress` (async, observe only) |
| Notifications | `Notification` | `Notification` |
| Handler types | command / **http** / **mcp_tool** / **prompt** / **agent** | command only |
| Extra events | permission (`PermissionRequest`/`Denied`), team/task (`TeammateIdle`, `TaskCreated/Completed`), worktree, file/cwd watch, MCP elicitation, `InstructionsLoaded` | — |

**Convergence:** the *core* is near-identical — gate tools, inject context, session lifecycle, pre-compaction, decisions via JSON-out + exit `0`/`2`. Either schema is a fine basis.

**Where each leads:**
- **Claude Code** — far more events (permission, team/task, worktree, file-watch, elicitation) and **5 handler types**. The handler types are the differentiator that matters for nexus.
- **Gemini CLI** — first-class **model-level hooks** (`BeforeModel` synthetic-response, `AfterModel` per-chunk redaction, `BeforeToolSelection` toolset filtering) that CC exposes only partially. Worth stealing the *concepts*.

## 4. Recommendation for the funnel — events to wire

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

**Design stance:** one funnel-level hook engine (events + matchers + stdin-JSON/JSON-out/exit-code), provider-agnostic across claude-code/codex/gemma, with CC's handler-type abstraction (`command`/`http`/`mcp_tool`/`prompt`). Re-express judge + rewriter + observability as hooks on this engine rather than as bespoke code paths. Borrow Gemini's model-level hook *concepts* (`BeforeToolSelection`, `AfterModel`) where they serve scoping/redaction.

## Sources
- Claude Code hooks reference — https://code.claude.com/docs/en/hooks
- Gemini CLI hooks reference — https://geminicli.com/docs/hooks/reference/
- Gemini CLI hooks overview — https://geminicli.com/docs/hooks/ ; Google Developers Blog — https://developers.googleblog.com/tailor-gemini-cli-to-your-workflow-with-hooks/
