// CredentialBundleForm (NEX-266) — kind-typed bundle field renderer.
//
// Switches on `kind` to render the correct fields per the validation
// rules in nexus/credentials/credentials.go:validateBundle. Each kind
// has its own required-field set and one or more secret fields that
// the operator must re-enter on Edit (the backend GET strips bundle
// content entirely, so we can't display masked values).
//
// Controlled component: parent owns the bundle object and passes
// onChange. Each input writes back the full bundle on change.

const { html } = window.__preact;

// Bundle field schemas — mirror validateBundle in credentials.go.
// `secret: true` marks fields that should never be pre-populated and
// render as type=password with autoComplete=off.
const FIELDS = {
  provider: [
    { key: 'api_shape',     label: 'API shape',     type: 'select', options: ['anthropic', 'openai'], required: true },
    { key: 'base_url',      label: 'Base URL',      type: 'url',    placeholder: 'https://api.anthropic.com', required: true },
    { key: 'key',           label: 'API key',       type: 'text',   secret: true, required: true },
    { key: 'default_model', label: 'Default model', type: 'text',   placeholder: 'optional' },
  ],
  jira: [
    { key: 'atlassian_email',     label: 'Email',     type: 'email', required: true },
    { key: 'atlassian_token',     label: 'API token', type: 'text',  secret: true, required: true },
    { key: 'atlassian_subdomain', label: 'Subdomain', type: 'text',  placeholder: 'mycompany (no .atlassian.net)', required: true },
  ],
  imap: [
    { key: 'host',     label: 'Host',     type: 'text',     placeholder: 'imap.gmail.com', required: true },
    { key: 'port',     label: 'Port',     type: 'number',   placeholder: '993 (default)' },
    { key: 'user',     label: 'User',     type: 'text',     required: true },
    { key: 'password', label: 'Password', type: 'text',     secret: true, required: true },
    { key: 'ssl',      label: 'SSL',      type: 'checkbox' },
  ],
};

export function bundleFieldsFor(kind) {
  return FIELDS[kind] || [];
}

// Validate a bundle against the field rules for `kind`. Returns null
// if valid; otherwise a string describing the first missing field.
// Mirrors validateBundle in the backend for fast client-side feedback;
// the backend still validates on PUT so server-truth wins.
export function validateBundleClient(kind, bundle) {
  for (const f of bundleFieldsFor(kind)) {
    if (!f.required) continue;
    const v = bundle[f.key];
    if (v === undefined || v === null || (typeof v === 'string' && v.trim() === '')) {
      return f.label + ' is required';
    }
  }
  return null;
}

export function CredentialBundleForm({ kind, bundle, onChange, editing }) {
  const fields = bundleFieldsFor(kind);
  if (fields.length === 0) {
    return html`<div class="settings-empty">Unsupported kind: ${kind}</div>`;
  }

  function update(key, value) {
    onChange({ ...bundle, [key]: value });
  }

  return html`
    <div class="settings-bundle-form">
      ${editing && html`
        <div class="settings-bundle-note">
          Backend replaces the entire credential on save — re-enter <strong>all bundle fields</strong> including secrets to update.
        </div>
      `}
      ${fields.map((f) => {
        const v = bundle[f.key];
        if (f.type === 'select') {
          return html`
            <label class="settings-field" key=${f.key}>
              <span class="settings-field-label">${f.label}${f.required ? ' *' : ''}</span>
              <select
                class="settings-select"
                value=${v || ''}
                onChange=${(e) => update(f.key, e.target.value)}
              >
                <option value=""></option>
                ${f.options.map((opt) => html`<option key=${opt} value=${opt}>${opt}</option>`)}
              </select>
            </label>
          `;
        }
        if (f.type === 'checkbox') {
          return html`
            <label class="settings-field settings-field-checkbox" key=${f.key}>
              <input
                type="checkbox"
                checked=${!!v}
                onChange=${(e) => update(f.key, e.target.checked)}
              />
              <span class="settings-field-label">${f.label}</span>
            </label>
          `;
        }
        // text / email / url / number / secret
        return html`
          <label class="settings-field" key=${f.key}>
            <span class="settings-field-label">${f.label}${f.required ? ' *' : ''}</span>
            <input
              type=${f.secret ? 'password' : (f.type || 'text')}
              class="settings-input"
              placeholder=${f.placeholder || ''}
              value=${v == null ? '' : String(v)}
              autoComplete=${f.secret ? 'new-password' : 'off'}
              onInput=${(e) => {
                const val = e.target.value;
                if (f.type === 'number') {
                  update(f.key, val === '' ? '' : Number(val));
                } else {
                  update(f.key, val);
                }
              }}
            />
          </label>
        `;
      })}
    </div>
  `;
}
