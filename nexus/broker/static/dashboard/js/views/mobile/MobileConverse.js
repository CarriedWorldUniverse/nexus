const { html, useEffect, useRef, useState } = window.__preact;

import { agents, agentColors, currentChannel, replyTo } from '../../state.js';
import { fetchMessages, sendDM, sendMessage } from '../../api.js';
import { subscribe } from '../../comms.js';
import { MessageBubble } from '../../components/MessageBubble.js';

const TEAM = 'general';

function isDM(channel) {
  return channel && channel !== TEAM && !channel.startsWith('topic:');
}

function messageBelongsToChannel(msg, channel) {
  if (!channel || channel === TEAM) return !msg.topic || msg.topic === 'general';
  if (channel.startsWith('topic:')) return msg.topic === channel.slice(6);
  return msg.topic === `dm:${channel}`;
}

function normalizeChatPayload(payload) {
  if (!payload) return null;
  return {
    id: payload.id,
    from: payload.from,
    content: payload.content || '',
    reply_to: payload.reply_to || 0,
    created_at: payload.created_at || payload.received_at || '',
    topic: payload.topic || '',
    thread_root: payload.thread_root || 0,
    reactions: payload.reactions || [],
  };
}

function channelTitle(channel) {
  if (!channel || channel === TEAM) return 'Team';
  if (channel.startsWith('topic:')) return `#${channel.slice(6)}`;
  return `@${channel}`;
}

function replyTargetId() {
  const target = replyTo.value;
  if (!target) return 0;
  if (typeof target === 'number') return target;
  return Number(target.id) || 0;
}

function rosterIds() {
  return (agents.value || [])
    .map((a) => (typeof a === 'string' ? a : (a.id || a.name || '')))
    .filter(Boolean);
}

export function MobileConverse({ onActive }) {
  const [view, setView] = useState('list');
  const roster = rosterIds().filter((a) => a !== 'operator');
  const ordered = ['shadow', ...roster.filter((a) => a !== 'shadow')];

  function open(channel) {
    currentChannel.value = channel;
    replyTo.value = null;
    setView('pane');
    if (onActive) onActive();
  }

  if (view === 'list') {
    return html`
      <section class="m-converse-list" aria-label="Conversations">
        <button type="button" class="m-conv-item m-team" onClick=${() => open(TEAM)}>
          <span class="m-conv-title">Team</span>
          <span class="m-conv-meta">general</span>
        </button>
        <div class="m-conv-section">Direct</div>
        ${ordered.map((agent) => {
          const color = (agentColors.value || {})[agent] || '#888';
          return html`
            <button key=${agent} type="button" class="m-conv-item" onClick=${() => open(agent)}>
              <span class="m-conv-avatar" style=${{ backgroundColor: color }}>${agent.slice(0, 2).toUpperCase()}</span>
              <span class="m-conv-title">
                ${agent === 'shadow' ? html`<span class="m-shadow-pin" title="Pinned">★</span>` : null}
                ${agent}
              </span>
            </button>
          `;
        })}
      </section>
    `;
  }

  return html`<${MobilePane} channel=${currentChannel.value || TEAM} onBack=${() => setView('list')} />`;
}

function MobilePane({ channel, onBack }) {
  const [msgs, setMsgs] = useState([]);
  const [text, setText] = useState('');
  const [loading, setLoading] = useState(true);
  const streamRef = useRef(null);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    replyTo.value = null;
    fetchMessages(channel, 0).then((result) => {
      if (!alive) return;
      const rows = Array.isArray(result) ? result : (result.messages || []);
      setMsgs(rows.filter((m) => messageBelongsToChannel(m, channel)));
    }).catch((e) => {
      console.error('[MobileConverse] fetchMessages failed', e);
      if (alive) setMsgs([]);
    }).finally(() => {
      if (alive) setLoading(false);
    });

    const off = subscribe('subscribe.chat', {}, (payload) => {
      const msg = normalizeChatPayload(payload);
      if (!msg || !messageBelongsToChannel(msg, channel)) return;
      setMsgs((prev) => prev.some((m) => m.id === msg.id) ? prev : [...prev, msg]);
    });

    return () => {
      alive = false;
      off();
    };
  }, [channel]);

  useEffect(() => {
    const el = streamRef.current;
    if (!el) return;
    requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
  }, [msgs.length]);

  async function send() {
    const body = text.trim();
    if (!body) return;
    const replyToId = replyTargetId();
    if (isDM(channel)) {
      await sendDM(channel, body, replyToId);
    } else {
      await sendMessage({
        from: 'operator',
        content: body,
        replyTo: replyToId,
        topic: channel === TEAM ? '' : channel,
      });
    }
    setText('');
    replyTo.value = null;
  }

  return html`
    <section class="m-pane" aria-label=${channelTitle(channel)}>
      <header class="m-pane-head">
        <button type="button" class="m-back" aria-label="Back to conversations" onClick=${onBack}>‹</button>
        <div class="m-pane-title">${channelTitle(channel)}</div>
      </header>
      <div class="m-pane-stream" ref=${streamRef}>
        ${loading ? html`<div class="m-empty">Loading messages.</div>` : null}
        ${!loading && msgs.length === 0 ? html`<div class="m-empty">No messages yet.</div>` : null}
        ${msgs.map((m) => html`<${MessageBubble} key=${m.id} msg=${m} />`)}
      </div>
      <div class="m-pane-composer">
        ${replyTo.value ? html`
          <div class="m-replying">
            <span>Replying to @${replyTo.value.from || 'unknown'}</span>
            <button type="button" onClick=${() => { replyTo.value = null; }}>Cancel</button>
          </div>
        ` : null}
        <div class="m-compose-row">
          <textarea
            rows="1"
            value=${text}
            placeholder=${isDM(channel) ? `Message @${channel}` : 'Message the team'}
            onInput=${(e) => setText(e.currentTarget.value)}
            onKeyDown=${(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                send();
              }
            }}
          ></textarea>
          <button type="button" onClick=${send} disabled=${!text.trim()}>Send</button>
        </div>
      </div>
    </section>
  `;
}
