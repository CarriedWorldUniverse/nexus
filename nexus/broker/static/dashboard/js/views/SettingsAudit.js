// SettingsAudit (NEX-268) — per-credential audit trail viewer.
//
// Reads from /api/admin/credentials/{name}/audit. Each row records
// who (aspect) did what (action) to which credential, when. Read-only
// — operator uses this to investigate access patterns, suspicious
// fetches, or just confirm a recent rotation took effect.
//
// Picker is driven both by user interaction AND by URL hash (so the
// Credentials page can deep-link to a credential's audit row via
// #/configure/audit?cred=<name>). The hash listener stays narrow:
// only the cred= query string of the audit sub-route is observed.

const { html, useState, useEffect } = window.__preact;
import { listCredentials, getCredentialAudit } from '../api/admin.js';

const DEFAULT_LIMIT = 100;

// Returns the cred=<name> query value from the current URL hash, or
// '' if absent. The hash format used elsewhere in the dashboard is
// #/path[?qs]; we parse the qs after the first '?'.
function getCredFromHash() {
  const hash = window.location.hash;
  if (!hash.startsWith('#/configure/audit')) return '';
  const q = hash.indexOf('?');
  if (q < 0) return '';
  const params = new URLSearchParams(hash.slice(q + 1));
  return params.get('cred') || '';
}

// Format ISO timestamp into a compact local display. Falls back to
// the raw value if Date can't parse it (preserves info rather than
// silently swallowing).
function formatTs(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

function ActionBadge({ action }) {
  // Common actions: create, update, delete, fetch, audit-fail.
  // Color the failures distinctly so they stand out on scroll.
  const failed = typeof action === 'string' && action.toLowerCase().includes('fail');
  return html`
    <span class=${'settings-audit-action' + (failed ? ' settings-audit-action-failed' : '')}>${action}</span>
  `;
}

function AuditRow({ row }) {
  const [expanded, setExpanded] = useState(false);
  const hasDetails = row.details && (typeof row.details === 'object' ? Object.keys(row.details).length > 0 : true);

  return html`
    <tr
      class=${'settings-audit-row' + (hasDetails ? ' settings-audit-row-clickable' : '')}
      onClick=${hasDetails ? () => setExpanded((e) => !e) : null}
    >
      <td class="settings-audit-ts">${formatTs(row.ts)}</td>
      <td class="settings-audit-aspect">${row.aspect || '—'}</td>
      <td><${ActionBadge} action=${row.action || 'unknown'} /></td>
      <td class="settings-audit-detail-cell">
        ${expanded && hasDetails && html`
          <pre class="settings-audit-detail">${JSON.stringify(row.details, null, 2)}</pre>
        `}
        ${!expanded && hasDetails && html`<span class="settings-audit-detail-hint">click to expand</span>`}
      </td>
    </tr>
  `;
}

export function SettingsAudit() {
  const [credentials, setCredentials] = useState([]);
  const [selected, setSelected] = useState(getCredFromHash());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [auditing, setAuditing] = useState(false);
  const [rows, setRows] = useState([]);

  // Load the credential list once on mount; selection drives audit
  // fetches separately.
  async function loadCredentials() {
    setLoading(true);
    setError('');
    try {
      const body = await listCredentials();
      const list = (body && body.credentials) || [];
      list.sort((a, b) => (a.name || '').localeCompare(b.name || ''));
      setCredentials(list);
    } catch (e) {
      setError(e.message || 'load failed');
    } finally {
      setLoading(false);
    }
  }

  async function loadAudit(name) {
    if (!name) {
      setRows([]);
      return;
    }
    setAuditing(true);
    setError('');
    try {
      const body = await getCredentialAudit(name, { limit: DEFAULT_LIMIT });
      const list = (body && body.audit) || [];
      // Backend returns most-recent first; assume sorted.
      setRows(list);
    } catch (e) {
      setError(e.message || 'audit fetch failed');
      setRows([]);
    } finally {
      setAuditing(false);
    }
  }

  useEffect(() => { loadCredentials(); }, []);
  useEffect(() => { loadAudit(selected); }, [selected]);

  // Re-sync selection from the URL when hash changes (so deep-link
  // navigation from another tab keeps working).
  useEffect(() => {
    function onHash() {
      const fromHash = getCredFromHash();
      if (fromHash !== selected) setSelected(fromHash);
    }
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, [selected]);

  function onPick(e) {
    const name = e.target.value;
    setSelected(name);
    // Reflect in URL so refresh / deep-link round-trip works.
    const base = '#/configure/audit';
    window.history.replaceState(null, '', name ? base + '?cred=' + encodeURIComponent(name) : base);
  }

  if (loading) return html`<div class="settings-loading">Loading credentials…</div>`;

  return html`
    <div class="settings-aspects">
      <div class="settings-aspects-header">
        <span class="settings-aspects-title">Audit Trail</span>
        <div style="display:flex;gap:8px;align-items:center;">
          <select class="settings-select" value=${selected} onChange=${onPick} disabled=${credentials.length === 0}>
            <option value="">(select a credential)</option>
            ${credentials.map((c) => html`
              <option key=${c.name} value=${c.name}>${c.name} · ${c.kind || 'provider'}</option>
            `)}
          </select>
          <button class="settings-btn" onClick=${() => loadAudit(selected)} disabled=${!selected || auditing}>Refresh</button>
        </div>
      </div>

      ${error && html`
        <div class="settings-error-box">
          <div>${error}</div>
          <button class="settings-btn" onClick=${() => selected ? loadAudit(selected) : loadCredentials()}>Retry</button>
        </div>
      `}

      ${!selected && !error && html`
        <div class="settings-empty">Pick a credential above to view its audit trail.</div>
      `}

      ${selected && !error && auditing && html`<div class="settings-loading">Loading audit rows…</div>`}

      ${selected && !error && !auditing && rows.length === 0 && html`
        <div class="settings-empty">No audit entries for <code>${selected}</code>.</div>
      `}

      ${selected && !error && !auditing && rows.length > 0 && html`
        <table class="settings-table settings-audit-table">
          <thead>
            <tr>
              <th>Timestamp</th>
              <th>Actor</th>
              <th>Action</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            ${rows.map((row) => html`<${AuditRow} key=${row.id} row=${row} />`)}
          </tbody>
        </table>
      `}
    </div>
  `;
}
