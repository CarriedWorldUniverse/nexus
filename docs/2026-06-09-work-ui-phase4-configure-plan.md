# Work UI Phase 4 (Configure — align + polish) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Align the existing 4-tab Configure area with the three-area shell: canonicalize routing under `#/configure`, make `SettingsAspects` the single per-agent editor (deep-linked from Team, with the dispatch-enabled toggle), and refresh the visual.

**Architecture:** Frontend-only. Reuse all existing admin endpoints + Phase 2's `getDispatchEnabled`/`setDispatchEnabled` wrappers. Change routing strings, move one toggle into the Aspects tab, deep-link + retire `AgentConfigPanel`, and polish `settings.css`. No backend, no new tests; verification is dev-mode + a clean `go build` (go:embed).

**Tech Stack:** Preact + htm (`window.__preact`, no build, go:embed), `api.js`/`api/admin.js` helpers, `settings.css`/`tokens.css`.

**Spec:** `docs/2026-06-09-work-ui-phase4-configure-design.md`. **Branch:** `design/work-ui-phase4`. **Commit trailer (every commit):** `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## File Structure

**Frontend (`nexus/broker/static/dashboard/`):**
- Modify `js/views/SettingsView.js` — `parseTab()` + `SettingsTabBar` hrefs: `#/settings/<tab>` → `#/configure/<tab>`; bare `#/configure` → aspects.
- Modify `js/app.js` — redirect `#/settings/*` → `#/configure/*`.
- Modify `js/views/SettingsAspects.js` — per-agent dispatch-enabled toggle; read focused agent from `#/configure/aspects/<agent>`.
- Modify `js/views/panels/TeamPanel.js` — "Configure" → `window.location.hash = '#/configure/aspects/<agent>'`; drop `AgentConfigPanel` import + render.
- Delete `js/views/panels/AgentConfigPanel.js`.
- Modify `css/settings.css` — visual alignment with the shell tokens.

**Backend:** none.

---

## Task 1: Canonicalize SettingsView routing to `#/configure`

**Files:**
- Modify: `nexus/broker/static/dashboard/js/views/SettingsView.js`

- [ ] **Step 1: Repoint `parseTab()` + the tab links**

In `SettingsView.js`, change the hash prefix from `#/settings` to `#/configure` in `parseTab()` (the `startsWith` + `slice`), and in `SettingsTabBar` change the `href` from `'#/settings/' + tab.id` to `'#/configure/' + tab.id`:

```javascript
// parseTab(): canonical prefix is #/configure (legacy #/settings is redirected
// to it in app.js, so this only sees #/configure).
function parseTab() {
  const hash = window.location.hash;
  if (!hash.startsWith('#/configure')) return DEFAULT_TAB;
  const after = hash.slice('#/configure'.length).replace(/^\/+/, '');
  const tab = after.split('/')[0]; // first segment after #/configure/ (agent focus is a later segment)
  return TABS.some((t) => t.id === tab) ? tab : DEFAULT_TAB;
}
```

```javascript
// SettingsTabBar: href -> #/configure/<tab>
href=${'#/configure/' + tab.id}
```

> Confirm the exact `parseTab` body + the `SettingsTabBar` href line against the current file and apply the prefix swap. `DEFAULT_TAB` (aspects) is unchanged. The agent-focus segment (`#/configure/aspects/<agent>`) is read in `SettingsAspects` (Task 3), so `parseTab` must take only the FIRST segment as the tab.

- [ ] **Step 2: Verify (dev mode)**

`#/configure` → Aspects; `#/configure/credentials` → Credentials; the tab bar links go to `#/configure/<tab>`. No console error.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/SettingsView.js
git commit -m "feat(dashboard): canonicalize Configure routing to #/configure

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Redirect legacy `#/settings/*` → `#/configure/*`

**Files:**
- Modify: `nexus/broker/static/dashboard/js/app.js`

- [ ] **Step 1: Add the redirect in `getRoute()`**

In `app.js` `getRoute()`, where `#/configure`/`#/settings` currently both return `'configure'`, rewrite a `#/settings*` hash to `#/configure*` before returning (mirror the Phase 3 `#/chat`→`#/converse` redirect):

```javascript
  if (hash.startsWith('#/settings')) {
    const target = '#/configure' + hash.slice('#/settings'.length);
    if (window.location.hash !== target) window.location.hash = target;
    return 'configure';
  }
  if (hash.startsWith('#/configure')) return 'configure';
```

> Confirm the current `getRoute()` shape (Phase 1-3 changed it) and slot this cleanly. `case 'configure': return html\`<${SettingsView} />\`` stays as-is.

- [ ] **Step 2: Verify (dev mode)**

Visiting `#/settings/credentials` rewrites the hash to `#/configure/credentials` and shows Credentials. No loop, no console error.

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/js/app.js
git commit -m "feat(dashboard): redirect legacy #/settings/* to #/configure/*

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: SettingsAspects — dispatch-enabled toggle + agent focus

**Files:**
- Modify: `nexus/broker/static/dashboard/js/views/SettingsAspects.js`

- [ ] **Step 1: Import the dispatch-enabled wrappers**

Phase 2 added `getDispatchEnabled`/`setDispatchEnabled`. Confirm their module (Phase 2 put them in `js/api.js`; `SettingsAspects` currently imports admin helpers from `../api/admin.js`). Import them from wherever they live, e.g.:

```javascript
import { getDispatchEnabled, setDispatchEnabled } from '../api.js'; // confirm path; reuse Phase 2 wrappers
```

> If the wrappers are in `api/admin.js`, import from there. Do NOT duplicate them — reuse Phase 2's. If they don't exist (Phase 2 inlined them in the panel), add the two thin wrappers to `api/admin.js` next to `getModelConfig`.

- [ ] **Step 2: Render + wire the per-agent dispatch-enabled toggle**

For each aspect row, load + render the toggle. In the per-aspect component (the one rendering `KINDS.map`), add a checkbox bound to the flag:

```javascript
const [dispatchEnabled, setDispatchEnabledState] = useState(true);
useEffect(() => {
  getDispatchEnabled(aspect).then((d) => setDispatchEnabledState(!!(d && d.enabled))).catch(() => {});
}, [aspect]);
const toggleDispatch = () => {
  const v = !dispatchEnabled;
  setDispatchEnabledState(v);
  setDispatchEnabled(aspect, v).catch(() => setDispatchEnabledState(!v)); // revert on failure
};
// in the row markup:
html`<label class="settings-dispatch">
  <input type="checkbox" checked=${dispatchEnabled} onChange=${toggleDispatch} /> Dispatchable
</label>`
```

> Match the aspect-name variable in SettingsAspects' per-row component (it iterates the roster). Place the toggle in the aspect's row header alongside the model kinds.

- [ ] **Step 3: Read the focused agent from `#/configure/aspects/<agent>`**

When the Aspects tab mounts, read a trailing agent segment and scroll/expand it:

```javascript
function focusedAgent() {
  const hash = window.location.hash; // #/configure/aspects/<agent>
  const m = hash.match(/^#\/configure\/aspects\/([^/?#]+)/);
  return m ? decodeURIComponent(m[1]) : null;
}
// in SettingsAspects: useEffect on mount → if focusedAgent() matches a row, scrollIntoView it.
```

Bare `#/configure/aspects` lists all (unchanged); an unknown agent is a no-op (best-effort focus).

- [ ] **Step 4: Verify (dev mode)**

Aspects tab shows a Dispatchable toggle per aspect that loads its state + persists on toggle (reload reflects it); `#/configure/aspects/anvil` scrolls to anvil. No console error.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/SettingsAspects.js nexus/broker/static/dashboard/js/api/admin.js
git commit -m "feat(dashboard): dispatch-enabled toggle + agent focus in Aspects tab

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: TeamPanel deep-link + retire AgentConfigPanel

**Files:**
- Modify: `nexus/broker/static/dashboard/js/views/panels/TeamPanel.js`
- Delete: `nexus/broker/static/dashboard/js/views/panels/AgentConfigPanel.js`

- [ ] **Step 1: Deep-link the Configure button**

In `TeamPanel.js`, change the "Configure" button (currently `onClick=${() => setSelected(id)}`) to navigate:

```javascript
<button class="team-config-btn" onClick=${() => { window.location.hash = `#/configure/aspects/${id}`; }}>Configure</button>
```

Remove the `AgentConfigPanel` import (line ~4) and its render (`<${AgentConfigPanel} … />` ~line 31) and the `selected`/`setSelected` state if it's no longer used for anything else.

- [ ] **Step 2: Delete the retired panel**

```bash
git rm nexus/broker/static/dashboard/js/views/panels/AgentConfigPanel.js
```

Grep for any other import of `AgentConfigPanel` (`grep -rn AgentConfigPanel nexus/broker/static/dashboard/js/`) and remove them so the app loads.

- [ ] **Step 3: Verify (dev mode)**

In Watch, open the Team panel → "Configure" on an agent navigates to `#/configure/aspects/<agent>` (Configure area, Aspects tab, focused). No console error (no dangling `AgentConfigPanel` import).

- [ ] **Step 4: Commit**

```bash
git add nexus/broker/static/dashboard/js/views/panels/TeamPanel.js
git commit -m "feat(dashboard): Team Configure deep-links to Aspects tab; retire AgentConfigPanel

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: Visual refresh of the Configure tabs

**Files:**
- Modify: `nexus/broker/static/dashboard/css/settings.css`

- [ ] **Step 1: Align with the shell tokens**

Update `settings.css` so the tab bar + forms match the three-area shell (Watch/Converse): use `var(--…)` tokens from `tokens.css` for colours/spacing/borders; make `.settings-tab.active` match the BottomBar/active-item treatment; align button + input styling with `watch.css`/`converse.css`. Do not restructure markup — only the styles. Add a `.settings-dispatch` style for the new toggle (Task 3).

- [ ] **Step 2: Verify (dev mode)**

Configure visually matches Watch/Converse (no jarring difference); tabs, forms, and the dispatch toggle look consistent. 

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/static/dashboard/css/settings.css
git commit -m "feat(dashboard): visual refresh of Configure to match the three-area shell

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: Integration verification (build + deploy + dogfood)

- [ ] **Step 1: Build**

Run: `cd /Users/jacinta/Source/nexus && go build ./...`
Expected: clean (go:embed picks up the deleted `AgentConfigPanel.js` + edits).

- [ ] **Step 2: Deploy to dMon**

Broker-only (dashboard is embedded): `deploy/broker/build.sh` + `kubectl rollout restart deploy/nexus-broker`.

- [ ] **Step 3: Dogfood**

In `#/configure`: tabs route under `#/configure/<tab>`; `#/settings/*` redirects; the Aspects tab has the Dispatchable toggle (loads + persists); Team → Configure deep-links + focuses the agent; the area matches the shell visually. Confirm Watch + Converse still work.

- [ ] **Step 4: Push the branch**

```bash
git push -u origin design/work-ui-phase4
```

---

## Decomposition

**One ticket (4a):** all of the above. Frontend-only; ~5 small changes + a verify pass.

## Self-Review notes (for the executor)

- **Spec coverage:** routing canonicalization (T1, T2) · one-source per-agent config = dispatch-enabled in Aspects (T3) + deep-link/retire (T4) · visual refresh (T5). All spec sections map to a task.
- **Type consistency:** `getDispatchEnabled`/`setDispatchEnabled` (T3) are Phase 2's wrappers, reused (not redefined); the deep-link hash `#/configure/aspects/<agent>` (T4) matches `focusedAgent()`'s regex (T3) and `parseTab()`'s first-segment-only tab read (T1).
- **Confirm-against-live-code seams (flagged inline):** the exact `parseTab`/`SettingsTabBar` href lines (T1); the current `getRoute()` shape (T2); the module of `getDispatchEnabled`/`setDispatchEnabled` (api.js vs api/admin.js) + SettingsAspects' per-row aspect variable (T3); the `AgentConfigPanel` import/render lines in TeamPanel (T4). Mirror the existing code; don't invent.
- **Risk:** the only deletion is `AgentConfigPanel.js` — grep for stragglers (T4 Step 2) so the app loads; `go build` (T6) confirms the embed is clean.
