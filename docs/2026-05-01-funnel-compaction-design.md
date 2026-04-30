# Funnel-controlled context compaction

**Date:** 2026-05-01  
**Author:** anvil  
**Status:** Design — locked before §6.5 P6 build starts  
**Tracking:** #102  
**Scope:** `nexus/frame/funnel/` — funnel-side only. No bridle changes.

---

## 1. Problem

Claude Code's subprocess-stream provider (claudecode) auto-compacts context when the session
approaches its token limit (~191k empirically on a 200k context window). Auto-compaction:

- Takes 2+ minutes silently — the agent appears hung.
- Fires at the CLI's discretion, not the funnel's — the funnel cannot predict or react to it.
- Resets context to a CLI-generated summary the funnel had no input into.

For direct-api providers (claude, openai, ollama), context is managed by `SessionTail` — the
funnel already owns this. Compaction is a SessionTail management policy.

---

## 2. Empirical basis

From inspection of `0f61d903-af56-5172-99d5-6801fa534b08.jsonl` (anvil session, 2026-04-30):

The CLI writes **two records** when auto-compaction fires:

**Record 1 — `compact_boundary` system event:**
```json
{
  "type": "system",
  "subtype": "compact_boundary",
  "content": "Conversation compacted",
  "isMeta": false,
  "compactMetadata": {
    "trigger": "auto",
    "preTokens": 191227,
    "postTokens": 14011,
    "durationMs": 133304,
    "preCompactDiscoveredTools": ["TodoWrite", "mcp__comms__read_chat_thread"]
  }
}
```

**Record 2 — summary injection (user message):**
```json
{
  "type": "user",
  "isCompactSummary": true,
  "isVisibleInTranscriptOnly": true,
  "message": {
    "role": "user",
    "content": "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\n..."
  }
}
```

Key observations:
- `trigger: "auto"` — no CLI flag exists to prevent or threshold auto-compaction.
- `durationMs: 133304` — 2m13s of silence while the CLI calls the API to summarize.
- `preTokens: 191227` on a 200k context model — the CLI fires at ~96% fill.
- The two-record shape is the format the CLI uses when resuming from a compacted session.
- `isCompactSummary: true` and `isVisibleInTranscriptOnly: true` are the distinguishing flags.

---

## 3. Design

### 3.1 Owner

The **funnel** owns compaction. bridle stays context-agnostic — it runs one turn and reports
usage. The funnel tracks cumulative usage across turns and decides when to compact.

No bridle API changes are required. `TurnResult.Usage` already exposes per-turn token counts.

### 3.2 CompactionPolicy

```go
// CompactionPolicy controls when and how the funnel rolls context.
type CompactionPolicy struct {
    // ThresholdTokens: cumulative input+output tokens before a compaction turn is
    // triggered. 0 = disabled (context rolls never; use for aspects with very short
    // turn rhythms where compaction is never needed).
    // Default: 150_000 (conservative headroom below the ~191k auto-compact trigger
    // on a 200k context model).
    ThresholdTokens int

    // SummarizationModel: model used for the cheap summarization turn.
    // Should be a fast/cheap model (e.g. haiku-4-5), not the aspect's full model.
    // Defaults to the aspect's configured model if empty.
    SummarizationModel string

    // MaxSummaryTokens: soft cap on the summary output. The summarization prompt
    // instructs the model to stay within this. Default: 4_000.
    MaxSummaryTokens int
}
```

This lives in the funnel's aspect config, not in bridle. Different aspects can tune it:
- `context_mode: global` aspects (cross-thread state accumulation) → higher threshold
- `context_mode: thread` aspects (isolated per thread) → lower threshold is fine; each
  thread is independent, so rolling context sooner costs nothing and cuts per-turn expense

### 3.3 Trigger condition

At the start of each `RunTurn` call, the funnel checks:

```
if policy.ThresholdTokens > 0 && cumulativeTokens >= policy.ThresholdTokens {
    runCompactionTurn(...)
}
```

`cumulativeTokens` is `sum(TurnResult.Usage.InputTokens + OutputTokens)` across all turns
in the current session window. It resets to 0 after a compaction.

### 3.4 Compaction turn

A compaction turn is a cheap, tool-free turn whose sole job is to summarize the current
`SessionTail`:

```go
func runCompactionTurn(ctx context.Context, h *bridle.Harness, tail []bridle.SessionEvent,
    policy CompactionPolicy) (string, error) {

    req := bridle.TurnRequest{
        AspectID:     aspectID,
        SystemPrompt: "You are a summarization assistant. Summarize the conversation history concisely.",
        SessionTail:  tail,
        UserMessage:  compactionPrompt(policy.MaxSummaryTokens),
        Tools:        nil, // no tools
        Model:        policy.SummarizationModel,
        MaxSteps:     1,
    }
    result, err := h.RunTurn(ctx, req, noopRunner, noopSink)
    if err != nil {
        return "", err
    }
    return result.FinalText, nil
}
```

The summarization prompt:

```
Summarize the conversation history above into a compact briefing. Cover:
- What was being worked on and why
- Key decisions made and their rationale  
- Current state (what's done, what's in progress, what's blocked)
- Any open questions or next steps

Target: under {{MaxSummaryTokens}} tokens. Write in third person. Be specific about
file paths, function names, and identifiers that a future turn will need to reference.
```

### 3.5 Session roll

After the summarization turn completes:

1. **Write compaction records** into the session JSONL, mirroring the CLI's two-record shape:
   - `compact_boundary` system event with `trigger: "funnel"`, `preTokens`, `postTokens: len(summary)/4`
   - `isCompactSummary: true` user message with the summary text

2. **Reset `SessionTail`** to a single entry:
   ```go
   newTail := []bridle.SessionEvent{{
       Role:    bridle.RoleUser,
       Content: "This session is being continued from a previous conversation. " +
                "The summary below covers the earlier portion.\n\n" + summary,
   }}
   ```

3. **Resume** from the same session ID (subprocess-stream) or continue with the new tail
   (direct-api). No new session needed — the compaction record is in the JSONL and the
   CLI's `--resume` picks up from the compact_boundary forward.

4. **Reset `cumulativeTokens` = 0.**

### 3.6 Subprocess-stream specifics

For `claudecode` (subprocess-stream), the funnel must ensure the CLI never auto-compacts.
The strategy: **stay below the auto-compact threshold** via the funnel threshold. Since the
funnel fires at 150k and the CLI fires at ~191k, a healthy gap exists.

If the gap is ever threatened (e.g. a single very large turn pushes past 150k without
triggering), the funnel should detect post-turn that it crossed the threshold and compact
before the next turn, not during.

There is no CLI flag to disable auto-compaction — this was confirmed empirically. The only
lever is token volume.

### 3.7 Direct-api specifics

For `direct-api` providers (claude, openai, ollama), the funnel owns `SessionTail` entirely
and the model never sees a context window boundary. Compaction is pure `SessionTail` trimming:
replace the tail with the summary entry and continue. No JSONL writing needed — the funnel's
session store is the source of truth, not the CLI's on-disk file.

---

## 4. Cost model

A compaction event costs approximately:
- **Input:** `cumulativeTokens` (the session tail being summarized)  
- **Output:** ~2k–4k tokens (the summary)

Against a turn that would otherwise transfer the full 150k+ context, compaction is cheap.
After compaction, the next N turns each transfer only the ~4k summary instead of 150k — the
savings compound until the context fills again.

At Haiku pricing, compaction cost is negligible relative to what it prevents.

---

## 5. Integration point in §6.5 P6

The deliberation loop in `nexus/frame/funnel/` should:

1. Accept a `CompactionPolicy` in its config struct.
2. Maintain a `cumulativeTokens int` field across turns.
3. Check the threshold at the top of each turn loop iteration.
4. Call `runCompactionTurn` and roll the session tail when triggered.

The deliberation loop already accumulates `SessionTail` across turns (per the §6.5 build plan).
Compaction is a SessionTail roll — it fits naturally in the same loop.

---

## 6. Non-goals

- **Preventing all auto-compact.** We stay below the threshold; we don't guarantee zero
  auto-compacts (a pathological single turn could still trigger one). If the CLI auto-compacts
  despite the funnel's efforts, the session continues — we just lost control of that compaction's
  summary quality. Acceptable.
- **Mid-turn compaction.** Compaction fires between turns, not within one. A running turn
  is not interrupted.
- **Configurable summarization quality.** The summarization prompt is fixed. Tuning it is a
  follow-up once there's operational data on summary quality.
- **Backfilling old sessions.** Applies to new sessions running against a funnel with
  CompactionPolicy configured. Existing sessions continue unaffected.
