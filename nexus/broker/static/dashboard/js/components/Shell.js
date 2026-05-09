const { html } = window.__preact;
import { BottomBar } from './BottomBar.js';

export function Shell({ activeRoute, children }) {
  return html`
    <div class="shell">
      <main class="shell-content">
        ${children}
      </main>
      <${BottomBar} activeRoute=${activeRoute} />
    </div>
  `;
}
