const { html, useEffect, useMemo, useState } = window.__preact;

import { buildDispatchLine, dispatchCompose } from '../../api.js';
import { agents } from '../../state.js';

function agentID(agent) {
  return typeof agent === 'string' ? agent : (agent.id || agent.name || '');
}

function CompactField({ label, value, onInput, placeholder = '' }) {
  return html`
    <label class="dispatch-field">
      <span>${label}</span>
      <input value=${value} placeholder=${placeholder} onInput=${(e) => onInput(e.currentTarget.value)} />
    </label>
  `;
}

export function DispatchComposePanel({ onClose, onPosted }) {
  const roster = agents.value || [];
  const agentOptions = roster.map(agentID).filter(Boolean);
  const [agent, setAgent] = useState(agentOptions[0] || '');
  const [provider, setProvider] = useState('codex-cli');
  const [repo, setRepo] = useState('');
  const [ticket, setTicket] = useState('');
  const [branch, setBranch] = useState('');
  const [task, setTask] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [posted, setPosted] = useState('');

  useEffect(() => {
    if (!agent && agentOptions.length) setAgent(agentOptions[0]);
  }, [agent, agentOptions.join('|')]);

  const preview = useMemo(() => {
    try {
      return buildDispatchLine({ agent, provider, repo, ticket, branch, task: task || '...' });
    } catch {
      return '!dispatch';
    }
  }, [agent, provider, repo, ticket, branch, task]);

  const submit = (e) => {
    e.preventDefault();
    setBusy(true);
    setError('');
    setPosted('');
    dispatchCompose({ agent, provider, repo, ticket, branch, task }).then(({ line }) => {
      setTask('');
      setPosted(line);
      if (onPosted) onPosted(line);
    }).catch((err) => {
      setError(err.message || 'dispatch failed');
    }).finally(() => setBusy(false));
  };

  return html`
    <aside class="watch-panel dispatch-panel">
      <header class="watch-panel-head">
        <span>Dispatch</span>
        <button class="panel-close" onClick=${onClose} aria-label="Close dispatch panel">x</button>
      </header>
      <form class="dispatch-form" onSubmit=${submit}>
        <label class="dispatch-field">
          <span>Agent</span>
          ${agentOptions.length ? html`
            <select value=${agent} onChange=${(e) => setAgent(e.currentTarget.value)}>
              ${agentOptions.map((id) => html`<option key=${id} value=${id}>${id}</option>`)}
            </select>
          ` : html`
            <input value=${agent} placeholder="anvil" onInput=${(e) => setAgent(e.currentTarget.value)} />
          `}
        </label>
        <${CompactField} label="Provider" value=${provider} onInput=${setProvider} placeholder="codex-cli" />
        <${CompactField} label="Repo" value=${repo} onInput=${setRepo} placeholder="org/repo" />
        <${CompactField} label="Ticket" value=${ticket} onInput=${setTicket} placeholder="NEX-1" />
        <${CompactField} label="Branch" value=${branch} onInput=${setBranch} placeholder="builder/NEX-1" />
        <label class="dispatch-field dispatch-task">
          <span>Task</span>
          <textarea value=${task} rows="6" onInput=${(e) => setTask(e.currentTarget.value)} />
        </label>
        <div class="dispatch-preview">${preview}</div>
        ${error ? html`<div class="watch-panel-error">${error}</div>` : null}
        ${posted ? html`<div class="dispatch-posted">posted</div>` : null}
        <button class="dispatch-submit" type="submit" disabled=${busy}>${busy ? 'Posting...' : 'Dispatch'}</button>
      </form>
    </aside>
  `;
}
