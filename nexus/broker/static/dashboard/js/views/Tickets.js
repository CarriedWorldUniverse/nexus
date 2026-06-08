const { html, useState, useEffect } = window.__preact;

import { fetchTickets, fetchTicket, BASE } from '../api.js';
import { agentColors } from '../state.js';

const COLUMNS = [
  { key: 'open',        label: 'Open' },
  { key: 'in-progress', label: 'In Progress' },
  { key: 'done',        label: 'Done' },
];

const PRIORITY_COLORS = {
  urgent: '#f85149',
  high:   '#d29922',
  normal: 'var(--text-secondary)',
  low:    'var(--text-muted)',
};

function formatAge(dateStr) {
  const diff = Date.now() - new Date(dateStr + 'Z').getTime();
  const mins  = Math.floor(diff / 60000);
  const hours = Math.floor(diff / 3600000);
  const days  = Math.floor(diff / 86400000);
  if (days  > 0) return `${days}d ago`;
  if (hours > 0) return `${hours}h ago`;
  return `${Math.max(1, mins)}m ago`;
}

function AssigneeDot({ name, colors }) {
  const color = colors[name] || 'var(--text-muted)';
  return html`
    <span class="card-assignee">
      <span class="assignee-dot" style=${{ background: color }}></span>
      ${name || 'unassigned'}
    </span>
  `;
}

function TicketCard({ ticket, colors }) {
  const [expanded, setExpanded] = useState(false);
  const priorityColor = PRIORITY_COLORS[ticket.priority] || PRIORITY_COLORS.normal;

  return html`
    <div
      class=${'ticket-card' + (expanded ? ' expanded' : '')}
      onClick=${() => setExpanded(e => !e)}
    >
      <div class="card-id">#${ticket.id}</div>
      <div class="card-title">${ticket.title}</div>
      <div class="card-meta">
        <${AssigneeDot} name=${ticket.assignee} colors=${colors} />
        <span class="card-priority" style=${{ color: priorityColor }}>
          ${ticket.priority || 'normal'}
        </span>
        <span class="card-age">${formatAge(ticket.created_at)}</span>
        ${ticket.source_msg_id && html`
          <a
            class="card-source-link"
            href=${'#/converse'}
            onClick=${e => e.stopPropagation()}
          >↗ msg</a>
        `}
      </div>
      ${expanded && ticket.description && html`
        <div class="card-description">${ticket.description}</div>
      `}
      ${expanded && ticket.notes && ticket.notes.length > 0 && html`
        <div class="card-notes">
          ${ticket.notes.map((note, i) => html`
            <div key=${i} class="card-note">
              <span class="note-author">${note.author}</span>
              <span class="note-content">${note.content}</span>
            </div>
          `)}
        </div>
      `}
    </div>
  `;
}

function TicketsColumn({ col, tickets, colors }) {
  return html`
    <div class="tickets-column">
      <div class="column-header">
        <span>${col.label}</span>
        <span class="column-count">${tickets.length}</span>
      </div>
      <div class="column-cards">
        ${tickets.map(t => html`
          <${TicketCard} key=${t.id} ticket=${t} colors=${colors} />
        `)}
        ${tickets.length === 0 && html`
          <div class="column-empty">No tickets</div>
        `}
      </div>
    </div>
  `;
}

export function Tickets() {
  const [ticketMap, setTicketMap] = useState({});
  const colors = agentColors.value;

  async function load() {
    try {
      const list = await fetchTickets();
      if (!Array.isArray(list)) return;

      // Seed with summary data immediately
      const map = {};
      list.forEach(t => { map[t.id] = t; });
      setTicketMap({ ...map });

      // Fetch full detail for each
      await Promise.all(list.map(async t => {
        try {
          const full = await fetchTicket(t.id);
          map[t.id] = full;
        } catch (_) {}
      }));
      setTicketMap({ ...map });
    } catch (e) {
      console.error('[Tickets] load failed', e);
    }
  }

  useEffect(() => {
    load();
    const iv = setInterval(load, 10000);
    return () => clearInterval(iv);
  }, []);

  const tickets = Object.values(ticketMap);

  return html`
    <div class="tickets-view">
      <div class="tickets-header">
        <h2>Tickets</h2>
      </div>
      <div class="tickets-board">
        ${COLUMNS.map(col => html`
          <${TicketsColumn}
            key=${col.key}
            col=${col}
            tickets=${tickets.filter(t => t.status === col.key)}
            colors=${colors}
          />
        `)}
      </div>
    </div>
  `;
}
