const { h, html, signal, useEffect, useState } = window.__preact;

import { setAgents, currentChannel, setIsAdminFromRole } from './state.js';
import { fetchAgents, setAuthToken, getAuthToken } from './api.js';
import { Shell } from './components/Shell.js';
import { Login } from './Login.js';
import { open as commsOpen } from './comms.js';
import { initNotifications } from './notifications.js';
import { FeedView } from './views/FeedView.js';
import { Chat } from './views/Chat.js';
import { ObserveView } from './views/ObserveView.js';
import { WatchView } from './views/WatchView.js';
import { FilesView } from './views/FilesView.js';
import { Tickets } from './views/Tickets.js';
import { Status } from './views/Status.js';
import { DocsView } from './views/DocsView.js';
import { SplitView } from './views/SplitView.js';
import { SettingsView } from './views/SettingsView.js';
import { Placeholder } from './views/Placeholder.js';

function getRoute() {
  const hash = window.location.hash;
  if (hash.startsWith('#/watch')) return 'watch';
  if (hash.startsWith('#/converse')) return 'converse';
  if (hash.startsWith('#/configure')) return 'configure';
  if (hash.startsWith('#/chat')) return 'chat';
  if (hash.startsWith('#/agents')) return 'agents';
  if (hash.startsWith('#/settings')) return 'settings';
  if (hash === '#/' || hash === '' || hash.startsWith('#/feed')) return 'watch';
  if (hash === '#/files') return 'files';
  if (hash === '#/tickets') return 'tickets';
  if (hash === '#/status') return 'status';
  if (hash === '#/docs') return 'docs';
  if (hash === '#/split') return 'split';
  return 'watch';
}

function getAgentFromHash() {
  const hash = window.location.hash;
  if (hash.startsWith('#/agents/')) return hash.slice('#/agents/'.length);
  return null;
}

function RouteView({ route }) {
  switch (route) {
    case 'watch':    return html`<${WatchView} />`;
    case 'converse': return html`<${Placeholder} title="Converse" note="Coming in Phase 3" />`;
    case 'configure': return html`<${SettingsView} />`;
    case 'feed':     return html`<${FeedView} />`;
    case 'chat':     return html`<${Chat} />`;
    case 'agents':   return html`<${ObserveView} />`;
    case 'files':    return html`<${FilesView} />`;
    case 'tickets':  return html`<${Tickets} />`;
    case 'status':   return html`<${Status} />`;
    case 'docs':     return html`<${DocsView} />`;
    case 'split':    return html`<${SplitView} />`;
    case 'settings': return html`<${SettingsView} />`;
    default:         return html`<${WatchView} />`;
  }
}

const route = signal(getRoute());

// Sync currentChannel from hash on load and on every navigation
function syncChannelFromHash() {
  const agent = getAgentFromHash();
  currentChannel.value = agent || 'general';
}
syncChannelFromHash();

window.addEventListener('hashchange', () => {
  route.value = getRoute();
  syncChannelFromHash();
});

async function pollAgents() {
  const result = await fetchAgents();
  setAgents(result);
}

// Mobile keyboard handling — lock body to visual viewport so only chat scrolls
function initMobileKeyboard() {
  if (!window.visualViewport) return;
  function onResize() {
    const vv = window.visualViewport;
    document.body.style.height = vv.height + 'px';
    document.body.style.top = vv.offsetTop + 'px';
    const msgs = document.querySelector('.chat-messages');
    if (msgs) requestAnimationFrame(() => { msgs.scrollTop = msgs.scrollHeight; });
  }
  window.visualViewport.addEventListener('resize', onResize);
  window.visualViewport.addEventListener('scroll', onResize);
  onResize();
  document.body.addEventListener('touchmove', (e) => {
    if (e.target.closest('.chat-messages') || e.target.closest('.chat-channels')) return;
    e.preventDefault();
  }, { passive: false });
}

export function App() {
  // Authed flips true when the WS opens after a successful passkey
  // login. JWT-in-memory means a page refresh always lands here in
  // the unauthed state — comms re-opens via Login on demand.
  //
  // bypassChecked guards a one-shot probe of /api/auth/mode. When the
  // broker reports {bypass:true}, the SPA skips the Login overlay
  // entirely and opens comms with no token. The broker's
  // resolveUpgradeAuth handles the no-token path. Dev-only knob; in
  // production the probe returns {bypass:false} and the normal Login
  // ceremony runs.
  const [authed, setAuthed] = useState(false);
  const [bypassChecked, setBypassChecked] = useState(false);

  useEffect(() => {
    if (bypassChecked) return;
    let cancelled = false;
    (async () => {
      try {
        // 1. Auth-mode probe (dev bypass).
        const res = await fetch('/api/auth/mode', { cache: 'no-store' });
        const data = await res.json();
        if (!cancelled && data && data.bypass) {
          await commsOpen('');
          // Under bypass the broker grants the operator Admin server-side
          // (auth() resolves token-less requests as {operator, Admin:true}).
          // The role probe below never runs on this branch, so set admin
          // explicitly here — otherwise isAdmin stays false and the
          // Settings nav is hidden even though every data path is admin.
          if (!cancelled) setIsAdminFromRole('operator');
          if (!cancelled) setAuthed(true);
          return;
        }

        // 2. Resume probe: do we have a JWT in localStorage that the
        // broker still considers valid? If so, restore the session
        // (token → in-memory + WS open) and skip the Login overlay.
        // Without this path, every refresh prompts for a passkey even
        // when the cached JWT has hours left on it — the source of
        // the "reauth on every refresh" UX bug. Pairs with
        // /api/auth/check + setAuthToken's localStorage write-through
        // + the 24h JWT TTL bump.
        const tok = (typeof localStorage !== 'undefined') ? localStorage.getItem('auth_token') : null;
        if (tok) {
          try {
            const probe = await fetch('/api/auth/check', {
              headers: { 'Authorization': 'Bearer ' + tok },
              cache: 'no-store',
            });
            if (!cancelled && probe.ok) {
              setAuthToken(tok); // sync in-memory holder with localStorage
              // NEX-264: server reports role on the check response;
              // operator → admin (per ws.go:179). Drive the Settings
              // nav visibility off this signal.
              try {
                const probeBody = await probe.json();
                if (probeBody && probeBody.role) setIsAdminFromRole(probeBody.role);
              } catch (_) {
                // Body parse failure leaves isAdmin at its default (false).
                // Worst case: operator doesn't see Settings tab and reloads.
              }
              await commsOpen(tok);
              if (!cancelled) setAuthed(true);
              return;
            }
            // 401 → token stale; drop it so the next reload doesn't loop.
            if (!cancelled && probe.status === 401) {
              localStorage.removeItem('auth_token');
            }
          } catch (e) {
            console.warn('auth check probe failed', e);
          }
        }
      } catch (e) {
        // Probe failure is non-fatal — fall through to the Login overlay.
        console.warn('auth mode probe failed', e);
      } finally {
        if (!cancelled) setBypassChecked(true);
      }
    })();
    return () => { cancelled = true; };
  }, [bypassChecked]);

  useEffect(() => {
    if (!authed) return;
    initNotifications();
    initMobileKeyboard();
    pollAgents();
    const interval = setInterval(pollAgents, 8000);

    // NEX-264: probe role on every auth → admin transition. The resume
    // path sets isAdmin inline before flipping authed, but fresh-login
    // (Login.onComplete) hits this path with isAdmin still false, so
    // re-probe to populate it. Cheap (returns from cache normally).
    (async () => {
      const tok = getAuthToken();
      if (!tok) return;
      try {
        const res = await fetch('/api/auth/check', {
          headers: { 'Authorization': 'Bearer ' + tok },
          cache: 'no-store',
        });
        if (!res.ok) return;
        const body = await res.json();
        if (body && body.role) setIsAdminFromRole(body.role);
      } catch (_) {
        // Non-fatal; isAdmin defaults to false (Settings nav hidden).
      }
    })();

    return () => clearInterval(interval);
  }, [authed]);

  // Hold rendering until the bypass probe completes. Keeps the Login
  // overlay from flashing when bypass is on.
  if (!bypassChecked) return html`<div style="display:none"></div>`;

  if (!authed) return html`<${Login} onComplete=${() => setAuthed(true)} />`;

  return html`<${Shell} activeRoute=${route.value}><${RouteView} route=${route.value} /></${Shell}>`;
}
