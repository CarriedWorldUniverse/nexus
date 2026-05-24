// SettingsView (NEX-264) — admin surface for nexus dashboard.
//
// Hosts four sub-pages, each landing in its own follow-up story:
//   #/settings/aspects      — per-aspect model picker (NEX-265)
//   #/settings/credentials  — credentials CRUD (NEX-266)
//   #/settings/defaults     — per-aspect credential defaults (NEX-267)
//   #/settings/audit        — credential audit trail viewer (NEX-268)
//
// Bare #/settings defaults to aspects (the first feature shipping with
// content; chosen because the model picker is the headline operator
// affordance driving NEX-219).
//
// Admin gate: the BottomBar already conditions the Settings tab on the
// isAdmin signal, so a non-admin shouldn't ordinarily land here. The
// gate below handles the manual-URL-typed case — non-admin sees an
// explanatory placeholder, no crash, no leak of sub-page chrome.

const { html, useState, useEffect } = window.__preact;
import { isAdmin } from '../state.js';
import { Placeholder } from './Placeholder.js';
import { SettingsAspects } from './SettingsAspects.js';

const TABS = [
  { id: 'aspects',     label: 'Aspects' },
  { id: 'credentials', label: 'Credentials' },
  { id: 'defaults',    label: 'Defaults' },
  { id: 'audit',       label: 'Audit' },
];

const DEFAULT_TAB = 'aspects';

// Sub-route lives in the hash after '#/settings/'. Returns one of the
// TABS ids, or DEFAULT_TAB when the hash is bare or unrecognised.
function getSubRoute() {
  const hash = window.location.hash;
  if (!hash.startsWith('#/settings')) return DEFAULT_TAB;
  const after = hash.slice('#/settings'.length).replace(/^\/+/, '');
  if (!after) return DEFAULT_TAB;
  const segment = after.split('/')[0];
  if (TABS.some((t) => t.id === segment)) return segment;
  return DEFAULT_TAB;
}

function SettingsTabBar({ activeTab }) {
  return html`
    <div class="settings-tabs" role="tablist">
      ${TABS.map((tab) => html`
        <a
          key=${tab.id}
          href=${'#/settings/' + tab.id}
          role="tab"
          aria-selected=${tab.id === activeTab}
          class=${'settings-tab' + (tab.id === activeTab ? ' active' : '')}
        >${tab.label}</a>
      `)}
    </div>
  `;
}

function SubRouteContent({ subRoute }) {
  switch (subRoute) {
    case 'aspects':     return html`<${SettingsAspects} />`;
    case 'credentials': return html`<${Placeholder} name="Credentials (NEX-266)" />`;
    case 'defaults':    return html`<${Placeholder} name="Defaults (NEX-267)" />`;
    case 'audit':       return html`<${Placeholder} name="Audit (NEX-268)" />`;
    default:            return html`<${Placeholder} name="Settings" />`;
  }
}

function AdminGate() {
  return html`
    <div style="display:flex;flex-direction:column;align-items:center;justify-content:center;height:100%;gap:8px;color:var(--text-muted);font-family:var(--font-display);">
      <div style="font-size:18px;">Settings is admin-only.</div>
      <div style="font-size:14px;opacity:0.7;">Sign in as the operator to access this surface.</div>
    </div>
  `;
}

export function SettingsView() {
  const [subRoute, setSubRoute] = useState(getSubRoute());

  // Track sub-route from URL hash so back/forward + direct nav both work.
  // Settings page is the only place editing the hash to a sub-route, but
  // listen broadly so any other navigator (BottomBar, manual edit) syncs.
  useEffect(() => {
    function onHash() { setSubRoute(getSubRoute()); }
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  if (!isAdmin.value) return html`<${AdminGate} />`;

  return html`
    <div class="settings-view">
      <${SettingsTabBar} activeTab=${subRoute} />
      <div class="settings-content">
        <${SubRouteContent} subRoute=${subRoute} />
      </div>
    </div>
  `;
}
