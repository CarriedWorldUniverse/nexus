# One-to-One Observability Plan — Per-Agent Activity View

**Status:** ⚠️ SUPERSEDED by `2026-05-12-nexus-watch-and-observability-core.md` ⚠️
**Date:** 2026-05-11
**Author:** Plumb
**Supersedes:** the holdover `AgentsView` (per-agent DM list, unused) and the `Terminal` view (xterm.js wired to a PTY model that no longer applies)

> **Why superseded:** This plan put the event-grouping logic in client-side JS, duplicating what Keel's `2026-05-10-chatgpt-mode-shape.md` already implied should be substrate-level. After operator added the requirement for a terminal renderer (`nexus-watch`), the right shape became a **shared Go observability core** that both dashboard and terminal consume — see the 2026-05-12 plan. This doc retained for reference (TurnBlock / FileDiffArtifact shapes are still relevant inputs to the new design).

## 1. Why this changes shape

The dashboard surfaces chat (the agent's front door) but nothing about what the agent is actually *doing* to produce a reply. Today, when plumb processes a message:

```
operator: "@plumb status?"
plumb:    "all good"            ← all the operator sees
```

What's invisible: which files plumb read, which tools it ran, what its reasoning was between read-tool-call and reply, how many tokens it spent, how long it took. That data exists — claude-code emits a structured event stream (`tool_use`, `tool_result`, `text`, `turn_start`, `turn_end`) and the broker already exposes it at `/harness/<agent>/stream` (SSE) — but it surfaces only inside Terminal view as a sidekick pane (`HarnessActivity.js`) below an xterm.js terminal that no longer reflects a meaningful runtime.

Operator's framing: "ChatGPT-style — see the tool calls, file diffs, everything — built from the turn jsonl, live as the agent works." This plan promotes the existing event stream to a first-class per-agent observability view, redesigns the renderer for daily-driver use, and retires the now-dead Terminal + AgentsView surfaces.

## 2. Architecture — already mostly built

The data plane is shipped and stable. The work is presentation.

**Server (no changes):**

```
bridle EventSink (claudecode subprocess-stream or claude-api native)
   ├─ funnel (internal bookkeeping)
   └─ broker.PublishAspectEvent
          ↓
   GET /harness/<agent>/tail?limit=N        ← one-shot history seed
   GET /harness/<agent>/stream              ← SSE live stream
```

Event shapes emitted today (see `HarnessActivity.js` `ActivityRow`):

```
{ kind: "turn_start",  threadId, msgId }
{ kind: "tool_use",    name, input }
{ kind: "tool_result", preview, is_error }
{ kind: "text",        text }
{ kind: "turn_end" }
```

**Client (already built):**

- `js/harness-stream-store.js` — persistent EventSource per agent, 500-event ring buffer, seed-from-tail on subscribe, reconnect with exponential backoff, grace-period teardown when subscriber count drops to zero.
- `js/components/HarnessActivity.js` — flat row-per-event renderer with sticky-bottom scroll and seeded-vs-live divider.

**What's missing (this plan delivers):**

1. A first-class view that uses this data plane.
2. A renderer that groups events by turn, allows expanding tool calls to see full input/output, renders file edits as diff artifacts, and links turn triggers back to the originating chat message.

## 3. Decisions locked

| # | Decision | Rationale |
|---|---|---|
| D1 | **Flowing chronological render, not collapsed-per-turn.** Each turn is visually demarcated by a divider with metadata; tool rows are inline by default; click-to-expand reveals full input + matched result. | Matches the operator's "surface everything" pattern (mirror of the chat-view flat-render decision). Operator skims live what plumb is doing; expand on-demand for detail. Collapsing-per-turn forces a click before any signal is visible. |
| D2 | **Replace `AgentsView`** at the existing `#/agents/<id>` route. Same URL, new content. | Preserves operator muscle memory; bottom-bar nav slot stays. |
| D3 | **Delete the Terminal view, its component, CSS, and xterm vendor JS.** Free the nav slot. Server-side `/proxy/<agent>/output` endpoint stays for now (audit removal in a follow-up — nothing else should reference it, but I'll confirm before deleting on the server side). | Operator: "Terminal is mostly pointless in this iteration." Saves dashboard download size (xterm.js is ~200KB). |
| D4 | **Use existing data plane unchanged.** No broker changes. | The event stream + buffer + reconnect are production-grade. Any presentation-layer features should fit the existing shape. |
| D5 | **Tool detail = inline collapsed preview, click-to-expand.** Default state shows tool name + first ~80 chars of input. Click reveals full input + matched tool_result inline below. | Mirrors the pattern Cursor/Claude desktop use. Operator can scan a turn's tool sequence without expanding anything. |
| D6 | **File edits rendered as diff artifacts.** When `tool_use.name` is `Edit`, `Write`, `MultiEdit`, or `NotebookEdit`, parse `input` for `file_path` + `old_string` + `new_string` and render a unified-diff panel. | This is the *most* operationally valuable use of the view — operator can see what plumb is changing in real time. |
| D7 | **Chat-trigger linkage.** `turn_start.msgId` becomes a clickable ref that navigates to `#/chat` and scrolls to the originating message (re-use existing `handleMsgRefClick` infra from `Chat.js`). | Closes the loop: from "what is plumb doing" back to "why" in one click. |
| D8 | **Multi-agent overview deferred.** v1 is per-agent timeline only. The existing `Feed` view kind of already does cross-agent activity; revisit after v1 ships. | Keeps scope bounded. |

## 4. File structure

### Created

```
nexus/broker/static/dashboard/
  js/views/
    ObserveView.js                  ← new: per-agent activity timeline (replaces AgentsView)
  js/components/
    TurnBlock.js                    ← new: groups + renders events of one turn
    ToolCall.js                     ← new: collapsible tool_use row + matched tool_result
    FileDiffArtifact.js             ← new: Edit/Write tool_use rendered as unified diff
  css/
    observe.css                     ← new: layout + turn dividers + artifact panels
```

### Modified

```
nexus/broker/static/dashboard/
  index.html                        ← link observe.css, drop xterm.css + terminal.css
  js/app.js                         ← route #/agents/<id> → ObserveView; remove #/terminal route
  js/components/BottomBar.js        ← drop Terminal tab; rename Agents tab "Activity"
  js/icons.js                       ← (maybe) new IconActivity icon
```

### Deleted

```
nexus/broker/static/dashboard/
  js/views/AgentsView.js
  js/views/Terminal.js
  js/components/HarnessActivity.js  ← logic absorbed into ObserveView + TurnBlock
  css/terminal.css
  js/vendor/xterm.css
  js/vendor/xterm.js
  js/vendor/xterm-esm.js
  js/vendor/addon-fit.js
  js/vendor/addon-fit-esm.js
  js/vendor/addon-web-links.js
```

`harness-stream-store.js` is **not** modified — its API (`subscribe(agentId, cb)`, `getSnapshot(agentId)`) is exactly what ObserveView needs.

## 5. Pre-implementation discovery

These need verification at the start of Task 2 — they shape rendering decisions but the existing `HarnessActivity.js` only renders a subset of the actual broker payload, so the real shape is wider than what's currently used:

- **`turn_start` payload**: confirmed has `threadId`, `msgId`. Does it also carry `model`, `provider`, `started_at`? Check `nexus/broker/` for the publish path or `nexus/frame/funnel/` for the emit site.
- **`turn_end` payload**: HarnessActivity renders it bare. Likely the broker emits `tokens` / `duration` / `cumulative` per the funnel logs I've seen (`funnel: turn complete steps=0 tool_calls=0 input_tokens=5 output_tokens=253 ...`). Confirm the published event includes these.
- **`tool_use` ↔ `tool_result` pairing**: claude-code's stream-json gives `tool_use_id`; check the broker forwards it. If yes → exact pairing. If no → positional pairing (next tool_result belongs to last tool_use), fragile but works.
- **`tool_use.input`**: today rendered as `${ev.input || ''}` — probably a string. For `Edit`/`Write` we need the structured fields. Check whether `input` is JSON-stringified args or the raw `arguments` object.

These discoveries happen in Task 2 at the start, before committing to the precise `TurnBlock` data shape. If something material is missing server-side, Task 2 surfaces it and either patches the broker emit path (small) or adapts the renderer to work with what's there.

## 6. Tasks

Each task is one logical commit. Tests are JS-only and not currently a thing in this SPA — the dashboard ships as static files and is tested visually. Verification is "load the page, do the thing, check it works."

### Task 1 — Decommission Terminal view

**Files:**
- Delete `js/views/Terminal.js`
- Delete `css/terminal.css`, `js/vendor/xterm.css`, `js/vendor/xterm.js`, `js/vendor/xterm-esm.js`, `js/vendor/addon-fit*.js`, `js/vendor/addon-web-links.js`
- Modify `index.html`: remove the two `<link>` lines for terminal.css and xterm.css
- Modify `js/components/BottomBar.js`: remove the `terminal` tab entry from the `TABS` array
- Modify `js/app.js`: remove any `#/terminal` route handling

**Verification:** dashboard loads without console errors, no Terminal tab, no 404s for xterm assets in network panel.

**Commit message:** `chore(dashboard): retire Terminal view — xterm sidekick replaced by observability stream`

### Task 2 — ObserveView skeleton + agent picker

**Files:**
- Create `js/views/ObserveView.js`
- Delete `js/views/AgentsView.js`
- Modify `js/app.js`: route `#/agents` and `#/agents/<id>` to ObserveView
- Modify `js/components/BottomBar.js`: relabel `agents` tab to `Activity` (keep route id `agents` for URL continuity)

**Code shape (ObserveView.js):**

```javascript
const { html, useState, useEffect, useRef } = window.__preact;
import { agents, agentColors } from '../state.js';
import { subscribe, getSnapshot } from '../harness-stream-store.js';
import { TurnBlock } from '../components/TurnBlock.js';

export function ObserveView() {
  const agentList = agents.value;

  function agentFromHash() {
    const hash = window.location.hash;
    if (hash.startsWith('#/agents/')) return hash.slice('#/agents/'.length);
    return null;
  }

  const [selectedAgent, setSelectedAgent] = useState(
    () => agentFromHash() || (typeof agentList[0] === 'string' ? agentList[0] : agentList[0]?.id) || null
  );
  const [, forceRender] = useState(0);
  const scrollRef = useRef(null);
  const stickBottomRef = useRef(true);

  useEffect(() => {
    if (!selectedAgent) return;
    stickBottomRef.current = true;
    const unsub = subscribe(selectedAgent, () => forceRender(v => v + 1));
    return unsub;
  }, [selectedAgent]);

  // Sticky-bottom auto-scroll (lifted from HarnessActivity.js — same behaviour)
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const onScroll = () => {
      const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
      stickBottomRef.current = nearBottom;
    };
    el.addEventListener('scroll', onScroll, { passive: true });
    return () => el.removeEventListener('scroll', onScroll);
  }, []);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (stickBottomRef.current) el.scrollTop = el.scrollHeight;
  });

  const snap = selectedAgent ? getSnapshot(selectedAgent) : { events: [], connected: false };
  const turns = groupEventsByTurn(snap.events);

  function selectAgent(id) {
    setSelectedAgent(id);
    window.location.hash = '#/agents/' + id;
  }

  return html`
    <div class="observe-view">
      <div class="observe-agent-picker">
        ${agentList.map(agent => {
          const id = typeof agent === 'string' ? agent : agent.id;
          const alive = typeof agent === 'object' ? agent.alive : true;
          const color = (agentColors.value || {})[id] || '#888';
          return html`
            <button
              key=${id}
              class=${'observe-agent-btn' + (selectedAgent === id ? ' active' : '')}
              style=${{ '--agent-color': color }}
              onClick=${() => selectAgent(id)}
            >
              <span class=${'observe-agent-dot' + (alive ? ' alive' : '')} style=${{ background: alive ? color : '#444' }}></span>
              <span>${id}</span>
            </button>
          `;
        })}
      </div>
      <div class="observe-stream" ref=${scrollRef}>
        ${!selectedAgent
          ? html`<div class="observe-empty">Pick an agent to observe.</div>`
          : turns.length === 0
            ? html`<div class="observe-empty">${snap.connected ? 'waiting for activity…' : 'connecting…'}</div>`
            : turns.map(turn => html`<${TurnBlock} key=${turn.key} turn=${turn} agentId=${selectedAgent} />`)
        }
      </div>
    </div>
  `;
}

// Groups the flat event list into turn-bounded buckets. Each bucket is
// everything between turn_start (inclusive) and turn_end (inclusive),
// plus a synthetic "pre-turn" bucket for events that arrived before any
// turn_start (rare — usually only seeded history). Events without turn
// boundaries (e.g. legacy emitters) fall through into an "ungrouped"
// bucket so they still render.
function groupEventsByTurn(events) {
  const turns = [];
  let current = null;
  let preTurn = [];
  for (const ev of events) {
    if (ev.kind === 'turn_start') {
      if (preTurn.length > 0) {
        turns.push({ key: 'pre-' + (preTurn[0]._seq), kind: 'pre', start: null, events: preTurn, end: null });
        preTurn = [];
      }
      if (current) turns.push(current);
      current = { key: 't-' + ev._seq, kind: 'turn', start: ev, events: [], end: null };
    } else if (ev.kind === 'turn_end') {
      if (current) {
        current.end = ev;
        turns.push(current);
        current = null;
      } else {
        preTurn.push(ev);
      }
    } else {
      if (current) current.events.push(ev);
      else preTurn.push(ev);
    }
  }
  if (preTurn.length > 0) {
    turns.push({ key: 'pre-' + (preTurn[0]._seq), kind: 'pre', start: null, events: preTurn, end: null });
  }
  if (current) turns.push(current); // in-flight turn (no end yet)
  return turns;
}
```

**Verification:** navigate to `#/agents/plumb` with plumb running. See the agent picker, see "waiting for activity…" or seeded events from /tail. No console errors.

**Commit message:** `feat(dashboard): ObserveView — per-agent activity timeline replaces AgentsView`

### Task 3 — TurnBlock + ToolCall (collapsible tool detail)

**Files:**
- Create `js/components/TurnBlock.js`
- Create `js/components/ToolCall.js`
- Delete `js/components/HarnessActivity.js` (its event rendering moves into TurnBlock + ToolCall)

**TurnBlock structure:**

A turn renders as a `<section>` with:

1. **Header** (only for `kind === 'turn'`):
   - Divider line + small caption: relative time (e.g. "2m ago"), "triggered by msg #NNN" (link), optionally model+provider when present in payload
2. **Body** — flat list of rows, one per event:
   - `text` → indented italic block (model reasoning / final reply preceding the auto-post)
   - `tool_use` → `<${ToolCall}>` with the matched `tool_result` from later in the bucket
   - Any `tool_result` not matched (orphan) renders as its own row (defensive — shouldn't happen if pairing works)
3. **Footer** (only when `turn.end` is set):
   - Tokens (input / output / cache_read if present), duration, error flag if `is_error` in any tool_result

**ToolCall structure:**

```javascript
export function ToolCall({ use, result }) {
  const [expanded, setExpanded] = useState(false);
  const isError = !!(result && result.is_error);
  const isEdit = use.name === 'Edit' || use.name === 'Write' || use.name === 'MultiEdit' || use.name === 'NotebookEdit';

  return html`
    <div class=${'tool-call' + (isError ? ' is-error' : '') + (expanded ? ' expanded' : '')}>
      <button class="tool-call-summary" onClick=${() => setExpanded(e => !e)}>
        <span class="tool-call-icon">${isError ? '❌' : '🔧'}</span>
        <span class="tool-call-name">${use.name}</span>
        <span class="tool-call-preview">${previewOf(use.input)}</span>
        ${result && html`<span class="tool-call-result-preview">→ ${result.preview || ''}</span>`}
        <span class="tool-call-chevron">${expanded ? '▾' : '▸'}</span>
      </button>
      ${expanded && html`
        <div class="tool-call-detail">
          ${isEdit ? html`<${FileDiffArtifact} input=${use.input} />` : html`
            <div class="tool-call-input">
              <div class="tool-call-detail-label">input</div>
              <pre>${formatInput(use.input)}</pre>
            </div>
          `}
          ${result && html`
            <div class="tool-call-output">
              <div class="tool-call-detail-label">output${result.is_error ? ' (error)' : ''}</div>
              <pre>${formatResult(result)}</pre>
            </div>
          `}
        </div>
      `}
    </div>
  `;
}
```

**Pairing strategy:** if `tool_use` events carry `id` (claude-code's `tool_use_id`) and `tool_result` events carry matching `tool_use_id`, pair by ID. Else fall back to positional pairing in `TurnBlock` (walk the turn's events, match the next `tool_result` to the most recent unpaired `tool_use`).

**Verification:** Plumb runs a turn that reads a file. ObserveView shows: turn_start divider → tool_use "Read" row (collapsed) → click expands → input shows the file path, output shows the file contents (truncated to preview length unless we want to extend).

**Commit message:** `feat(dashboard): TurnBlock + ToolCall — group events by turn, collapsible tool detail`

### Task 4 — FileDiffArtifact (Edit/Write rendering)

**Files:**
- Create `js/components/FileDiffArtifact.js`

**Behaviour:** when `tool_use.name` is `Edit`/`Write`/`MultiEdit`/`NotebookEdit`, parse `input` for the file path and the change content. Render a unified-diff panel:

- File path as a header (use a code-styled monospace look)
- For `Edit`: render `old_string` lines prefixed with `-` and styled red, `new_string` lines prefixed with `+` and styled green. Limit to ~30 lines visible by default with "show all" expander if longer.
- For `Write`: render the new content as a single green block (no old, this is a new file or full overwrite).
- For `MultiEdit`: render each edit's old/new pair sequentially.
- For `NotebookEdit`: render `new_source` as green; if `edit_mode` is `replace` (default) and an `old_source` is present (it isn't — claude-code doesn't carry it), render as Write-style green block.

**Code shape:**

```javascript
export function FileDiffArtifact({ input }) {
  let parsed;
  try { parsed = typeof input === 'string' ? JSON.parse(input) : input; }
  catch { return html`<pre class="diff-fallback">${String(input)}</pre>`; }

  const filePath = parsed.file_path || parsed.notebook_path || '?';
  // Branch on shape — Edit has old_string + new_string; Write has content;
  // MultiEdit has edits[]; NotebookEdit has new_source.
  return html`
    <div class="file-diff-artifact">
      <div class="file-diff-path">${filePath}</div>
      <div class="file-diff-body">
        ${renderDiffBody(parsed)}
      </div>
    </div>
  `;
}

function renderDiffBody(parsed) {
  if (parsed.edits) return parsed.edits.map((e, i) => html`<${UnifiedDiff} key=${i} oldText=${e.old_string} newText=${e.new_string} />`);
  if (parsed.old_string != null) return html`<${UnifiedDiff} oldText=${parsed.old_string} newText=${parsed.new_string} />`;
  if (parsed.content) return html`<pre class="diff-add">${parsed.content}</pre>`;
  if (parsed.new_source) return html`<pre class="diff-add">${parsed.new_source}</pre>`;
  return html`<pre class="diff-fallback">${JSON.stringify(parsed, null, 2)}</pre>`;
}

function UnifiedDiff({ oldText, newText }) {
  const oldLines = (oldText || '').split('\n');
  const newLines = (newText || '').split('\n');
  return html`
    <pre class="unified-diff">
${oldLines.map(l => html`<div class="diff-line diff-line-del">- ${l}</div>`)}
${newLines.map(l => html`<div class="diff-line diff-line-add">+ ${l}</div>`)}
    </pre>
  `;
}
```

(Note: this is "two blocks, old then new" — not a real LCS-aligned diff. Real diff alignment is overkill for v1; if it becomes valuable, swap `UnifiedDiff` impl for `diff` or `diff-match-patch` later.)

**Verification:** Plumb runs an Edit on this exact file. The Observe view shows the file path + a red old block + a green new block, scoped under the matching `tool_use` row.

**Commit message:** `feat(dashboard): FileDiffArtifact — render Edit/Write tool calls as diff panels`

### Task 5 — Turn header metadata + footer

**Files:**
- Modify `js/components/TurnBlock.js`

**Header:** turn divider becomes informative. Layout:

```
━━━ turn ━━━  2m ago · triggered by #NNN · model: claude-opus-4-7
```

If `turn_start` carries `msgId`:

```javascript
${turn.start.msgId && html`
  triggered by <a href="#msg-${turn.start.msgId}" class="msg-id-ref" data-msg-ref=${turn.start.msgId}>#${turn.start.msgId}</a>
`}
```

The `data-msg-ref` attribute hooks into Chat.js's `handleMsgRefClick` — but that handler is scoped to Chat.js's onClick. We need a similar handler at the ObserveView level OR (cleaner) extract `handleMsgRefClick` to a shared util and import it in both views. Pick: extract to `js/msg-ref-click.js`. One-liner: also update the click handler to handle `#/agents/...` route case (navigate to `#/chat` first, then scroll on next tick).

**Footer:** if `turn.end` carries `tokens` / `duration_ms`:

```
━━━ turn end ━━━  output: 253 tok · cache_read: 23617 tok · 4.2s
```

Both header and footer use small, dim styling — they're chrome, not signal.

**Discovery dependency:** confirm broker emits these fields. If not, this task degrades to "show what's available, leave room for the rest." Spec the broker change as a follow-up rather than blocking v1.

**Commit message:** `feat(dashboard): turn header/footer — chat-trigger linkage + cost/timing`

### Task 6 — CSS polish (observe.css)

**Files:**
- Create `css/observe.css`
- Modify `index.html`: link observe.css

**Sections:**
- `.observe-view` — flex column, full height, hidden overflow
- `.observe-agent-picker` — horizontal scrolling pill row (mirror existing `.agents-bar` styling from chat.css, port the rules over since AgentsView is gone)
- `.observe-stream` — flex:1, scrollable
- `.turn-block` — generous vertical space between, subtle border-top
- `.turn-header`, `.turn-footer` — small monospace, dimmed
- `.tool-call` — clickable summary row, expanded state shows inset detail
- `.file-diff-artifact` — boxed panel, monospace, line-numbered if cheap
- `.diff-line-add` — pale green tint
- `.diff-line-del` — pale red tint

Use existing token vars (`var(--bg-deep)`, `var(--text-muted)`, `var(--accent)`, `var(--font-mono)`). Don't invent new colors except for diff add/del (`rgba(90, 200, 120, 0.12)` / `rgba(233, 69, 96, 0.12)` keep within the existing palette).

**Verification:** view looks intentional, not raw. Hover states work. Mobile (narrow window) — agent picker scrolls horizontally; turn blocks reflow.

**Commit message:** `style(dashboard): observe.css — layout, turn dividers, artifact panels`

### Task 7 — Plan-level cleanup + smoke test

**Files:**
- Update `js/icons.js` if a new `IconActivity` is wanted (replace `IconAgents` reference in BottomBar)
- Sweep for any stale references to AgentsView / Terminal / HarnessActivity in `app.js`, state.js, etc.

**Smoke:** with plumb running on <operator-host>:
1. Send `@plumb status?` from dashboard.
2. Observe view shows: turn divider → `Read tool` rows for any files plumb consults → final auto-post text → turn_end with token count.
3. Click "triggered by #NNN" — dashboard navigates to chat and highlights the operator's message.
4. Edit a file via @plumb — see FileDiffArtifact render the changes inline.
5. Refresh page — seeded history loads, "live" divider appears, new turns append below.

**Commit message:** `chore(dashboard): observe-view smoke pass — cleanup, icons, stale refs`

## 7. Open questions for future work

These are intentionally out of scope for v1 but worth recording so they don't get lost:

- **Multi-agent activity overview.** A view that shows turn-headers from every aspect interleaved chronologically — "what is the whole team doing right now." The existing `Feed` view is the closest analogue today; could be extended or merged.
- **Persistence layer.** v1 relies on in-browser buffer + agent-host session jsonl. Operator-side review of "what plumb did yesterday afternoon" requires either server-side event persistence or a way for the dashboard to request a jsonl-tail-from-disk for a specific date range. Defer until the use case becomes pressing.
- **Inline retry / kill controls.** If plumb is stuck in a tool loop the operator can see (e.g. retrying a failed Bash 5 times), an "interrupt turn" action right there would be valuable. Requires a new WS frame `aspect.interrupt {aspect, turn_id}` and broker → aspect routing. Plumb-territory follow-up.
- **Filter / scope controls.** "Show me only turns that called Edit." "Hide read-only tool calls." Once the volume grows this matters; not at v1.
- **Server-side filter for noisy events.** If text events (model reasoning) are too verbose, broker-side could rate-limit or summarize. Defer until measured.

## 8. Self-review

- **Spec coverage:** every section of the brainstorm conversation maps to a task. Live primary (D4 + Task 2). ChatGPT-style tool surfaces (Task 3). Artifact rendering for file edits (Task 4). Chat-trigger linkage (Task 5). Multi-agent: deferred per §7.
- **No placeholders:** every task has concrete file paths and code shapes. Discovery items in §5 are real "find out at task start" items, not vague TODOs.
- **Type consistency:** `turn` object shape (`{key, kind, start, events, end}`) is the same across Task 2 (where it's built) and Task 3 (where it's consumed).
- **Scope check:** 7 tasks, one substantial (Task 3), most small. Could finish in 1-2 days of focused work. Each task ships independently — partial completion still leaves the dashboard better than the current state.
