const { html, useState, useEffect, useRef } = window.__preact;
import { agents, agentColors, currentChannel } from '../state.js';
import { BASE } from '../api.js';
import { chatWS } from '../chat-ws.js';
import { ChatInput } from '../components/ChatInput.js';
import { MessageBubble } from '../components/MessageBubble.js';

export function AgentsView() {
  const agentList = agents.value;
  const colors = agentColors.value;

  function agentFromHash() {
    const hash = window.location.hash;
    if (hash.startsWith('#/agents/')) return hash.slice('#/agents/'.length);
    return null;
  }

  const [selectedAgent, setSelectedAgent] = useState(
    () => agentFromHash() || (agentList[0] ? (typeof agentList[0] === 'string' ? agentList[0] : agentList[0].id) : null)
  );
  const [agentMessages, setAgentMessages] = useState([]);
  const msgsRef = useRef(null);

  async function loadMessages() {
    if (!selectedAgent) return;
    try {
      const token = localStorage.getItem('auth_token');
      const res = await fetch(`${BASE}/api/chat?topic=dm:${selectedAgent}&limit=100`, {
        headers: token ? { Authorization: 'Bearer ' + token } : {}
      });
      if (!res.ok) return;
      const data = await res.json();
      setAgentMessages(Array.isArray(data) ? data : (data.messages || []));
    } catch {}
  }

  useEffect(() => {
    loadMessages();

    // Live updates via WebSocket. Only accept messages whose topic matches
    // this DM channel, and only accept reaction updates for messages we
    // currently have rendered. Unsubscribe on effect re-run (agent switch)
    // or unmount so stale handlers can't write to the previous agent's state.
    const wantTopic = `dm:${selectedAgent}`;

    chatWS.start();
    const offMsg = chatWS.on('message.created', (ev) => {
      const msg = ev && ev.msg;
      if (!msg || typeof msg.id !== 'number') return;
      if (msg.topic !== wantTopic) return;
      setAgentMessages(prev => {
        if (prev.some(m => m.id === msg.id)) return prev;
        return [...prev, msg];
      });
    });
    const offReact = chatWS.on('reaction.changed', (ev) => {
      if (!ev || typeof ev.msg_id !== 'number') return;
      setAgentMessages(prev => {
        const idx = prev.findIndex(m => m.id === ev.msg_id);
        if (idx === -1) return prev;
        const next = [...prev];
        next[idx] = { ...next[idx], reactions: ev.reactions || {} };
        return next;
      });
    });
    const offReconnect = chatWS.on('reconnect', () => loadMessages());

    // Polling kept as a 30s safety net (was 4s).
    const iv = setInterval(loadMessages, 30000);
    return () => {
      clearInterval(iv);
      offMsg();
      offReact();
      offReconnect();
    };
  }, [selectedAgent]);

  useEffect(() => {
    const el = msgsRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [agentMessages]);

  function selectAgent(id) {
    setSelectedAgent(id);
    window.location.hash = '#/agents/' + id;
    // currentChannel is set by the hashchange handler in app.js
  }

  return html`
    <div class="agents-view">
      <div class="agents-bar">
        ${agentList.map(agent => {
          const id = typeof agent === 'string' ? agent : agent.id;
          const alive = typeof agent === 'object' ? agent.alive : true;
          const task = typeof agent === 'object' ? agent.task : '';
          const color = colors[id] || '#888';
          return html`
            <button
              key=${id}
              class=${'agent-btn' + (selectedAgent === id ? ' active' : '')}
              style=${{ '--agent-color': color }}
              onClick=${() => selectAgent(id)}
            >
              <span class=${'agent-dot' + (alive ? ' alive' : '')} style=${{ background: alive ? color : '#444' }}></span>
              <span class="agent-btn-name">${id}</span>
              ${task && html`<span class="agent-btn-task">${task}</span>`}
            </button>
          `;
        })}
      </div>
      <div class="agents-messages" ref=${msgsRef}>
        ${agentMessages.length === 0
          ? html`<div class="agents-empty">No messages with @${selectedAgent} yet.</div>`
          : agentMessages.map(msg => html`<${MessageBubble} key=${msg.id} msg=${msg} />`)
        }
      </div>
      <${ChatInput} onSent=${loadMessages} />
    </div>
  `;
}
