# Feed as Trust Surface — Design Spec

**Date:** 2026-05-23
**Status:** Draft — pending operator review

## Goal

Redesign the dashboard Feed view so the operator can trust the agent
network is alive and working without having to read the message log —
and when they do drop in, the message log is legible, the relevant
agents auto-receive their replies, and they can move between active
threads in one motion.

## Why

Current FeedView (`nexus/broker/static/dashboard/js/views/FeedView.js`,
550 lines) is structured as a thread-list-with-drill-down: one-line
summary rows that the operator clicks to expand into a single accordion
panel. That model breaks down for the operator's actual workflow:

1. **The operator kicks off discussions; agents leap ahead faster than
   the operator can follow.** Three threads can be live simultaneously
   with multiple agents racing in each one. Single-thread accordion is
   the wrong shape — switching between threads requires collapsing one
   to expand another, and the operator loses the live view of the one
   they collapse.

2. **Trust signal is scattered.** Whether an aspect is online lives in
   Status; whether it's mid-deliberation is buried in Observe; what
   it's saying is in Feed. The operator wants all three in one glance:
   "agent X is present and thinking, in thread Y." Today they have to
   tab between three views.

3. **Sender-based filters are meaningless.** The role-hint chips
   (planner / worker / operator / casual) filter by who's speaking,
   not what they're talking about. The operator wants "show me the
   threads about X," not "show me planner-dispatch messages."

4. **Operator-vs-agent legibility is poor.** Operator messages and
   agent messages render identically in MessageBubble. In a screen
   of text it's hard to find "what did I last say."

5. **Replies don't reach the participants.** `recipients.go` (audited
   clean per code, but the design is the bug) routes a thread reply
   only to the direct parent author plus explicit `@`-mentions —
   *not* to all aspects active in the thread. The operator has to
   remember to `@`-tag everyone every time, or agents that joined
   the thread silently miss subsequent replies.

The reframe: **Feed is a trust surface, not a comms log.** The
messages are there if the operator wants them, but the primary job is
ambient situational awareness so the operator can leave the dashboard
in a small window or background tab and *trust* the agents are
working, glancing at it only when something needs attention.

## Out of scope

- **Multi-pane / TweetDeck layout** ("Shape B"). Considered and
  rejected in favour of the sidebar pattern below.
- **Wholesale transplant of agora UX.** Agora is a 1:1 TUI for talking
  to a single aspect; Feed is the many-to-many room. Don't conflate.
- **Configuration UI for systems and agents.** Separate workstream;
  flagged for follow-up but not part of this spec.
- **Topic derivation / LLM clustering of threads.** Threads already
  ARE the topics in this model — each thread is one discussion the
  operator or an agent started. No need for derived labels.
- **Backend changes beyond routing.** The observability frames,
  presence events, and per-aspect activity signals already flow on
  the wire; this spec is mostly UI wiring + one recipients.go change.

## The model

- **Threads are topics.** Each thread = one discussion. The operator's
  unit of attention is the thread, not the message.
- **The sidebar is the trust surface.** Threads listed with activity
  dots per thinking aspect. Glanceable from across the room. Three
  threads with green dots = system alive, even when no thread is
  focused.
- **The main area is the focused thread.** One thread visible at a
  time (split-view a possible follow-up; not v1). Sticky per-thread
  strip at the top shows who's present in this thread + who's
  thinking. Messages flow beneath. Operator's own messages are
  visually distinct.
- **Replies route to thread participants.** Slack/Teams semantics:
  reply in a thread → every aspect that has posted in this thread
  receives it (plus explicit `@`-mentions).

## Architecture

### Layout (Shape A)

```
┌─────────────────┬──────────────────────────────────────────┐
│ Thread sidebar  │ Focused thread                           │
│                 │ ┌──────────────────────────────────────┐ │
│ ● thread-a      │ │ Sticky presence strip                │ │
│   2 dots        │ │  [aspect-1●] [aspect-2●thinking…]    │ │
│                 │ ├──────────────────────────────────────┤ │
│ ● thread-b      │ │ Messages (scrolls)                   │ │
│   1 dot         │ │   — since you left —                 │ │
│                 │ │   agent: …                           │ │
│ ○ thread-c      │ │              operator: … (right)     │ │
│   idle          │ │   agent: …                           │ │
│                 │ ├──────────────────────────────────────┤ │
│                 │ │ ChatInput (composer)                 │ │
│                 │ └──────────────────────────────────────┘ │
└─────────────────┴──────────────────────────────────────────┘
```

### Sidebar

- **Position:** left side, ~240px wide. [Open: L vs R — L is
  conventional; pick before plan.]
- **Each row:** thread title (root preview, truncated), unread count,
  activity dots column.
- **Activity dots:** one dot per aspect currently participating in
  the thread. Dot states:
  - filled green = aspect is present (connected) and idle
  - animated/pulsing green = aspect is mid-turn (TurnStart received,
    no TurnDone yet)
  - filled yellow = aspect is mid-tool-call (ToolCallStart, no
    ToolCallResult)
  - grey = aspect was a participant but is currently offline
- **Sort order:** active first (any aspect thinking), then by most
  recent message.
- **Selection:** click → focused thread changes. Selected row
  highlighted.
- **Keyboard:** `j` / `k` or arrow keys move selection; `enter` no-op
  (already focused on selection). `cmd-K` palette deferred to v2.
- **Persisted state:** which thread is focused (URL hash + localStorage
  fallback); collapsed/expanded state of sidebar itself (localStorage).

### Focused thread

- **Sticky presence strip** at the top. `position: sticky; top: 0`
  inside the scroll container. Per-aspect pill showing:
  name, presence dot (same vocabulary as sidebar dots), tool name
  when mid-tool-call.
- **Messages** scroll beneath the strip. Autoscroll-on-bottom: when
  scroll position is at the bottom, new messages keep the viewport
  pinned to bottom. When scrolled up, new messages do NOT yank.
  Standard Slack/Discord behaviour.
- **Since-you-left divider** — horizontal rule with timestamp,
  rendered above the first message whose `id` > the per-thread
  `lastSeen` value persisted in localStorage. Updated when the thread
  becomes focused (visit = mark as read).
- **Composer** at the bottom — existing ChatInput, no change.

### Operator-vs-agent visual

- **Operator messages:** right-aligned, indented from the right,
  distinct background (accent colour), "you" label.
- **Agent messages:** left-aligned, full-width, sender chip with
  per-aspect colour (existing `agentColors` signal).
- **System messages** (joins, leaves, errors): centered, muted,
  smaller font.

### Data wiring

- **Presence + thinking:** subscribe to broker observability frames.
  Observability hub (`nexus/observability/hub.go`) per-aspect Grouper
  already emits PresenceFrame on connect/disconnect, plus bridle
  events (TurnStart, TurnDone, ToolCallStart, ToolCallResult,
  StepBoundary). UI maintains a per-aspect activity map keyed by
  aspect name; per-thread participant list is derived from
  `Thread.participants`.
- **Thread list:** existing `models/threads.js` registry + initial
  `/general` fetch. Sidebar subscribes to the registry; new threads
  prepend, dead ones (no activity for N hours) age out of the
  active list. [Open: aging threshold.]
- **Routing:** see Backend below.

### Backend changes

**One file changes:** `nexus/broker/recipients.go`.

New lookup on `RecipientPolicy`:

```go
type ThreadParticipantsLookup func(threadRootMsgID int64) ([]string, error)
```

backed at the broker by:

```sql
SELECT DISTINCT from_agent FROM chat_messages WHERE thread_root_msg_id = ?
```

Index `idx_chat_thread_root_msg_id` already exists (per `schema.go`
post-migration indexes — created 2026-05-XX with #226).

`Compute()` change: when `replyTo > 0`, look up the thread root for
that message; pull thread participants; union into the recipient set
(modulo self-exclude). Direct parent author rule collapses into the
participants rule (parent is always a participant). `@`-mentions
still add named aspects not in the thread. `@all` still overrides
with full broadcast.

**Recency window — open question.** Should an aspect that posted
into a thread weeks ago and hasn't been heard from since still
receive new replies in that thread? Two options:

- **Accept it:** thread membership is permanent; the agent decides
  whether to act on each reply (most can ignore irrelevant ones).
- **Cap by recency:** only aspects who posted in the last N hours
  (e.g., 24h) are auto-included. Older participants drop off.
  Re-engagement is by `@`-mention or by posting again.

Decision needed before plan. My read: start with "accept it" since
it's the simpler model and matches Slack/Teams; add recency cap as a
follow-up only if feedback-loop pressure becomes a real problem.

**Tests:** the existing 12-case recipient suite stays; add ~4 new
cases:
- reply with 3 thread participants → all 3 receive (modulo sender)
- reply with thread participants + extra `@mention` → union
- reply with `@all` → still broadcasts (overrides)
- empty thread participants (stub returns nil) → fallback to current
  parent-author-only behaviour (graceful degradation)

### State persistence

- **localStorage keys** (versioned with `v1` suffix for schema bump
  safety):
  - `nexus.feed.sidebarCollapsed.v1` — bool
  - `nexus.feed.focusedThread.v1` — number (root msg id); URL hash
    wins if present
  - `nexus.feed.lastSeen.v1` — `{ [rootId]: lastMsgId }`
  - `nexus.feed.filters.v1` — currently roleFilter + mentionsMe; these
    may be removed entirely once thread-based view replaces role
    filter
- **Module-scoped signals** for in-session navigation (lost on reload
  but survive view switches): focused-thread, sidebar selection.

### What survives across view changes vs reload

| State                | Across nav | Across reload |
|----------------------|------------|---------------|
| Focused thread       | URL hash   | URL hash + LS |
| Sidebar collapse     | signal     | LS            |
| Per-thread lastSeen  | LS         | LS            |
| Per-thread scroll    | signal     | not persisted |
| Thread registry      | always     | rebuilt       |
| Activity map         | always     | rebuilt       |

## Implementation sequence

Five PRs, each independently shippable and each delivers a felt
improvement:

1. **Backend routing** (`recipients.go` + tests). Smallest, isolated,
   immediately fixes "agents don't hear my reply." No UI change.
2. **Operator-vs-agent visual** (MessageBubble + CSS). Smallest
   visible win. Lands legibility regardless of layout.
3. **Sticky per-thread presence strip** (new component, observability
   subscription wiring). Lands the trust signal *inside* the current
   accordion structure — sidebar comes next.
4. **Sidebar + keyboard nav** (layout restructure). The biggest
   change. By this point operator has felt #2 and #3 and can judge
   sidebar layout against real use.
5. **State persistence + autoscroll + since-you-left divider**
   (localStorage + signal lifting + scroll handling). Polish layer
   on top of the new structure.

Each PR ships behind no flags — single-deploy operator-only
environment, no need for gradual rollout.

## Open questions (must resolve before plan)

1. **Sidebar position** — left or right? (Recommend left.)
2. **Single focused thread or split-view option in v1?** (Recommend
   single; split is a v2 if it turns out to be needed.)
3. **Thread aging threshold** — when does a quiet thread drop out of
   the "active" list? 24h? 7d? Never? (Recommend 7d; tunable.)
4. **Thread-participants recency window** for routing — accept all
   historical participants or cap by recency? (Recommend accept all
   to start, add cap only if feedback-loop pressure shows up.)
5. **Sidebar width** — fixed 240px or resizable? (Recommend fixed for
   v1; resizable adds complexity that may not earn its keep.)
6. **What happens to existing role-hint and mentions-me filters?**
   Drop entirely, or keep as additional dimension? (Recommend drop —
   sidebar list + activity dots make them redundant.)
