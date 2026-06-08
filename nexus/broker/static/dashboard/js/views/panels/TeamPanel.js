const { html, useState, Fragment } = window.__preact;

import { agents } from '../../state.js';
import { AgentConfigPanel } from './AgentConfigPanel.js';

function agentID(agent) {
  return typeof agent === 'string' ? agent : (agent.id || agent.name || '');
}

function agentState(agent) {
  if (typeof agent === 'string') return 'idle';
  if (agent.state) return agent.state;
  if (typeof agent.alive === 'boolean') return agent.alive ? 'online' : 'offline';
  return 'idle';
}

export function TeamPanel({ onClose }) {
  const list = agents.value || [];
  const [selected, setSelected] = useState('');

  function setLocalDispatchEnabled(id, enabled) {
    agents.value = (agents.value || []).map((agent) => {
      if (agentID(agent) !== id || typeof agent === 'string') return agent;
      return { ...agent, dispatch_enabled: enabled };
    });
  }

  const selectedRow = list.find((agent) => agentID(agent) === selected) || null;

  return html`<${Fragment}>
    <${AgentConfigPanel}
      agent=${selected}
      rosterRow=${selectedRow}
      onClose=${() => setSelected('')}
      onDispatchEnabledChange=${setLocalDispatchEnabled}
    />
    <aside class="watch-panel team-panel">
      <header class="watch-panel-head">
        <span>Team</span>
        <button class="panel-close" onClick=${onClose} aria-label="Close team panel">x</button>
      </header>
      <ul class="team-list">
        ${list.map((agent) => {
          const id = agentID(agent);
          const state = agentState(agent);
          const dispatchEnabled = typeof agent === 'object' ? agent.dispatch_enabled !== false : true;
          if (!id) return null;
          return html`
            <li key=${id} class=${'team-row' + (selected === id ? ' team-row-active' : '')}>
              <span class=${`team-state team-${state}`}></span>
              <span class="team-name">${id}</span>
              <span class=${'team-dispatch ' + (dispatchEnabled ? 'on' : 'off')}>${dispatchEnabled ? 'dispatch' : 'blocked'}</span>
              <span class="team-statelbl">${state}</span>
              <button class="team-config-btn" onClick=${() => setSelected(id)}>Configure</button>
            </li>
          `;
        })}
      </ul>
      ${list.length === 0 ? html`<div class="watch-panel-empty">No agents registered.</div>` : null}
    </aside>
  </${Fragment}>`;
}
