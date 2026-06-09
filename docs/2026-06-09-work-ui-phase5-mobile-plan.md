# Work UI Phase 5 (Dedicated Mobile) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A dedicated mobile experience — detect mobile at the App root and render a Converse-first single-column `MobileApp` (mobile Converse + glanceable read-only Runs + light notifications) instead of the desktop console.

**Architecture:** Frontend-only. In `app.js` `App()`, after the shared auth gates, branch on an `isMobile` signal (`matchMedia('(max-width: 768px)')`) to render `MobileApp` vs the desktop `Shell`. The mobile views reuse all existing data helpers (chat: `fetchMessages`/`sendMessage`/`sendDM`/`subscribe.chat`; runs: `runsList`/`runGet`/`runs.update`/`subscribe.observe`; `MessageBubble`) — only the mobile layout/nav and a notification surface are new. No backend.

**Tech Stack:** Preact + htm (`window.__preact`, no build, go:embed), `comms.js`/`api.js` WS helpers, `MessageBubble`, signals in `state.js`, the existing `notifications.js`.

**Spec:** `docs/2026-06-09-work-ui-phase5-mobile-design.md`. **Branch:** `design/work-ui-phase5`. **Commit trailer (every commit):** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## File Structure

**Frontend (`nexus/broker/static/dashboard/`):**
- Create `js/state-mobile.js` (or add to `state.js`) — the `isMobile` signal + the `matchMedia` listener.
- Modify `js/app.js` — branch `App()`'s final return to `MobileApp` vs `Shell`.
- Create `js/views/mobile/MobileApp.js` — shell: bottom-tab nav (Converse|Runs), renders the active tab, mounts notifications.
- Create `js/views/mobile/MobileConverse.js` — conversation list + drill-in pane + composer (reuses chat helpers).
- Create `js/views/mobile/MobileRuns.js` — glanceable run list + read-only run detail (reuses runs/timeline helpers).
- Create `js/views/mobile/MobileNotifications.js` — unread badge + toast + foreground `Notification` (reuse `notifications.js` infra where it fits).
- Create `css/mobile.css` — dedicated mobile styles; link in `index.html`.

**Backend:** none.

---

## Task 1: `isMobile` signal + the App() branch

**Files:**
- Create: `nexus/broker/static/dashboard/js/state-mobile.js`
- Modify: `nexus/broker/static/dashboard/js/app.js`

- [ ] **Step 1: The detection signal**

```javascript
// js/state-mobile.js
const { signal } = window.__preactSignals || window.__preact; // match how state.js imports signal

const MQ = '(max-width: 768px)';
function mqMatches() {
  return typeof window.matchMedia === 'function' ? window.matchMedia(MQ).matches : false;
}

export const isMobile = signal(mqMatches());

if (typeof window.matchMedia === 'function') {
  const mql = window.matchMedia(MQ);
  const update = () => { isMobile.value = mql.matches; };
  // addEventListener('change') is the modern API; addListener is the fallback.
  if (mql.addEventListener) mql.addEventListener('change', update);
  else if (mql.addListener) mql.addListener(update);
}
```

> Confirm how `state.js` creates signals (the `signal` import source) and mirror it. `matchMedia` absent → `false` (desktop), per the spec.

- [ ] **Step 2: Branch the App() render**

In `app.js`, import `isMobile` + `MobileApp`, and change the final return (currently `return html\`<${Shell} activeRoute=${route.value}><${RouteView} route=${route.value} /></${Shell}>\``) to branch AFTER the auth gates:

```javascript
import { isMobile } from './state-mobile.js';
import { MobileApp } from './views/mobile/MobileApp.js';

// ... inside App(), the gates stay unchanged:
//   if (!bypassChecked) return html`<div style="display:none"></div>`;
//   if (!authed) return html`<${Login} onComplete=${...} />`;

  if (isMobile.value) return html`<${MobileApp} />`;
  return html`<${Shell} activeRoute=${route.value}><${RouteView} route=${route.value} /></${Shell}>`;
```

> Confirm the exact final-return line (Phases 1-4 changed `app.js`) and keep the `bypassChecked`/`authed` gates above the branch (auth is shared).

- [ ] **Step 3: Verify (dev mode)**

Resize the viewport below 768px → the app renders `MobileApp` (a stub at this point is fine — Task 2 fills it); above → the desktop console. Auth still gates both. No console error.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/state-mobile.js nexus/broker/static/dashboard/js/app.js
git commit -m "feat(dashboard): isMobile detection + App branch to MobileApp

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: MobileApp shell (bottom-tab nav)

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/mobile/MobileApp.js`
- Create: `nexus/broker/static/dashboard/css/mobile.css`
- Modify: `nexus/broker/static/dashboard/index.html` (link mobile.css)

- [ ] **Step 1: Implement the shell**

```javascript
// js/views/mobile/MobileApp.js
const { html, useState } = window.__preact;

import { MobileConverse } from './MobileConverse.js';
import { MobileRuns } from './MobileRuns.js';
import { MobileNotifications } from './MobileNotifications.js';

const TABS = [
  { id: 'converse', label: 'Converse' },
  { id: 'runs',     label: 'Runs' },
];

export function MobileApp() {
  const [tab, setTab] = useState('converse');
  const [unread, setUnread] = useState(0);
  return html`
    <div class="m-app">
      <main class="m-main">
        ${tab === 'converse'
          ? html`<${MobileConverse} onActive=${() => setUnread(0)} />`
          : html`<${MobileRuns} />`}
      </main>
      <nav class="m-tabbar">
        ${TABS.map((t) => html`
          <button key=${t.id} class=${tab === t.id ? 'm-tab active' : 'm-tab'} onClick=${() => { if (t.id === 'converse') setUnread(0); setTab(t.id); }}>
            ${t.label}${t.id === 'converse' && unread > 0 ? html`<span class="m-badge">${unread > 9 ? '9+' : unread}</span>` : null}
          </button>`)}
      </nav>
      <${MobileNotifications} activeTab=${tab} onUnread=${(n) => setUnread((u) => u + n)} />
    </div>`;
}
```

- [ ] **Step 2: mobile.css + link**

Create `css/mobile.css`: `.m-app` (column, 100dvh), `.m-main` (flex:1, overflow), `.m-tabbar` (fixed bottom, safe-area `padding-bottom: env(safe-area-inset-bottom)`), `.m-tab`/`.active`, `.m-badge`. Use `tokens.css` vars. Add `<link rel="stylesheet" href="css/mobile.css" />` to `index.html`.

- [ ] **Step 3: Verify (dev mode)**

Below 768px: two bottom tabs (Converse default, Runs); tapping switches; the badge slot exists. No console error (stub MobileConverse/MobileRuns/MobileNotifications can be one-line placeholders until their tasks).

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/mobile/MobileApp.js nexus/broker/static/dashboard/css/mobile.css nexus/broker/static/dashboard/index.html
git commit -m "feat(dashboard): MobileApp shell + bottom-tab nav

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: MobileConverse (list + drill-in pane)

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/mobile/MobileConverse.js`

Reuse the Phase 3 helpers (`fetchMessages`, `sendMessage`/`sendDM`, `subscribe('subscribe.chat')`, the `messageBelongsToChannel`/dm convention) + `MessageBubble` + the `agents`/`currentChannel` signals. Mobile is drill-in (list ↔ pane), not side-by-side.

- [ ] **Step 1: Implement**

```javascript
// js/views/mobile/MobileConverse.js
const { html, useState, useEffect } = window.__preact;

import { currentChannel, agents } from '../../state.js';
import { fetchMessages, sendMessage, sendDM } from '../../api.js';
import { subscribe } from '../../comms.js';
import { MessageBubble } from '../../components/MessageBubble.js';

const TEAM = 'general';
function isDM(ch) { return ch && ch !== TEAM && !ch.startsWith('topic:'); }
function belongs(msg, ch) {
  if (!ch || ch === TEAM) return !msg.topic || msg.topic === 'general';
  return msg.topic === `dm:${ch}`;
}

export function MobileConverse({ onActive }) {
  const [view, setView] = useState('list'); // 'list' | 'pane'
  const roster = (agents.value || []).map((a) => (typeof a === 'string' ? a : a.id));
  const dmAgents = ['shadow', ...roster.filter((a) => a !== 'shadow' && a !== 'operator')];

  const open = (ch) => { currentChannel.value = ch; setView('pane'); if (onActive) onActive(); };

  if (view === 'list') {
    return html`
      <div class="m-converse-list">
        <button class="m-conv-item" onClick=${() => open(TEAM)}>Team</button>
        <div class="m-conv-section">Direct</div>
        ${dmAgents.map((a) => html`<button key=${a} class="m-conv-item" onClick=${() => open(a)}>${a === 'shadow' ? '★ ' : ''}${a}</button>`)}
      </div>`;
  }
  return html`<${MobilePane} channel=${currentChannel.value || TEAM} onBack=${() => setView('list')} />`;
}

function MobilePane({ channel, onBack }) {
  const [msgs, setMsgs] = useState([]);
  const [text, setText] = useState('');
  useEffect(() => {
    fetchMessages(channel, 0).then((r) => setMsgs((r.messages || []).filter((m) => belongs(m, channel))));
    const off = subscribe('subscribe.chat', {}, (m) => { if (belongs(m, channel)) setMsgs((p) => [...p, m]); });
    return off;
  }, [channel]);
  const send = () => {
    const body = text.trim(); if (!body) return;
    if (isDM(channel)) sendDM(channel, body); else sendMessage({ from: 'operator', content: body, topic: channel === TEAM ? '' : channel });
    setText('');
  };
  return html`
    <div class="m-pane">
      <header class="m-pane-head"><button class="m-back" onClick=${onBack}>‹</button>${isDM(channel) ? channel : 'Team'}</header>
      <div class="m-pane-stream">${msgs.map((m) => html`<${MessageBubble} key=${m.id} message=${m} />`)}</div>
      <div class="m-pane-composer">
        <input value=${text} placeholder="Message…" onInput=${(e) => setText(e.target.value)} onKeyDown=${(e) => { if (e.key === 'Enter') send(); }} />
        <button onClick=${send}>Send</button>
      </div>
    </div>`;
}
```

> Confirm `fetchMessages`'s channel-arg shape, `sendDM` (Phase 3), and `MessageBubble`'s prop name against the live code; match them. Add `.m-converse-list`, `.m-conv-item`, `.m-pane*` to `mobile.css`.

- [ ] **Step 2: Verify (dev mode)** — list shows Team + shadow(★) + DMs; tap → pane with back; DM shadow delivers + a reply shows; back returns to the list. No console error.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/mobile/MobileConverse.js nexus/broker/static/dashboard/css/mobile.css
git commit -m "feat(dashboard): MobileConverse — list + drill-in pane

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: MobileRuns (glanceable + read-only detail)

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/mobile/MobileRuns.js`

Reuse `runsList`/`runGet` + `onPushKind('runs.update')` + `subscribe('subscribe.observe')`. Read-only (no Stop/dispatch).

- [ ] **Step 1: Implement**

```javascript
// js/views/mobile/MobileRuns.js
const { html, useState, useEffect } = window.__preact;

import { runsList, runGet } from '../../api.js';
import { onPushKind, subscribe } from '../../comms.js';
import { MessageBubble } from '../../components/MessageBubble.js';

export function MobileRuns() {
  const [runs, setRuns] = useState([]);
  const [open, setOpen] = useState(null); // run_id

  useEffect(() => {
    runsList().then(setRuns);
    return onPushKind('runs.update', (f) => {
      const r = f.payload || f;
      setRuns((prev) => { const i = prev.findIndex((x) => x.run_id === r.run_id); if (i >= 0) { const n = prev.slice(); n[i] = r; return n; } return [r, ...prev]; });
    });
  }, []);

  if (open) return html`<${MobileRunDetail} runId=${open} onBack=${() => setOpen(null)} />`;
  return html`
    <div class="m-runs">
      ${runs.length === 0 ? html`<div class="m-empty">No runs yet.</div>` : null}
      ${runs.map((r) => html`
        <button key=${r.run_id} class="m-run-card" onClick=${() => setOpen(r.run_id)}>
          <span class=${`m-run-dot ${r.status}`}></span>
          <span class="m-run-agent">${r.agent}</span> <span class="m-run-ticket">${r.ticket}</span>
          <span class="m-run-cmd">${(r.command || '').slice(0, 48)}</span>
        </button>`)}
    </div>`;
}

function MobileRunDetail({ runId, onBack }) {
  const [items, setItems] = useState([]);
  const [agent, setAgent] = useState(null);
  useEffect(() => {
    let unsub = null;
    runGet(runId).then((res) => {
      setItems(res.timeline || []);
      const a = res.run && res.run.agent; setAgent(a);
      if (a) unsub = subscribe('subscribe.observe', { aspect: a }, (frame) => {
        if (frame.aspect !== a) return;
        setItems((p) => [...p, { kind: 'activity', at: Date.now(), activity: { type: frame.kind, text: (frame.payload && frame.payload.text) || '' } }]);
      });
    });
    return () => { if (unsub) unsub(); };
  }, [runId]);
  return html`
    <div class="m-run-detail">
      <header class="m-pane-head"><button class="m-back" onClick=${onBack}>‹</button>${runId}</header>
      <div class="m-pane-stream">
        ${items.map((it, i) => it.kind === 'chat'
          ? html`<${MessageBubble} key=${'c' + i} message=${{ id: it.chat.msg_id, from: it.chat.from, content: it.chat.content }} />`
          : html`<div key=${'a' + i} class="m-act">${it.activity.type}: ${it.activity.text || ''}</div>`)}
      </div>
    </div>`;
}
```

> Confirm `runsList`/`runGet` shapes + `onPushKind` (Phase 1) against the live api.js; match. Add `.m-runs`, `.m-run-card`, `.m-run-dot.<status>`, `.m-run-detail`, `.m-act` to `mobile.css`.

- [ ] **Step 2: Verify (dev mode)** — Runs tab lists runs (dispatch one to see it appear live); tap → read-only timeline; back returns. No Stop/dispatch controls. No console error.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/mobile/MobileRuns.js nexus/broker/static/dashboard/css/mobile.css
git commit -m "feat(dashboard): MobileRuns — glanceable list + read-only detail

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: MobileNotifications (badge + toast + foreground Notification)

**Files:**
- Create: `nexus/broker/static/dashboard/js/views/mobile/MobileNotifications.js`

- [ ] **Step 1: Check the existing `notifications.js`**

`app.js` already imports `initNotifications` from `./notifications.js`. Read it first: if it already drives a badge/`Notification` from `chat.deliver`, reuse/extend it (pass the mobile callbacks in) rather than duplicating. The code below is the standalone fallback if `notifications.js` is desktop-specific.

- [ ] **Step 2: Implement the module**

```javascript
// js/views/mobile/MobileNotifications.js
const { html, useState, useEffect } = window.__preact;

import { onPushKind, subscribe } from '../../comms.js';

export function MobileNotifications({ activeTab, onUnread }) {
  const [toast, setToast] = useState(null);

  // Request permission on first user interaction (not on load).
  useEffect(() => {
    const ask = () => {
      if ('Notification' in window && Notification.permission === 'default') Notification.requestPermission().catch(() => {});
      window.removeEventListener('pointerdown', ask);
    };
    window.addEventListener('pointerdown', ask);
    return () => window.removeEventListener('pointerdown', ask);
  }, []);

  const notify = (title, body) => {
    setToast(title); setTimeout(() => setToast(null), 3000);
    if (activeTab !== 'converse') onUnread && onUnread(1);
    if (document.hidden && 'Notification' in window && Notification.permission === 'granted') {
      try { new Notification(title, { body }); } catch (e) { /* ignore */ }
    }
  };

  useEffect(() => {
    const offChat = subscribe('subscribe.chat', {}, (m) => {
      if (m.from && m.from !== 'operator') notify(`${m.from}`, String(m.content || '').slice(0, 80));
    });
    const offRuns = onPushKind('runs.update', (f) => {
      const r = f.payload || f;
      if (r.status === 'complete' || r.status === 'failed') notify(`run ${r.ticket}`, r.status);
    });
    return () => { offChat && offChat(); offRuns && offRuns(); };
  }, [activeTab]);

  return toast ? html`<div class="m-toast">${toast}</div>` : null;
}
```

> If `notifications.js` already owns the `subscribe.chat`→Notification path, DON'T double-subscribe — instead have it call back into the mobile badge. Confirm before wiring. Add `.m-toast` to `mobile.css`.

- [ ] **Step 3: Verify (dev mode)** — a new chat message while on the Runs tab badges Converse + shows a toast; backgrounding the tab (after granting permission) fires a browser notification; permission is asked on first tap, not load. No console error.

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/mobile/MobileNotifications.js nexus/broker/static/dashboard/css/mobile.css
git commit -m "feat(dashboard): MobileNotifications — badge/toast + foreground Notification

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Integration verification (build + deploy + dogfood)

- [ ] **Step 1: Build**

Run: `cd /Users/jacinta/Source/nexus && go build ./...`
Expected: clean (go:embed picks up the new mobile files + css).

- [ ] **Step 2: Deploy to dMon**

Broker-only (dashboard embedded): `deploy/broker/build.sh` + `kubectl rollout restart deploy/nexus-broker`.

- [ ] **Step 3: Dogfood**

On a phone (or DevTools device emulation < 768px) at the dashboard URL: `MobileApp` renders (bottom tabs); Converse list → DM shadow → reply shows; Runs lists live runs → tap → read-only timeline; a message badges Converse + toasts; install-to-homescreen works (PWA meta). Desktop (>768px) still shows the full console.

- [ ] **Step 4: Push the branch**

```bash
git push -u origin design/work-ui-phase5
```

---

## Decomposition

**One ticket (5a):** all of the above. Frontend-only; the detection branch + the shell + three mobile views + the notification module + mobile.css. (If it runs long, Tasks 4-5 — Runs + notifications — can split to 5b, but it's one cohesive mobile surface.)

## Self-Review notes (for the executor)

- **Spec coverage:** detect + render dedicated tree (T1) · MobileApp bottom-tab shell (T2) · Converse-first list+pane (T3) · glanceable read-only Runs (T4) · light in-app/foreground notifications (T5) · mobile.css + PWA (T2/T6). All spec sections map to a task.
- **Type consistency:** `isMobile` (T1) gates the branch; `MobileApp` (T2) mounts `MobileConverse`/`MobileRuns`/`MobileNotifications` (T3/T4/T5); the `belongs()`/dm convention matches Phase 3's `messageBelongsToChannel`; `runsList`/`runGet`/`onPushKind` match Phase 1.
- **Confirm-against-live-code seams (flagged inline):** `state.js`'s `signal` import (T1); the exact `App()` final-return line (T1, Phases 1-4 changed it); `fetchMessages` channel-arg + `MessageBubble` prop + `sendDM` (T3); `runsList`/`runGet`/`onPushKind` (T4); **whether `notifications.js` already owns the chat→Notification path** (T5 — reuse, don't double-subscribe). Match the live code.
- **Risk:** double-subscription if `notifications.js` already handles notifications — T5 Step 1 checks first. The `signal` import in a non-`state.js` module (T1) must match how state.js gets `signal` (vendored preact-signals); if unclear, add `isMobile` to `state.js` itself.
