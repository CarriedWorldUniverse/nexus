// Login.js — passkey-driven login flow for the operator dashboard.
//
// Two flows, the second is the bootstrap-only path:
//
//   LOGIN. POST /api/operator/login/begin → WebAuthn .get() →
//          POST /api/operator/login/finish → JWT.
//
//   REGISTER (bootstrap, only when no passkeys exist OR when an
//          already-logged-in operator adds a device). POST
//          /api/operator/register/begin → WebAuthn .create() →
//          POST /api/operator/register/finish → on success, falls
//          through to the LOGIN flow above.
//
// The SPA chrome (app.js / Shell.js) renders a "<Login />"-shaped
// overlay until login() resolves. After login, the JWT is stashed
// in api.js's in-memory holder (refresh = re-auth, per spec §2.2)
// and comms.open() brings the WS up.

// Globals come from index.html's pre-load shim (the same shape app.js
// uses): window.__preact bundles { h, html, useState, useEffect }.
const { html, useState, useEffect } = window.__preact;

import { startAuthentication, startRegistration } from './vendor/webauthn/index.js';
import { setAuthToken } from './api.js';
import { open as commsOpen } from './comms.js';

// fetchJSON wraps fetch+JSON+error mapping. Used only on /api/operator/*
// routes — every other call goes via comms.js / WS.
async function fetchJSON(path, init = {}) {
  const res = await fetch(path, {
    method: init.method || 'POST',
    headers: { 'Content-Type': 'application/json', ...(init.headers || {}) },
    body: init.body ? JSON.stringify(init.body) : undefined,
  });
  let data;
  try { data = await res.json(); } catch { data = {}; }
  if (!res.ok) {
    const err = new Error(data.error || `HTTP ${res.status}`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

// runLogin drives the login ceremony end-to-end. Returns the JWT
// on success; throws with err.status === 409 if no passkeys are
// registered yet (caller falls back to runRegister).
export async function runLogin() {
  // Begin: get challenge + session_token.
  const begin = await fetchJSON('/api/operator/login/begin');
  // begin.options is the CredentialAssertion options the lib expects.
  // simplewebauthn's startAuthentication takes { optionsJSON: options }.
  const assertion = await startAuthentication({ optionsJSON: begin.options.publicKey });
  // The browser's response goes back as the request body of finish.
  const finish = await fetchJSON(
    `/api/operator/login/finish?session_token=${encodeURIComponent(begin.session_token)}`,
    { body: assertion },
  );
  return finish.session_jwt;
}

// runRegister enrols a new device. Used in two cases:
//
//   - BOOTSTRAP: operator_passkeys is empty; the broker accepts the
//     registration without a Bearer header.
//   - ADD-DEVICE: an already-logged-in operator wants another
//     device. Broker requires a current operator JWT in the
//     Authorization header.
//
// Returns nothing; on success, the caller should run runLogin to
// authenticate with the freshly-registered passkey.
export async function runRegister(label, jwt = null) {
  const headers = {};
  if (jwt) headers['Authorization'] = `Bearer ${jwt}`;
  const begin = await fetchJSON('/api/operator/register/begin', {
    body: { label },
    headers,
  });
  const cred = await startRegistration({ optionsJSON: begin.options.publicKey });
  await fetchJSON(
    `/api/operator/register/finish?session_token=${encodeURIComponent(begin.session_token)}`,
    { body: cred },
  );
}

// Login is the Preact view rendered before app boot. Drives the
// WebAuthn ceremony, stashes the JWT, opens the WS, then calls
// onComplete so the SPA can swap to the Shell.
export function Login({ onComplete }) {
  const [phase, setPhase] = useState('idle'); // 'idle' | 'logging-in' | 'register' | 'registering' | 'error'
  const [err, setErr] = useState(null);
  const [label, setLabel] = useState('');

  // Auto-start the login attempt on mount. If it 409s ("no
  // passkeys registered"), drop into the register flow.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      setPhase('logging-in');
      try {
        const jwt = await runLogin();
        if (cancelled) return;
        setAuthToken(jwt);
        await commsOpen(jwt);
        if (cancelled) return;
        onComplete && onComplete();
      } catch (e) {
        if (cancelled) return;
        if (e.status === 409) {
          setPhase('register');
          return;
        }
        setErr(e.message || String(e));
        setPhase('error');
      }
    })();
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function doRegister(e) {
    e.preventDefault();
    if (!label.trim()) return;
    setPhase('registering');
    setErr(null);
    try {
      await runRegister(label.trim(), null);
      // Registration done; flip back to login flow.
      const jwt = await runLogin();
      setAuthToken(jwt);
      await commsOpen(jwt);
      onComplete && onComplete();
    } catch (e2) {
      setErr(e2.message || String(e2));
      setPhase('error');
    }
  }

  function retry() {
    setErr(null);
    setPhase('idle');
    // useEffect doesn't re-run on phase reset alone; force a remount
    // by reloading the route.
    window.location.reload();
  }

  if (phase === 'logging-in') {
    return html`
      <div class="login-overlay" role="status" aria-live="polite">
        <div class="login-card">
          <h1>Nexus</h1>
          <p>Touch your passkey to sign in…</p>
        </div>
      </div>
    `;
  }

  if (phase === 'register' || phase === 'registering') {
    return html`
      <div class="login-overlay">
        <div class="login-card">
          <h1>Nexus</h1>
          <p>Register this device.</p>
          <form onSubmit=${doRegister}>
            <label for="device-label">Device label</label>
            <input
              id="device-label"
              type="text"
              autocomplete="off"
              placeholder="<operator-host>, dMon, …"
              value=${label}
              onInput=${(ev) => setLabel(ev.target.value)}
              disabled=${phase === 'registering'}
              required
            />
            <button type="submit" disabled=${phase === 'registering' || !label.trim()}>
              ${phase === 'registering' ? 'Registering…' : 'Register passkey'}
            </button>
          </form>
        </div>
      </div>
    `;
  }

  if (phase === 'error') {
    return html`
      <div class="login-overlay" role="alert">
        <div class="login-card login-card-error">
          <h1>Nexus</h1>
          <p>Sign-in failed.</p>
          <pre class="login-err">${err}</pre>
          <button type="button" onClick=${retry}>Try again</button>
        </div>
      </div>
    `;
  }

  // 'idle' — should never render once useEffect kicks; placeholder
  // so the component is always something.
  return html`<div class="login-overlay"><div class="login-card"><h1>Nexus</h1></div></div>`;
}
