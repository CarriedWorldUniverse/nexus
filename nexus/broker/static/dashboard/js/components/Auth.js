const { html, useState, useEffect } = window.__preact;
import { setAuthToken, clearAuthToken, BASE } from '../api.js';

let _webauthnMod = null;
const _webauthnReady = import('/js/vendor/webauthn/index.js').then(m => { _webauthnMod = m; }).catch(() => {});

let _qrMod = null;
const _qrReady = import('/js/vendor/qrcode.js').then(m => { _qrMod = m.default || m; }).catch(() => {});

async function checkAuth() {
  const token = localStorage.getItem('auth_token');
  if (!token) return false;
  try {
    const res = await fetch(`${BASE}/api/auth/check`, {
      headers: { 'Authorization': `Bearer ${token}` }
    });
    return res.ok;
  } catch { return false; }
}

export function useAuthGate() {
  const [authed, setAuthed] = useState(null); // null = checking

  useEffect(() => {
    checkAuth().then(ok => setAuthed(ok));
  }, []);

  function onLogin() { setAuthed(true); }

  return { authed, onLogin };
}

export function AuthOverlay({ onLogin }) {
  const [status, setStatus] = useState('');
  const [mode, setMode] = useState('login'); // 'login' | 'register' | 'invite'
  const [inviteUrl, setInviteUrl] = useState('');

  // Detect ?register=TOKEN invite link and auto-start registration
  const inviteToken = new URLSearchParams(window.location.search).get('register');
  useEffect(() => {
    if (inviteToken) {
      setMode('register');
      handleRegisterWithInvite(inviteToken);
    }
  }, []);

  async function handleRegisterWithInvite(invite) {
    setStatus('Starting registration...');
    try {
      await _webauthnReady;
      if (!_webauthnMod) { setStatus('Error: failed to load WebAuthn library'); return; }
      let res = await fetch(`${BASE}/api/auth/register-options`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ invite }),
      });
      if (!res.ok) {
        const err = await res.json().catch(() => ({}));
        setStatus('Invite error: ' + (err.error || 'invalid or expired invite'));
        return;
      }
      const options = await res.json();
      setStatus('Create your passkey...');
      const regResp = await _webauthnMod.startRegistration({ optionsJSON: options });
      setStatus('Verifying...');
      res = await fetch(`${BASE}/api/auth/register`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(regResp),
      });
      const result = await res.json();
      if (result.verified) {
        setStatus('Device registered! Signing in...');
        // Clean up the URL
        window.history.replaceState({}, '', window.location.pathname);
        await handleLogin();
      } else {
        setStatus('Registration failed: ' + (result.error || 'unknown'));
      }
    } catch (e) {
      if (e.name === 'NotAllowedError') setStatus('Passkey prompt dismissed. Tap the button to try again.');
      else setStatus('Error: ' + e.message);
    }
  }

  async function handleLogin() {
    setStatus('Waiting for passkey...');
    try {
      await _webauthnReady;
      if (!_webauthnMod) { setStatus('Error: failed to load WebAuthn library'); return; }
      let res = await fetch(`${BASE}/api/auth/login-options`, { method: 'POST', headers: { 'Content-Type': 'application/json' } });
      if (res.status === 404) { setStatus('No passkey registered — register this device first.'); setMode('register'); return; }
      const options = await res.json();
      const authResp = await _webauthnMod.startAuthentication({ optionsJSON: options });
      res = await fetch(`${BASE}/api/auth/login`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(authResp) });
      const result = await res.json();
      if (result.token) {
        setAuthToken(result.token);
        onLogin();
      } else {
        setStatus('Login failed: ' + (result.error || 'unknown error'));
      }
    } catch (e) {
      if (e.name === 'NotAllowedError') setStatus('Passkey prompt dismissed.');
      else setStatus('Error: ' + e.message);
    }
  }

  async function handleRegister() {
    setStatus('Starting registration...');
    try {
      await _webauthnReady;
      if (!_webauthnMod) { setStatus('Error: failed to load WebAuthn library'); return; }
      let res = await fetch(`${BASE}/api/auth/register-options`, { method: 'POST', headers: { 'Content-Type': 'application/json' } });
      if (!res.ok) { setStatus('Registration requires an invite — ask the primary device to generate one.'); return; }
      const options = await res.json();
      setStatus('Create your passkey...');
      const regResp = await _webauthnMod.startRegistration({ optionsJSON: options });
      setStatus('Verifying...');
      res = await fetch(`${BASE}/api/auth/register`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(regResp) });
      const result = await res.json();
      if (result.verified) {
        setStatus('Device registered! Signing in...');
        await handleLogin();
      } else {
        setStatus('Registration failed: ' + (result.error || 'unknown'));
      }
    } catch (e) {
      setStatus('Error: ' + e.message);
    }
  }

  async function handleGenerateInvite() {
    setStatus('Generating invite...');
    try {
      const token = localStorage.getItem('auth_token');
      const res = await fetch(`${BASE}/api/auth/register-invite`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` }
      });
      const result = await res.json();
      if (result.url) {
        setInviteUrl(result.url);
        setMode('invite');
        setStatus('');
      } else {
        setStatus('Error: ' + (result.error || 'unknown'));
      }
    } catch (e) {
      setStatus('Error: ' + e.message);
    }
  }

  const styles = {
    overlay: 'position:fixed;inset:0;z-index:9999;background:#0a0a0a;display:flex;align-items:center;justify-content:center;flex-direction:column;gap:16px;',
    title: 'font-size:20px;font-weight:600;color:#e0e0e0;letter-spacing:0.05em;',
    sub: 'font-size:13px;color:#666;max-width:280px;text-align:center;min-height:20px;',
    btn: 'padding:11px 28px;font-size:13px;background:#4fc3f7;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600;',
    btnGhost: 'padding:8px 20px;font-size:12px;background:transparent;color:#555;border:1px solid #333;border-radius:6px;cursor:pointer;',
    inviteBox: 'background:#111;border:1px solid #333;border-radius:6px;padding:12px 16px;font-size:11px;font-family:monospace;color:#aaa;word-break:break-all;max-width:320px;',
    copyBtn: 'padding:6px 16px;font-size:12px;background:#222;color:#aaa;border:1px solid #333;border-radius:4px;cursor:pointer;',
  };

  if (mode === 'invite') {
    return html`
      <div style=${styles.overlay}>
        <div style=${styles.title}>The Nexus</div>
        <div style="font-size:13px;color:#888;">Register Another Device</div>
        <div style="font-size:12px;color:#555;max-width:300px;text-align:center;">Open this URL on your phone or other device to register it as a passkey:</div>
        <div style=${styles.inviteBox}>${inviteUrl}</div>
        <button style=${styles.copyBtn} onClick=${() => navigator.clipboard.writeText(inviteUrl).then(() => setStatus('Copied!'))}>Copy URL</button>
        <div style=${styles.sub}>${status}</div>
        <button style=${styles.btnGhost} onClick=${() => { setMode('login'); setInviteUrl(''); }}>Done</button>
      </div>
    `;
  }

  if (mode === 'register') {
    return html`
      <div style=${styles.overlay}>
        <div style=${styles.title}>The Nexus</div>
        <div style="font-size:13px;color:#888;">Register This Device</div>
        <div style=${styles.sub}>${status || 'Create a passkey for this device.'}</div>
        <button style=${styles.btn} onClick=${() => inviteToken ? handleRegisterWithInvite(inviteToken) : handleRegister()}>Create Passkey</button>
        <button style=${styles.btnGhost} onClick=${() => setMode('login')}>Back</button>
      </div>
    `;
  }

  return html`
    <div style=${styles.overlay}>
      <div style=${styles.title}>The Nexus</div>
      <div style=${styles.sub}>${status || 'Authenticate to continue'}</div>
      <button style=${styles.btn} onClick=${handleLogin}>Sign in with Passkey</button>
      <button style=${styles.btnGhost} onClick=${() => setMode('register')}>Register This Device</button>
    </div>
  `;
}

export function RegisterDeviceButton() {
  const [open, setOpen] = useState(false);
  const [status, setStatus] = useState('');
  const [inviteUrl, setInviteUrl] = useState('');
  const [qrDataUrl, setQrDataUrl] = useState('');

  async function generate() {
    setOpen(true);
    setStatus('Generating invite...');
    setInviteUrl('');
    setQrDataUrl('');
    try {
      const token = localStorage.getItem('auth_token');
      const res = await fetch(`${BASE}/api/auth/register-invite`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` }
      });
      const result = await res.json();
      if (result.url) {
        setInviteUrl(result.url);
        setStatus('Scan to register your device. Valid 10 minutes.');
        await _qrReady;
        if (_qrMod && _qrMod.toDataURL) {
          const dataUrl = await _qrMod.toDataURL(result.url, { width: 220, margin: 2, color: { dark: '#000000', light: '#ffffff' } });
          setQrDataUrl(dataUrl);
        }
      } else {
        setStatus('Error: ' + (result.error || 'unknown'));
      }
    } catch (e) { setStatus('Error: ' + e.message); }
  }

  function close() { setOpen(false); setInviteUrl(''); setQrDataUrl(''); setStatus(''); }

  return html`
    <button onClick=${generate} style="font-size:12px;color:#555;background:none;border:none;cursor:pointer;padding:4px 0;">+ Register another device</button>
    ${open && html`
      <div onClick=${close} style="position:fixed;inset:0;z-index:9998;background:rgba(0,0,0,0.7);display:flex;align-items:center;justify-content:center;">
        <div onClick=${e => e.stopPropagation()} style="background:#111;border:1px solid #2a2a2a;border-radius:12px;padding:28px 32px;display:flex;flex-direction:column;align-items:center;gap:14px;min-width:280px;max-width:340px;">
          <div style="font-size:14px;font-weight:600;color:#ccc;letter-spacing:0.04em;">Register Another Device</div>
          ${qrDataUrl
            ? html`<img src=${qrDataUrl} style="width:220px;height:220px;border-radius:6px;display:block;" alt="QR code" />`
            : html`<div style="width:220px;height:220px;background:#0a0a0a;border-radius:6px;display:flex;align-items:center;justify-content:center;font-size:12px;color:#444;">${status || 'Loading...'}</div>`
          }
          <div style="font-size:12px;color:#555;text-align:center;">${status}</div>
          ${inviteUrl && html`
            <div style="display:flex;gap:8px;align-items:center;width:100%;">
              <div style="flex:1;font-size:10px;font-family:monospace;color:#555;word-break:break-all;background:#0a0a0a;padding:8px;border-radius:4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title=${inviteUrl}>${inviteUrl}</div>
              <button onClick=${() => navigator.clipboard.writeText(inviteUrl).then(() => setStatus('Copied!'))} style="flex-shrink:0;padding:5px 10px;font-size:11px;background:#1a1a1a;color:#888;border:1px solid #333;border-radius:4px;cursor:pointer;">Copy</button>
            </div>
          `}
          <button onClick=${close} style="padding:7px 24px;font-size:12px;background:transparent;color:#555;border:1px solid #333;border-radius:6px;cursor:pointer;margin-top:4px;">Done</button>
        </div>
      </div>
    `}
  `;
}
