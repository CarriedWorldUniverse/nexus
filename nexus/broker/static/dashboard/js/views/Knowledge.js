const { html, useState, useEffect } = window.__preact;

import { fetchKnowledge, BASE, getAuthToken } from '../api.js';
import { agents, agentColors } from '../state.js';

function formatDate(dateStr) {
  if (!dateStr) return '';
  const d = new Date(dateStr + 'Z');
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' +
    d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

export function Knowledge() {
  const [entries, setEntries] = useState([]);
  const [agentFilter, setAgentFilter] = useState('');
  const [search, setSearch] = useState('');
  const [searchInput, setSearchInput] = useState('');
  const [expanded, setExpanded] = useState({});
  const [loading, setLoading] = useState(false);

  const agentList = agents.value;
  const colors = agentColors.value;

  useEffect(() => {
    const timer = setTimeout(() => setSearch(searchInput), 300);
    return () => clearTimeout(timer);
  }, [searchInput]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    fetchKnowledge({ agent: agentFilter || undefined, search: search || undefined, limit: 200 })
      .then(rows => {
        if (!cancelled) setEntries(Array.isArray(rows) ? rows : []);
      })
      .catch(e => console.error('[Knowledge] fetch failed', e))
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [agentFilter, search]);

  function toggleEntry(id) {
    setExpanded(prev => ({ ...prev, [id]: !prev[id] }));
  }

  async function deleteEntry(e, id) {
    e.stopPropagation();
    if (!confirm('Delete this knowledge entry?')) return;
    try {
      const token = getAuthToken();
      const headers = {};
      if (token) headers["Authorization"] = `Bearer ${token}`;
      const res = await fetch(`${BASE}/api/knowledge/${id}`, { method: 'DELETE', headers });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setEntries(prev => prev.filter(en => en.id !== id));
    } catch (err) {
      console.error('[Knowledge] delete failed', err);
      alert('Delete failed: ' + err.message);
    }
  }

  return html`
    <div class="knowledge-view">
      <div class="knowledge-header">
        <span class="knowledge-header-title">Commonplace</span>
        <span class="knowledge-header-count">${entries.length} entr${entries.length === 1 ? 'y' : 'ies'}</span>
      </div>

      <div class="knowledge-controls">
        <select
          class="knowledge-select"
          value=${agentFilter}
          onChange=${e => setAgentFilter(e.target.value)}
        >
          <option value="">All agents</option>
          ${agentList.map(a => {
            const id = typeof a === 'string' ? a : a.id;
            return html`<option key=${id} value=${id}>${id}</option>`;
          })}
        </select>
        <input
          class="knowledge-search"
          type="text"
          placeholder="Search knowledge..."
          value=${searchInput}
          onInput=${e => setSearchInput(e.target.value)}
        />
      </div>

      <div class="knowledge-list">
        ${loading && entries.length === 0 && html`<div class="knowledge-empty">Loading...</div>`}
        ${!loading && entries.length === 0 && html`<div class="knowledge-empty">No entries found.</div>`}
        ${entries.map(entry => {
          const isExpanded = !!expanded[entry.id];
          const color = colors[entry.from] || '#888';
          const preview = entry.content ? entry.content.slice(0, 200) + (entry.content.length > 200 ? '…' : '') : '';

          return html`
            <div
              key=${entry.id}
              class="knowledge-entry"
              onClick=${() => toggleEntry(entry.id)}
            >
              <div class="knowledge-entry-header">
                <span class="knowledge-entry-agent" style=${{ color }}>${entry.from}</span>
                <span class="knowledge-entry-topic">${entry.topic}</span>
                <span class="knowledge-entry-date">${formatDate(entry.updated_at)}</span>
                <button
                  class="knowledge-delete-btn"
                  onClick=${e => deleteEntry(e, entry.id)}
                  title="Delete entry"
                >✕</button>
              </div>
              ${!isExpanded && html`
                <div class="knowledge-entry-preview">${preview}</div>
              `}
              ${isExpanded && html`
                <div class="knowledge-entry-full">${entry.content}</div>
              `}
            </div>
          `;
        })}
      </div>
    </div>
  `;
}
