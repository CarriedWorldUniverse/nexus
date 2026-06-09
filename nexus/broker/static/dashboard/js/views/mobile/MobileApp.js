const { html, useState, useEffect, useRef } = window.__preact;

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
  const appRef = useRef(null);

  // iOS Safari doesn't shrink the layout viewport when the keyboard opens, so a
  // 100dvh app leaves the composer/messages hidden behind the keyboard. Drive
  // the app height from window.visualViewport (which DOES track the keyboard),
  // and flag keyboard-open so the tab bar gets out of the way (NEX-535 follow-up).
  useEffect(() => {
    const vv = window.visualViewport;
    const el = appRef.current;
    if (!vv || !el) return undefined;
    const apply = () => {
      el.style.height = `${vv.height}px`;
      el.classList.toggle('keyboard-open', window.innerHeight - vv.height > 120);
    };
    apply();
    vv.addEventListener('resize', apply);
    vv.addEventListener('scroll', apply);
    return () => {
      vv.removeEventListener('resize', apply);
      vv.removeEventListener('scroll', apply);
    };
  }, []);

  function selectTab(id) {
    if (id === 'converse') setUnread(0);
    setTab(id);
  }

  return html`
    <div class="m-app" ref=${appRef}>
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
