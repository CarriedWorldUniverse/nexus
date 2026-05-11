# Funnel Observability Audit — Phase E Readiness

**Status:** Audit complete; interface negotiated with keel-cli; awaiting funnel-side wiring
**Date:** 2026-05-12
**Author:** Plumb
**Parent:** `2026-05-12-nexus-watch-and-observability-core.md` (the observability stack plan; this audit informs Phase E)
**Conversation thread:** chat msgs #209 / #212 / #213 (plumb ↔ keel-cli)

## 1. Why this audit exists

Phase E of the observability stack ("bridle events on broker") was framed in the parent plan as depending on "Keel's parallel work" — broker subscribing to the funnel's emit channel. Before committing to a build, plumb audited the funnel to understand what's actually flowing through it, what's being captured, and what's being discarded.

Operator's specific ask: *"honest evaluation of funnel against need."*

## 2. Findings

### 2.1 The funnel has two semantically-distinct event channels today

| Channel | Source | Carries | Today's wiring |
|---|---|---|---|
| `Config.Events EventSink` | funnel's own taxonomy (`nexus/frame/funnel/events.go`) | Lifecycle: turn.start, turn.end (with `bridle.Usage`), turn.tool_call (name + count ONLY), compact.start/end, filter.judging | Wherever Frame / agentfunnel wire it — telemetry only |
| `bridle.EventSink` passed to `Harness.RunTurn` | bridle itself (`bridle/events.go`) | Raw stream: ModelChunk, ToolCallStart (with full Args), ToolCallResult (with Result), TurnDone (with full Usage), StepBoundary | **`collectSink{}` — literal no-op. Discarded.** |

The collectSink doc-comment is explicit: *"v1 funnel doesn't act on bridle events"* (`funnel.go:803-809`).

### 2.2 Three call sites discard bridle events

| Site | Context | Today's sink |
|---|---|---|
| `nexus/frame/funnel/funnel.go:416` | `Deliberate` — the main deliberation turn | `collectSink{}` |
| `nexus/frame/funnel/funnel.go:673` | `compact` — summarization turn | `collectSink{}` |
| `nexus/frame/funnel/filter.go:274` | Cheap-judge filter — meaningfulness evaluation | `collectSink{}` |

(Filter site flagged by keel-cli during interface negotiation; plumb's initial audit missed it.)

### 2.3 The information Phase E needs already passes through the funnel

Spec promises: rich TurnBlocks with **inline tool calls** (name + full input + result), **file diffs** rendered as artifacts, **thinking text** between tool calls, cost/timing footer.

All of that lives in `bridle.Event` values that are being constructed and discarded:

- `ToolCallStart.Args` (json.RawMessage) — what `observability/artifact.go` parses for Edit/Write/MultiEdit
- `ToolCallResult.Result` (json.RawMessage) — tool output, preview-truncated by Grouper
- `ModelChunk.Text` — model reasoning between tool calls
- `TurnDone.Result.Usage` — input/output/cache tokens for footer
- `StepBoundary` — round boundaries within a turn

The Phase A Grouper (`nexus/observability/grouper.go`) consumes `bridle.Event` directly. It's downstream-ready.

## 3. Architecture paths considered

### Path 1 — Promote `Config.Events` to carry rich detail (REJECTED)

Extend the funnel's existing `EventType` taxonomy with new types: `tool.call.start` (with Args), `tool.call.result` (with Result), `text.chunk` (with Text). Wrap collectSink with a translator that converts `bridle.Event` → `funnel.Event` before emitting on `Config.Events`.

**Pros:** reuses existing infrastructure (single Config field, single subscriber model).

**Cons:**
- Duplicates bridle's event taxonomy into funnel-shaped wrappers
- Translation layer to maintain when bridle adds an event type
- Conflates two semantically-distinct concerns (lifecycle telemetry vs. observability frames)

### Path 2 — Add parallel `bridle.EventSink` (RECOMMENDED, ACCEPTED)

```go
type Config struct {
    ...
    Events            EventSink            // existing — lifecycle telemetry
    ObservabilityHook ObservabilityHook    // new — bridle-event-level observability
}

// ObservabilityHook consumes bridle's raw event stream plus boundary
// signals from the funnel. Implemented by observability.Grouper.
type ObservabilityHook interface {
    BeginTurn(turnID, label, model, provider string, triggerMsg int64)
    OnBridleEvent(ev bridle.Event)
    EndTurn()
}
```

In each `Harness.RunTurn` call site:
```go
var sink bridle.EventSink = collectSink{}
if f.cfg.ObservabilityHook != nil {
    f.cfg.ObservabilityHook.BeginTurn(turnID, "main", model, provider, triggerMsg)
    sink = MultiSink{collectSink{}, hookSinkAdapter{f.cfg.ObservabilityHook}}
    defer f.cfg.ObservabilityHook.EndTurn()
}
result, err := f.cfg.Harness.RunTurn(turnCtx, req, f.cfg.Runner, sink)
```

Where the label distinguishes which call site is wrapping ("main" / "compact" / "filter-judge").

**Pros:**
- Grouper consumes its native input directly — zero translation
- Bridle adds an event type → funnel doesn't change
- Cleanly separates Config.Events (telemetry: "what is the funnel doing?") from ObservabilityHook (rich: "what is the model doing inside this turn?")
- One new Config field; ≤30 LoC funnel-side wiring

**Cons:**
- Funnel now has a typed dependency on the observability package interface (minimal — just an interface declaration in the funnel package; observability.Grouper implements it via duck-typing).

### Path 3 — Hybrid: extend Config.Events for tool detail only (REJECTED)

Add Args + a new `EventTurnToolResult` to Config.Events for file-diff artifacts, but don't ship ModelChunk text (too noisy). Halfway house.

**Pros:** smaller scope than Path 2; still gets file diffs.

**Cons:** still does duplication-of-shape (Args + Result on funnel.Event payloads), still loses thinking text, brittle to bridle changes. If you're going to forward bridle events at all, may as well forward all of them.

## 4. Why the dual scoping is correct (not duplicate)

When Phase E wires Path 2, both `Config.Events.turn.start` AND `ObservabilityHook.BeginTurn` fire for the same logical event. This isn't redundant; they're answering different questions for different consumers:

- **Config.Events** answers: *"what is the funnel doing right now?"* Lifecycle telemetry. Subscribers: dashboard activity strip, agentfunnel outbound WS frames, anything that wants coarse "is something happening?" signal.
- **ObservabilityHook** answers: *"what is the model doing inside this turn?"* Rich observability. Subscribers: the Grouper, which builds TurnFrames for the observability stream.

Same logical event from one viewpoint; different aggregation level. Both deliberate. The interface comment makes this explicit so a future maintainer doesn't tear one out thinking it's redundant.

## 5. Concurrency check

- `Funnel.mu` (funnel.go:195) guards inbox + sessionTail + cumulativeTokens
- `Deliberate` is serialized per Funnel instance by that mutex — **no overlapping turns on one aspect**
- Inside one Deliberate call, the funnel may invoke `Harness.RunTurn` up to three times (main + compact + filter-judge). Each is a distinct turn from the Grouper's perspective — BeginTurn/EndTurn pairs fire for each, distinguished by label.
- Phase A's Grouper already has `sync.Mutex` (added in fa80719) — concurrent-safe even when the funnel calls into it from any goroutine.

## 6. What needs to change where

### In bridle (`~/Source/bridle/`)

Optional convenience helper:
```go
// MultiSink fans Emit calls out to multiple sinks. Nil entries are
// silently skipped. Order of Emit calls matches the order sinks were
// passed at construction.
type MultiSink []EventSink

func (m MultiSink) Emit(ev Event) {
    for _, s := range m {
        if s != nil { s.Emit(ev) }
    }
}
```

Three lines. Lives in `bridle/events.go` or a new `bridle/sinks.go`. Not a blocker — could live in nexus instead, but bridle is its natural home.

### In nexus (`nexus/frame/funnel/`)

Three changes:

1. **`funnel.go`** — Add `ObservabilityHook ObservabilityHook` to `Config`, plus the interface declaration. In `Deliberate` (line 416) and `compact` (line 673), wrap RunTurn with BeginTurn/EndTurn + MultiSink.

2. **`filter.go`** — Same wrapping at line 274, with label="filter-judge". This is the filter's bridle.Harness, not the funnel's, but the operator should still see what the cheap-judge looked at.

3. **`events.go`** — Add the `ObservabilityHook` interface declaration (or in a separate `observability_hook.go` for clarity). No import of `nexus/observability` needed — the interface is structurally compatible with `observability.Grouper`'s methods.

Estimated funnel changes: ~30 LoC + the interface declaration + the filter wrap.

### In nexus (`nexus/observability/`)

Minor Phase A patch:

1. **`grouper.go`** — Add `label string` parameter to `BeginTurn`. Default to "main" if empty.
2. **`types.go`** — Add `Label string` field to `TurnFrame` (json:"label,omitempty").
3. **Tests** — update grouper_test.go for the new signature; add a sub-turn-label test for label routing.

Estimated Grouper changes: ~10 LoC + tests.

### In nexus broker / Frame wiring

When constructing a funnel for a Frame (or agentfunnel for a remote aspect), pass an `ObservabilityHook` that adapts to the Grouper for that aspect:

```go
funnelCfg := funnel.Config{
    ...
    ObservabilityHook: obsHub.GrouperFor(aspectName), // Grouper satisfies ObservabilityHook
}
```

`obsHub` is the broker's `observability.Hub` from Phase B. The Grouper's existing `BeginTurn` / `OnBridleEvent` / `EndTurn` methods satisfy the new interface (after the label-arg patch).

Estimated broker / Frame wiring: 1 line per call site, 2-3 sites total.

## 7. Build sequencing

1. **(plumb)** Patch Phase A's Grouper: add `label` arg to BeginTurn + `Label` field to TurnFrame + tests
2. **(bridle)** Add `MultiSink` utility (3 lines, optional but nice)
3. **(keel)** Funnel-side wiring: ObservabilityHook interface in Config, wrap all three RunTurn call sites
4. **(plumb)** Frame / agentfunnel wiring: pass `obsHub.GrouperFor(aspect)` as Config.ObservabilityHook
5. **Smoke test:** chat to plumb → observe TurnFrames emerging on dashboard + nexus-watch with full tool detail, file diffs, thinking text

Items 1-2 can land independently. Items 3-4 want to merge in close sequence because the interface contract spans them.

## 8. Interface negotiation history

- **Plumb's initial proposal (msg #211)**: `BeginTurn(turnID, model, provider, triggerMsg int64) / OnBridleEvent / EndTurn`. Two call sites (Deliberate + compact).
- **Keel-cli's response (msg #212)**: accepted shape; flagged third call site (filter.go:274); requested sub-turn label for filter-judge differentiation.
- **Plumb's refinement (msg #213)**: added `label string` parameter; documented dual-scoping vs. Config.Events in interface comment.
- **Status:** awaiting keel-cli's final sign-off on the refined interface; then implementation can begin.

## 9. What this audit deliberately doesn't cover

- **Frame's own bridle.EventSink wiring** (Frame is the embedded keel; it constructs its own funnel via `nexus/cmd/nexus/main.go`). When Phase E lands, Frame needs the same `ObservabilityHook` wiring as agentfunnel.
- **Multi-funnel aspects** (none today, but the Hub's per-aspect Grouper model supports it cleanly).
- **Bridle-side event type additions** (e.g. partial Usage on TurnError, ModelThinking distinct from ModelChunk). Not blockers; deferred until measured need.
- **Performance budget under realistic event rates.** Grouper's snapshot-per-event emission may produce many TurnFrame copies per turn. Buffer cap is 500; turn-level (not event-level) framing. Worth measuring at Phase F.

## 10. Self-review

- **Scope check:** audit + interface negotiation; no code committed yet. Implementation is staged but gated on keel-cli's sign-off.
- **Coverage:** every funnel call site to `Harness.RunTurn` identified (3); every gap between bridle's emission and the Grouper's consumption named; both architectural paths weighed.
- **Type consistency:** ObservabilityHook signature is consistent between funnel.Config declaration, Grouper implementation (after label patch), and broker wiring.
- **Confidence:** high. The funnel is structurally ready; only the literal `collectSink{}` no-op stands between Phase A's Grouper and the rich observability the spec promises.
