const { html, useEffect, useState } = window.__preact;

import { envHealth } from '../../api.js';

export function EnvHealthPanel({ onClose }) {
  const [health, setHealth] = useState(null);
  const [error, setError] = useState('');

  useEffect(() => {
    let alive = true;
    function tick() {
      envHealth().then((data) => {
        if (!alive) return;
        setHealth(data);
        setError('');
      }).catch((e) => {
        if (alive) setError(e.message || 'env.health failed');
      });
    }
    tick();
    const interval = setInterval(tick, 15000);
    return () => {
      alive = false;
      clearInterval(interval);
    };
  }, []);

  return html`
    <aside class="watch-panel env-panel">
      <header class="watch-panel-head">
        <span>Env</span>
        <button class="panel-close" onClick=${onClose} aria-label="Close env panel">x</button>
      </header>
      ${!health && !error ? html`<div class="watch-panel-empty">loading...</div>` : null}
      ${error ? html`<div class="watch-panel-error">${error}</div>` : null}
      ${health ? html`
        <div class="env-components">
          ${(health.components || []).map((component) => html`
            <div key=${component.name} class=${`env-comp ${component.healthy ? 'ok' : 'bad'}`}>
              <span>${component.name}</span>
              <small>${component.detail || component.kind || ''}</small>
            </div>
          `)}
        </div>
        <div class="env-pods">
          <span>pods</span>
          <strong>${health.pods_running || 0}/${health.pods_total || 0}</strong>
        </div>
        <ul class="env-pvcs">
          ${(health.pvcs || []).map((pvc) => html`
            <li key=${pvc.name} class=${pvc.status === 'Bound' ? 'ok' : 'bad'}>
              <span>${pvc.name}</span>
              <small>${pvc.status}</small>
            </li>
          `)}
        </ul>
        ${health.last_deploy ? html`<div class="env-deploy">deploy: ${health.last_deploy}</div>` : null}
      ` : null}
    </aside>
  `;
}
