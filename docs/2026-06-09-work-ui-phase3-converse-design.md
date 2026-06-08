# Work UI — Phase 3: Converse (Design)

**Date:** 2026-06-09
**Status:** Design — pending review
**Phase:** 3 of 5. Builds on Phases 1+2 (Watch + control actions, live + deployed). Promotes Converse from the inert placeholder to the first-class chat home.

## Goal

Make Converse the operator's primary chat-with-the-AI surface: a messaging-app layout with a unified **Team** stream and focused **1:1 DM** conversations (shadow pinned, any aspect reachable), consolidating the existing chat views into one home. Everything stays doable via shadow/CLI.

## Scope

**In scope (Phase 3):** Converse as the chat home — a conversation list (Team + per-aspect DMs) + a selected-conversation pane + composer, **reusing the existing chat plumbing**; retire the legacy `#/chat` + `#/feed` routes.

**Out of scope:** Phase 4 (Configure-area re-IA), Phase 5 (dedicated mobile). No backend changes — DM routing reuses the existing mention path (see Architecture).

**Frontend-only.** No new backend, no new RPCs. This is a refactor of `Chat.js` into the list+pane layout + shell wiring.

## Architecture

Converse reuses the full chat stack: `chat.list` (load history), `subscribe.chat` (live `chat.deliver`), `chat.send` (send), `MessageBubble`, and the `topic` / `thread_root` / `RecipientPolicy` model. Layout:

```
┌───────────────┬──────────────────────────────┐
│ CONVERSATIONS │  CONVERSATION PANE            │
│  Team (#all)  │   messages (MessageBubble,    │
│  ─ DMs ─      │   inline reply-threading)     │
│  shadow  ●    │                               │
│  keel         │                               │
│  anvil        │   ┌─────────────────────────┐ │
│  + new        │   │ composer (@mention auto) │ │
└───────────────┴──────────────────────────────┘
```

### Conversation list

- **Team** — the unified stream: the `general` topic. The composer supports `@mention` autocomplete; `RecipientPolicy.Compute` routes mentioned aspects (+ `@all`).
- **DMs** — one entry per `dm:<agent>` conversation. **shadow** is pinned as the default. A "+ new" picker starts a DM with any registered aspect (from the roster). The list shows aspects you've DM'd (topics matching `dm:*`) + shadow always.

### DM routing (the load-bearing reuse)

A DM is a message in a `dm:<agent>` topic. Routing reuses the existing mention path — **no backend change**:
- `RecipientPolicy.Compute(sender, content, replyTo)` routes by `@mentions` + the reply-parent's author (it does not read the topic).
- So the Converse **DM composer auto-includes `@<agent>`** in the sent content → mention-routing delivers the DM to that aspect.
- The aspect's reply (a `replyTo` the operator's DM message) **inherits the `dm:<agent>` topic** via the existing thread-topic inheritance in `chat.Insert` → the conversation stays grouped.
- Operators receive every message via the operator broadcast (tail of `HandleChatSend`), so the operator always sees the aspect's replies; Converse filters the pane by the conversation's topic.

This is exactly how `Chat.js`'s `dm:` channel already groups; Converse formalizes it with the auto-mention so DMs actually deliver.

### Components

- **`ConverseView.js`** (refactor of `Chat.js`) — owns the layout + selected-conversation state; drives the list + pane.
- **`ConversationList.js`** — Team + the `dm:*` conversations + shadow-pinned + the "+ new" aspect picker (from the `agents` roster signal).
- **`ConversationPane.js`** — the selected conversation's messages (reuse `MessageBubble`, inline reply-threading via `thread_root`) + the composer.
- **`Composer.js`** — text input; in Team, `@mention` autocomplete; in a DM, auto-prefixes/includes `@<agent>` on send so the DM routes.

(These can be one `ConverseView.js` with internal sub-components if that tracks the existing Chat.js structure better; split by responsibility where Chat.js is already large.)

## Data flow

- **Open a conversation:** `chat.list` filtered to the conversation's topic (`general` for Team, `dm:<agent>` for a DM) → render with `MessageBubble`.
- **Live:** `subscribe.chat` → append delivered messages belonging to the open conversation (filter by topic, as Chat.js's `messageBelongsToChannel` already does).
- **Send:** `chat.send` with the topic set (`general` or `dm:<agent>`); in a DM the content carries `@<agent>` so `RecipientPolicy` routes it; replies set `replyTo` (topic inherited).
- **Reload:** fully reconstructable from `chat.list` (existing pagination).

## Retiring Chat + Feed

- The shell's `#/converse` route renders `ConverseView` (replacing the Phase 1 placeholder).
- `#/chat` and `#/feed` **redirect to `#/converse`** (keep the hashes working for bookmarks, route them to Converse). Remove the legacy nav entries.
- Dispatch threads live in Watch's timeline (Phase 1); general conversation threads thread inline in the Converse pane — so Feed's threaded view is covered.

## Error handling

- Empty conversation (no history) → a clear empty state ("No messages yet"), not a blank pane.
- DM to an aspect with no live connection → the message still persists + delivers on the aspect's next register (existing `since_msg_id` replay); the UI shows it sent.
- WS drop → existing reconnect re-subscribes; on reconnect re-`chat.list` the open conversation to backfill any gap.
- Unknown/typo aspect in "+ new" → restrict the picker to the roster (no free-text aspect names).

## Testing

- **DM routing:** a unit/integration check that a `dm:<agent>` send with `@<agent>` content computes `<agent>` as a recipient (`RecipientPolicy.Compute` already tested; add a case asserting the Converse-composed DM string routes correctly).
- **Topic inheritance:** assert a reply to a `dm:<agent>` message stays in the `dm:<agent>` topic (existing `chat.Insert` behaviour; pin it).
- **Frontend:** conversation list shows Team + shadow + DM'd aspects; selecting loads the topic's history; live deliver appends to the right conversation; the DM composer routes (the aspect receives); `#/chat`/`#/feed` redirect to `#/converse`. Manual + existing harness (JS not in CI).

## File structure

**Frontend (`nexus/broker/static/dashboard/`):**
- Create `js/views/ConverseView.js` (refactor from `Chat.js` — list + pane layout).
- Create `js/views/converse/ConversationList.js`, `ConversationPane.js`, `Composer.js` (or keep as internal components of ConverseView if cleaner).
- Modify `js/app.js` — `#/converse` → `ConverseView`; redirect `#/chat` + `#/feed` → `#/converse`; drop the legacy nav entries (keep the three-area nav).
- Modify `js/api.js` — if needed, a thin `sendDM(agent, content)` helper that sets topic `dm:<agent>` + ensures the `@<agent>` mention (otherwise reuse the existing send).
- Create `css/converse.css` (reuse tokens; `MessageBubble`/`chat.css` unchanged).
- Remove/retire `js/views/Chat.js` + `FeedView.js` once ConverseView subsumes them (or keep Chat.js as the base ConverseView is refactored from — delete FeedView).

**Backend:** none.

## Decomposition

**One ticket (3a):** the Converse view (list + pane + composer, reusing the chat plumbing + the auto-mention DM routing) + shell retire/redirect of `#/chat`/`#/feed`. Frontend-only.

## Resolved decisions

1. **No backend** — DM delivery reuses mention-routing (the DM composer auto-`@mentions` the target); the `dm:<agent>` topic groups; replies inherit the topic. Confirmed against `recipients.go` (`Compute` is mention/reply-based) + `Chat.js` (`dm:<agent>` topic convention already exists).
2. **Converse subsumes Feed** — retire the separate threaded view; Watch owns dispatch threads, general threads thread inline in the Converse pane.
3. **shadow is the pinned default DM** — the operator's primary interlocutor.
