// SettingsDefaults (NEX-267) — per-aspect default-credential editor.
//
// Each aspect has four nullable default-credential columns on the
// aspects table (default_anthropic / default_openai / default_jira /
// default_imap). When an MCP client or runtime resolver asks for a
// credential of a given kind with no explicit name, the broker falls
// back to the per-aspect default. This view lets the operator pick
// which credential each aspect uses by default.
//
// Distinct from SettingsAspects (NEX-265) — that page sets per-aspect
// *model* overrides; this page sets per-aspect *credential* defaults.
// They share infrastructure (roster.list for the aspect list, admin
// REST surface) but address different concerns.

const { html, useState, useEffect, useMemo } = window.__preact;
import { fetchAgents } from '../api.js';
import {
  listCredentials,
  getCredentialDefaults, setCredentialDefaults,
  getNetworkDefaults, setNetworkDefaults,
} from '../api/admin.js';

// Each row corresponds to one column in the request schema (see
// adminAspectDefaultsReq in admin_credentials.go). The label is what
// the operator sees; the column is the JSON field name on the PUT body.
//
// kindFilter is used to filter the dropdown options by credential
// kind. Both anthropic + openai map to kind=provider (api_shape lives
// inside the bundle and isn't exposed via Metadata). Operator sees all
// provider credentials in both dropdowns with kind shown alongside —
// they pick the right one based on the credential name + description.
const KINDS = [
  { id: 'anthropic', label: 'Anthropic provider', column: 'default_anthropic_credential', kindFilter: 'provider' },
  { id: 'openai',    label: 'OpenAI provider',    column: 'default_openai_credential',    kindFilter: 'provider' },
  { id: 'jira',      label: 'Jira',               column: 'default_jira_credential',      kindFilter: 'jira'     },
  { id: 'imap',      label: 'IMAP',               column: 'default_imap_credential',      kindFilter: 'imap'     },
];

function DefaultsKindRow({ aspect, kind, current, options, onSave, busy }) {
  const [error, setError] = useState('');
  const placeholder = options.length === 0
    ? `(no ${kind.kindFilter} credentials — create one in Credentials tab)`
    : '(none)';

  function onChange(e) {
    const value = e.target.value;
    setError('');
    onSave(kind.column, value).catch((err) => {
      setError(err.message || 'save failed');
    });
  }

  return html`
    <div class="settings-kind-row">
      <div class="settings-defaults-label">${kind.label}:</div>
      <select
        class="settings-select"
        value=${current || ''}
        onChange=${onChange}
        disabled=${busy || options.length === 0}
      >
        <option value="">${placeholder}</option>
        ${options.map((o) => html`
          <option key=${o.name} value=${o.name}>${o.name}${o.description ? ' · ' + o.description : ''}</option>
        `)}
      </select>
      ${error && html`<span class="settings-error">${error}</span>`}
    </div>
  `;
}

function AspectDefaultsCard({ aspect, defaults, credentialsByKind, onSaved }) {
  const [busy, setBusy] = useState(false);

  async function onSave(column, value) {
    setBusy(true);
    try {
      const payload = { [column]: value || '' };
      const fresh = await setCredentialDefaults(aspect, payload);
      onSaved(aspect, fresh);
    } finally {
      setBusy(false);
    }
  }

  return html`
    <div class="settings-aspect-card">
      <div class="settings-aspect-header">
        <span class="settings-aspect-name">${aspect}</span>
      </div>
      ${KINDS.map((k) => html`
        <${DefaultsKindRow}
          key=${k.id}
          aspect=${aspect}
          kind=${k}
          current=${defaults ? defaults[k.column] : null}
          options=${credentialsByKind[k.kindFilter] || []}
          onSave=${onSave}
          busy=${busy}
        />
      `)}
    </div>
  `;
}

// NetworkDefaultsPanel (NEX-294 Slice 3) — single-row network-wide
// judge + compact defaults. Layers UNDER per-aspect overrides
// (NEX-263) and OVER the legacy hardcoded fallback ("haiku" model,
// ambient env credential).
//
// Model fields are free-form text (model ids are provider-specific
// strings; no validation server-side). Credential fields are
// dropdowns sourced from the provider credentials in the store.
// Empty string clears the default. Primary fields are intentionally
// absent — primary is per-aspect by design.
// CommittedField is a text input that defers save until the user
// finishes typing — committed on blur or Enter. Pre-fix the parent
// fired save() on every onChange (every keystroke), which set
// busy=true and disabled the input mid-type. Operator-reported
// 2026-05-27: "still cant edit the defaults". A select-style input
// fires onChange exactly once per user action, so selects didn't
// hit the same issue, but the text inputs were unusable.
//
// status: 'saved' | 'saving' | 'error' — drives a tiny indicator
// to the right of the field so the operator can see what happened.
function CommittedField({ initial, placeholder, onCommit, status }) {
  const [local, setLocal] = useState(initial || '');
  // Re-sync when the server-side initial changes (e.g. after a save
  // round-trips and we want to reflect the canonical value).
  useEffect(() => { setLocal(initial || ''); }, [initial]);

  function commit() {
    const next = local;
    if (next === (initial || '')) return; // no-op
    onCommit(next);
  }

  function onKeyDown(e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      e.target.blur(); // triggers onBlur → commit
    } else if (e.key === 'Escape') {
      setLocal(initial || '');
      e.target.blur();
    }
  }

  let indicator = '';
  if (status === 'saving') indicator = '…';
  else if (status === 'saved') indicator = '✓';
  else if (status === 'error') indicator = '✗';

  return html`
    <input
      class="settings-input"
      type="text"
      placeholder=${placeholder}
      value=${local}
      onInput=${(e) => setLocal(e.target.value)}
      onBlur=${commit}
      onKeyDown=${onKeyDown}
    />
    <span class="settings-defaults-status" aria-label=${status || ''}>${indicator}</span>
  `;
}

function NetworkDefaultsPanel({ providerCreds }) {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [nd, setNd] = useState({
    judge_model: '', judge_credential: '',
    compact_model: '', compact_credential: '',
  });
  // Per-field save status — 'idle' | 'saving' | 'saved' | 'error'.
  // Lets each row show its own indicator instead of one panel-wide
  // busy lockout that pre-fix disabled all inputs together.
  const [status, setStatus] = useState({});

  async function load() {
    setLoading(true);
    setError('');
    try {
      const fresh = await getNetworkDefaults();
      setNd({
        judge_model: (fresh && fresh.judge_model) || '',
        judge_credential: (fresh && fresh.judge_credential) || '',
        compact_model: (fresh && fresh.compact_model) || '',
        compact_credential: (fresh && fresh.compact_credential) || '',
      });
    } catch (e) {
      setError(e.message || 'load failed');
    } finally {
      setLoading(false);
    }
  }
  useEffect(() => { load(); }, []);

  async function save(field, value) {
    setStatus((s) => ({ ...s, [field]: 'saving' }));
    setError('');
    try {
      const fresh = await setNetworkDefaults({ [field]: value });
      setNd({
        judge_model: (fresh && fresh.judge_model) || '',
        judge_credential: (fresh && fresh.judge_credential) || '',
        compact_model: (fresh && fresh.compact_model) || '',
        compact_credential: (fresh && fresh.compact_credential) || '',
      });
      setStatus((s) => ({ ...s, [field]: 'saved' }));
    } catch (e) {
      setError(`${field}: ${e.message || 'save failed'}`);
      setStatus((s) => ({ ...s, [field]: 'error' }));
    }
  }

  if (loading) {
    return html`<div class="settings-aspect-card"><div class="settings-loading">Loading network defaults…</div></div>`;
  }

  const credPlaceholder = providerCreds.length === 0
    ? '(no provider credentials — create one in Credentials tab)'
    : '(none — falls through to per-aspect / legacy)';

  return html`
    <div class="settings-aspect-card">
      <div class="settings-aspect-header">
        <span class="settings-aspect-name">Network defaults (judge + compact)</span>
      </div>
      <div class="settings-defaults-note">
        Applies when an aspect has no per-aspect override.
        Text fields commit on blur or Enter (Esc to cancel).
        Primary model + credential are per-aspect only (set on the Aspects tab).
      </div>
      ${error && html`<div class="settings-error">${error}</div>`}

      <div class="settings-kind-row">
        <div class="settings-defaults-label">Judge model:</div>
        <${CommittedField}
          initial=${nd.judge_model}
          placeholder="e.g. deepseek-chat or haiku"
          status=${status.judge_model}
          onCommit=${(v) => save('judge_model', v)}
        />
      </div>
      <div class="settings-kind-row">
        <div class="settings-defaults-label">Judge credential:</div>
        <select
          class="settings-select"
          value=${nd.judge_credential}
          disabled=${providerCreds.length === 0}
          onChange=${(e) => save('judge_credential', e.target.value)}
        >
          <option value="">${credPlaceholder}</option>
          ${providerCreds.map((c) => html`
            <option key=${c.name} value=${c.name}>${c.name}${c.description ? ' · ' + c.description : ''}</option>
          `)}
        </select>
        <span class="settings-defaults-status">
          ${status.judge_credential === 'saving' ? '…' :
            status.judge_credential === 'saved' ? '✓' :
            status.judge_credential === 'error' ? '✗' : ''}
        </span>
      </div>
      <div class="settings-kind-row">
        <div class="settings-defaults-label">Compact model:</div>
        <${CommittedField}
          initial=${nd.compact_model}
          placeholder="e.g. deepseek-chat or haiku"
          status=${status.compact_model}
          onCommit=${(v) => save('compact_model', v)}
        />
      </div>
      <div class="settings-kind-row">
        <div class="settings-defaults-label">Compact credential:</div>
        <select
          class="settings-select"
          value=${nd.compact_credential}
          disabled=${providerCreds.length === 0}
          onChange=${(e) => save('compact_credential', e.target.value)}
        >
          <option value="">${credPlaceholder}</option>
          ${providerCreds.map((c) => html`
            <option key=${c.name} value=${c.name}>${c.name}${c.description ? ' · ' + c.description : ''}</option>
          `)}
        </select>
        <span class="settings-defaults-status">
          ${status.compact_credential === 'saving' ? '…' :
            status.compact_credential === 'saved' ? '✓' :
            status.compact_credential === 'error' ? '✗' : ''}
        </span>
      </div>
    </div>
  `;
}

export function SettingsDefaults() {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [aspects, setAspects] = useState([]);
  const [defaultsByAspect, setDefaultsByAspect] = useState({});
  const [credentials, setCredentials] = useState([]);

  async function loadAll() {
    setLoading(true);
    setError('');
    try {
      const [agents, credsBody] = await Promise.all([fetchAgents(), listCredentials()]);
      const named = (agents || []).filter((a) => a && (a.name || a.id));
      const map = {};
      const settled = await Promise.allSettled(
        named.map((a) => getCredentialDefaults(a.name || a.id).then((d) => [a.name || a.id, d])),
      );
      for (const r of settled) {
        if (r.status === 'fulfilled') {
          const [name, d] = r.value;
          map[name] = d;
        }
      }
      setAspects(named);
      setDefaultsByAspect(map);
      setCredentials((credsBody && credsBody.credentials) || []);
    } catch (e) {
      setError(e.message || 'load failed');
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { loadAll(); }, []);

  // Pre-bucket credentials by kind so each row's dropdown gets O(1)
  // option lookup. Re-bucket on every credentials change so add /
  // delete from another tab eventually reflects after Reload.
  const credentialsByKind = useMemo(() => {
    const out = { provider: [], jira: [], imap: [] };
    for (const c of credentials) {
      const k = c.kind || 'provider';
      if (out[k]) out[k].push(c);
    }
    return out;
  }, [credentials]);

  function onSaved(aspect, fresh) {
    setDefaultsByAspect((prev) => ({ ...prev, [aspect]: fresh }));
  }

  if (loading) return html`<div class="settings-loading">Loading defaults…</div>`;
  if (error) {
    return html`
      <div class="settings-error-box">
        <div>${error}</div>
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
        <span class="settings-aspects-title">Defaults</span>
        <button class="settings-btn" onClick=${loadAll}>Reload</button>
      </div>
      <${NetworkDefaultsPanel} providerCreds=${credentialsByKind.provider || []} />
      <div class="settings-aspects-subheader">Per-aspect credential defaults (${aspects.length} aspects)</div>
      ${aspects.map((a) => {
        const name = a.name || a.id;
        return html`
          <${AspectDefaultsCard}
            key=${name}
            aspect=${name}
            defaults=${defaultsByAspect[name]}
            credentialsByKind=${credentialsByKind}
            onSaved=${onSaved}
          />
        `;
      })}
    </div>
  `;
}
