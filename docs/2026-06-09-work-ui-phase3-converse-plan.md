# Work UI Phase 3 (Converse — the chat home) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Promote Converse to the first-class chat home — a messaging-app layout (Team stream + per-aspect DMs, shadow pinned) reusing the existing chat plumbing, retiring the legacy Chat + Feed views.

**Architecture:** Frontend-only refactor of `Chat.js` into a conversation-list + conversation-pane layout. DMs route via the existing mention path (the DM composer auto-`@mentions` the target; `RecipientPolicy.Compute` is mention/reply-based; the `dm:<agent>` topic groups; replies inherit the topic). One Go test pins the routing reuse (the only CI-covered part); the rest is build-free Preact+htm verified in dev mode.

**Tech Stack:** Preact + htm via `window.__preact` (no build, go:embed), `comms.js`/`api.js` WS helpers, the `MessageBubble` component, signals in `state.js`. One Go test in `nexus/broker`.

**Spec:** `docs/2026-06-09-work-ui-phase3-converse-design.md`. **Branch:** `design/work-ui-phase3`. **Commit trailer (every commit):** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## File Structure

**Frontend (`nexus/broker/static/dashboard/`):**
- Create `js/views/ConverseView.js` — the list+pane layout + selected-conversation state (refactored from `Chat.js`, reusing its `messageBelongsToChannel`, `fetchMessages`, `subscribe.chat` wiring).
- Modify `js/api.js` — add a `sendDM(agent, content, replyTo)` helper (topic `dm:<agent>` + ensures `@<agent>` in content).
- Modify `js/app.js` — `#/converse` → `ConverseView`; redirect `#/chat` + `#/feed` → `#/converse`; drop the legacy nav entries.
- Create `css/converse.css` — list+pane styles (reuse `tokens.css`; `chat.css`/`MessageBubble` unchanged); link in `index.html`.
- Delete `js/views/FeedView.js` (subsumed) and `js/views/Chat.js` (becomes ConverseView).

**Backend:** none, except one test.
- Modify `nexus/broker/recipients_test.go` — pin that a Converse-composed DM (content with `@<agent>`) routes to `<agent>`.

---

## Task 1: Pin DM routing in Go (the load-bearing reuse)

**Files:**
- Test: `nexus/broker/recipients_test.go` (add)

DMs work only if a `@<agent>`-bearing message computes `<agent>` as a recipient. Pin it so the reuse can't silently break.

- [ ] **Step 1: Write the test**

```go
// nexus/broker/recipients_test.go (add). Confirm the RecipientPolicy constructor
// + AspectLookup against the existing tests in this file and mirror them.
func TestComputeRoutesDMByMention(t *testing.T) {
	p := RecipientPolicy{Aspects: func() []string { return []string{"anvil", "shadow", "operator"} }}
	// A Converse DM to anvil: composer auto-includes "@anvil".
	got := p.Compute("operator", "@anvil please cancel the run", 0)
	found := false
	for _, r := range got {
		if r == "anvil" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DM @anvil did not route to anvil: %v", got)
	}
}
```

> Match `RecipientPolicy`'s real fields/constructor (see the existing `recipients_test.go` + `recipients.go` — `Aspects`/`AspectLookup`). The assertion is the contract: a `@<agent>` message routes to `<agent>`.

- [ ] **Step 2: Run**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestComputeRoutesDMByMention -v`
Expected: PASS (documents the routing Converse depends on). If it fails, the mention path differs from the spec's assumption — STOP and reconcile before the frontend work.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/recipients_test.go
git commit -m "test(broker): pin that a @<agent> DM routes to that aspect (Converse reuse)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `sendDM` api helper

**Files:**
- Modify: `nexus/broker/static/dashboard/js/api.js`

- [ ] **Step 1: Add the helper**

```javascript
// js/api.js (add near sendMessage, ~line 234)
// sendDM posts a 1:1 message to an aspect: the dm:<agent> topic groups the
// conversation, and the @<agent> mention is what actually routes it (the
// broker's RecipientPolicy is mention-based, not topic-based).
export function sendDM(agent, content, replyTo = 0) {
  const body = String(content || '').trim();
  if (!agent || !body) return Promise.resolve();
  const mention = `@${agent}`;
  const withMention = body.includes(mention) ? body : `${mention} ${body}`;
  return sendMessage({ from: 'operator', content: withMention, replyTo, topic: `dm:${agent}` });
}
```

- [ ] **Step 2: Verify (dev mode)**

Open the dashboard in dev mode (`--dashboard-dir`), hard-refresh; confirm `api.js` imports without console error. (No JS unit harness; manual.)

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/api.js
git commit -m "feat(dashboard): sendDM helper (dm:<agent> topic + auto @mention routing)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: ConverseView — conversation list + pane

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/ConverseView.js`
- Create: `nexus/broker/static/dashboard/css/converse.css`
- Modify: `nexus/broker/static/dashboard/index.html` (link converse.css)

Refactor `Chat.js` into ConverseView. Reuse its `messageBelongsToChannel`, the `currentChannel`/`messages`/`lastMessageId`/`replyTo`/`agents` signals from `state.js`, `fetchMessages`, and `MessageBubble`. The channel value drives the selected conversation: `'general'` = Team, `'dm:<agent>'`... — note `Chat.js`'s `messageBelongsToChannel` (line 243) treats the dm channel as the **bare agent name** (`msg.topic === \`dm:${channel}\``); keep that convention (channel `'anvil'` ↔ topic `'dm:anvil'`), or normalize to full `dm:<agent>` channel values consistently across the list, the filter, and `fetchMessages` — pick one and apply it everywhere.

- [ ] **Step 1: Implement ConverseView**

```javascript
// js/views/ConverseView.js
const { html, useEffect, useState, useRef } = window.__preact;

import { currentChannel, messages, lastMessageId, replyTo, agents, agentColors } from '../state.js';
import { fetchMessages, sendMessage, sendDM } from '../api.js';
import { subscribe } from '../comms.js';
import { MessageBubble } from '../components/MessageBubble.js';

const TEAM = 'general';

// Bare-agent-name channel for a DM (matches Chat.js messageBelongsToChannel line 243).
function isDM(ch) { return ch && ch !== TEAM && !ch.startsWith('topic:'); }

function messageBelongsToChannel(msg, channel) {
  if (!channel || channel === TEAM) return !msg.topic || msg.topic === 'general';
  if (channel.startsWith('topic:')) return msg.topic === channel.slice(6);
  return msg.topic === `dm:${channel}`; // DM: channel is the bare agent name
}

function ConversationList({ roster, active, onSelect }) {
  const dmAgents = roster.filter((a) => a !== 'operator');
  // shadow pinned first, then the rest.
  const ordered = ['shadow', ...dmAgents.filter((a) => a !== 'shadow')];
  return html`
    <nav class="cv-list">
      <button class=${active === TEAM ? 'cv-item active' : 'cv-item'} onClick=${() => onSelect(TEAM)}>
        <span class="cv-team">Team</span>
      </button>
      <div class="cv-section">Direct</div>
      ${ordered.map((a) => html`
        <button key=${a} class=${active === a ? 'cv-item active' : 'cv-item'} onClick=${() => onSelect(a)}>
          ${a === 'shadow' ? html`<span class="cv-pin">★</span>` : null}${a}
        </button>`)}
    </nav>`;
}

function Composer({ channel }) {
  const [text, setText] = useState('');
  const send = () => {
    const body = text.trim();
    if (!body) return;
    if (isDM(channel)) sendDM(channel, body, replyTo.value || 0);
    else sendMessage({ from: 'operator', content: body, replyTo: replyTo.value || 0, topic: channel === TEAM ? '' : channel });
    setText('');
    replyTo.value = 0;
  };
  return html`
    <div class="cv-composer">
      <textarea value=${text} placeholder=${isDM(channel) ? `Message @${channel}…` : 'Message the team… (@mention to route)'}
        onInput=${(e) => setText(e.target.value)}
        onKeyDown=${(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); } }}></textarea>
      <button onClick=${send}>Send</button>
    </div>`;
}

export function ConverseView() {
  const ch = currentChannel.value || TEAM;
  const roster = (agents.value || []).map((a) => (typeof a === 'string' ? a : a.id));
  const streamRef = useRef(null);

  // Load history on channel change.
  useEffect(() => {
    messages.value = [];
    lastMessageId.value = 0;
    fetchMessages(ch, 0).then((res) => {
      messages.value = res.messages || [];
      if (messages.value.length) lastMessageId.value = messages.value[messages.value.length - 1].id;
    });
  }, [ch]);

  // Live deliver → append if it belongs to the open conversation.
  useEffect(() => {
    return subscribe('subscribe.chat', {}, (msg) => {
      if (!messageBelongsToChannel(msg, currentChannel.value || TEAM)) return;
      messages.value = [...messages.value, msg];
    });
  }, []);

  const visible = (messages.value || []).filter((m) => messageBelongsToChannel(m, ch));
  return html`
    <div class="converse">
      <${ConversationList} roster=${roster} active=${ch} onSelect=${(c) => { currentChannel.value = c; }} />
      <div class="cv-pane">
        <div class="cv-stream" ref=${streamRef}>
          ${visible.length === 0 ? html`<div class="cv-empty">No messages yet.</div>`
            : visible.map((m) => html`<${MessageBubble} key=${m.id} message=${m} />`)}
        </div>
        <${Composer} channel=${ch} />
      </div>
    </div>`;
}
```

> Confirm against `Chat.js`: the `fetchMessages(channel, afterId)` channel argument shape (does it expect `'general'`/`'dm:anvil'`/the bare name?), the `MessageBubble` prop name (`message` vs `msg`), and the `subscribe`/push wiring (Chat.js uses `subscribe('subscribe.chat', …)` or `onPushKind`). Match Chat.js exactly — it's the working reference.

- [ ] **Step 2: Add converse.css + link it**

Create `css/converse.css` with `.converse` (flex row), `.cv-list`, `.cv-item`/`.active`, `.cv-section`, `.cv-pin`, `.cv-pane`, `.cv-stream`, `.cv-empty`, `.cv-composer` using `var(--…)` tokens. Add `<link rel="stylesheet" href="/css/converse.css">` to `index.html` alongside the others.

- [ ] **Step 3: Verify (dev mode)**

`#/converse`: Team + shadow(pinned) + the roster as DMs; selecting Team loads general; selecting an agent loads its `dm:<agent>` history; sending in a DM delivers to that aspect (it replies, threaded in the DM); sending in Team with `@anvil` routes. No console errors.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/ConverseView.js nexus/broker/static/dashboard/css/converse.css nexus/broker/static/dashboard/index.html
git commit -m "feat(dashboard): ConverseView — conversation list + pane (Team + DMs, shadow pinned)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Shell — route Converse, retire Chat + Feed

**Files:**
- Modify: `nexus/broker/static/dashboard/js/app.js`
- Delete: `nexus/broker/static/dashboard/js/views/Chat.js`, `js/views/FeedView.js`

- [ ] **Step 1: Wire Converse + redirect legacy routes**

In `app.js`: import `ConverseView`; in `RouteView`, `case 'converse': return html\`<${ConverseView} />\`;`. In `getRoute()`, redirect legacy hashes:

```javascript
function getRoute() {
  const hash = window.location.hash;
  if (hash.startsWith('#/chat') || hash.startsWith('#/feed')) {
    if (window.location.hash !== '#/converse') window.location.hash = '#/converse';
    return 'converse';
  }
  if (hash.startsWith('#/converse')) return 'converse';
  if (hash.startsWith('#/watch')) return 'watch';
  if (hash.startsWith('#/configure') || hash.startsWith('#/settings')) return 'configure';
  if (hash.startsWith('#/agents')) return 'agents'; // ObserveView stays (per-aspect observability)
  if (hash === '#/' || hash === '') return 'watch';
  return 'watch';
}
```

Remove the `Chat`/`FeedView`/`Placeholder`(for converse) imports + their `RouteView` cases. Keep the three-area nav (Converse · Watch · Configure) — Converse now points at the real view.

> Confirm the current `app.js` `RouteView`/`getRoute` shape (Phase 1/2 changed it) and apply the redirect cleanly. Leave `#/agents` (ObserveView) untouched unless it's also being retired — the spec only retires Chat + Feed.

- [ ] **Step 2: Delete the subsumed views**

```bash
git rm nexus/broker/static/dashboard/js/views/Chat.js nexus/broker/static/dashboard/js/views/FeedView.js
```

Grep for any remaining imports of `Chat`/`FeedView` (`grep -rn "Chat.js\|FeedView" nexus/broker/static/dashboard/js/`) and remove them so the app loads.

- [ ] **Step 3: Verify (dev mode)**

Hard-refresh: `#/converse` is the Converse home; visiting `#/chat` or `#/feed` redirects to `#/converse`; the three-area nav works; no console errors (no dangling Chat/FeedView imports). Build the broker (`go build ./...`) to confirm the `go:embed` still embeds cleanly with the deleted files.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/app.js
git commit -m "feat(dashboard): route Converse + retire Chat/Feed (redirect legacy hashes)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Integration verification (build + deploy + dogfood)

- [ ] **Step 1: Build + the one test**

Run: `cd /Users/jacinta/Source/nexus && go build ./... && go test ./nexus/broker/ -run TestComputeRoutesDMByMention`
Expected: clean build (go:embed picks up the new/removed dashboard files) + PASS.

- [ ] **Step 2: Deploy to dMon**

Frontend is embedded in the broker, so **broker-only**: `deploy/broker/build.sh` + `kubectl rollout restart deploy/nexus-broker`. (No worker change.)

- [ ] **Step 3: Dogfood**

In `#/converse`: Team stream loads + `@mention` routes; DM **shadow** → shadow replies in the DM thread; DM another aspect → it receives + replies; `#/chat`/`#/feed` redirect to `#/converse`. Then confirm Watch + Configure still work (nav intact).

- [ ] **Step 4: Push the branch**

```bash
git push -u origin design/work-ui-phase3
```

---

## Decomposition

**One ticket (3a):** all of the above. Frontend-only + one Go test; ~5 tasks. No sub-tickets — it's a single cohesive refactor.

## Self-Review notes (for the executor)

- **Spec coverage:** Team stream + DMs + shadow-pinned (T3) · DM routing via mention (T1 pins it, T2 `sendDM` implements it) · retire Chat/Feed + redirect (T4) · frontend-only (no backend beyond the T1 test). All spec sections map to a task.
- **Type consistency:** `sendDM(agent, content, replyTo)` (T2) is called by the Composer (T3); `messageBelongsToChannel` (T3) matches Chat.js's dm-channel convention (bare agent name ↔ `dm:<agent>` topic); `ConverseView` routed in `app.js` (T4).
- **Confirm-against-live-code seams (flagged inline):** `RecipientPolicy` fields/constructor (T1, vs recipients_test.go); `fetchMessages` channel-arg shape + `MessageBubble` prop name + the `subscribe`/push wiring (T3, vs Chat.js); the current `app.js` RouteView/getRoute shape + whether `#/agents` ObserveView stays (T4). These mirror the working Chat.js — match it, don't invent.
- **Risk:** Chat.js is 593 lines; ConverseView reuses its proven `messageBelongsToChannel`/fetch/subscribe logic rather than reinventing — port those helpers verbatim, only the layout changes.
