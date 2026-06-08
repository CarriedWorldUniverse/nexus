const { html } = window.__preact;

export function Placeholder({ name, title, note }) {
  const heading = title || name || 'Coming soon';
  const detail = note || (name ? 'coming soon' : '');

  return html`
    <div style="display:flex;flex-direction:column;align-items:center;justify-content:center;height:100%;gap:8px;color:var(--text-muted);font-family:var(--font-display);font-size:18px;">
      <div>${heading}</div>
      ${detail ? html`<div style="font-size:14px;opacity:0.72;">${detail}</div>` : null}
    </div>
  `;
}
