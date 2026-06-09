# Work UI — Phase 5: Dedicated Mobile (Design)

**Date:** 2026-06-09
**Status:** Design — pending review
**Phase:** 5 of 5 (final). Builds on Phases 1-4 (live + deployed). Adds a dedicated mobile experience — a real component-tree switch, not a responsive reflow.

## Goal

A pocket version of the console: open the dashboard on a phone and get a **Converse-first** single-column app — DM shadow + the team on the go, glance at live runs, with light in-app notifications. Steering still happens at the desk/CLI (mobile is read-mostly). Everything stays doable via shadow/CLI.

## Scope

**In scope (Phase 5):** a dedicated `MobileApp` shell (detected at the root), mobile Converse, glanceable read-only mobile Runs, and light in-app/foreground notifications. **Frontend-only.**

**Out of scope:** real web push (service worker + Push API + backend — the deferred follow-on); mobile control actions (cancel/dispatch — read-only on mobile); Configure on mobile (rare; use the desktop). This is the last phase of epic NEX-522.

## Architecture

In `app.js` `App()` (which today renders `<Shell><RouteView/></Shell>` after auth), add a mobile branch: a reactive `isMobile` signal (driven by `matchMedia`) decides whether to render the dedicated **`MobileApp`** tree or the existing desktop `Shell`. **Auth is shared** (the Login overlay + bypass check stay in `App()`, before the branch). Reuses all existing plumbing — the WS/api (`chat.*`, `runs.*`), `MessageBubble`, the signals (`agents`, `currentChannel`, etc.). The PWA meta (`viewport`, `apple-mobile-web-app-capable`, black-translucent status bar) already in `index.html` makes it installable to the home screen.

### Detection

```
isMobile = signal(matchMedia('(max-width: 768px)').matches)
```
A `matchMedia('(max-width: 768px)')` listener updates the signal on resize/orientation change; `App()` re-renders into the right tree. No manual "desktop site" override in v1 (a tablet/desktop gets the full console; a phone gets mobile). Pure viewport, no UA sniffing.

### `MobileApp` shell

Single column + a **bottom tab bar**: **Converse** (default) · **Runs**. The active tab renders below the (small) header; the notification module mounts once at this level.

### Mobile Converse (drill-in, not side-by-side)

Two states within the Converse tab:
- **List** — the conversation list (Team + per-aspect DMs, **shadow pinned**), full-width rows; tap → pane.
- **Pane** — the selected conversation's messages (`MessageBubble`) + a composer, with a **back** affordance returning to the list.

Reuses the Phase 3 chat helpers (`fetchMessages`, `subscribe.chat`, `sendMessage`/`sendDM`, `messageBelongsToChannel`). The desktop `ConverseView` is *not* reused directly (it's a side-by-side layout); mobile has its own list/pane components sharing the same data helpers.

### Mobile Runs (glanceable, read-only)

- **List** — active + recent runs (`runs.list` + live `runs.update`): a compact card per run (status dot, agent, ticket, command snippet).
- **Detail** — tap → the run's unified timeline (`run.get`), **read-only** (no Stop/Force/dispatch — those stay desktop/CLI). Reuses the Phase 1 timeline rendering (chat items via `MessageBubble`, activity items compact) + live `subscribe.observe`.

### Notifications (light)

A `MobileNotifications` module mounted in `MobileApp`, subscribed to `chat.deliver` (operator-relevant messages) + `runs.update` (status flips to complete/failed):
- **In-app:** an unread badge on the Converse tab + a transient toast ("shadow replied", "run NEX-x complete").
- **Foreground/backgrounded-alive:** when `document.hidden` and `Notification.permission === 'granted'`, fire a `new Notification(...)`. Request permission on the first user interaction (a one-time prompt), never on load.
- **No service worker, no backend** — reuses the existing WS delivery. Real lock-screen-when-closed push is a separate deferred subsystem.

## Data flow

- Converse: `fetchMessages(channel)` + `subscribe.chat` + `sendMessage`/`sendDM` (Phase 3) — same as desktop, mobile layout.
- Runs: `runs.list` + `runs.update` push (feed), `run.get` + `subscribe.observe` (detail) — Phase 1 endpoints, read-only.
- Notifications: the same `subscribe.chat`/`runs.update` streams, fanned to the badge/toast/Notification.

## Error handling

- Detection: if `matchMedia` is unavailable (ancient browser), default to desktop (no crash).
- Notification permission denied → in-app badge/toast only (no browser notification); never re-prompt.
- Empty states: no conversations / no runs → clear empty copy, not a blank screen.
- WS drop → the existing reconnect + re-subscribe; on reconnect, re-`fetchMessages`/`runs.list` the open view.
- Orientation/resize across the breakpoint mid-session → re-render into the other tree cleanly (auth + WS persist; they live above the branch).

## Testing

- **Frontend (dev mode, JS not in CI):** resize the viewport below 768px → `MobileApp` renders (bottom tabs Converse/Runs); above → the desktop console. Converse list → tap → pane → back; DM shadow delivers + replies show. Runs list shows live runs; tap → read-only timeline. A new message while on the Runs tab badges Converse + toasts; backgrounding the tab (with permission) fires a browser notification. No console errors.
- **Build:** `go build ./...` clean (go:embed picks up the new files).
- No Go tests (no backend change).

## File structure

**Frontend (`nexus/broker/static/dashboard/`):**
- Modify `js/app.js` — the `isMobile` signal (matchMedia) + the `App()` branch to `MobileApp` vs `Shell` (after auth).
- Create `js/views/mobile/MobileApp.js` — the shell + bottom-tab nav + mounts notifications.
- Create `js/views/mobile/MobileConverse.js` — list + drill-in pane + composer (reusing chat helpers).
- Create `js/views/mobile/MobileRuns.js` — glanceable run list + read-only run detail (reusing runs/timeline helpers).
- Create `js/views/mobile/MobileNotifications.js` — the badge/toast/Notification module.
- Create `css/mobile.css` — the dedicated mobile styles (reuse tokens); link in `index.html`.

**Backend:** none.

## Decomposition

**One ticket (5a):** detection + `MobileApp` shell + Mobile Converse + Mobile Runs (read-only) + light notifications. Frontend-only. (If it proves large in execution, notifications + Runs can split to a 5b, but it's one cohesive mobile surface.)

## Resolved decisions

1. **Detect + render a dedicated tree** — `matchMedia` viewport switch in `App()`, a real `MobileApp` component tree (not a CSS reflow); shared auth above the branch.
2. **Converse-first, Runs read-only** — mobile is for staying in touch + glancing; control stays desktop/CLI.
3. **Light notifications only** — in-app badge/toast + foreground `Notification` API; no service worker / backend (real push deferred).
4. **No manual desktop override in v1** — phone → mobile, tablet/desktop → console; revisit if the breakpoint misfires for someone.
