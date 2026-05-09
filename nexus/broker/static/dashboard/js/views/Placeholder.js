const { html } = window.__preact;

export function Placeholder({ name }) {
  return html`
    <div style="display:flex;align-items:center;justify-content:center;height:100%;color:var(--text-muted);font-family:var(--font-display);font-size:18px;">
      ${name} — coming soon
    </div>
  `;
}
