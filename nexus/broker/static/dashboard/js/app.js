const { h, html, signal, useEffect, useState } = window.__preact;

import { setAgents, currentChannel } from './state.js';
import { fetchAgents } from './api.js';
import { Shell } from './components/Shell.js';
import { Login } from './Login.js';
import { open as commsOpen } from './comms.js';
import { initNotifications } from './notifications.js';
import { FeedView } from './views/FeedView.js';
import { AgentsView } from './views/AgentsView.js';
import { FilesView } from './views/FilesView.js';
import { Tickets } from './views/Tickets.js';
import { Status } from './views/Status.js';
import { Terminal } from './views/Terminal.js';
import { DocsView } from './views/DocsView.js';
import { SplitView } from './views/SplitView.js';

function getRoute() {
  const hash = window.location.hash;
  if (hash === '#/' || hash === '' || hash.startsWith('#/feed') || hash.startsWith('#/chat')) return 'feed';
  if (hash.startsWith('#/agents')) return 'agents';
  if (hash === '#/terminal') return 'terminal';
  if (hash === '#/files') return 'files';
  if (hash === '#/tickets') return 'tickets';
  if (hash === '#/status') return 'status';
  if (hash === '#/docs') return 'docs';
  if (hash === '#/split') return 'split';
  return 'feed';
}

function getAgentFromHash() {
  const hash = window.location.hash;
  if (hash.startsWith('#/agents/')) return hash.slice('#/agents/'.length);
  return null;
}

function RouteView({ route }) {
  switch (route) {
    case 'feed':     return html`<${FeedView} />`;
    case 'agents':   return html`<${AgentsView} />`;
    case 'terminal': return html`<${Terminal} />`;
    case 'files':    return html`<${FilesView} />`;
    case 'tickets':  return html`<${Tickets} />`;
    case 'status':   return html`<${Status} />`;
    case 'docs':     return html`<${DocsView} />`;
    case 'split':    return html`<${SplitView} />`;
    default:         return html`<${FeedView} />`;
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
        const res = await fetch('/api/auth/mode', { cache: 'no-store' });
        const data = await res.json();
        if (!cancelled && data && data.bypass) {
          // Open comms with no token — broker accepts via bypass path.
          await commsOpen('');
          if (!cancelled) setAuthed(true);
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
    return () => clearInterval(interval);
  }, [authed]);

  // Hold rendering until the bypass probe completes. Keeps the Login
  // overlay from flashing when bypass is on.
  if (!bypassChecked) return html`<div style="display:none"></div>`;

  if (!authed) return html`<${Login} onComplete=${() => setAuthed(true)} />`;

  return html`<${Shell} activeRoute=${route.value}><${RouteView} route=${route.value} /></${Shell}>`;
}
