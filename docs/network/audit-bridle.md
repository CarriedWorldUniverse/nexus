# Bridle Harness Audit — Phase-2 §3 (role-at-spawn) + §5 (worker status) readiness

*Opus auditor, 2026-07-04. Repo: `/home/operator/src/bridle` @ `9f7fe7b`. Read against PHASE2-DESIGN §3/§5.*

## Executive summary

Bridle is a mature, single-turn harness: `Harness.RunTurn(ctx, TurnRequest, ToolRunner, EventSink) → TurnResult` (`harness.go:310`). Deliberately **one-turn, stateless, session-owned-by-the-funnel**. Ten providers across two categories. The openai provider + `toolrunner` path (builder/tester seat) is production-grade. The two Phase-2 pieces that do **not** exist: (a) a wall-clock mid-turn heartbeat hook for §5's ~60s cadence, (b) any goal-loop / DoD-until-done construct — both are funnel responsibilities; bridle gives seams, not mechanisms.

## 1. Provider matrix

Two categories (`provider.go:11-17`): `direct-api` (bridle owns the tool loop, funnel's ToolRunner executes, hooks fire, MCP consumed) vs `subprocess-stream` (CLI owns its loop; bridle observes the event stream, does NOT re-run tools — `run.go:198-202`).

| Provider | ID | Category | Custom tools | Before/AfterTool | MCP | Streaming | Role fit |
|---|---|---|---|---|---|---|---|
| **openai** | `openai-api` | direct-api | ✅ | ✅/✅ | ✅ | ✅ live deltas | **builder/tester (Ornith via vLLM /v1)** — proven |
| claude (native) | `claude-api` | direct-api | ✅ | ✅/✅ | ✅ | ✅ | reviewer/security/orchestrator (native thinking) |
| claude-code | `claude-code` | subprocess-stream | ❌ | ❌/✅ | ❌ | ✅ stream-json | frontier CLI seat — NO BeforeToolCall deny |
| bedrock | `bedrock` | direct-api | ✅ | ✅/✅ | ✅ | ✅ | claude via AWS |
| gemini | `gemini-api` | direct-api | ✅ | ✅/✅ | ✅ | ✅ | alt |
| ollama | `ollama-local` | direct-api | ✅ | ✅/✅ | ✅ | ✅ | local models (`num_ctx