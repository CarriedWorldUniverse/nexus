# JSONL Rewriter — Per-Turn Distillation Between Turns

**Status:** draft, 2026-05-10
**Author:** keel
**Scope:** claude-code-backed Frames running under nexus funnel

## Why

claude-code's session jsonl grows fast. In a 5000-record sample of keel's working session, ~10MB:

| Record kind                     | Bytes  | %     |
| ------------------------------- | ------ | ----- |
| `user[tool_result]`             | 3.77MB | 37.8% |
| `assistant[tool_use]`           | 2.12MB | 21.2% |
| `file-history-snapshot`         | 1.42MB | 14.2% |
| `attachment`                    | 1.15MB | 11.6% |
| `assistant[text]`               | 1.12MB | 11.2% |

Sub-agent (`Agent` tool) results are the densest: median 20KB, max 36KB, total ~2MB across 103 calls. Tool results (Bash/Read/Grep/Agent) are the dominant token sink.

Each `--resume` re-reads everything. Without trimming, context grows monotonically until the model's window pushes us toward auto-compaction — which is opaque, lossy, and not under our control.

## Strategy

**Distill the just-completed turn before the next turn fires.** Older turns are immutable. This preserves prefix-cache stability: only the tail changes between resumes, so claude-code's read-cache stays warm above the boundary.

We are NOT changing schema. We are NOT touching record types, uuids, parentUuid links, tool_use_ids, or stop_reason fields. We modify only the **content payload** inside specific record kinds.

## Targets (in priority order)

### 1. `tool_result` content blocks (highest ROI)

Record shape:
```jsonc
{
  "type": "user",
  "uuid": "...",
  "parentUuid": "...",
  "message": {
    "role": "user",
    "content": [{
      "type": "tool_result",
      "tool_use_id": "toolu_...",
      "content": [{ "type": "text", "text": "<distillable>" }],
      "is_error": false
    }]
  },
  "toolUseResult": { /* metadata for non-Agent tools is small; Agent tools have rich metadata */ }
}
```

Distill `message.content[i].content[j].text` when its length > 1000 bytes.

For Agent tool_results, ALSO distill `toolUseResult.content` to the same string — they duplicate the same report, both must shrink consistently. Other `toolUseResult` fields (`status`, `agentId`, `usage`, `toolStats`, etc.) are preserved untouched.

Heuristic per tool:
- **Bash**: keep first/last 200 chars + line count + exit-relevant signal. "Exit 0; 50 lines; head: ...; tail: ..."
- **Read**: keep file path + line count + a one-line summary if it can be derived. Often the next-turn assistant text already extracted the relevant bit; the raw file content is dead.
- **Grep**: keep match count + first 5 matches. Drop the rest.
- **Agent**: distill the report through a fast model (haiku) — it's already prose.
- **Write/Edit**: usually small results, pass through.
- **mcp__comms__\***: small, pass through.

Below the threshold (1000B), tool_results pass through untouched.

### 2. `assistant[text]` blocks

Record shape:
```jsonc
{
  "type": "assistant",
  "message": {
    "content": [{ "type": "text", "text": "<distillable>" }]
  }
}
```

When `text` length > 500 bytes, distill via haiku. The model's reasoning prose is the second-densest target. Keep tool_use blocks in the same record untouched.

### 3. Out of scope for v1

- `file-history-snapshot` — claude-code internal, hard to reason about safely.
- `attachment` — re-referenced unpredictably by the model; risky.
- `assistant[tool_use]` blocks — structured calls; shrinking would risk parser drift.
- `user[text]` — the operator's actual messages. Inviolable.

## Distillation prompt

Per-tool-result distiller (haiku):
> You are distilling a tool result for context compression. The original output was {tool_name} producing {N} bytes. Reduce to ≤200 bytes capturing: what was queried, the key signal in the response, and any error. No prose framing — just the dense summary.

Per-assistant-text distiller (haiku):
> You are distilling an assistant's reasoning trace for context compression. The original was {N} bytes. Reduce to ≤150 bytes capturing the conclusion or decision; drop exploration. Preserve any explicit hand-off, plan reference, or commitment to action.

## When the rewriter runs

After every funnel turn, before the next `--resume` fires:

```
funnel.runTurn() → provider.RunTurn() returns
  → rewriter.distillTurn(sessionID, turnBoundary)
  → next inbox item picked up → next turn fires (--resume)
```

`turnBoundary` is the uuid of the assistant message that ended the turn (`stop_reason != "tool_use"`). The rewriter walks back from there to the last-seen distillation marker (or the previous turn's boundary) and rewrites that span.

## Idempotence + state

Each rewritten record gets a marker:
```jsonc
"_nexus_distilled": { "at": "2026-05-10T12:00:00Z", "model": "haiku-4-5", "originalBytes": 19822 }
```

Rewriter skips records already carrying `_nexus_distilled`. This makes re-runs idempotent and lets us measure compression ratios.

The marker lives at the record root (sibling to `uuid`, `type`, etc.) — claude-code ignores unknown root fields when replaying.

## Failure handling

If the rewriter errors (haiku timeout, jsonl parse failure, write failure):
1. **Don't corrupt the jsonl.** Atomic temp-file rewrite (`.jsonl.tmp` → rename).
2. **Don't block the next turn.** Log and continue with the un-distilled jsonl — the turn still fires, context is just heavier.
3. **Track sustained failures.** If three consecutive turns fail to distill, fall back to fresh `--session-id` (option A failure shape — discussed earlier). The new session loses memory but unblocks the aspect.

## Layer 2 — session rollup (deferred to v2)

When cumulative session bytes cross a threshold (~250KB? ~500KB? configurable), insert a synthetic `compact_boundary` record matching claude-code's native compaction format, with a roll-up summary covering everything before it. Subsequent resumes will read the rollup + recent tail.

This isn't v1 — get per-turn distillation working and observed first, then layer the rollup on top once we know what falls out at the boundary.

## Implementation surface

New package: `nexus/nexus/frame/funnel/rewriter/`

```go
package rewriter

type Rewriter struct {
    Distiller    Distiller       // wraps haiku provider via bridle
    SessionPath  string          // path to .claude/projects/<id>/<session>.jsonl
    Threshold    int             // tool_result distill threshold (bytes)
}

type Distiller interface {
    DistillToolResult(ctx context.Context, tool, content string) (string, error)
    DistillAssistantText(ctx context.Context, content string) (string, error)
}

// DistillTurn rewrites records from the previous turn boundary up to (and
// including) turnBoundaryUUID. Idempotent — records with _nexus_distilled
// markers are skipped.
func (r *Rewriter) DistillTurn(ctx context.Context, turnBoundaryUUID string) (Stats, error)

type Stats struct {
    RecordsScanned    int
    RecordsRewritten  int
    BytesBefore       int
    BytesAfter        int
    DistillerErrors   int
}
```

Wired into funnel between turns:
```go
result, err := provider.RunTurn(...)
if err == nil && result.StopReason == bridle.StopReasonTurnEnd {
    go func() {
        stats, rwErr := rewriter.DistillTurn(ctx, lastAssistantUUID)
        if rwErr != nil { log.Warn(...) }
        log.Info("distilled", "before", stats.BytesBefore, "after", stats.BytesAfter)
    }()
}
```

Async — distillation overlaps with the funnel's idle time (waiting for next inbox arrival). If a new inbox item arrives mid-distillation, the funnel waits on the rewriter to finish before firing `--resume`.

## Configuration

Per-aspect `aspect.json`:
```jsonc
{
  "rewriter": {
    "enabled": true,
    "tool_result_threshold": 1000,
    "assistant_text_threshold": 500,
    "distiller_provider": "anthropic",
    "distiller_model": "claude-haiku-4-5"
  }
}
```

Default ON for claude-code-backed Frames; OFF for direct-API providers (they don't replay jsonl, distillation is moot).

## Open questions

- Concurrency with claude-code: claude-code holds the jsonl open during a turn. We MUST only rewrite when no turn is in flight. The funnel already serializes turns, so this is naturally enforced — but we need a guard (lockfile or in-process mutex) to be defensive.
- Marker storage: root-field vs comment-style preamble line in the jsonl. Root-field is cleaner; verify claude-code tolerates unknown root keys (it ignores them in our reading of the parser, but worth a smoke-test).
- Should the rewriter run on the FIRST turn (which has no prior turns to distill)? No — it's a no-op until at least one turn boundary exists. Skip silently.

## Validation

- Smoke test: aspect runs 20 turns, rewriter on. Compare jsonl bytes vs. control aspect rewriter-off. Expected: 50%+ reduction in tool_result bytes, 40%+ overall.
- Behavior test: aspect that previously asked follow-up questions referencing past tool output should still do so correctly with distilled context. Rewriter is preserving signal, not just bytes.
- Cache test: prefix-cache hit rate stays high turn-over-turn (only the just-completed-turn tail changed).

## Rollout

1. Build + unit tests (in-memory jsonl, no real haiku).
2. Wire to funnel, default OFF, manual flag enable.
3. Test on test-keel (low-stakes Frame).
4. Default ON for claude-code-backed Frames after a week of observation.
5. Layer 2 (session rollup) as separate spec once Layer 1 is steady.
