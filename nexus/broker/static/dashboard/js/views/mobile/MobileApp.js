const { html, useState } = window.__preact;

import { MobileConverse } from './MobileConverse.js';
import { MobileRuns } from './MobileRuns.js';
import { MobileNotifications } from './MobileNotifications.js';

const TABS = [
  { id: 'converse', label: 'Converse' },
  { id: 'runs', label: 'Runs' },
];

export function MobileApp() {
  const [tab, setTab] = useState('converse');
  const [unread, setUnread] = useState(0);

  function selectTab(id) {
    if (id === 'converse') setUnread(0);
    setTab(id);
  }

  return html`
    <div class="m-app">
      <main class="m-main">
        ${tab === 'converse'
          ? html`<${MobileConverse} onActive=${() => setUnread(0)} />`
          : html`<${MobileRuns} />`}
      </main>
      <nav class="m-tabbar" aria-label="Mobile dashboard sections">
        ${TABS.map((t) => html`
          <button
            key=${t.id}
            type="button"
            class=${tab === t.id ? 'm-tab active' : 'm-tab'}
            aria-current=${tab === t.id ? 'page' : null}
            onClick=${() => selectTab(t.id)}
          >
            <span>${t.label}</span>
            ${t.id === 'converse' && unread > 0
              ? html`<span class="m-badge">${unread > 9 ? '9+' : unread}</span>`
              : null}
          </button>
        `)}
      </nav>
      <${MobileNotifications} activeTab=${tab} onUnread=${(n) => setUnread((u) => u + n)} />
    </div>
  `;
}
