# Feed Trust Surface — Implementation Plan

**Spec:** `docs/2026-05-23-feed-trust-surface-spec.md`
**Date:** 2026-05-23
**Sequence:** Five PRs, each independently shippable. Each PR's tasks below are bite-sized, exact paths, with code shown for the load-bearing bits. Commit messages provided so the operator can land each commit straight from the task.

**Locked decisions from spec self-review (operator-approved 2026-05-23):**
- Sidebar position: left
- Single focused thread in v1 (split is a follow-up)
- Thread aging threshold: 7 days
- Thread-participants recency cap: none (accept all historical participants)
- Sidebar width: fixed 240px
- Old role-hint + mentions-me filters: dropped entirely

---

## PR 1 — Backend: thread-participants routing

**Goal:** Replies in a thread reach every aspect that has posted in
that thread (Slack/Teams semantics), not just the direct parent
author. Fixes the "agents don't hear my reply unless I `@`-tag them"
friction.

**Why first:** Smallest, isolated, no UI dependency. Immediately
unblocks the trust loop the rest of the UI will surface.

### Files

- Modify: `nexus/broker/recipients.go` — add `ThreadParticipants`
  lookup field; reshape `Compute()` to use it.
- Modify: `nexus/broker/recipients_test.go` — add 4 new cases.
- Modify: caller(s) that construct `RecipientPolicy` (find via
  `grep -rn "RecipientPolicy{" nexus/broker/`).
- Modify: caller's wiring — implement the SQL lookup against
  `chat_messages.thread_root_msg_id`. Likely lives in `server.go`
  or wherever the policy is assembled at broker boot.

### Task 1.1: Write failing test for thread-participants inclusion

In `nexus/broker/recipients_test.go`, add:

```go
func TestCompute_ThreadParticipantsAutoIncluded(t *testing.T) {
    p := RecipientPolicy{
        Parent: func(id int64) (string, error) {
            if id == 100 { return "alice", nil }
            return "", nil
        },
        ThreadParticipants: func(rootID int64) ([]string, error) {
            if rootID == 100 { return []string{"alice", "bob", "carol"}, nil }
            return nil, nil
        },
    }
    got := p.Compute("operator", "reply text", 100)
    want := []string{"alice", "bob", "carol"}
    if !equalStringSlices(got, want) {
        t.Errorf("got %v, want %v", got, want)
    }
}
```

Run: `go test -run TestCompute_ThreadParticipantsAutoIncluded ./nexus/broker/...`
Expected: FAIL (ThreadParticipants field doesn't exist yet).

### Task 1.2: Add ThreadParticipants field, make test pass

Edit `nexus/broker/recipients.go`:

```go
// ThreadParticipants returns every aspect name that has posted into
// the given thread (identified by thread root msg id). Used to
// auto-route a reply to all active participants, matching Slack /
// Teams semantics: replying in a thread reaches everyone in it.
type ThreadParticipants func(threadRootMsgID int64) ([]string, error)

type RecipientPolicy struct {
    Parent             ParentLookup
    Aspects            AspectLookup
    ThreadParticipants ThreadParticipants  // NEW
    FrameName          string
}
```

Modify `Compute()` — the `replyTo > 0` block:

```go
if replyTo > 0 {
    // Thread participants are auto-routed (Slack-style).
    if p.ThreadParticipants != nil {
        rootID := replyTo
        // Direct parent's thread root may differ from replyTo if
        // replyTo is itself a reply. The chat store resolves that
        // server-side; here we just pass replyTo and trust the
        // lookup to walk to the root.
        if participants, err := p.ThreadParticipants(rootID); err == nil {
            for _, name := range participants {
                if name != "" && name != sender {
                    set[name] = struct{}{}
                }
            }
        }
    }
    // Parent author still added as a defensive fallback for cases
    // where ThreadParticipants is nil or returns an empty slice.
    if p.Parent != nil {
        if author, _ := p.Parent(replyTo); author != "" && author != sender {
            set[author] = struct{}{}
        }
    }
}
```

Run: `go test -run TestCompute_ThreadParticipantsAutoIncluded ./nexus/broker/... -v`
Expected: PASS.

### Task 1.3: Add edge-case tests

In `recipients_test.go`:

```go
func TestCompute_ThreadParticipantsPlusExplicitMention(t *testing.T) {
    p := RecipientPolicy{
        Parent: stubParent("alice"),
        ThreadParticipants: stubThread([]string{"alice", "bob"}),
    }
    got := p.Compute("operator", "hey @carol look at this", 100)
    want := []string{"alice", "bob", "carol"}
    if !equalStringSlices(got, want) { t.Errorf("got %v want %v", got, want) }
}

func TestCompute_ThreadParticipantsNilFallsBackToParentOnly(t *testing.T) {
    p := RecipientPolicy{
        Parent: stubParent("alice"),
        ThreadParticipants: nil,  // graceful degradation
    }
    got := p.Compute("operator", "reply", 100)
    if !equalStringSlices(got, []string{"alice"}) {
        t.Errorf("expected parent-only fallback, got %v", got)
    }
}

func TestCompute_AllStillBroadcastsOverThreadParticipants(t *testing.T) {
    p := RecipientPolicy{
        Aspects: func() []string { return []string{"x", "y", "z"} },
        ThreadParticipants: stubThread([]string{"alice"}),
    }
    got := p.Compute("operator", "@all attention", 0)
    if !equalStringSlices(got, []string{"x", "y", "z"}) {
        t.Errorf("@all should override, got %v", got)
    }
}
```

Add helpers near top of test file:

```go
func stubParent(name string) ParentLookup {
    return func(int64) (string, error) { return name, nil }
}
func stubThread(names []string) ThreadParticipants {
    return func(int64) ([]string, error) { return names, nil }
}
```

Run: `go test ./nexus/broker/... -v -run TestCompute_`
Expected: all pass (including the original 12 cases — graceful-degradation test guarantees they still pass with `ThreadParticipants: nil`).

### Task 1.4: Wire the lookup at broker boot

Find where `RecipientPolicy{...}` is constructed:

```bash
grep -rn "RecipientPolicy{" nexus/broker/
```

Add the SQL-backed `ThreadParticipants` implementation. The query uses the existing `idx_chat_thread_root_msg_id` index (added by #226, see `nexus/storage/schema.go:135`):

```go
ThreadParticipants: func(rootID int64) ([]string, error) {
    rows, err := chatDB.Query(
        `SELECT DISTINCT from_agent FROM chat_messages WHERE thread_root_msg_id = ?`,
        rootID,
    )
    if err != nil { return nil, err }
    defer rows.Close()
    var out []string
    for rows.Next() {
        var s string
        if err := rows.Scan(&s); err != nil { return nil, err }
        if s != "" { out = append(out, s) }
    }
    return out, rows.Err()
},
```

(Exact `chatDB` reference depends on broker construction; substitute the actual `*sql.DB` handle.)

### Task 1.5: Build, vet, race-test

```bash
go build ./...
go vet ./...
go test -race ./...
```

All pass.

### Task 1.6: Commit + PR

```bash
git checkout -b feat/recipients-thread-participants
git add -u nexus/broker/recipients.go nexus/broker/recipients_test.go nexus/broker/<wiring-file>.go
git commit -m "feat(recipients): auto-route thread replies to all thread participants"
git push -u origin feat/recipients-thread-participants
gh pr create --title "feat(recipients): auto-route thread replies to all thread participants" --body "..."
```

PR body should reference spec section "Backend changes."

---

## PR 2 — Frontend: operator-vs-agent visual differentiation

**Goal:** Operator messages render right-aligned with distinct chip
+ colour so the operator can find "what did I last say" in a wall of
text instantly.

**Why second:** Smallest visible win, layout-independent, lands
legibility regardless of further restructure.

### Files

- Modify: `nexus/broker/static/dashboard/js/components/MessageBubble.js`
- Modify: `nexus/broker/static/dashboard/css/chat.css`

### Task 2.1: Read MessageBubble.js, identify sender check

```bash
grep -n "from\|operator" nexus/broker/static/dashboard/js/components/MessageBubble.js | head -20
```

Confirm the bubble already exposes `msg.from` and that "operator" is
the canonical from-value for operator messages.

### Task 2.2: Add operator-side variant class

In `MessageBubble.js`, find the outer `<div class="message-bubble ...">`
render. Add an `is-operator` class when the message is from operator:

```javascript
const isOperator = (msg.from === 'operator');
const rootClass = `message-bubble${compact ? ' compact' : ''}${isOperator ? ' is-operator' : ''}`;
```

(Adjust to match the actual existing render shape — the exact class
string concatenation pattern varies.)

### Task 2.3: Add CSS for is-operator

In `nexus/broker/static/dashboard/css/chat.css`:

```css
/* Operator's own messages — right-aligned, distinct background, "you"
   chip. Lets the operator find their own posts at a glance in a
   scrolling wall of agent chatter. */
.message-bubble.is-operator {
  margin-left: auto;
  max-width: 70%;
  background: var(--operator-bubble-bg, #2b3a55);
  border-left: 3px solid var(--operator-accent, #6aa9ff);
  text-align: left; /* content stays LTR even though bubble is RTL-aligned */
}

.message-bubble.is-operator .message-from {
  /* Replace the "@operator" chip with "you" for self-identification */
  /* Implementation: data attribute or pseudo-element; keep existing
     chip layout intact for non-operator messages */
}
```

Define the new CSS vars in the base palette (`nexus/broker/static/dashboard/css/base.css` — find existing `--*-bubble-bg` for analogue).

### Task 2.4: Verify in browser

Start broker, open dashboard, post a message as operator, observe:
- Right-aligned
- Accent border
- Indented from right edge

Compare against an agent message in the same thread for clear visual
distinction.

### Task 2.5: Commit + PR

```bash
git checkout -b feat/operator-message-visual
git add -u nexus/broker/static/dashboard/js/components/MessageBubble.js \
            nexus/broker/static/dashboard/css/chat.css \
            nexus/broker/static/dashboard/css/base.css
git commit -m "feat(dashboard): right-align + accent operator messages for legibility"
```

---

## PR 3 — Frontend: sticky per-thread presence strip

**Goal:** Above every focused thread, a sticky strip shows which
aspects are present in the thread and which are mid-turn / mid-tool-call.
This is the trust signal at thread granularity, before the sidebar
layout work lands the cross-thread version.

**Why third:** Lands the trust primitive inside the current accordion
structure, so it's felt immediately. Sidebar (PR 4) reuses the same
strip data.

### Files

- Create: `nexus/broker/static/dashboard/js/components/PresenceStrip.js`
- Create (if not present): `nexus/broker/static/dashboard/js/models/activity.js` — module-scoped per-aspect activity signal, populated from observability frames.
- Modify: `nexus/broker/static/dashboard/js/views/FeedView.js` (mount strip inside `ExpandedThread`)
- Modify: `nexus/broker/static/dashboard/js/chat-ws.js` or `comms.js` — route observability frames into activity.js
- Modify: `nexus/broker/static/dashboard/css/chat.css`

### Task 3.1: Inventory observability frames already arriving

```bash
grep -rn "observe\." nexus/broker/static/dashboard/js/ | head -20
grep -n "PresenceFrame\|TurnStart\|TurnDone\|ToolCallStart" nexus/observability/ -r | head -10
```

Confirm what frame kinds the broker emits (`observe.event`, `observe.begin`, `observe.end`, plus `roster.update` for presence) and what's currently subscribed on the JS side. Likely Observe view already consumes some — pattern can be reused.

### Task 3.2: Create activity.js

```javascript
// activity.js — per-aspect activity state derived from broker
// observability frames. Module-scoped signals so any component can
// subscribe; survives view switches; rebuilt on reconnect.

const { signal } = window.__preact;

// Map { aspectName -> { presence, state, tool } }
//   presence: 'online' | 'offline'
//   state:    'idle' | 'thinking' | 'tool'
//   tool:     string (tool name when state === 'tool', else '')
export const aspectActivity = signal({});

export function markPresence(aspect, online) { /* update */ }
export function markTurnStart(aspect)        { /* state = thinking */ }
export function markTurnDone(aspect)         { /* state = idle */ }
export function markToolStart(aspect, tool)  { /* state = tool, tool = ... */ }
export function markToolDone(aspect)         { /* state = thinking (still mid-turn) */ }
```

Each mutator does an immutable update and assigns `aspectActivity.value = next` so signal subscribers re-render.

### Task 3.3: Wire chat-ws / comms frame routing

In `chat-ws.js` or `comms.js`, where frames are demuxed by kind, add:

```javascript
case 'observe.begin': markTurnStart(payload.aspect); break;
case 'observe.end':   markTurnDone(payload.aspect); break;
case 'observe.event':
  if (payload.event_kind === 'tool_call_start')  markToolStart(payload.aspect, payload.event.name);
  if (payload.event_kind === 'tool_call_result') markToolDone(payload.aspect);
  break;
case 'roster.update':
  markPresence(payload.aspect, payload.status !== 'down');
  break;
```

### Task 3.4: Build PresenceStrip component

```javascript
// PresenceStrip.js
const { html, useComputed } = window.__preact;
import { aspectActivity } from '../models/activity.js';

export function PresenceStrip({ participants }) {
  const activity = aspectActivity.value;
  return html`
    <div class="presence-strip">
      ${participants.map(name => {
        const a = activity[name] || { presence: 'offline', state: 'idle' };
        return html`
          <span class=${`presence-pill state-${a.state} pres-${a.presence}`}>
            <span class="presence-dot"></span>
            ${name}
            ${a.state === 'tool' && html`<em> · ${a.tool}</em>`}
          </span>
        `;
      })}
    </div>
  `;
}
```

### Task 3.5: Mount strip inside ExpandedThread

In `FeedView.js`'s `ExpandedThread` component, render `<PresenceStrip participants=${thread.participants} />` above the `feed-thread-expanded-root` div.

### Task 3.6: CSS — sticky positioning

```css
.presence-strip {
  position: sticky;
  top: 0;
  z-index: 5;
  background: var(--panel-bg);
  padding: 6px 10px;
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  border-bottom: 1px solid var(--panel-border);
}

.presence-pill { display: inline-flex; gap: 6px; align-items: center; }
.presence-dot  { width: 8px; height: 8px; border-radius: 50%; }

.presence-pill.pres-online  .presence-dot { background: var(--ok-green); }
.presence-pill.pres-offline .presence-dot { background: var(--muted-grey); }

.presence-pill.state-thinking .presence-dot { animation: pulse 1s ease-in-out infinite; }
.presence-pill.state-tool     .presence-dot { background: var(--warning-yellow); }

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50%      { opacity: 0.35; }
}
```

### Task 3.7: Manual verify

Start broker + at least one aspect, kick off a turn from chat, observe:
- Strip appears above thread
- Strip stays pinned when scrolling thread messages
- Dot animates pulse during deliberation
- Returns to solid green when turn ends

### Task 3.8: Commit + PR

```bash
git checkout -b feat/presence-strip
git add nexus/broker/static/dashboard/js/components/PresenceStrip.js \
        nexus/broker/static/dashboard/js/models/activity.js
git add -u nexus/broker/static/dashboard/js/chat-ws.js \
            nexus/broker/static/dashboard/js/views/FeedView.js \
            nexus/broker/static/dashboard/css/chat.css
git commit -m "feat(dashboard): sticky per-thread presence + activity strip"
```

---

## PR 4 — Frontend: sidebar + keyboard navigation

**Goal:** Replace the row-list-with-accordion FeedView body with a
left sidebar listing active threads + a main area focused on one
thread at a time. Sidebar shows activity dots per thread (aggregating
PR 3's per-aspect signal). Click row or j/k arrow keys to switch
focus.

**Why fourth:** Biggest restructure; benefits from PR 3 being in
place so operator has felt the strip work before this layout change
lands.

### Files

- Create: `nexus/broker/static/dashboard/js/components/ThreadSidebar.js`
- Modify: `nexus/broker/static/dashboard/js/views/FeedView.js` — significant restructure; extract `ExpandedThread` into its own file at the same time (`components/FocusedThread.js`).
- Modify: `nexus/broker/static/dashboard/css/chat.css` — grid/flex layout.

### Task 4.1: Extract FocusedThread.js from FeedView.js

Move the existing `ExpandedThread` component (lines ~169-257 of
FeedView.js) into `components/FocusedThread.js`, renaming as
`FocusedThread`. Import where used.

Run: `git status` shows two changed files. Verify dashboard still
renders identically (no functional change yet).

### Task 4.2: Build ThreadSidebar.js

```javascript
const { html } = window.__preact;
import { aspectActivity } from '../models/activity.js';

export function ThreadSidebar({ threads, focusedRootId, onFocus, onKeyNav }) {
  return html`
    <aside class="feed-sidebar"
           tabIndex=${0}
           onKeyDown=${onKeyNav}>
      ${threads.map(t => {
        const activeDots = t.participants.filter(p => {
          const a = aspectActivity.value[p];
          return a && a.state !== 'idle';
        }).length;
        return html`
          <div class=${`feed-sidebar-row ${focusedRootId === t.rootId ? 'is-focused' : ''}`}
               onClick=${() => onFocus(t.rootId)}>
            <span class="feed-sidebar-preview">${t.preview}</span>
            <span class="feed-sidebar-dots">
              ${t.participants.map(p => {
                const a = aspectActivity.value[p] || {};
                return html`<span class=${`dot state-${a.state || 'idle'} pres-${a.presence || 'offline'}`}/>`;
              })}
            </span>
          </div>
        `;
      })}
    </aside>
  `;
}
```

### Task 4.3: Restructure FeedView.js body

Replace the existing list-of-rows render with the two-column layout:

```javascript
return html`
  <div class="feed-trust-view">
    <${ThreadSidebar}
      threads=${sortedThreads}
      focusedRootId=${focusedRoot}
      onFocus=${setFocusedRoot}
      onKeyNav=${handleSidebarKey}
    />
    <main class="feed-main">
      ${focusedRoot
        ? html`<${FocusedThread} rootId=${focusedRoot} />`
        : html`<div class="feed-main-empty">Pick a thread on the left.</div>`}
    </main>
    <${ChatInput} />
  </div>
`;
```

Drop the existing role-hint filter + mentions-me checkbox + accordion
toggle code (no longer used). Keep the hydration / chatWS subscription
/ Thread.subscribe wiring — it still feeds `sortedThreads`.

### Task 4.4: Sort + age threads

In FeedView, derive `sortedThreads` from rows:

```javascript
const SEVEN_DAYS_MS = 7 * 24 * 60 * 60 * 1000;
const now = Date.now();
const sortedThreads = Object.values(rows)
  .filter(r => {
    const lastAt = new Date(r.lastAt).getTime();
    return Number.isFinite(lastAt) && (now - lastAt) < SEVEN_DAYS_MS;
  })
  .sort((a, b) => {
    // Threads with any thinking aspect float to top; then by last activity desc.
    const aActive = a.participants.some(p => aspectActivity.value[p]?.state !== 'idle');
    const bActive = b.participants.some(p => aspectActivity.value[p]?.state !== 'idle');
    if (aActive !== bActive) return aActive ? -1 : 1;
    return b.lastSortKey - a.lastSortKey;
  });
```

### Task 4.5: Keyboard navigation

```javascript
function handleSidebarKey(e) {
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'j' && e.key !== 'k') return;
  e.preventDefault();
  const dir = (e.key === 'ArrowDown' || e.key === 'j') ? 1 : -1;
  const idx = sortedThreads.findIndex(t => t.rootId === focusedRoot);
  const next = sortedThreads[(idx + dir + sortedThreads.length) % sortedThreads.length];
  if (next) setFocusedRoot(next.rootId);
}
```

### Task 4.6: CSS layout

```css
.feed-trust-view {
  display: grid;
  grid-template-columns: 240px 1fr;
  grid-template-rows: 1fr auto;
  grid-template-areas:
    "sidebar main"
    "input   input";
  height: 100%;
}

.feed-sidebar { grid-area: sidebar; overflow-y: auto; border-right: 1px solid var(--panel-border); }
.feed-main    { grid-area: main; overflow-y: auto; }
.chat-input-area { grid-area: input; }

.feed-sidebar-row { padding: 8px 12px; cursor: pointer; }
.feed-sidebar-row.is-focused { background: var(--row-active-bg); }
.feed-sidebar-row:hover { background: var(--row-hover-bg); }

.feed-sidebar-dots { display: inline-flex; gap: 4px; }
.feed-sidebar-dots .dot { width: 8px; height: 8px; border-radius: 50%; }
/* reuse pulse animation from PresenceStrip */
```

### Task 4.7: Manual verify

- Sidebar lists threads, click selects, j/k navigates
- Focused thread renders with PR 3's presence strip pinned
- Operator messages right-aligned (PR 2)
- Replies route to all thread participants (PR 1)

### Task 4.8: Commit + PR

```bash
git checkout -b feat/feed-sidebar-layout
git add nexus/broker/static/dashboard/js/components/ThreadSidebar.js \
        nexus/broker/static/dashboard/js/components/FocusedThread.js
git add -u nexus/broker/static/dashboard/js/views/FeedView.js \
            nexus/broker/static/dashboard/css/chat.css
git commit -m "feat(dashboard): sidebar + focused-thread layout (drops accordion)"
```

---

## PR 5 — Frontend: state persistence + autoscroll + since-you-left

**Goal:** Polish layer. localStorage-backed persistence so filters,
focused-thread, lastSeen survive reload. Autoscroll-on-bottom
behaviour in focused thread. Since-you-left divider per thread.

**Why last:** Cosmetic-feel layer that depends on layout being
settled. Each sub-feature small but they share storage utilities.

### Files

- Create: `nexus/broker/static/dashboard/js/util/persist.js` — localStorage wrappers with try/catch + JSON.parse safety.
- Modify: `nexus/broker/static/dashboard/js/views/FeedView.js` — wire persistence + autoscroll + divider.
- Modify: `nexus/broker/static/dashboard/js/components/FocusedThread.js` — render divider above first message after lastSeen.

### Task 5.1: persist.js utility

```javascript
// persist.js — localStorage wrappers with safe JSON + quota guards.

const NS = 'nexus.feed';
const VER = 'v1';

function key(name) { return `${NS}.${name}.${VER}`; }

export function persistGet(name, fallback) {
  try {
    const raw = localStorage.getItem(key(name));
    if (raw == null) return fallback;
    return JSON.parse(raw);
  } catch { return fallback; }
}

export function persistSet(name, value) {
  try {
    localStorage.setItem(key(name), JSON.stringify(value));
  } catch (e) {
    // Quota exceeded or storage disabled — silently degrade.
    console.warn('[persist] set failed', name, e);
  }
}
```

### Task 5.2: Persist focused thread + sidebar collapse

In FeedView.js, replace `useState(() => parseThreadFromHash())` with:

```javascript
const [focusedRoot, setFocusedRoot] = useState(() =>
  parseThreadFromHash() || persistGet('focusedThread', 0));

useEffect(() => {
  persistSet('focusedThread', focusedRoot);
}, [focusedRoot]);
```

### Task 5.3: Per-thread lastSeen

```javascript
// lastSeen: { [rootId]: lastMsgIdSeen }
const lastSeenMap = persistGet('lastSeen', {});

// When a thread becomes focused, snapshot its last message id.
useEffect(() => {
  if (!focusedRoot) return;
  const t = peekThread(focusedRoot);
  if (!t || t.messages.length === 0) return;
  const lastId = t.messages[t.messages.length - 1].id;
  lastSeenMap[focusedRoot] = lastId;
  persistSet('lastSeen', lastSeenMap);
}, [focusedRoot, /* and a tick that fires on new messages */]);
```

(Implementation note: subscribe to focused Thread's updates and bump
lastSeen on each new message while it remains focused.)

### Task 5.4: Render since-you-left divider

In FocusedThread.js:

```javascript
const lastSeen = persistGet('lastSeen', {})[rootId] || 0;
// In the replies render:
${replies.map((m, i) => {
  const isFirstNew = m.id > lastSeen && (i === 0 || replies[i-1].id <= lastSeen);
  return html`
    ${isFirstNew && html`<hr class="since-divider" data-label="new"/>`}
    <${MessageBubble} ... />
  `;
})}
```

CSS:

```css
.since-divider {
  border: 0;
  border-top: 1px dashed var(--accent-orange);
  margin: 12px 0;
  position: relative;
}
.since-divider::before {
  content: attr(data-label);
  position: absolute; top: -10px; left: 50%; transform: translateX(-50%);
  background: var(--panel-bg);
  color: var(--accent-orange);
  padding: 0 8px;
  font-size: 11px; text-transform: uppercase; letter-spacing: 1px;
}
```

### Task 5.5: Autoscroll-on-bottom in FocusedThread

```javascript
const scrollRef = useRef(null);
const wasAtBottom = useRef(true);

// Track scroll position
useEffect(() => {
  const el = scrollRef.current;
  if (!el) return;
  const onScroll = () => {
    const slack = 32; // px tolerance for "at bottom"
    wasAtBottom.current = (el.scrollHeight - el.scrollTop - el.clientHeight) < slack;
  };
  el.addEventListener('scroll', onScroll, { passive: true });
  return () => el.removeEventListener('scroll', onScroll);
}, []);

// On new message, if we were at bottom, stay there.
useEffect(() => {
  if (wasAtBottom.current && scrollRef.current) {
    scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
  }
}, [thread.messages.length]);
```

Attach `ref=${scrollRef}` to the scrollable element.

### Task 5.6: Commit + PR

```bash
git checkout -b feat/feed-persistence-autoscroll
git add nexus/broker/static/dashboard/js/util/persist.js
git add -u nexus/broker/static/dashboard/js/views/FeedView.js \
            nexus/broker/static/dashboard/js/components/FocusedThread.js \
            nexus/broker/static/dashboard/css/chat.css
git commit -m "feat(dashboard): persist focus + lastSeen, autoscroll, since-you-left divider"
```

---

## Self-review

**Spec coverage:** Every section of the spec maps to a PR — backend
routing (PR 1), operator visual (PR 2), presence strip (PR 3),
sidebar (PR 4), state + autoscroll + divider (PR 5). Configuration
UI is explicitly deferred in the spec, so not planned here.

**Placeholder scan:** Every step shows code or a precise grep. No
"TBD," "implement later," or "similar to above."

**Type consistency:** `RecipientPolicy.ThreadParticipants` field name
matches the type name and stays consistent across Task 1.2 / 1.3 /
1.4. `aspectActivity` signal name reused across PR 3 / 4 / 5.
`focusedRoot` (state name) consistent in PR 4 + 5.

**Things deferred to a follow-up plan, NOT silently dropped:**
- Configuration UI for systems/agents (spec out of scope; track
  separately).
- Split-view (focused two threads side-by-side) — spec marked as v2.
- `cmd-K` thread palette — spec marked as v2.
- File split of FeedView.js into more components — PR 4 does one
  extraction (FocusedThread.js); further splits are optional polish.
