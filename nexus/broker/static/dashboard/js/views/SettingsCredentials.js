// SettingsCredentials (NEX-266) — admin CRUD for broker-managed credentials.
//
// Pairs with the existing admin REST surface in admin_credentials.go.
// List / Create / Edit / Delete; no audit (NEX-268 owns that).
//
// Constraint surfaced honestly in the UX: the backend PUT replaces the
// entire credential on update (there's no metadata-only patch), so
// Edit forces re-entering all bundle fields. A note in
// CredentialBundleForm calls this out so operators don't lose
// trailing-character secrets to a "just update description" mistake.

const { html, useState, useEffect, useMemo } = window.__preact;
import {
  listCredentials,
  upsertCredential,
  deleteCredential,
} from '../api/admin.js';
import {
  CredentialBundleForm,
  bundleFieldsFor,
  validateBundleClient,
} from '../components/CredentialBundleForm.js';

const KINDS = ['provider', 'jira', 'imap'];
const MODES = ['proxy', 'fetch', 'both'];

function emptyBundleFor(kind) {
  const b = {};
  bundleFieldsFor(kind).forEach((f) => {
    b[f.key] = f.type === 'checkbox' ? false : '';
  });
  return b;
}

function CredentialRow({ row, onEdit, onDelete, busy }) {
  const aspectsLabel = (row.allowed_aspects || []).join(', ') || '*';
  return html`
    <tr>
      <td class="settings-cred-name">${row.name}</td>
      <td>${row.kind || 'provider'}</td>
      <td>${row.mode || 'fetch'}</td>
      <td title=${aspectsLabel}>${aspectsLabel.length > 28 ? aspectsLabel.slice(0, 25) + '…' : aspectsLabel}</td>
      <td class="settings-cred-actions">
        <button class="settings-btn" onClick=${() => onEdit(row)} disabled=${busy}>Edit</button>
        <button class="settings-btn settings-btn-secondary" onClick=${() => onDelete(row)} disabled=${busy}>Delete</button>
      </td>
    </tr>
  `;
}

function DeleteConfirm({ name, kind, onConfirm, onCancel, busy }) {
  return html`
    <div class="settings-modal-backdrop" onClick=${onCancel}>
      <div class="settings-modal" onClick=${(e) => e.stopPropagation()}>
        <h3 class="settings-modal-title">Delete credential</h3>
        <p>Delete <code>${name}</code> (kind: ${kind})?</p>
        <p class="settings-modal-warn">
          Aspects using this credential will fall back to defaults or fail. This cannot be undone.
        </p>
        <div class="settings-modal-actions">
          <button class="settings-btn" onClick=${onCancel} disabled=${busy}>Cancel</button>
          <button class="settings-btn settings-btn-secondary" onClick=${onConfirm} disabled=${busy}>Delete</button>
        </div>
      </div>
    </div>
  `;
}

function CredentialModal({ mode, initial, onSave, onCancel, busy }) {
  // mode: 'create' | 'edit'
  // Create: pick kind + name first, then bundle fields.
  // Edit: name + kind are locked (path-scoped on PUT); operator updates
  // description + allowed_aspects + mode and re-enters the bundle.
  const isEdit = mode === 'edit';
  const [name, setName] = useState(initial?.name || '');
  const [kind, setKind] = useState(initial?.kind || 'provider');
  const [description, setDescription] = useState(initial?.description || '');
  const [allowedAspects, setAllowedAspects] = useState(
    (initial?.allowed_aspects || ['*']).join(', '),
  );
  const [credMode, setCredMode] = useState(initial?.mode || 'fetch');
  const [bundle, setBundle] = useState(emptyBundleFor(initial?.kind || 'provider'));
  // Edit-mode toggle: default OFF (keep existing bundle on server).
  // When OFF, submit omits the bundle and the backend preserves what's
  // stored — operator can flip mode / description / allowed-aspects
  // without re-entering the API key. Toggle ON only when actually
  // rotating the secret. Create mode is unaffected (must enter bundle).
  const [rotateBundle, setRotateBundle] = useState(false);
  const [error, setError] = useState('');

  // When kind changes on Create, reset bundle to that kind's shape.
  useEffect(() => {
    if (!isEdit) setBundle(emptyBundleFor(kind));
  }, [kind, isEdit]);

  function submit() {
    setError('');
    if (!name.trim()) {
      setError('Name is required.');
      return;
    }
    // Only validate the bundle when we're going to send it — either
    // a fresh create or an explicit rotate.
    const sendBundle = !isEdit || rotateBundle;
    if (sendBundle) {
      const bundleErr = validateBundleClient(kind, bundle);
      if (bundleErr) {
        setError(bundleErr);
        return;
      }
    }
    const allowed = allowedAspects
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
    if (allowed.length === 0) allowed.push('*');
    const payload = {
      kind,
      description: description.trim(),
      allowed_aspects: allowed,
      mode: credMode,
    };
    if (sendBundle) {
      payload.bundle = bundle;
    }
    onSave(name.trim(), payload).then(
      () => { /* parent closes modal */ },
      (err) => setError(err.message || 'save failed'),
    );
  }

  return html`
    <div class="settings-modal-backdrop" onClick=${onCancel}>
      <div class="settings-modal settings-modal-wide" onClick=${(e) => e.stopPropagation()}>
        <h3 class="settings-modal-title">${isEdit ? 'Edit credential' : 'New credential'}</h3>

        <div class="settings-bundle-form">
          <label class="settings-field">
            <span class="settings-field-label">Name *</span>
            <input
              type="text"
              class="settings-input"
              value=${name}
              disabled=${isEdit}
              onInput=${(e) => setName(e.target.value)}
              placeholder="e.g. claude-api"
            />
          </label>
          <label class="settings-field">
            <span class="settings-field-label">Kind *</span>
            <select class="settings-select" value=${kind} disabled=${isEdit} onChange=${(e) => setKind(e.target.value)}>
              ${KINDS.map((k) => html`<option key=${k} value=${k}>${k}</option>`)}
            </select>
          </label>
          <label class="settings-field">
            <span class="settings-field-label">Mode</span>
            <select class="settings-select" value=${credMode} onChange=${(e) => setCredMode(e.target.value)}>
              ${MODES.map((m) => html`<option key=${m} value=${m}>${m}</option>`)}
            </select>
          </label>
          <label class="settings-field">
            <span class="settings-field-label">Allowed aspects</span>
            <input
              type="text"
              class="settings-input"
              value=${allowedAspects}
              onInput=${(e) => setAllowedAspects(e.target.value)}
              placeholder="* (any) or comma-separated aspect names"
            />
          </label>
          <label class="settings-field">
            <span class="settings-field-label">Description</span>
            <input
              type="text"
              class="settings-input"
              value=${description}
              onInput=${(e) => setDescription(e.target.value)}
              placeholder="optional"
            />
          </label>
        </div>

        <h4 class="settings-modal-section">Bundle (${kind})</h4>
        ${isEdit && html`
          <label class="settings-field" style="margin-bottom: 8px;">
            <input
              type="checkbox"
              checked=${rotateBundle}
              onChange=${(e) => setRotateBundle(e.target.checked)}
            />
            <span style="margin-left: 6px;">Re-enter bundle (rotate secret) — leave unchecked to keep the stored bundle</span>
          </label>
        `}
        ${(!isEdit || rotateBundle) && html`
          <${CredentialBundleForm}
            kind=${kind}
            bundle=${bundle}
            onChange=${setBundle}
            editing=${isEdit}
          />
        `}

        ${error && html`<div class="settings-error settings-error-banner">${error}</div>`}

        <div class="settings-modal-actions">
          <button class="settings-btn" onClick=${onCancel} disabled=${busy}>Cancel</button>
          <button class="settings-btn settings-btn-primary" onClick=${submit} disabled=${busy}>
            ${isEdit ? 'Save' : 'Create'}
          </button>
        </div>
      </div>
    </div>
  `;
}

export function SettingsCredentials() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [rows, setRows] = useState([]);
  const [filterKind, setFilterKind] = useState('');
  const [filterText, setFilterText] = useState('');
  const [modal, setModal] = useState(null); // { mode: 'create'|'edit', initial }
  const [deleting, setDeleting] = useState(null); // row to delete
  const [busy, setBusy] = useState(false);

  async function loadAll() {
    setLoading(true);
    setError('');
    try {
      const body = await listCredentials(filterKind || undefined);
      const list = (body && body.credentials) || [];
      list.sort((a, b) => (a.name || '').localeCompare(b.name || ''));
      setRows(list);
    } catch (e) {
      setError(e.message || 'load failed');
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { loadAll(); }, [filterKind]);

  const filtered = useMemo(() => {
    if (!filterText.trim()) return rows;
    const q = filterText.trim().toLowerCase();
    return rows.filter((r) =>
      (r.name || '').toLowerCase().includes(q) ||
      (r.description || '').toLowerCase().includes(q),
    );
  }, [rows, filterText]);

  async function onSave(name, payload) {
    setBusy(true);
    try {
      await upsertCredential(name, payload);
      setModal(null);
      await loadAll();
    } finally {
      setBusy(false);
    }
  }

  async function onConfirmDelete() {
    if (!deleting) return;
    setBusy(true);
    try {
      await deleteCredential(deleting.name);
      setDeleting(null);
      await loadAll();
    } catch (e) {
      setError(e.message || 'delete failed');
    } finally {
      setBusy(false);
    }
  }

  return html`
    <div class="settings-credentials">
      <div class="settings-aspects-header">
        <span class="settings-aspects-title">Credentials (${filtered.length}${rows.length !== filtered.length ? '/' + rows.length : ''})</span>
        <div style="display:flex;gap:8px;align-items:center;">
          <select class="settings-select" value=${filterKind} onChange=${(e) => setFilterKind(e.target.value)}>
            <option value="">all kinds</option>
            ${KINDS.map((k) => html`<option key=${k} value=${k}>${k}</option>`)}
          </select>
          <input
            type="text"
            class="settings-input"
            placeholder="search name / description"
            value=${filterText}
            onInput=${(e) => setFilterText(e.target.value)}
            style="min-width:220px;"
          />
          <button class="settings-btn settings-btn-primary" onClick=${() => setModal({ mode: 'create' })}>+ New</button>
        </div>
      </div>

      ${loading && html`<div class="settings-loading">Loading credentials…</div>`}
      ${error && !loading && html`
        <div class="settings-error-box">
          <div>${error}</div>
          <button class="settings-btn" onClick=${loadAll}>Retry</button>
        </div>
      `}
      ${!loading && !error && filtered.length === 0 && html`
        <div class="settings-empty">${rows.length === 0 ? 'No credentials yet. Click "+ New" to add one.' : 'No credentials match the current filter.'}</div>
      `}
      ${!loading && !error && filtered.length > 0 && html`
        <table class="settings-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>Mode</th>
              <th>Aspects</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            ${filtered.map((row) => html`
              <${CredentialRow}
                key=${row.name}
                row=${row}
                onEdit=${(r) => setModal({ mode: 'edit', initial: r })}
                onDelete=${(r) => setDeleting(r)}
                busy=${busy}
              />
            `)}
          </tbody>
        </table>
      `}

      ${modal && html`
        <${CredentialModal}
          mode=${modal.mode}
          initial=${modal.initial}
          onSave=${onSave}
          onCancel=${() => setModal(null)}
          busy=${busy}
        />
      `}

      ${deleting && html`
        <${DeleteConfirm}
          name=${deleting.name}
          kind=${deleting.kind || 'provider'}
          onConfirm=${onConfirmDelete}
          onCancel=${() => setDeleting(null)}
          busy=${busy}
        />
      `}
    </div>
  `;
}
