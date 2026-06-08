# nexus-watch + shared observability core

**Status:** Draft, awaiting operator review
**Date:** 2026-05-12
**Author:** Plumb
**Parents:**
  - `2026-05-10-chatgpt-mode-shape.md` (keel) — the dashboard observability spec; this plan extends its substrate to support a terminal renderer too.
  - `2026-05-11-one-to-one-observability-plan.md` (plumb) — **superseded by this doc**; that plan had the SPA-side build but put grouping logic in JS. The shared-core reframing below replaces it.

## 1. Why this exists

The operator wanted a terminal version of the observability surface — same data, different presentation, for when they're in a shell anyway and don't want to context-switch to the browser. While sketching it, the right shape emerged: **factor the observability logic into a shared Go core, and let the dashboard and terminal both render it**, instead of implementing the grouping/pairing logic twice in two languages.

This is operator's specific direction: *"we can use the same observability code (reframe for the dashboard) for both."* The dashboard SPA shrinks; new code lives in a place both clients can use.

## 2. Architecture

```
                  ┌─────────────────────────────────────────┐
                  │  nexus broker                           │
                  │   ├─ chat substrate (today)             │
                  │   └─ bridle event stream (keel's plan)  │
                  │                                          │
                  │   /api/observe/<aspect>  (new)          │
                  │     ↓ pushes grouped observability      │
                  │       frames over WS                    │
                  └──────────────┬──────────────────────────┘
                                 │
                                 │ observability frames:
                                 │   { kind: "turn", turn_id, events: [...] }
                                 │   { kind: "chat", msg: {...} }
                                 │   { kind: "presence", connected, ... }
                                 │
                ┌────────────────┴────────────────┐
                │                                  │
                ▼                                  ▼
   ┌───────────────────────┐         ┌──────────────────────────┐
   │  Dashboard SPA        │         │  nexus-watch (terminal)  │
   │  (preact)             │         │  (Go binary)             │
   │                       │         │                          │
   │  consumes frames →    │         │  consumes frames →       │
   │  HTML rendering       │         │  ANSI rendering          │
   │                       │         │                          │
   │  (no client-side      │         │  /switch, /say,          │
   │   grouping logic)     │         │  /history slash cmds     │
   └───────────────────────┘         └──────────────────────────┘
```

**Key architectural call**: the broker emits **pre-grouped observability frames**, not raw events. The grouping logic (turn boundaries, tool_use ↔ tool_result pairing, file-edit detection) lives in a Go package on the broker side. Both renderers consume identical structured frames and only render.

This collapses the work: yesterday's plan had ~600 LoC of JS doing the grouping; that becomes ~400 LoC of Go (more testable, sharable) plus thin renderers in JS (~200 LoC) and Go (~300 LoC including TTY layout).

## 3. The shared observability core

**Package:** `nexus/observability/`

### 3.1 Wire types

```go
// In nexus/observability/types.go

type Frame struct {
    Kind     FrameKind       `json:"kind"`
    Aspect   string          `json:"aspect"`
    Sequence int64           `json:"seq"`  // monotonic per-aspect
    TS       time.Time       `json:"ts"`
    Payload  json.RawMessage `json:"payload"`
}

type FrameKind string
const (
    FrameTurn     FrameKind = "turn"      // a complete or in-flight turn
    FrameChat     FrameKind = "chat"      // a chat msg in/out of this aspect
    FramePresence FrameKind = "presence"  // connect/disconnect state
)

type TurnFrame struct {
    TurnID     string         `json:"turn_id"`
    Status     TurnStatus     `json:"status"`     // "in_flight" | "complete" | "errored"
    Started    time.Time      `json:"started"`
    Ended      *time.Time     `json:"ended,omitempty"`
    TriggerMsg int64          `json:"trigger_msg,omitempty"`
    Model      string         `json:"model,omitempty"`
    Provider   string         `json:"provider,omitempty"`
    Events     []TurnEvent    `json:"events"`
    Usage      *UsageStats    `json:"usage,omitempty"`
}

type TurnEvent struct {
    Kind   TurnEventKind   `json:"kind"`   // "text" | "tool_call" | "tool_result_orphan"
    Text   string          `json:"text,omitempty"`
    Tool   *ToolCall       `json:"tool,omitempty"`
}

type ToolCall struct {
    Name        string          `json:"name"`
    Input       json.RawMessage `json:"input"`
    Result      *ToolResult     `json:"result,omitempty"`  // nil while in-flight
    Artifact    *Artifact       `json:"artifact,omitempty"` // pre-computed for Edit/Write
}

type ToolResult struct {
    Preview string `json:"preview"`
    Full    string `json:"full,omitempty"`  // present only when explicitly requested
    IsError bool   `json:"is_error"`
}

type Artifact struct {
    Kind     string `json:"kind"`     // "file_edit" | "file_write" | ...
    FilePath string `json:"file_path"`
    OldText  string `json:"old_text,omitempty"`
    NewText  string `json:"new_text,omitempty"`
}

type ChatFrame struct {
    MsgID    int64     `json:"msg_id"`
    From     string    `json:"from"`
    Content  string    `json:"content"`
    ReplyTo  int64     `json:"reply_to,omitempty"`
    Topic    string    `json:"topic,omitempty"`
    Direction string   `json:"direction"`  // "inbound" | "outbound"
}

type UsageStats struct {
    InputTokens  int           `json:"input_tokens"`
    OutputTokens int           `json:"output_tokens"`
    CacheRead    int           `json:"cache_read,omitempty"`
    Duration     time.Duration `json:"duration"`
}
```

### 3.2 Grouper

`nexus/observability/grouper.go` — the core logic. Takes the raw event streams (bridle events for the funnel half, chat events for the chat half) and produces grouped `Frame`s.

```go
type Grouper struct {
    aspect    string
    out       chan<- Frame
    inFlight  *TurnFrame              // turn currently being built
    pending   map[string]*ToolCall    // tool_use_id → tool_call awaiting result
    seq       int64
}

func (g *Grouper) OnBridleEvent(ev bridle.Event) {
    switch ev.Kind {
    case "turn_start":
        g.flushAndStartTurn(ev)
    case "tool_use":
        g.attachToolCall(ev)
    case "tool_result":
        g.pairToolResult(ev)
    case "text":
        g.appendText(ev)
    case "turn_end":
        g.completeTurn(ev)
    }
}

func (g *Grouper) OnChatDeliver(msg chat.Message, direction string) {
    // ChatFrame is independent of turn boundaries — emit immediately.
    g.emit(Frame{
        Kind: FrameChat,
        Aspect: g.aspect,
        Sequence: g.nextSeq(),
        TS: msg.CreatedAt,
        Payload: marshalChat(msg, direction),
    })
}

// Detect Edit/Write/MultiEdit tool calls; pre-compute the Artifact so
// renderers don't have to re-parse the input JSON every render.
func (g *Grouper) computeArtifact(tc *ToolCall) {
    switch tc.Name {
    case "Edit", "MultiEdit", "Write", "NotebookEdit":
        // Parse input, populate tc.Artifact
    }
}
```

The Grouper holds in-memory state (the in-flight turn, pending tool calls) and emits Frames as they become "displayable" — i.e., a turn frame is emitted on every state change so renderers can show the in-flight turn progressively.

**Testing**: pure logic, table-driven tests with synthetic event sequences → expected Frame outputs. No transport, no broker, no UI. Tested in isolation.

### 3.3 Buffer

`nexus/observability/buffer.go` — per-aspect ring buffer of recent Frames. Replaces the SPA-side `harness-stream-store` for newcomers (any client subscribing gets the buffer first, then live frames).

```go
type Buffer struct {
    cap     int                        // e.g. 500
    rings   map[string]*ring.Buffer    // per aspect
}

func (b *Buffer) Append(frame Frame) { /* ... */ }
func (b *Buffer) Tail(aspect string, sinceSeq int64) []Frame { /* ... */ }
```

Lives on the broker side. Survives client reconnects. Per-aspect cap = ~500 frames (turn-sized, not event-sized → fewer frames retain more meaningful history than the old event-buffer-of-500).

### 3.4 WS frame surface

New broker frames (additions to `nexus/frames/frames.go`):

```
subscribe.observe     { aspect: "plumb", since_seq?: 0 }
  → ack: ack contains the tail (Frames since since_seq), then live pushes
unsubscribe.observe   { aspect: "plumb" }
observe.frame         { aspect, frame }     ← server push
```

Auth: operator-only (mirrors `subscribe.chat` operator-gating in `operator_subs.go`).

## 4. The two renderers

### 4.1 Dashboard — Observe view (SPA)

`nexus/broker/static/dashboard/js/views/ObserveView.js` (new). Replaces the existing `AgentsView.js`. Subscribes via the new `subscribe.observe` frame; renders Frames in chronological order; supports `/switch` between aspects.

Because grouping is server-side, the JS is **dramatically thinner** than yesterday's plan envisioned. No `groupEventsByTurn`, no `pending` map, no artifact detection. Just:

- Component-per-frame-kind: `TurnBlock`, `ChatBubble`, `PresenceMarker`
- Subscription bookkeeping (start one per visible aspect; teardown on unmount with grace)
- Sticky-bottom scroll, seeded-vs-live divider (port from current `HarnessActivity.js`)

Yesterday's plan tasks 2-5 collapse — most of the work moves to the Go core.

### 4.2 Terminal — `nexus-watch`

`runtime/cmd/nexus-watch/` (new). Operator-side terminal binary; same `subscribe.observe` channel, ANSI rendering.

```
$ nexus-watch plumb
─── nexus-watch · @plumb · connected ───

⌁ thu 12 may, 09:14
   ┌── turn ─── triggered by #189 ── model=claude-opus-4-7
   │  💭 reading docs/X
   │  🔧 Read              docs/X.md
   │     ↳ 1.2 KB · 38 lines
   │  💭 one nit at line 42
   │  → @operator: looking at it now (sent #190)
   │  → @operator: one nit at line 42 (sent #191)
   └── 4.2s · 5 in · 253 out

▸ #192 from operator: can you fix it?

⌁ thu 12 may, 09:16
   ┌── turn ─── triggered by #192
   │  🔧 Edit              docs/X.md
   │     ↳ - one nit at line 42
   │       + one nit at line 42 → fixed
   │  → @operator: done — pushed as commit abc123 (sent #193)
   └── 1.1s · 4 in · 18 out
```

Slash commands:

- `/switch <aspect>` — change focus to a different aspect (one buffered subscription per aspect, instant switch)
- `/say <message>` — operator types a message addressed to the current aspect. Goes out as the operator identity via `chat.send`. The aspect's autonomous funnel handles it.
- `/history <N>` — scroll back N frames in the current aspect's buffer
- `/expand <tool>` — toggle expansion of a tool call (default collapsed-with-preview)
- `/diff <n>` — open the artifact for the Nth most recent file-edit tool call in `$EDITOR` for full inspection
- `/quit` — exit
- `Ctrl-C` — exit gracefully (sends `unsubscribe.observe` first)

**Sizing**: ~600 LoC of Go total — TTY input loop, ANSI rendering helpers, WS-client thin wrapper (reusing `runtime/wsclient`).

**TUI library**: starts with raw ANSI for v0.1 (cheap, portable, no dep). Upgrade to `bubbletea` if the layout demands a richer model (split panes for multi-aspect view, etc.) — not for v0.1.

## 5. Migration strategy

The dashboard's existing chat view ships fine today. The Observe surface is what changes:

| Surface | Today | After this plan |
|---|---|---|
| Dashboard `#/agents/<id>` | Stale `AgentsView` (DM panel that doesn't show #general traffic) | Subscribes to `subscribe.observe`; renders frames |
| Dashboard `#/terminal` | xterm.js wired to `/proxy/<agent>/output` (deprecated) | View deleted |
| `nexus-watch` (binary) | — | New |
| `harness-stream-store.js` | Module-scoped EventSource per agent | Deleted (server-side buffer replaces it) |
| `HarnessActivity.js` | Flat row-per-event renderer | Logic moves server-side; component becomes thin frame renderer |

## 6. Staging — what ships when

Because the bridle-event server-side work (Keel's `2026-05-10-chatgpt-mode-shape.md` §"Server-side new work") isn't done yet, this plan stages:

### v0.1 — chat-only observability

What's needed: only the chat half of the Grouper (ChatFrame frames). No TurnFrames yet.

Renderers display the inbound/outbound chat traffic for the selected aspect. No tool calls, no file diffs, no thinking. Just "what arrived in plumb's inbox, what plumb sent back, in order."

This already gives the operator real value vs. today: today's dashboard shows the aspect's outbound but mixes everyone's into #general; v0.1 gives a per-aspect filter. And it ships **today** — all wire pieces exist (`chat.deliver`, `chat.send`).

### v0.2 — full observability (depends on Keel's bridle-events frame)

What's needed:
- Broker subscribes to the funnel's event channel (Keel's spec — `subscribe.bridle_events` server side)
- Grouper consumes `bridle.event` frames + computes TurnFrames
- Renderers light up `TurnBlock`s with all the rich detail

This is the "everything" version.

### v0.3+ — operator-injected input ("/say")

Already works in v0.1 via existing `chat.send` from the operator identity. Adds no new wire — slash command implementation only.

### Deferred

- Multi-aspect split-screen view (terminal-side; either tmux/screen handles it, or a future `bubbletea` upgrade does)
- Server-side filter pushdown (e.g., "only frames where tool_use.name = 'Edit'")
- Long-term frame persistence (today: in-memory buffer only; if needed, add a `nexus.observability_frames` table later)

## 7. File structure

### New

```
nexus/observability/
  types.go           ← Frame, TurnFrame, ChatFrame, ToolCall, Artifact, UsageStats
  grouper.go         ← Grouper: raw event → Frame state machine
  grouper_test.go    ← table-driven; pure logic tests
  buffer.go          ← per-aspect ring buffer
  buffer_test.go
  artifact.go        ← Edit/Write/MultiEdit/NotebookEdit input → Artifact
  artifact_test.go

nexus/broker/
  observe.go         ← subscribe.observe / unsubscribe.observe handlers; wire Grouper into broker
  observe_test.go

runtime/cmd/nexus-watch/
  main.go            ← TTY loop, slash commands, WS subscription
  render.go          ← ANSI rendering of Frame variants
  render_test.go     ← golden-file tests on Frame → ANSI output

nexus/broker/static/dashboard/js/views/
  ObserveView.js     ← replaces AgentsView

nexus/broker/static/dashboard/js/components/
  TurnBlock.js       ← consumes TurnFrame
  ChatBubble.js      ← consumes ChatFrame (or reuses MessageBubble)
  ArtifactDiff.js    ← consumes Artifact, renders unified diff
```

### Modified

```
nexus/broker/server.go               ← register new frame handlers
nexus/frames/frames.go               ← define subscribe.observe / observe.frame Kinds
nexus/broker/static/dashboard/index.html  ← drop terminal.css + xterm assets
nexus/broker/static/dashboard/js/components/BottomBar.js  ← drop Terminal tab
nexus/broker/static/dashboard/js/app.js                   ← route #/agents → ObserveView
```

### Deleted

```
nexus/broker/static/dashboard/js/views/AgentsView.js
nexus/broker/static/dashboard/js/views/Terminal.js
nexus/broker/static/dashboard/js/components/HarnessActivity.js
nexus/broker/static/dashboard/js/harness-stream-store.js
nexus/broker/static/dashboard/css/terminal.css
nexus/broker/static/dashboard/js/vendor/xterm*.{js,css}
nexus/broker/static/dashboard/js/vendor/addon-*.js
```

## 8. Open questions

1. **Identity of operator's "/say"** — confirmed: messages sent via `nexus-watch /say` go out under the operator's identity, not the aspect's. The aspect's funnel sees them as normal addressed chat. Same model as the dashboard chat input. **Locked.**

2. **Should `nexus-watch` use a keyfile or an operator JWT?** **LOCKED: passkey-login operator JWT.** Same auth flow the dashboard already uses — operator unlocks via passkey, broker mints a session JWT. `nexus-watch` reads the JWT from a known token file (e.g. `$XDG_RUNTIME_DIR/nexus/operator-token` or `$HOME/.nexus/operator-token`). The companion login flow that populates that file (`nexus-login` or a `--login` subcommand) does the passkey handshake the same way the SPA does (existing `WebAuthn` infra in `nexus/operator/passkeys.go`). On JWT expiry: exit cleanly, operator re-logs in and re-launches — mirrors the agentfunnel supervisor-restart model.

3. **Frame sequence vs timestamps** — both. Sequence is monotonic per-aspect (for ordering + dedup on reconnect-replay). Timestamps are wall-clock for display. **Locked above.**

4. **What's the right cap on the per-aspect buffer?** 500 frames at turn-granularity probably covers a few hours of normal activity (a busy aspect has maybe a few hundred frames/day). Operator can `/history <N>` to scan; for older, the bridle session jsonl is the forensic backstop. **Tunable, default 500.**

5. **Does v0.1's chat-only observability include `text` / "thinking" content for the aspect?** No — without bridle events, we can't see the aspect's reasoning, only what it said publicly. v0.2 adds that.

6. **Are bridle events operator-only?** Yes (already in Keel's plan §"Server-side new work" — "Operator-only access. Bridle events are sensitive (tool calls, thinking)").

7. **Does the broker need to authenticate which aspects an operator can observe?** Currently every operator can see everything (operator is admin-ish). If multi-operator support lands, gate `subscribe.observe` on aspect-ownership. **Defer until needed.**

## 9. Build plan

This spec defines the architecture. A separate plan doc breaks down the tasks:

1. **Plan Phase A — observability core** (Go package only, no integration yet)
   - `nexus/observability/types.go`
   - `nexus/observability/grouper.go` + tests
   - `nexus/observability/buffer.go` + tests
   - `nexus/observability/artifact.go` + tests
   - Deliverable: green test suite. Nothing wired yet.

2. **Plan Phase B — broker integration (v0.1, chat-only)**
   - `nexus/frames/frames.go` — define subscribe.observe / observe.frame kinds
   - `nexus/broker/observe.go` — wire Grouper into chat.deliver/chat.send pipelines (just the ChatFrame half)
   - `subscribe.observe` handler returns buffer tail + live pushes
   - Deliverable: WS client can subscribe to an aspect's observability stream; gets chat traffic only

3. **Plan Phase C — `nexus-watch` v0.1**
   - `runtime/cmd/nexus-watch/main.go` — TTY loop, WS connection, slash commands
   - `runtime/cmd/nexus-watch/render.go` — ANSI for ChatFrame
   - Deliverable: operator can `nexus-watch plumb`, see chat traffic, `/switch`, `/say`

4. **Plan Phase D — Dashboard ObserveView v0.1**
   - Delete AgentsView, Terminal, HarnessActivity, harness-stream-store
   - New ObserveView consuming subscribe.observe (chat traffic only)
   - Deliverable: dashboard #/agents/<id> shows the same chat-only observability

5. **Plan Phase E — bridle events on broker (depends on Keel's parallel work)**
   - Broker subscribes to funnel's emit channel
   - Grouper consumes bridle events → TurnFrames
   - Deliverable: both renderers light up rich turn detail

6. **Plan Phase F — Artifact + TurnBlock rendering in both renderers**
   - ANSI side: diff rendering, tool-call expansion (slash command)
   - HTML side: TurnBlock, ToolCall, ArtifactDiff components
   - Deliverable: feature-complete v0.2

7. **Plan Phase G — Polish**
   - Smoke tests both renderers
   - Mobile sanity for SPA (the touch-up the existing chat view got)
   - `nexus-watch` color profile + `--no-color` flag for piping
   - Documentation: operator runbook

A through D ship today's value (chat-only observability) and unblock the operator's "I want to watch from terminal" need. E through G complete the picture when Keel's bridle-events work lands.

## 10. Self-review

- **Spec coverage**: operator's framing ("same observability code for both") is the architectural backbone (§2-3). Terminal binary spec'd (§4.2). Staging (§6) handles the unshipped bridle dependency without blocking v0.1.
- **No placeholders**: types are concrete (§3.1). Slash command surface is concrete (§4.2). File structure is concrete (§7). Build phases are concrete (§9).
- **Scope check**: 7 phases, 1-2 days each (the core is the biggest, the renderers are thin). Phases A-D ship today's value. E-G are conditional on Keel.
- **Type consistency**: `Frame` / `TurnFrame` / `ChatFrame` / `ToolCall` / `Artifact` field names used consistently across §3 types, §4.1 renderer references, §6 v0.1 vs v0.2 split.
- **What's NOT here**: the per-task implementation plan (this is the design doc, that's the next doc). And the answer to whether `nexus-watch` should grow read/write parity with the dashboard chat input — for now, `/say` is the minimal operator-injection; if it grows reactions, replies, etc., that's a v0.3 concern.
