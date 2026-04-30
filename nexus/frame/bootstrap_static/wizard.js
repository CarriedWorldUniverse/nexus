// Wizard for first-boot Frame setup. Vanilla JS, no framework.
//
// Flow:
//   1. Fetch /bootstrap/config to learn allowed providers + default models.
//      Populate the provider dropdown; auto-fill model when provider changes.
//   2. On form submit, POST /bootstrap/setup. Disable form, show success or
//      surface the error code+message from the server's JSON envelope.
//   3. On success, the server is shutting down; show the home path and
//      restart hint.

(() => {
  const form = document.getElementById('setup-form');
  const submitBtn = document.getElementById('submit-btn');
  const providerSel = document.getElementById('provider');
  const modelInput = document.getElementById('model');
  const errorBox = document.getElementById('error');
  const successBox = document.getElementById('success');
  const successMsg = document.getElementById('success-message');
  const successRestart = document.getElementById('success-restart');
  const successPath = document.getElementById('success-path');

  let defaultModels = {};

  function showError(text) {
    errorBox.textContent = text;
    errorBox.hidden = false;
    successBox.hidden = true;
  }

  function clearError() {
    errorBox.hidden = true;
    errorBox.textContent = '';
  }

  function clearChildren(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  async function loadConfig() {
    try {
      const resp = await fetch('/bootstrap/config');
      if (!resp.ok) throw new Error(`config fetch failed: ${resp.status}`);
      const cfg = await resp.json();
      defaultModels = cfg.default_models || {};
      const providers = (cfg.providers || []).slice().sort();
      clearChildren(providerSel);
      for (const p of providers) {
        const opt = document.createElement('option');
        opt.value = p;
        opt.textContent = p;
        providerSel.appendChild(opt);
      }
      // Default to claude-api if present, else the first.
      if (providers.includes('claude-api')) {
        providerSel.value = 'claude-api';
      }
      updateModelPlaceholder();
    } catch (err) {
      showError(`Could not load config: ${err.message}`);
    }
  }

  function updateModelPlaceholder() {
    const p = providerSel.value;
    const def = defaultModels[p] || '';
    modelInput.placeholder = def ? `(default: ${def})` : '';
  }

  providerSel.addEventListener('change', () => {
    clearError();
    updateModelPlaceholder();
  });

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearError();
    submitBtn.disabled = true;
    submitBtn.textContent = 'Creating…';

    const data = {
      name: document.getElementById('name').value.trim(),
      voice: document.getElementById('voice').value.trim(),
      values: document.getElementById('values').value.trim(),
      provider: providerSel.value,
      model: modelInput.value.trim(),
    };

    // Strip empty optional fields so the server applies its defaults
    // (rather than getting "" from us, which the strict-vars contract
    // would still consume but with confusing semantics).
    for (const k of ['voice', 'values', 'model']) {
      if (data[k] === '') delete data[k];
    }

    try {
      const resp = await fetch('/bootstrap/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(data),
      });
      const body = await resp.json();
      if (!resp.ok) {
        const msg = body.message || body.error || `HTTP ${resp.status}`;
        showError(msg);
        submitBtn.disabled = false;
        submitBtn.textContent = 'Create Frame';
        return;
      }
      successMsg.textContent = body.message || 'Frame created.';
      successRestart.textContent = body.restart_hint || '';
      successPath.textContent = body.frame_path || '';
      successBox.hidden = false;
      form.hidden = true;
    } catch (err) {
      showError(`Network error: ${err.message}`);
      submitBtn.disabled = false;
      submitBtn.textContent = 'Create Frame';
    }
  });

  loadConfig();
})();
