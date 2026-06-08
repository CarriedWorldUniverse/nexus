const { html } = window.__preact;

import { agents } from '../../state.js';

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
  return html`
    <aside class="watch-panel team-panel">
      <header class="watch-panel-head">
        <span>Team</span>
        <button class="panel-close" onClick=${onClose} aria-label="Close team panel">x</button>
      </header>
      <ul class="team-list">
        ${list.map((agent) => {
          const id = agentID(agent);
          const state = agentState(agent);
          if (!id) return null;
          return html`
            <li key=${id} class="team-row">
              <span class=${`team-state team-${state}`}></span>
              <span class="team-name">${id}</span>
              <span class="team-statelbl">${state}</span>
            </li>
          `;
        })}
      </ul>
      ${list.length === 0 ? html`<div class="watch-panel-empty">No agents registered.</div>` : null}
    </aside>
  `;
}
