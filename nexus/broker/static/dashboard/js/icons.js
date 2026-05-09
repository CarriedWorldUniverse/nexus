// TCW-themed inline SVG icons for The Nexus dashboard
// All icons use currentColor and are designed for 20x20 viewBox

const { html } = window.__preact;

const svg = (inner, size = 20) => html`
  <svg width=${size} height=${size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
       stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"
       style="flex-shrink:0">
    ${inner}
  </svg>
`;

// ── Nexus Brand Mark ──
// Converging threads meeting at a central node — the nexus point
export function NexusMark({ size = 28 }) {
  return html`
    <svg width=${size} height=${size} viewBox="0 0 32 32" fill="none" style="flex-shrink:0">
      <circle cx="16" cy="16" r="3" fill="currentColor" opacity="0.9"/>
      <line x1="16" y1="3" x2="16" y2="13" stroke="currentColor" stroke-width="1.2" opacity="0.6"/>
      <line x1="16" y1="19" x2="16" y2="29" stroke="currentColor" stroke-width="1.2" opacity="0.6"/>
      <line x1="3" y1="16" x2="13" y2="16" stroke="currentColor" stroke-width="1.2" opacity="0.6"/>
      <line x1="19" y1="16" x2="29" y2="16" stroke="currentColor" stroke-width="1.2" opacity="0.6"/>
      <line x1="6.5" y1="6.5" x2="13.2" y2="13.2" stroke="currentColor" stroke-width="1" opacity="0.35"/>
      <line x1="18.8" y1="18.8" x2="25.5" y2="25.5" stroke="currentColor" stroke-width="1" opacity="0.35"/>
      <line x1="25.5" y1="6.5" x2="18.8" y2="13.2" stroke="currentColor" stroke-width="1" opacity="0.35"/>
      <line x1="13.2" y1="18.8" x2="6.5" y2="25.5" stroke="currentColor" stroke-width="1" opacity="0.35"/>
      <circle cx="16" cy="3" r="1.5" fill="currentColor" opacity="0.5"/>
      <circle cx="16" cy="29" r="1.5" fill="currentColor" opacity="0.5"/>
      <circle cx="3" cy="16" r="1.5" fill="currentColor" opacity="0.5"/>
      <circle cx="29" cy="16" r="1.5" fill="currentColor" opacity="0.5"/>
      <circle cx="16" cy="16" r="7" stroke="currentColor" stroke-width="0.6" opacity="0.2"/>
      <circle cx="16" cy="16" r="12" stroke="currentColor" stroke-width="0.4" opacity="0.1"/>
    </svg>
  `;
}

// ── Nav Icons ──

// Hearthfire — where agents gather to talk
export function IconChat() {
  return svg(html`
    <path d="M4 18 C4 18 5 14 5 10 C5 6 8 3 12 3 C16 3 19 6 19 10 C19 14 16 17 12 17 L8 20 L8 17"/>
    <circle cx="9" cy="10.5" r="0.8" fill="currentColor" stroke="none"/>
    <circle cx="12" cy="10.5" r="0.8" fill="currentColor" stroke="none"/>
    <circle cx="15" cy="10.5" r="0.8" fill="currentColor" stroke="none"/>
  `);
}

// Scroll — ancient knowledge, unrolled
export function IconKnowledge() {
  return svg(html`
    <path d="M8 3 C6 3 5 4 5 5 L5 19 C5 20 6 21 8 21 L19 21"/>
    <path d="M8 3 L19 3 L19 17 L8 17 C6 17 5 18 5 19"/>
    <line x1="9" y1="7" x2="16" y2="7"/>
    <line x1="9" y1="10" x2="16" y2="10"/>
    <line x1="9" y1="13" x2="13" y2="13"/>
  `);
}

// Stone slab — filed records
export function IconFiles() {
  return svg(html`
    <rect x="4" y="4" width="16" height="16" rx="1"/>
    <line x1="4" y1="9" x2="20" y2="9"/>
    <line x1="9" y1="4" x2="9" y2="9"/>
    <line x1="8" y1="13" x2="16" y2="13"/>
    <line x1="8" y1="16" x2="13" y2="16"/>
  `);
}

// Carved tally marks — task tracking
export function IconTickets() {
  return svg(html`
    <rect x="3" y="3" width="18" height="18" rx="2"/>
    <line x1="7" y1="8" x2="7" y2="16"/>
    <line x1="10" y1="8" x2="10" y2="16"/>
    <line x1="13" y1="8" x2="13" y2="16"/>
    <line x1="16" y1="8" x2="16" y2="16"/>
    <line x1="5" y1="10" x2="9" y2="14" stroke-width="1.2"/>
  `);
}

// Rune stone — command interface
export function IconTerminal() {
  return svg(html`
    <rect x="3" y="4" width="18" height="16" rx="2"/>
    <polyline points="7,9 10,12 7,15"/>
    <line x1="13" y1="15" x2="17" y2="15"/>
  `);
}

// Sealed letter — correspondence
export function IconCorrespondence() {
  return svg(html`
    <rect x="3" y="5" width="18" height="14" rx="1.5"/>
    <polyline points="3,5 12,13 21,5"/>
    <line x1="3" y1="19" x2="8.5" y2="13" opacity="0.5"/>
    <line x1="21" y1="19" x2="15.5" y2="13" opacity="0.5"/>
  `);
}

// Network overview
export function IconStatus() {
  return svg(html`
    <circle cx="12" cy="12" r="2" fill="currentColor" stroke="none"/>
    <circle cx="12" cy="12" r="6" opacity="0.5"/>
    <circle cx="12" cy="12" r="9.5" opacity="0.25"/>
  `);
}

// ── Agent Role Glyphs ──
// Small 16x16 icons representing each agent's domain

const agentSvg = (inner) => html`
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor"
       stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"
       style="flex-shrink:0">
    ${inner}
  </svg>
`;

// The Frame — structural lattice (@keel)
export function GlyphFrame() {
  return agentSvg(html`
    <rect x="2" y="2" width="12" height="12" rx="1"/>
    <line x1="8" y1="2" x2="8" y2="14"/>
    <line x1="2" y1="8" x2="14" y2="8"/>
  `);
}

// Coherence — interlocking rings (@verity)
export function GlyphCoherence() {
  return agentSvg(html`
    <circle cx="6" cy="8" r="4" fill="none"/>
    <circle cx="10" cy="8" r="4" fill="none"/>
  `);
}

// Artifice — circuit-rune hybrid (@forge)
export function GlyphArtifice() {
  return agentSvg(html`
    <circle cx="8" cy="8" r="2.5"/>
    <line x1="8" y1="2" x2="8" y2="5.5"/>
    <line x1="8" y1="10.5" x2="8" y2="14"/>
    <line x1="2" y1="8" x2="5.5" y2="8"/>
    <line x1="10.5" y1="8" x2="14" y2="8"/>
  `);
}

// Form — prism/cube (@wren)
export function GlyphForm() {
  return agentSvg(html`
    <polygon points="8,2 14,5.5 14,10.5 8,14 2,10.5 2,5.5" fill="none"/>
    <line x1="8" y1="2" x2="8" y2="14" opacity="0.4"/>
    <line x1="2" y1="5.5" x2="14" y2="5.5" opacity="0.4"/>
  `);
}

// Expression — open eye (@maren)
export function GlyphExpression() {
  return agentSvg(html`
    <path d="M2 8 C4 4 12 4 14 8 C12 12 4 12 2 8 Z"/>
    <circle cx="8" cy="8" r="2" fill="currentColor" stroke="none"/>
  `);
}

// Inquiry — compass/lens (@harrow)
export function GlyphInquiry() {
  return agentSvg(html`
    <circle cx="7" cy="7" r="4.5"/>
    <line x1="10.5" y1="10.5" x2="14" y2="14" stroke-width="1.5"/>
  `);
}

// The Will — open hand, reaching outward (@operator)
export function GlyphWill() {
  return agentSvg(html`
    <path d="M8 14 C8 14 4 12 3 9 C2.5 7.5 3 6 4 6 C5 6 5.5 7 5.5 7 L6 5 C6 5 6 4 7 4 C8 4 8 5 8 5 L8 3.5 C8 3.5 8 2.5 9 2.5 C10 2.5 10 3.5 10 3.5 L10 5 C10 5 10 4 11 4 C12 4 12 5 12 5 L12 8 C12 8 13 7.5 13 9 C13 11 10 14 8 14 Z" fill="none"/>
  `);
}

// Constellation — agents as interconnected nodes (@agents view)
export function IconAgents() {
  return svg(html`
    <circle cx="12" cy="5" r="1.5" fill="currentColor" stroke="none"/>
    <circle cx="5" cy="17" r="1.5" fill="currentColor" stroke="none"/>
    <circle cx="19" cy="17" r="1.5" fill="currentColor" stroke="none"/>
    <line x1="12" y1="5" x2="5" y2="17" opacity="0.5"/>
    <line x1="12" y1="5" x2="19" y2="17" opacity="0.5"/>
    <line x1="5" y1="17" x2="19" y2="17" opacity="0.5"/>
  `);
}

export function IconDocs() {
  return svg(html`
    <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/>
    <polyline points="14 2 14 8 20 8"/>
    <line x1="16" y1="13" x2="8" y2="13"/>
    <line x1="16" y1="17" x2="8" y2="17"/>
    <polyline points="10 9 9 9 8 9"/>
  `);
}

// Two vertical panes divided — toggles split-view mode
export function IconSplit() {
  return svg(html`
    <rect x="3" y="4" width="8" height="16" rx="1.5"/>
    <rect x="13" y="4" width="8" height="16" rx="1.5"/>
  `);
}

// Map agent IDs to their glyphs
export const AGENT_GLYPHS = {
  infra: GlyphFrame,
  canon: GlyphCoherence,
  ai: GlyphArtifice,
  unity: GlyphForm,
  artist: GlyphExpression,
  research: GlyphInquiry,
  operator: GlyphWill,
};
