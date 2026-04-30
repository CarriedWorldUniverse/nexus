# §6.5 Frame Harness — STOP Decisions

**Date:** 2026-05-01
**Status:** Decisions locked (operator #8508 — 2026-05-01)
**Scope:** Two architectural calls flagged STOP by the §6.5 planner; both block §6.5 build start (#78).

---

## Decision 1 (#79) — Admin surface shape

### Context

The frame-role spec (§3.3) says the Frame "holds admin endpoints — `/nexus/admin/*` (rewind, compact, shutdown) are operations on the Nexus, which is the Frame's own body." Today's broker exposes only `/api/aspects` and `/health` over HTTP plus `/connect` over WebSocket; there is no `/api/admin/*` namespace.

When the Frame is embedded as a global-context aspect inside the Nexus process, admin operations need a transport. Two shapes are on the table.

### Options

**A — REST-only.** All admin endpoints (`rewind`, `compact`, `shutdown`, `roster`, `dispatch-status`) are HTTP request/response. Dashboard hits them on user action.
- Pros: simplest. No state-syncing channel. Each call is a discrete transaction with a clear request/response/error shape. Easy to call from operator scripts (`curl` works). Easy to authn/authz per call.
- Cons: no live server-push. The dashboard polls `/api/aspects`, `/api/dispatch-status` etc. for state changes. Admin events (an alarm firing, a hand failing, a registration arriving) reach the operator via the existing chat WS — no parallel admin event stream.

**B — REST + WS.** REST for admin actions, WS for admin events (a separate `/admin/events` channel parallel to the existing chat WS).
- Pros: live admin event stream. Dashboard reflects state changes instantly without polling. Surfaces network-level events the spec (§3.3) says the Frame manages.
- Cons: two channels. State-sync between them. Auth twice. The chat WS already exists for chat events; an "admin event stream" feels like the same thing in a different costume.

### Lean

**Option A — REST-only.** Two reasons:

1. **The chat WS is already the admin event stream by another name.** Spec §3.3 says the Frame "decides what reaches the operator's awareness vs. what stays in logs." The mechanism for "reaching the operator's awareness" is *the Frame posting to chat* — that's the whole point of the Frame having a chat handle. A registration arrives → Frame posts "anvil registered" to chat. A hand fails → Frame posts the failure. An alarm fires → Frame posts the alarm. There is no separate admin event stream because the chat IS the admin event stream, viewed from the operator's seat.
2. **REST is auditable; WS is not (or not as easily).** Every admin action becomes a request log line. With WS we'd be inventing event-log discipline from scratch.

What we'd build under A:
- `POST /api/admin/rewind`, `/compact`, `/shutdown` — admin-flag-gated (per Drift C/D auth).
- `GET /api/admin/dispatch-status` — current pool occupancy, queue depth.
- `GET /api/admin/roster` — extended `/api/aspects` with admin metadata.
- Dashboard polls these on the relevant views; chat WS continues to surface events the Frame deems operator-relevant.

What we'd punt: any "live network telemetry" view in the dashboard. If that need surfaces, we add a `/admin/events` WS as an extension — but only when there's a concrete view that needs it.

### Open carve-outs

- **Long-running admin actions** (e.g., rewind across many threads, compact across a large session set). REST-only means we either return after kicking off (operation-id pattern, poll for status) or block. Pattern: kick off → `202 Accepted` with operation ID → `GET /api/admin/op/<id>` for status. Fine for v1.
- **Streaming logs.** If we need to tail Nexus logs from the dashboard, that's WS territory. Defer until needed; not part of the Frame decision.

---

## Decision 2 (#80) — Frame chat routing

### Context

The Frame has a chat handle (`@<frame_name>`, default `frame`). When the Frame runs as an embedded global-context aspect, the broker routes some chat messages to it. The question: **which messages?**

This matters because the Frame is doing model-driven deliberation on every message it receives. Routing too much wastes tokens and time; routing too little misses operator broadcasts.

### Options

**A — All frames (everything reaches the Frame).** The Frame subscribes to the entire chat firehose. Triage decides what to engage with.
- Pros: spec §3.3 says "Operator messages addressed to no specific aspect land on the Frame, because the Frame is who the operator is talking to when not directing." Treating the firehose uniformly subsumes that case. Maximum context for the Frame's deliberation — it sees what's happening across the network.
- Cons: every message triggers a triage turn. Cost scales linearly with chat volume. Most chat traffic is aspect-to-aspect and irrelevant to the Frame; triage has to dismiss it constantly.

**B — @-mentions + un-addressed operator broadcasts only.** Frame receives messages where (a) it's @-mentioned, (b) operator broadcast has no @-address, or (c) it's a reply to a message the Frame posted.
- Pros: triage runs on a much smaller set. Cost proportional to actual Frame-relevant traffic. Matches how aspects already filter today (per `agents/CLAUDE.md` chat discipline rules).
- Cons: Frame loses passive awareness of the network. "Surface network-level events" (spec §3.3) becomes a separate code path — registration/failure/alarm hooks fire in the Nexus process directly, not through chat-routing. Frame can't react to a peer-aspect conversation it wasn't @-mentioned in, even if it should.

### Lean

**Option B — @-mentions + un-addressed operator broadcasts + replies-to-Frame.** Three reasons:

1. **The Frame is an aspect, and aspects don't tap the firehose.** `agents/CLAUDE.md` already codifies: "Only respond to messages that @mention you, are replies to your messages, or are in a thread you're already participating in." Making the Frame an exception is special-pleading — the Frame should follow the same chat discipline as everyone else, especially because it's the most expensive aspect to wake up (global context, every turn).
2. **Network events are a different surface from chat.** Registrations, hand failures, alarm fires — these arrive through Nexus internals, not through chat. The Frame's process embeds those event sources directly (in-process method calls per spec §3.4). The Frame decides which to *post to chat for the operator*. "Surface network-level events" doesn't require "subscribe to the chat firehose"; it requires "have a hook into the event sources." Those hooks already exist in the Nexus process.
3. **Operator broadcasts (no @-address) still reach the Frame.** Per spec §3.3 the Frame is the default addressee when the operator isn't directing. Broker routes un-addressed operator messages to the Frame, same as un-addressed messages today land in the global topic. That handles the "talking to the network at large" case without the firehose cost.

What we'd build under B (refined per operator #8508):

The Frame receives a chat message iff **one** of:
1. **The message is un-addressed** (no `@<aspect>` mention in content). All un-addressed traffic reaches the Frame regardless of `from`. Rationale: the Frame may need to route un-addressed traffic — to surface to operator, fan out to a topic, or take action — even when it's not the originator. This is the routing-awareness role.
2. **The Frame is a participant** in the addressed traffic, where participant = one of:
   - `@<frame_name>` is in the content (Frame is the addressee)
   - `from = <frame_name>` (Frame is the addressor — though this is a no-op delivery; the Frame already knows about its own posts)
   - `reply_to` references a message the Frame authored
   - `thread_id` / `topic` matches a thread the Frame has previously posted in (Frame is a thread member)

So: **un-addressed traffic always reaches the Frame** (routing layer). **Addressed traffic only reaches the Frame when the Frame is a participant** (same chat discipline as any aspect).

Network-event hooks (registration, hand failure, alarm fire) go directly to the Frame's in-process event handler, not through chat. Frame decides what to surface as chat posts.

What B does NOT do: route addressed aspect-to-aspect messages to the Frame when the Frame isn't already in the thread. The Frame doesn't tap the addressed-chat firehose; the *operator* does, via the dashboard.

### Open carve-outs

- **"Frame should know about X-class events even if they happen between aspects."** If we discover a real case (e.g., the Frame should detect when two aspects are stuck in a loop), that's a separate signal — a watcher mechanism — not a justification for firehose routing. Add a watcher when the case appears.
- **Topic subscriptions.** Can the Frame opt into watching a specific topic (e.g., a `harness-naming` style coordination thread)? Yes — same as any aspect — by participating in the thread, which puts subsequent posts in scope per the rules above.

---

## Summary

| # | Decision | Lean | Why |
|---|---|---|---|
| 79 | Admin surface | **REST-only** | Chat WS already surfaces admin events through the Frame; REST gives auditability and simpler ops |
| 80 | Chat routing | **All un-addressed traffic + addressed traffic only when Frame is a participant** | Frame plays a routing role on un-addressed; otherwise follows aspect chat discipline. Network events arrive via in-process hooks, not the chat firehose. |

Both leans bias toward simpler/smaller. Both leave room to add the heavier path (admin WS, firehose routing) when a concrete need surfaces. Neither closes a door — a future decision can revisit either.

Pending operator sign-off, then §6.5 build (#78) can start.
