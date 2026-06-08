# Work UI — Phase 4: Configure (Align + Polish) (Design)

**Date:** 2026-06-09
**Status:** Design — pending review
**Phase:** 4 of 5. Builds on Phases 1-3 (live + deployed). Aligns the existing Configure area with the three-area shell and makes per-agent config one source of truth.

## Goal

Configure already works (a 4-tab admin area: Aspects · Credentials · Defaults · Audit). Phase 4 is a **light align + polish**: canonicalize its routing under `#/configure`, collapse the two per-agent config editors into one (the Aspects tab), and refresh the visual to match Watch/Converse. Everything stays doable via shadow/CLI.

## Scope

**In scope (Phase 4):**
1. **Routing canonicalization** — `#/configure` (+ `#/configure/<tab>`) is the canonical Configure home; redirect legacy `#/settings/*` → `#/configure/*`.
2. **One source of truth for per-agent config** — make `SettingsAspects` the single per-agent editor: the Team panel's "Configure" deep-links to it focused on the agent, and `AgentConfigPanel` is retired. Add the **dispatch-enabled** toggle to `SettingsAspects` (Phase 2 added the endpoint but wired the toggle only into the retired panel).
3. **Visual refresh** — align the Settings tab styling with the three-area shell tokens/look.

**Out of scope:** Phase 5 (dedicated mobile). No re-grouping of the tabs (the Aspects/Credentials/Defaults/Audit split stays — the user chose "align + polish," not re-IA). No backend changes (all admin endpoints exist). **Frontend-only.**

## Architecture

All three changes are in the dashboard (`nexus/broker/static/dashboard/`). No new backend, no new RPCs — reuse the existing admin endpoints (`model-config`, `personality`, `provider-binding`, `mcp_profile`, `dispatch-enabled`, `roster`, credentials, defaults, audit) and the Phase 2 `getDispatchEnabled`/`setDispatchEnabled` api wrappers.

### 1. Routing canonicalization

- `SettingsView` parses the active tab from the hash. Change its tab links + parser from `#/settings/<tab>` to `#/configure/<tab>` (the `SettingsTabBar` hrefs + the tab-from-hash logic).
- `app.js` already maps `#/configure` and `#/settings` → the `configure` route → `SettingsView`. Add a **redirect**: a bare `#/settings` or `#/settings/<tab>` rewrites to `#/configure[/​<tab>]` (mirroring how Phase 3 redirected `#/chat`/`#/feed` → `#/converse`). Keep the hashes working for bookmarks.
- A bare `#/configure` defaults to the **aspects** tab (the current `#/settings` default).

### 2. One source of truth for per-agent config (deep-link + retire)

- **`SettingsAspects` becomes the single per-agent editor.** It already edits the per-aspect model picker; add the **dispatch-enabled** toggle per agent (reuse `getDispatchEnabled`/`setDispatchEnabled`).
- **Agent focus via the URL:** support `#/configure/aspects/<agent>` — `SettingsAspects` reads the trailing `<agent>` segment and scrolls/expands that agent's row. (Bare `#/configure/aspects` lists all, as today.)
- **Team panel deep-links:** the Team panel's "Configure" button navigates to `#/configure/aspects/<agent>` (sets `window.location.hash`) instead of opening `AgentConfigPanel`.
- **Retire `AgentConfigPanel.js`** — remove its import + render from `TeamPanel.js` and delete the file. The Phase 2 api wrappers it used (`getAgentModelConfig`/`getDispatchEnabled`/…) stay — `SettingsAspects` uses them now.

### 3. Visual refresh

- Align the Settings tab bar + forms with the three-area shell: the `tokens.css` variables, the BottomBar/Watch/Converse look (spacing, the active-tab treatment, button styling). Scope to `settings.css` (or the relevant Settings styles); don't restructure the markup beyond the tab links.

## Data flow

- Open Configure → `#/configure/<tab>` → `SettingsView` renders the tab. Each tab loads via its existing admin GET; saves via PUT (unchanged).
- Aspects tab: per-agent model-config + the new dispatch-enabled toggle (GET to load, PUT to save), focused agent from `#/configure/aspects/<agent>`.
- Team "Configure" → `window.location.hash = '#/configure/aspects/<agent>'` → the shell routes to Configure, Aspects tab, focused.

## Error handling

- Unknown tab in `#/configure/<tab>` → default to aspects (don't blank-screen).
- Unknown agent in `#/configure/aspects/<bad>` → show the full aspects list (the focus is best-effort), no error.
- Admin-gated tabs (Credentials/Defaults/Audit) when not admin → the existing admin gate behaviour is preserved (Configure is visible as a primary area; the gated tabs show their existing not-admin state).
- Save failures → the existing per-tab error handling (unchanged).

## Testing

- **Frontend (dev mode, JS not in CI):** `#/configure` defaults to Aspects; the four tabs route under `#/configure/<tab>`; `#/settings/credentials` redirects to `#/configure/credentials`; the Aspects tab shows + persists the dispatch-enabled toggle; Team → Configure deep-links to `#/configure/aspects/<agent>` and focuses that agent; no console errors (no dangling `AgentConfigPanel` import); the visual matches the shell.
- **Build:** `go build ./...` clean (go:embed picks up the deleted `AgentConfigPanel.js` + the edits).
- No new Go tests (no backend change; the dispatch-enabled endpoint is already tested from Phase 2).

## File structure

**Frontend (`nexus/broker/static/dashboard/`):**
- Modify `js/views/SettingsView.js` — tab links + tab-from-hash → `#/configure/<tab>`; bare `#/configure` → aspects.
- Modify `js/views/SettingsAspects.js` — add the per-agent dispatch-enabled toggle; read the focused agent from `#/configure/aspects/<agent>`.
- Modify `js/views/panels/TeamPanel.js` — "Configure" deep-links to `#/configure/aspects/<agent>`; drop the `AgentConfigPanel` import + render.
- Delete `js/views/panels/AgentConfigPanel.js`.
- Modify `js/app.js` — redirect `#/settings/*` → `#/configure/*`.
- Modify the Settings CSS (`css/settings.css` or equivalent) — visual alignment with the shell tokens.

**Backend:** none.

## Decomposition

**One ticket (4a):** routing canonicalization + the deep-link/retire + the dispatch-enabled toggle in SettingsAspects + the visual pass. Frontend-only.

## Resolved decisions

1. **Light, not a re-IA** — keep the Aspects/Credentials/Defaults/Audit tabs; only canonicalize routing, unify per-agent config, and polish visuals.
2. **Deep-link + retire `AgentConfigPanel`** — the Aspects tab is the single per-agent editor; Team's "Configure" navigates to it (rather than keeping an in-Team panel sharing a component). Lightest path to one source of truth.
3. **dispatch-enabled lands in `SettingsAspects`** — the endpoint exists (Phase 2); only the toggle moves from the retired panel into the tab.
