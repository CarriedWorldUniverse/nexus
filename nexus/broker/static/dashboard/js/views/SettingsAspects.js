// SettingsAspects (NEX-265) — per-aspect model + credential override picker.
//
// Loads the roster (existing fetchAgents path) for the canonical list of
// aspects + their keyfile-loaded primary model/provider, plus each
// aspect's override row from /api/admin/aspects/{name}/model-config.
// Operator sees a row per aspect with three kinds (primary / judge /
// compact) and an inline edit form per kind.
//
// Scope limit worth knowing: the backend's GetAspectModelConfig only
// returns the override state — the funnel reads the keyfile separately
// at startup. roster.list surfaces the primary model/provider (because
// every running aspect publishes them on registration), so we can show
// the primary keyfile value. Judge + compact have no equivalent roster
// hook today, so when their overrides are unset we render
// "(keyfile default)" with no concrete value. Phase-3 follow-up could
// extend the model-config endpoint to resolve effective values; not
// shipped here to keep the slice tight.

const { html, useState, useEffect } = window.__preact;
import { fetchAgents } from '../api.js';
import { getModelConfig, setModelConfig, listCredentials } from '../api/admin.js';

const KINDS = [
  { id: 'primary', label: 'Primary' },
  { id: 'judge',   label: 'Judge'   },
  { id: 'compact', label: 'Compact' },
];

// Pulls the override row + roster row apart into a per-kind view
// model: { model, credential, keyfileModel, hasOverride, source }.
function summarizeKind(kind, override, rosterRow) {
  const overrideModel      = override ? override[kind + '_model']      : null;
  const overrideCredential = override ? override[kind + '_credential'] : null;
  // Roster surfaces primary only (rosterRow.model). Judge + compact
  // have no roster-side data; their keyfile values are visible to the
  // funnel at startup but not to this UI.
  const keyfileModel = (kind === 'primary' && rosterRow && rosterRow.model) || '';
  const hasOverride = !!(overrideModel || overrideCredential);
  return {
    model:        overrideModel || '',
    credential:   overrideCredential || '',
    keyfileModel,
    hasOverride,
    source:       hasOverride ? 'override' : 'keyfile',
  };
}

function SourceBadge({ source }) {
  return html`<span class=${'settings-source settings-source-' + source}>${source}</span>`;
}

function KindRow({ aspect, kind, vm, credentials, onSave, onClear, busy }) {
  const [editing, setEditing] = useState(false);
  const [model, setModel] = useState(vm.model || vm.keyfileModel || '');
  const [credential, setCredential] = useState(vm.credential);
  const [error, setError] = useState('');

  // Re-sync local form when the underlying vm changes (after save).
  useEffect(() => {
    setModel(vm.model || vm.keyfileModel || '');
    setCredential(vm.credential);
    setError('');
  }, [vm.model, vm.credential, vm.keyfileModel]);

  function submit() {
    const m = model.trim();
    const c = credential.trim();
    // If either is set, model is required (an unnamed model with a credential is meaningless).
    if (c && !m) {
      setError('Model is required when credential is set.');
      return;
    }
    onSave(kind, { model: m, credential: c }).then(
      () => { setEditing(false); setError(''); },
      (err) => setError(err.message || 'save failed'),
    );
  }

  function clear() {
    onClear(kind).then(
      () => { setEditing(false); setError(''); },
      (err) => setError(err.message || 'clear failed'),
    );
  }

  const displayModel = vm.hasOverride
    ? vm.model
    : (vm.keyfileModel || '(keyfile default)');
  const displayCredential = vm.credential || '—';

  return html`
    <div class="settings-kind-row">
      <div class="settings-kind-label">${kind === 'primary' ? 'Primary' : kind === 'judge' ? 'Judge' : 'Compact'}:</div>
      ${!editing && html`
        <div class="settings-kind-value">
          <span class="settings-kind-model">${displayModel}</span>
          <span class="settings-kind-credential">| ${displayCredential}</span>
          <${SourceBadge} source=${vm.source} />
          <button class="settings-btn" onClick=${() => setEditing(true)} disabled=${busy}>Edit</button>
          ${vm.hasOverride && html`
            <button class="settings-btn settings-btn-secondary" onClick=${clear} disabled=${busy}>Clear</button>
          `}
        </div>
      `}
      ${editing && html`
        <div class="settings-kind-edit">
          <input
            type="text"
            class="settings-input"
            placeholder="model id (e.g. claude-opus-4-7)"
            value=${model}
            onInput=${(e) => setModel(e.target.value)}
            disabled=${busy}
          />
          <select
            class="settings-select"
            value=${credential}
            onChange=${(e) => setCredential(e.target.value)}
            disabled=${busy}
          >
            <option value="">(no credential override)</option>
            ${credentials.map((c) => html`
              <option key=${c.name} value=${c.name}>${c.name} · ${c.kind || 'provider'}</option>
            `)}
          </select>
          <button class="settings-btn settings-btn-primary" onClick=${submit} disabled=${busy}>Save</button>
          <button class="settings-btn" onClick=${() => { setEditing(false); setError(''); }} disabled=${busy}>Cancel</button>
          ${error && html`<span class="settings-error">${error}</span>`}
        </div>
      `}
    </div>
  `;
}

function AspectCard({ aspect, override, rosterRow, credentials, onSaved }) {
  const [busy, setBusy] = useState(false);
  const [needsReload, setNeedsReload] = useState(false);

  function buildPayloadForKind(kind, { model, credential }) {
    // Each kind has two columns. Provide both fields so empty-string
    // clears (PUT semantics: omitted = unchanged, "" = clear).
    return {
      [kind + '_model']:      model || '',
      [kind + '_credential']: credential || '',
    };
  }

  async function onSave(kind, payload) {
    setBusy(true);
    try {
      const fresh = await setModelConfig(aspect, buildPayloadForKind(kind, payload));
      setNeedsReload(true);
      onSaved(aspect, fresh);
    } finally {
      setBusy(false);
    }
  }

  async function onClear(kind) {
    setBusy(true);
    try {
      const fresh = await setModelConfig(aspect, buildPayloadForKind(kind, { model: '', credential: '' }));
      setNeedsReload(true);
      onSaved(aspect, fresh);
    } finally {
      setBusy(false);
    }
  }

  return html`
    <div class="settings-aspect-card">
      <div class="settings-aspect-header">
        <span class="settings-aspect-name">${aspect}</span>
        ${rosterRow && rosterRow.provider && html`<span class="settings-aspect-meta">provider: ${rosterRow.provider}</span>`}
        ${needsReload && html`<span class="settings-reload-banner">Saved. Restart ${aspect} to apply.</span>`}
      </div>
      ${KINDS.map((k) => html`
        <${KindRow}
          key=${k.id}
          aspect=${aspect}
          kind=${k.id}
          vm=${summarizeKind(k.id, override, rosterRow)}
          credentials=${credentials}
          onSave=${onSave}
          onClear=${onClear}
          busy=${busy}
        />
      `)}
    </div>
  `;
}

export function SettingsAspects() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  // aspects: ordered list of { name, model, provider } from roster
  // overrides: map of aspect-name → AspectModelConfig
  // credentials: array from listCredentials
  const [aspects, setAspects] = useState([]);
  const [overrides, setOverrides] = useState({});
  const [credentials, setCredentials] = useState([]);

  async function loadAll() {
    setLoading(true);
    setError('');
    try {
      const [agents, creds] = await Promise.all([fetchAgents(), listCredentials()]);
      const namedAgents = (agents || []).filter((a) => a && (a.name || a.id));
      const overridesByName = {};
      // Fetch overrides per aspect in parallel. Per-aspect failures
      // don't abort the page — operator still sees the rest of the
      // list with the offender's row absent.
      const settled = await Promise.allSettled(
        namedAgents.map((a) => getModelConfig(a.name || a.id).then((cfg) => [a.name || a.id, cfg])),
      );
      for (const r of settled) {
        if (r.status === 'fulfilled') {
          const [name, cfg] = r.value;
          overridesByName[name] = cfg;
        }
      }
      setAspects(namedAgents);
      setOverrides(overridesByName);
      setCredentials(creds || []);
    } catch (e) {
      setError(e.message || 'load failed');
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { loadAll(); }, []);

  function onSaved(name, fresh) {
    setOverrides((prev) => ({ ...prev, [name]: fresh }));
  }

  if (loading) {
    return html`<div class="settings-loading">Loading aspects…</div>`;
  }
  if (error) {
    return html`
      <div class="settings-error-box">
        <div>Failed to load: ${error}</div>
        <button class="settings-btn" onClick=${loadAll}>Retry</button>
      </div>
    `;
  }
  if (aspects.length === 0) {
    return html`<div class="settings-empty">No aspects registered. Register one via the keyfile and reload.</div>`;
  }

  return html`
    <div class="settings-aspects">
      <div class="settings-aspects-header">
        <span class="settings-aspects-title">Aspects (${aspects.length})</span>
        <button class="settings-btn" onClick=${loadAll}>Reload</button>
      </div>
      ${aspects.map((a) => {
        const name = a.name || a.id;
        return html`
          <${AspectCard}
            key=${name}
            aspect=${name}
            rosterRow=${a}
            override=${overrides[name]}
            credentials=${credentials}
            onSaved=${onSaved}
          />
        `;
      })}
    </div>
  `;
}
