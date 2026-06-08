const { html } = window.__preact;
import { IconChat, IconStatus, IconSettings } from '../icons.js';

const TABS = [
  { id: 'converse',  label: 'Converse',  href: '#/converse',  Icon: IconChat     },
  { id: 'watch',     label: 'Watch',     href: '#/watch',     Icon: IconStatus   },
  { id: 'configure', label: 'Configure', href: '#/configure', Icon: IconSettings },
];

export function BottomBar({ activeRoute }) {
  const highlightRoute = activeRoute === 'settings' ? 'configure' : activeRoute;

  return html`
    <nav class="bottom-bar">
      <svg xmlns="http://www.w3.org/2000/svg" width="120" height="52" viewBox="0 0 120 52" role="img" aria-label="Nexus" style="flex-shrink:0">
        <defs>
          <filter id="etch" x="-10%" y="-10%" width="120%" height="120%">
            <feDropShadow dx="0" dy="1" stdDeviation="0.5" flood-color="rgba(0,0,0,0.6)" flood-opacity="1" result="shadow"/>
            <feDropShadow dx="0" dy="-1" stdDeviation="0.4" flood-color="rgba(255,255,255,0.12)" flood-opacity="1"/>
          </filter>
        </defs>
        <rect x="12" y="18" width="2.5" height="16" rx="1.25" fill="rgba(255,255,255,0.08)" filter="url(#etch)"/>
        <text x="22" y="33" font-family="-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif" font-size="14" font-weight="700" letter-spacing="3" fill="rgba(255,255,255,0.10)" filter="url(#etch)">NEXUS</text>
      </svg>
      ${TABS.map(tab => html`
        <a
          key=${tab.id}
          href=${tab.href}
          class=${'bottom-bar-tab' + (highlightRoute === tab.id ? ' active' : '')}
        >
          <span class="bottom-bar-icon"><${tab.Icon} /></span>
          <span class="bottom-bar-label">${tab.label}</span>
        </a>
      `)}
    </nav>
  `;
}
