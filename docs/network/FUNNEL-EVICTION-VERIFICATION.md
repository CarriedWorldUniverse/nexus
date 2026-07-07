# Funnel v2 Section 2 Tool-Result Eviction — Verification Report

**Evaluation:** EVAL E1
**Date:** 2026-07-07
**Ticket:** NET-65
**Implementation:** PR #422 (commit bb5f584)

## Implementation Summary

Funnel v2 §2 tool-result eviction was implemented in PR #422 and is present on main. This document verifies the implementation against the design specification in `docs/network/FUNNEL-V2-DESIGN.md` section 2.

## Requirements vs Implementation

| Requirement | Implementation Location | Status |
|-------------|------------------------|--------|
| Single tool result > ~20k tokens → workspace file | `evictOversizeResults()` called from `commitTurnState()` | ✅ |
| 10-line preview stub in-window | `writeEvictedResult()` generates preview with `truncateEvictionPreviewLine()` | ✅ |
| Context-pressure sweep at ~85% of window | `sweepContextPressure()` triggered by `sawContextBudgetWarning` | ✅ |
| Sweep via bridle ContextPolicy PromptBudget/ContextBudgetWarning | `ContextPolicy.PromptBudget` wired to `sweepBudgetTokens()` in `buildTurnRequest()` | ✅ |
| Sweeps BATCHED, never per-step | Both passes run once per turn from `commitTurnState()` | ✅ |
| Ordered vs existing compact | Eviction runs before `popHeadForTurn()` which snapshots for `maybeCompact()` | ✅ |
| Reuse estimateContextTokens | Used for both result threshold and sweep budget checks | ✅ |
| Env-gate: FUNNEL_WORKSPACE_EVICT=1 | Resolved in `resolveWorkspaceEviction()` | ✅ |
| FUNNEL_EVICT_RESULT_TOKENS default 20000 | `defaultEvictResultTokens = 20_000` | ✅ |
| FUNNEL_CTX_SWEEP_PCT default 85 | `defaultCtxSweepPercent = 85` | ✅ |
| Workspace directory: AspectHome/.funnel-workspace | `workspaceDir()` uses `Config.WorkspaceDir` or fallback | ✅ |
| Owner-only file permissions (0o600) | `os.WriteFile(..., 0o600)` in `writeEvictedResult()` | ✅ |

## Test Coverage

All tests in `workspace_eviction_test.go` pass:

1. **TestWorkspaceEviction_OversizeResultWrittenToFile**
   - Verifies oversize results are evicted to workspace file
   - Verifies in-window stub contains preview
   - Confirms workspace file contains original content

2. **TestWorkspaceEviction_DisabledByDefault**
   - Verifies eviction is off when FUNNEL_WORKSPACE_EVICT is unset
   - Confirms no workspace files are created when disabled

3. **TestWorkspaceEviction_ContextPressureSweep_OldestFirstBatched**
   - Verifies sweep triggers when budget exceeded
   - Confirms oldest-first eviction order
   - Proves batched behavior (stops once budget satisfied)

4. **TestCommitTurnState_EvictionOrderedBeforeCompaction**
   - Verifies eviction pointers survive compaction rotate
   - Confirms ordering: eviction → compaction → next turn

## Race Safety

Implementation uses concurrency-safe primitives:
- `evictionSeq atomic.Int64` - workspace file numbering
- `contextBudgetSink.saw atomic.Bool` - ContextBudgetWarning flag
- Minimal lock-held sections for sessionTail mutation
- Disk I/O outside lock (write in `writeEvictedResult()`, swap in `evictSessionTailEntry()`)

## Audit Results (from Design Doc)

| Source | Verdict | Note |
|---|---|---|
| `composeSystemPrompt` | deterministic | No timestamps, no map iteration |
| openai provider request assembly | deterministic | Serializes pre-built list |
| MCP tool listing → `mergeToolSurface` | Fixed | PR #422 doesn't touch this; was fixed earlier in bridle |
| `hookAdditionalContext` | Fixed | Tail-injection only, no system prompt mutation |

## Design Doc Open Questions — Resolved

### Does bridle expose an eviction seam?
**Answer:** No, and none needed. Funnel owns `sessionTail` directly. Eviction is funnel-local via `commitTurnState()`.

### Tokeniser for thresholds?
**Answer:** Reuse `estimateContextTokens()` (chars/4 heuristic). Consistent with existing telemetry estimate.

### Prefix stability in bridle's own framing?
**Answer:** Distiller path only affects claude-code session jsonl. Eviction doesn't interact with it. Compaction and eviction ordered via `commitTurnState()` placement.

## Conclusion

PR #422's implementation is complete and correct per the design specification. All requirements are met, tests pass, and the code is already merged on main.

**No additional work required.** This evaluation confirms the implementation is production-ready.