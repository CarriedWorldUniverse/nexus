const { signal } = window.__preact;

export const agents = signal([]);
export const agentColors = signal({ operator: '#bb86fc' });
export const currentChannel = signal('general');
export const connected = signal(true);
export const lastMessageId = signal(0);
export const messages = signal([]);
export const replyTo = signal(null);
export const usageData = signal(null); // { period, by_agent, totals } from /api/usage

const AGENT_PALETTE = [
  '#cf6679', '#81d4fa', '#a5d6a7', '#ffcc80', '#ce93d8',
  '#80deea', '#f48fb1', '#b39ddb', '#80cbc4', '#ffab91',
];

export function setAgents(agentList) {
  agents.value = agentList;
  const colors = { operator: '#bb86fc' };
  agentList.forEach((agent, i) => {
    const id = typeof agent === 'string' ? agent : agent.id;
    if (id) {
      // Use the agent's configured color from the API, fall back to palette
      colors[id] = (typeof agent === 'object' && agent.color) || AGENT_PALETTE[i % AGENT_PALETTE.length];
    }
  });
  agentColors.value = colors;
}
