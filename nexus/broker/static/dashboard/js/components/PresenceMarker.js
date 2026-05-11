// PresenceMarker — single dimmed line summarising a PresenceFrame.
//
// Kept intentionally minimal: the operator scans these to spot
// connect/disconnect transitions in the otherwise chat-dense Observe
// stream. Mono font + muted color so it recedes; the dividers anchor
// it visually as a "between things happened" marker rather than a
// piece of content.

const { html } = window.__preact;

export function PresenceMarker({ aspect, payload, ts }) {
  const connected = !!payload.connected;
  const reason = payload.reason ? ` (${payload.reason})` : '';
  const verb = connected ? 'connected' : 'disconnected';
  const t = formatTime(ts);
  return html`
    <div class=${'observe-presence' + (connected ? ' connected' : ' disconnected')} title=${ts}>
      <span class="observe-presence-rule" aria-hidden="true">─</span>
      <span class="observe-presence-text">@${aspect} ${verb}${reason}</span>
      ${t && html`<span class="observe-presence-time">${t}</span>`}
      <span class="observe-presence-rule" aria-hidden="true">─</span>
    </div>
  `;
}

function formatTime(dateStr) {
  if (!dateStr) return '';
  const isISO = /Z$|[+-]\d\d:?\d\d$/.test(dateStr);
  const d = new Date(isISO ? dateStr : dateStr + 'Z');
  if (isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}
