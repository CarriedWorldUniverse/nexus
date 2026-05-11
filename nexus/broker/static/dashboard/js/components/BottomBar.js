const { html } = window.__preact;
import { IconChat, IconFiles, IconTickets, IconStatus, IconAgents, IconDocs, IconSplit } from '../icons.js';
import { usageData } from '../state.js';

// "Activity" rather than the old "Agents" label — the tab now points
// at ObserveView (per-aspect observability stream), not the dead DM
// list. URL stays #/agents to preserve operator muscle memory and
// existing bookmarks.
const TABS = [
  { id: 'feed',     label: 'Feed',     Icon: IconChat     },
  { id: 'agents',   label: 'Activity', Icon: IconAgents   },
  { id: 'files',    label: 'Files',    Icon: IconFiles    },
  { id: 'tickets',  label: 'Tickets',  Icon: IconTickets  },
  { id: 'docs',     label: 'Docs',     Icon: IconDocs     },
  { id: 'status',   label: 'Status',   Icon: IconStatus   },
];

function fmtTokens(n) {
  if (!n || n === 0) return '0';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000)     return (n / 1_000).toFixed(0) + 'k';
  return String(n);
}

export function BottomBar({ activeRoute }) {
  const usage = usageData.value;
  const totalOutput = usage?.totals?.output || 0;
  const totalCacheRead = usage?.totals?.cache_read || 0;
  const period = usage?.period || '7d';

  function toggleSplit(e) {
    e.preventDefault();
    if (activeRoute === 'split') {
      // Exit split: go to the left pane's last view, or feed.
      const back = localStorage.getItem('split.left.view') || 'feed';
      window.location.hash = '#/' + back;
    } else {
      window.location.hash = '#/split';
    }
  }

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
          href=${'#/' + tab.id}
          class=${'bottom-bar-tab' + (activeRoute === tab.id ? ' active' : '')}
        >
          <span class="bottom-bar-icon">${tab.Icon ? html`<${tab.Icon} />` : '🤖'}</span>
          <span class="bottom-bar-label">${tab.label}</span>
        </a>
      `)}
      <button
        class=${'bottom-bar-split-toggle' + (activeRoute === 'split' ? ' active' : '')}
        onClick=${toggleSplit}
        title=${activeRoute === 'split' ? 'Exit split view' : 'Enter split view'}
        aria-label=${activeRoute === 'split' ? 'Exit split view' : 'Enter split view'}
      >
        <${IconSplit} />
      </button>
      ${totalOutput > 0 && html`
        <div class="bottom-bar-usage" title=${`Token usage (${period}) — output: ${totalOutput.toLocaleString()}, cache read: ${totalCacheRead.toLocaleString()}`}>
          <span class="bottom-bar-usage-value">${fmtTokens(totalOutput)}</span>
          <span class="bottom-bar-usage-label">out · ${period}</span>
        </div>
      `}
    </nav>
  `;
}
