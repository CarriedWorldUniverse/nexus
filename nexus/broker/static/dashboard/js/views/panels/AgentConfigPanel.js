const { html, useEffect, useState } = window.__preact;

import { getAgentModelConfig, putAgentModelConfig, getDispatchEnabled, setDispatchEnabled } from '../../api.js';

const MODEL_FIELDS = [
  ['primary_model', 'Primary model'],
  ['primary_credential', 'Primary credential'],
  ['judge_provider', 'Judge provider'],
  ['judge_model', 'Judge model'],
  ['judge_credential', 'Judge credential'],
  ['compact_model', 'Compact model'],
  ['compact_credential', 'Compact credential'],
];

export function AgentConfigPanel({ agent, rosterRow, onClose, onDispatchEnabledChange }) {
  const [cfg, setCfg] = useState(null);
  const [enabled, setEnabled] = useState(true);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState('');

  useEffect(() => {
    if (!agent) return;
    let cancelled = false;
    setLoading(true);
    setError('');
    setSaved('');
    Promise.all([
      getAgentModelConfig(agent).catch((err) => {
        throw new Error(err.message || 'model config load failed');
      }),
      getDispatchEnabled(agent).catch(() => ({ enabled: rosterRow && rosterRow.dispatch_enabled !== false })),
    ]).then(([modelCfg, dispatchCfg]) => {
      if (cancelled) return;
      setCfg(modelCfg || { aspect: agent });
      setEnabled(dispatchCfg ? dispatchCfg.enabled !== false : true);
    }).catch((err) => {
      if (!cancelled) setError(err.message || 'load failed');
    }).finally(() => {
      if (!cancelled) setLoading(false);
    });
    return () => { cancelled = true; };
  }, [agent]);

  function updateField(field, value) {
    setCfg((prev) => ({ ...(prev || { aspect: agent }), [field]: value }));
    setSaved('');
  }

  async function saveConfig() {
    if (!cfg) return;
    setBusy(true);
    setError('');
    setSaved('');
    const payload = {};
    MODEL_FIELDS.forEach(([field]) => {
      payload[field] = (cfg[field] || '').trim();
    });
    try {
      const fresh = await putAgentModelConfig(agent, payload);
      setCfg(fresh || { aspect: agent });
      setSaved('Saved');
    } catch (err) {
      setError(err.message || 'save failed');
    } finally {
      setBusy(false);
    }
  }

  async function toggleEnabled() {
    const next = !enabled;
    setEnabled(next);
    setError('');
    setSaved('');
    if (onDispatchEnabledChange) onDispatchEnabledChange(agent, next);
    try {
      const fresh = await setDispatchEnabled(agent, next);
      const actual = fresh ? fresh.enabled !== false : next;
      setEnabled(actual);
      if (onDispatchEnabledChange) onDispatchEnabledChange(agent, actual);
    } catch (err) {
      setEnabled(!next);
      if (onDispatchEnabledChange) onDispatchEnabledChange(agent, !next);
      setError(err.message || 'toggle failed');
    }
  }

  if (!agent) return null;
  return html`
    <aside class="watch-panel agent-config-panel">
      <header class="watch-panel-head">
        <span>${agent} config</span>
        <button class="panel-close" onClick=${onClose} aria-label="Close agent config panel">x</button>
      </header>
      <div class="agent-config-body">
        ${rosterRow ? html`
          <div class="agent-config-meta">
            <span>${rosterRow.provider || 'provider unknown'}</span>
            <span>${rosterRow.model || 'model unknown'}</span>
          </div>
        ` : null}
        <label class="agent-config-toggle">
          <span>Dispatch enabled</span>
          <input type="checkbox" checked=${enabled} onChange=${toggleEnabled} disabled=${loading || busy} />
        </label>
        ${loading ? html`<div class="watch-panel-empty">Loading...</div>` : null}
        ${error ? html`<div class="watch-panel-error">${error}</div>` : null}
        ${!loading && cfg ? html`
          <div class="agent-config-form">
            ${MODEL_FIELDS.map(([field, label]) => html`
              <label class="dispatch-field" key=${field}>
                ${label}
                <input
                  value=${cfg[field] || ''}
                  placeholder=${field.endsWith('_credential') ? '(inherit credential)' : '(inherit model config)'}
                  onInput=${(e) => updateField(field, e.target.value)}
                  disabled=${busy}
                />
              </label>
            `)}
            <div class="agent-config-actions">
              ${saved ? html`<span class="dispatch-posted">${saved}</span>` : null}
              <button class="dispatch-submit" onClick=${saveConfig} disabled=${busy}>${busy ? 'Saving...' : 'Save config'}</button>
            </div>
          </div>
        ` : null}
      </div>
    </aside>
  `;
}
