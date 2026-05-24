const { signal } = window.__preact;

export const agents = signal([]);
export const agentColors = signal({ operator: '#bb86fc' });
export const currentChannel = signal('general');
export const connected = signal(true);
export const lastMessageId = signal(0);
export const messages = signal([]);
export const replyTo = signal(null);
export const usageData = signal(null); // { period, by_agent, totals } from /api/usage

// NEX-264: tracks whether the current session has admin privileges
// (i.e. the operator persona). Drives the Settings nav visibility in
// BottomBar.js and the admin-only guard in SettingsView.js. Backed by
// /api/auth/check's role field (operator → admin, aspect → not). UI
// gate only — server still enforces via requireAdmin on every endpoint,
// so a forged client-side flip would just produce 403s.
export const isAdmin = signal(false);

export function setIsAdminFromRole(role) {
  isAdmin.value = role === 'operator';
}

const AGENT_PALETTE = [
  '#cf6679', '#81d4fa', '#a5d6a7', '#ffcc80', '#ce93d8',
  '#80deea', '#f48fb1', '#b39ddb', '#80cbc4', '#ffab91',
];

// Stable palette fallback: hash the agent id into a palette slot so the
// same name always gets the same color, regardless of registration order
// or which agents are currently online. Without this, register/deregister
// reshuffles indices and existing chips change color.
//
// FNV-1a 32-bit — fast, deterministic, well-distributed for short strings.
function paletteFor(id) {
  let h = 0x811c9dc5;
  for (let i = 0; i < id.length; i++) {
    h ^= id.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return AGENT_PALETTE[(h >>> 0) % AGENT_PALETTE.length];
}

// colorForAgent returns the color to use for an agent id. Server-
// configured colors win; otherwise a deterministic palette slot keyed
// off the id (NOT registration order) so colors stay stable across
// register/deregister.
export function colorForAgent(id, configured) {
  if (id === 'operator') return '#bb86fc';
  if (configured) return configured;
  return paletteFor(id || '');
}

export function setAgents(agentList) {
  agents.value = agentList;
  // Carry forward any colors we've already assigned so a deregister
  // doesn't drop the entry — messages from offline agents still need
  // their color to render correctly.
  const colors = { ...agentColors.value, operator: '#bb86fc' };
  agentList.forEach((agent) => {
    const id = typeof agent === 'string' ? agent : agent.id;
    if (!id) return;
    const configured = (typeof agent === 'object' && agent.color) || null;
    colors[id] = colorForAgent(id, configured);
  });
  agentColors.value = colors;
}
