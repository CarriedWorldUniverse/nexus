# Aspect Runtime Architecture — Funnel/Bridle on the Aspect Side

**Date:** 2026-05-02
**Status:** Draft — operator-confirmed locks at #9085, #9089; awaiting review
**Owner:** keel
**Companion to:** [`2026-04-22-nexus-registration-spec.md`](2026-04-22-nexus-registration-spec.md) §2.2.1 (harness shape) and [`2026-04-25-nexus-transport-spec.md`](2026-04-25-nexus-transport-spec.md) (WS protocol)

---

## Purpose

This doc captures the architecture decisions made in chat #9078–9089 about where the funnel and bridle run, how recipient routing works, and how comms tools behave mid-turn. It's the load-bearing decision set the rebuild Nexus + aspect runtime + transport will all build against.

## Three locks

### Lock 1 — Aspect-side funnel + bridle in a single binary

The aspect executable is one Go binary. It contains:

- **Bridle** — single-turn provider driver. One call drives one model invocation (with internal tool-call rounds bridle manages itself). Bridle is the primitive.
- **Funnel** — deliberation engine. Composes turns. Receives chat messages from the WS, runs the deliberation loop, decides whether to post outputs. Funnel consumes bridle.
- **WS client** — the aspect's connection to Nexus. Carries `chat.deliver`, `chat.send`, `register`, `turn`-related frames per the transport spec §5.

Aspect process structure:

```
aspect-binary <home>
  ├── reads home/aspect.json, SOUL.md, NEXUS.md, PRIMER.md
  ├── derives identity, system prompt, provider, model
  ├── opens WS to NEXUS_UPSTREAM
  ├── sends `register` frame
  └── enters main loop:
       ┌──────────────────────────────────────┐
       │ read top message from queue          │
       │ funnel.Deliberate(message)           │
       │   ↓ runs bridle.RunTurn(s) as needed │
       │   ↓ filter judges final output       │
       │ if post=true → fire send_chat        │
       │ advance to next message              │
       └──────────────────────────────────────┘
```

**Why aspect-side, not Nexus-side:**

- Provider credentials live with the aspect, not centralized in Nexus. Each aspect can use its own provider/model independently.
- Model invocation is local to the aspect process — the WS doesn't carry turn requests or token streams, only chat-shaped events.
- Federation works the same way as same-host: the aspect runtime is the same binary, the WS just hits a different upstream.
- Matches transport spec §2.1 ("Aspect — long-running process… connects to its upstream and stays connected").
- The §6.5 Frame harness embedded in Nexus is the *exception*, not the rule — keel-as-Frame is in-process for tight admin coupling. Every other aspect runs out-of-process via this binary shape.

**Bridle's scope is preserved as primitive:**

Bridle is single-turn driver only. Funnel composes deliberation on top. Collapsing the layers (e.g. bridle absorbs deliberation) muddies the abstraction split — transport, federation, multi-Frame, and remote-aspect stories all rely on a clean primitive/orchestrator boundary.

### Lock 2 — Nexus-side recipient routing; non-recipients pull via tool call

The fan-out token-burn problem (today's agent-network harness with #72 + #125) is solved at the **routing layer**, not the harness:

- **Nexus computes recipients** when chat lands. By default: a single addressed recipient.
- **Default recipient rule:** the agent the operator (or originator) is replying to. `reply_to` chain → addressee.
- **Override:** explicit `@<agent>` mention(s). Multiple mentions = multi-recipient delivery.
- **Other agents are NOT pushed** the message. Nexus does not fan out; aspects only receive `chat.deliver` frames addressed to them.
- **Non-recipients pull via `chat.read` tool call** during their own already-running turns. If anvil wants to know what's been said in a thread it isn't subscribed to, anvil's running turn calls `chat.read(thread_id)` as a tool. Reading the thread is a tool call inside an existing deliberation — it does NOT trigger a fresh deliberation cycle.

**Why this solves the token-burn problem:**

Today (agent-network) every aspect that *might* care about a chat receives it via WS, runs a turn, post-hoc-filters the output, suppresses if not meaningful. That's burning a turn per aspect per message. The post-hoc filter prevents the BAD post but not the WASTED turn.

In the rebuild, the routing decision is in Nexus before the aspect's turn runs. Aspects receive only what's addressed to them. Token cost matches engagement intent.

**Read invariant:** `chat.read` results never enqueue. The aspect's queue-driven loop only advances on `chat.deliver` push frames + tool-call results from already-running turns. Outbound `chat.read` calls are read-only and synchronous to the calling turn. (Forge confirmed this clarification at #9087.)

### Lock 3 — Full comms tools restored; mid-turn calls are first-class

Aspects get the full comms toolset as live tools, callable any time during a turn:

- `send_chat(content, reply_to?, topic?)` — posts immediately, not deferred to end-of-turn
- `react_to(msg_id, emoji)` — toggle a reaction
- `chat.read(thread_id, since_id?)` — read thread history (per Lock 2)
- `announce_file(path, description)` — surface a file
- `share_file(path, recipients[])` — direct file share
- `react_to_message(msg_id, emoji)` — same as react_to (legacy alias)
- (Plus the existing knowledge / ticket tools from registration spec §2.8 / pending #82.)

**Mid-turn semantics:**

- A `send_chat` call mid-turn posts to chat *immediately*. The aspect can use this to ask clarifying questions, surface status pulses, update operator on long work, react to operator interjections, etc.
- The post-hoc filter judges ONLY the aspect's final natural reply — the model's text output at the conclusion of a turn (the "did the model emit something as the turns conclusion?" content). Mid-turn `send_chat` calls are intentional and authoritative; they post unfiltered. Filter does not rejudge them.
- Multiple mid-turn posts per turn are permitted. The aspect controls cadence via tool calls; the filter only governs the implicit final reply.

**Why this matters (operator's observed cost):**

Without mid-turn comms, aspects:
- Couldn't ask clarifying questions mid-work; had to either guess or wait for next turn
- Couldn't post "this is taking longer than expected" pulses (closes #118 status-pulses naturally)
- Couldn't react mid-tool-chain (e.g. 👀 on a message before doing the work)
- Couldn't surface intermediate findings if a long task generated useful side data
- Couldn't respond to mid-turn operator interjections (#66 mid-turn operator-chat surfacing)

Mute-until-done was the wrong shape. Restoring full comms tools fixes a pile of pending tickets at the protocol layer rather than each at the harness layer.

**What the post-hoc filter still catches:**

- The "did the model emit a meaningful natural reply, or did it ramble / produce empty content / leak thinking" case. This is now a *narrower* filter than agent-network's #72 — it only governs the final reply, not the whole output stream.
- "Self-suppression" outputs ("I don't have anything to add to this thread" / "this isn't for me") still get suppressed. The aspect engaged because Nexus pushed it, but if the model decides not to emit a substantive reply, the filter respects that.

## Frame as a special case

The §6.5 Frame harness embedded in Nexus stays as-is:

- Frame (keel-as-Frame) is the operator-identity Frame inside the Nexus process
- Frame's funnel runs in-process — same Go runtime as Nexus
- Frame uses bridle the same way an out-of-process aspect would
- Frame does NOT use the WS aspect protocol — it's not a peer, it's the host
- All three locks apply to Frame *internally* (deliberation, recipient gating to Frame, mid-turn comms)
- Other aspects are out-of-process and use the WS path

The asymmetry is deliberate: Frame is owner-identity; other aspects are tenants of the Frame's Nexus.

## Implications for build sequence

The locks shape what we build first:

1. **Funnel rewrite (post-hoc filter pattern, mid-turn comms support)** — the single biggest piece of code work. The §6.5 funnel needs to support mid-turn send_chat calls (currently the model output is collected at end of turn; we need to wire send_chat as a real-time tool that posts directly to the WS / broker rather than to a sink).

2. **Aspect binary scaffold** — `cmd/aspect/main.go` that takes `<home>` arg, opens WS, registers, runs the funnel-driven main loop. Probably ~300 LOC.

3. **Nexus-side WS handler** — accepts aspect connections, routes `chat.deliver` to the right aspect based on recipient computation, accepts `chat.send` / `chat.read` etc. from aspects.

4. **Recipient routing module** — Nexus computes recipients per Lock 2. Single source of truth.

5. **Comms tool set in bridle/funnel** — wire `send_chat`, `react_to`, `chat.read`, etc. as bridle.ToolDef instances backed by a runner that translates tool calls to outbound WS frames.

6. **Migrate forge** — first real test aspect. Forge in WSL connects to Nexus over real network address (not localhost — operator #9081). Verifies the whole stack end-to-end on a non-trivial deployment shape.

7. **Migrate anvil + others** — once forge proves the path.

8. **Then transport extras** — Outpost (multi-aspect-per-host fan-in), federation, etc. None of those are blockers for the basic shape.

## Open questions for review

1. **Post-hoc filter granularity:** does the filter judge the full final-reply output as one chunk, or per-paragraph / per-streaming-chunk? Earlier agent-network #72 was full-output. Lean: keep full-output. Mid-turn streaming = mid-turn `send_chat` calls handle real-time emission; the natural-reply at end is one decision.

2. **`chat.read` rate limiting:** if an aspect calls `chat.read` aggressively in a long turn, do we need a per-turn limit? Probably not for v1 — read is cheap on the broker side. Surface if it becomes a problem.

3. **Recipient when operator chats `@all`:** does that route to every aspect, or is `@all` an explicit override of the single-recipient default? Lean: `@all` routes to all; `@<agent>` routes to that agent; reply-to routes to the addressee of the parent. Same as today's agent-network model, just enforced at routing rather than harness.

4. **Reply chains across non-recipients:** if aspect A is the recipient and replies, and the operator replies back to A, does aspect B (not in chain) ever see it pushed? Answer per Lock 2: no, B has to `chat.read` if curious. Confirms.

5. **Cross-aspect direct send:** aspect A calls `send_chat("@B do this")`. Does Nexus route to B per the @-mention rule? Yes — same routing module, same rules. Aspects are first-class senders.

## Acceptance criteria (when this is built)

- [ ] Forge runs as `aspect-binary <forge-home>` from inside WSL
- [ ] Forge connects to Nexus over real network address (`wss://agentnetwork.<tailnet>.ts.net:port/connect`), not localhost
- [ ] Operator chats `@forge what about X` → forge receives, runs turn, posts a reply
- [ ] Operator chats something WITHOUT `@forge` → forge does NOT receive a `chat.deliver` frame
- [ ] If forge wants to know what was said: forge's running turn calls `chat.read(thread)` as a tool — no fresh deliberation triggered
- [ ] Forge can call `send_chat` mid-turn (e.g. "checking deps, will be back in 30s") and the message posts immediately
- [ ] If forge's natural final reply is empty / scratch / triage-shaped, post-hoc filter suppresses it
- [ ] Anvil and other aspects do NOT receive `chat.deliver` for messages addressed only to forge
- [ ] Frame (keel) deliberation continues to work in-process; no regression from Lock 1 for the embedded case

---

## References

- Architecture lock messages: #9085 (locks 1+2), #9087 (forge tool-call clarification), #9089 (lock 3 full comms tools)
- Registration spec §2.2.1 (harness shape, deliberation lock from #81)
- Transport spec (WS protocol, single-aspect direct mode at §3.4)
- Backlog: #119 (token burn — solved by Lock 2), #66 (mid-turn comms — solved by Lock 3), #118 (status pulses — solved by Lock 3)
