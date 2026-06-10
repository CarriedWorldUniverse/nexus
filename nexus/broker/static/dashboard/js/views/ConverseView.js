const { html, signal, useEffect, useRef, useState } = window.__preact;

import { currentChannel, messages, lastMessageId, replyTo, agents, agentColors } from '../state.js';
import { fetchMessages, fetchOlderMessages, fetchReactionsForIds, sendDM, sendMessage } from '../api.js';
import { checkForMentions } from '../notifications.js';
import { chatWS } from '../chat-ws.js';
import { MessageBubble } from '../components/MessageBubble.js';

const TEAM = 'general';

const oldestLoadedId = signal(0);
const hasMoreOlder = signal(true);
const loadingOlder = signal(false);

const msgCache = {};
if (typeof window !== 'undefined') window.__msgCache = msgCache;

function isDM(channel) {
  return channel && channel !== TEAM && !channel.startsWith('topic:');
}

function messageBelongsToChannel(msg, channel) {
  if (!channel || channel === TEAM) {
    return !msg.topic || msg.topic === 'general';
  }
  if (channel.startsWith('topic:')) {
    return msg.topic === channel.slice(6);
  }
  return msg.topic === `dm:${channel}`;
}

function msgAt(msg) {
  return msg && (msg.created_at || msg.received_at || msg.at) || '';
}

function dayLabel(dateStr) {
  if (!dateStr) return '';
  const isISO = /Z$|[+-]\d\d:?\d\d$/.test(dateStr);
  const d = new Date(isISO ? dateStr : dateStr + 'Z');
  if (isNaN(d.getTime())) return '';
  return d.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' });
}

function channelTitle(channel) {
  if (!channel || channel === TEAM) return 'Team';
  if (channel.startsWith('topic:')) return `#${channel.slice(6)}`;
  return `@${channel}`;
}

function channelDesc(channel) {
  if (!channel || channel === TEAM) return 'General team stream';
  if (channel.startsWith('topic:')) return `Topic ${channel.slice(6)}`;
  return 'Direct message';
}

function replyTargetId() {
  const target = replyTo.value;
  if (!target) return 0;
  if (typeof target === 'number') return target;
  return Number(target.id) || 0;
}

function clearMessageState() {
  messages.value = [];
  lastMessageId.value = 0;
  oldestLoadedId.value = 0;
  hasMoreOlder.value = true;
  loadingOlder.value = false;
}

async function loadMessages() {
  try {
    const result = await fetchMessages(currentChannel.value, lastMessageId.value);
    const rows = Array.isArray(result) ? result : (result?.messages || []);
    const haveRows = rows.length > 0;

    if (oldestLoadedId.value === 0) {
      if (haveRows) {
        const minId = rows.reduce((min, m) => Math.min(min, m.id), Infinity);
        if (Number.isFinite(minId)) oldestLoadedId.value = minId;
      } else if (lastMessageId.value > 0) {
        oldestLoadedId.value = lastMessageId.value;
      }
    }

    if (!haveRows) return;
    rows.forEach(m => { msgCache[m.id] = m; });

    const existingIds = new Set(messages.value.map(m => m.id));
    const newRows = rows.filter(m => !existingIds.has(m.id));
    if (newRows.length === 0) return;

    checkForMentions(newRows);

    const maxId = newRows.reduce((max, m) => Math.max(max, m.id), lastMessageId.value);
    lastMessageId.value = maxId;

    let updated = [...messages.value];
    let changed = false;
    for (const m of newRows) {
      if (m.reply_to) {
        const idx = updated.findIndex(p => p.id === m.reply_to);
        if (idx !== -1) {
          updated[idx] = { ...updated[idx], reply_count: (updated[idx].reply_count || 0) + 1 };
          changed = true;
        }
      }
    }
    messages.value = [...(changed ? updated : messages.value), ...newRows];

    const el = document.querySelector('.cv-stream');
    if (el) {
      const isFirstLoad = el.scrollTop === 0 && el.scrollHeight > el.clientHeight;
      const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 150;
      if (isFirstLoad || nearBottom) {
        requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
      }
    }
  } catch (e) {
    console.error('[Converse] loadMessages failed', e);
  }
}

async function loadOlder() {
  if (loadingOlder.value || !hasMoreOlder.value || oldestLoadedId.value <= 0) return;
  loadingOlder.value = true;
  const ch = currentChannel.value;
  const beforeId = oldestLoadedId.value;
  const el = document.querySelector('.cv-stream');
  const prevScrollHeight = el ? el.scrollHeight : 0;
  const prevScrollTop = el ? el.scrollTop : 0;
  try {
    const result = await fetchOlderMessages(ch, beforeId);
    const rows = Array.isArray(result) ? result : (result?.messages || []);
    if (currentChannel.value !== ch) return;
    if (rows.length === 0) {
      hasMoreOlder.value = false;
      return;
    }
    rows.forEach(m => { msgCache[m.id] = m; });
    const existingIds = new Set(messages.value.map(m => m.id));
    const fresh = rows.filter(m => !existingIds.has(m.id));
    if (fresh.length === 0) {
      hasMoreOlder.value = false;
      return;
    }
    const minId = fresh.reduce((min, m) => Math.min(min, m.id), Infinity);
    if (Number.isFinite(minId)) oldestLoadedId.value = minId;
    messages.value = [...fresh, ...messages.value];
    requestAnimationFrame(() => {
      if (!el) return;
      el.scrollTop = prevScrollTop + (el.scrollHeight - prevScrollHeight);
    });
  } catch (e) {
    console.error('[Converse] loadOlder failed', e);
  } finally {
    loadingOlder.value = false;
  }
}

async function refreshReactions() {
  try {
    const ids = messages.value.map(m => m.id).filter(n => typeof n === 'number');
    if (!ids.length) return;
    const map = await fetchReactionsForIds(ids);
    if (!map) return;
    const next = messages.value.map(m => {
      const r = map[m.id];
      if (r === undefined) return m;
      return { ...m, reactions: r };
    });
    for (const m of next) msgCache[m.id] = m;
    messages.value = next;
  } catch (e) {
    console.error('[Converse] refreshReactions failed', e);
  }
}

function onWsMessageCreated(ev) {
  const msg = ev && ev.msg;
  if (!msg || typeof msg.id !== 'number') return;
  if (!messageBelongsToChannel(msg, currentChannel.value)) return;
  msgCache[msg.id] = msg;
  if (messages.value.some(m => m.id === msg.id)) return;

  checkForMentions([msg]);
  lastMessageId.value = Math.max(lastMessageId.value, msg.id);

  let updated = messages.value;
  if (msg.reply_to) {
    const idx = updated.findIndex(p => p.id === msg.reply_to);
    if (idx !== -1) {
      updated = [...updated];
      updated[idx] = { ...updated[idx], reply_count: (updated[idx].reply_count || 0) + 1 };
    }
  }
  messages.value = [...updated, msg];

  const el = document.querySelector('.cv-stream');
  if (el) {
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 150;
    if (nearBottom) {
      requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
    }
  }
}

function onWsReactionChanged(ev) {
  if (!ev || typeof ev.msg_id !== 'number') return;
  const id = ev.msg_id;
  const reactions = ev.reactions || {};
  if (msgCache[id]) msgCache[id] = { ...msgCache[id], reactions };
  const idx = messages.value.findIndex(m => m.id === id);
  if (idx === -1) return;
  const next = [...messages.value];
  next[idx] = { ...next[idx], reactions };
  messages.value = next;
}

function computeOpInvolvedSet() {
  const opIds = new Set();
  const byId = new Map();

  for (const m of messages.value) byId.set(m.id, m);

  const isOpish = (m) => {
    if (m.from === 'operator' || m.type === 'input') return true;
    const c = (m.content || '').toLowerCase();
    return c.includes('@operator') || c.includes('@all');
  };

  for (const m of messages.value) {
    if (isOpish(m)) opIds.add(m.id);
  }

  for (const m of messages.value) {
    if (!isOpish(m)) continue;
    let cur = m;
    for (let i = 0; i < 64 && cur && cur.reply_to; i++) {
      const parent = byId.get(cur.reply_to);
      if (!parent || opIds.has(parent.id)) break;
      opIds.add(parent.id);
      cur = parent;
    }
  }

  let grew = true;
  while (grew) {
    grew = false;
    for (const m of messages.value) {
      if (m.reply_to && opIds.has(m.reply_to) && !opIds.has(m.id)) {
        opIds.add(m.id);
        grew = true;
      }
    }
  }

  return opIds;
}

function handleMsgRefClick(e) {
  const a = e.target.closest('a.msg-id-ref');
  if (!a) return;
  const id = a.getAttribute('data-msg-ref');
  if (!id) return;
  const target = document.getElementById(`msg-${id}`);
  if (!target) return;
  e.preventDefault();
  target.scrollIntoView({ behavior: 'smooth', block: 'center' });
  target.classList.add('msg-flash');
  setTimeout(() => target.classList.remove('msg-flash'), 1600);
}

function ConversationList({ roster, active, onSelect }) {
  const dmAgents = roster.filter((a) => a && a !== 'operator');
  const ordered = ['shadow', ...dmAgents.filter((a) => a !== 'shadow')];

  return html`
    <nav class="cv-list" aria-label="Conversations">
      <button
        type="button"
        class=${'cv-item cv-team-item' + (active === TEAM ? ' active' : '')}
        onClick=${() => onSelect(TEAM)}
      >
        <span class="cv-item-main">Team</span>
        <span class="cv-item-meta">general</span>
      </button>

      <div class="cv-section">Direct</div>
      ${ordered.map((agent) => {
        const color = (agentColors.value || {})[agent] || '#888';
        return html`
          <button
            key=${agent}
            type="button"
            class=${'cv-item' + (active === agent ? ' active' : '')}
            onClick=${() => onSelect(agent)}
          >
            <span class="cv-avatar" style=${{ backgroundColor: color }}>${agent.slice(0, 2).toUpperCase()}</span>
            <span class="cv-item-main">
              ${agent === 'shadow' ? html`<span class="cv-pin" title="Pinned">★</span>` : null}
              ${agent}
            </span>
          </button>
        `;
      })}
      <button type="button" class="cv-item cv-new" disabled>
        <span class="cv-plus">+</span>
        <span class="cv-item-main">New</span>
      </button>
    </nav>
  `;
}

function Composer({ channel, onSent }) {
  const [text, setText] = useState('');
  const target = replyTo.value;

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
    onSent();
  }

  return html`
    <div class="cv-composer">
      ${target && html`
        <div class="cv-replying">
          <span>Replying to @${target.from || 'unknown'}</span>
          <button type="button" onClick=${() => { replyTo.value = null; }}>Cancel</button>
        </div>
      `}
      <div class="cv-compose-row">
        <textarea
          value=${text}
          placeholder=${isDM(channel) ? `Message @${channel}` : 'Message the team'}
          onInput=${(e) => setText(e.target.value)}
          onKeyDown=${(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              send();
            }
          }}
        ></textarea>
        <button type="button" class="cv-send" onClick=${send}>Send</button>
      </div>
    </div>
  `;
}

export function ConverseView() {
  const initialized = useRef(false);
  const ch = currentChannel.value || TEAM;
  const roster = (agents.value || []).map((a) => (typeof a === 'string' ? a : a.id)).filter(Boolean);

  useEffect(() => {
    if (!initialized.current) {
      initialized.current = true;
      clearMessageState();
      currentChannel.value = currentChannel.value || TEAM;
      loadMessages().then(() => refreshReactions());
    }

    chatWS.start();
    const offMsg = chatWS.on('message.created', onWsMessageCreated);
    const offReact = chatWS.on('reaction.changed', onWsReactionChanged);
    const offReconnect = chatWS.on('reconnect', () => {
      loadMessages();
      refreshReactions();
    });

    const msgInterval = setInterval(loadMessages, 30000);
    return () => {
      clearInterval(msgInterval);
      offMsg();
      offReact();
      offReconnect();
    };
  }, []);

  function selectConversation(next) {
    if (currentChannel.value === next) return;
    currentChannel.value = next;
    replyTo.value = null;
    clearMessageState();
    loadMessages().then(() => refreshReactions());
  }

  const visibleMessages = (messages.value || []).filter((m) => messageBelongsToChannel(m, ch));
  const opIds = ch === TEAM ? computeOpInvolvedSet() : null;

  let lastFrom = null;
  let lastDay = null;

  return html`
    <div class="converse chat-view">
      <${ConversationList} roster=${roster} active=${ch} onSelect=${selectConversation} />
      <section class="cv-pane">
        <header class="cv-header">
          <div>
            <div class="cv-title">${channelTitle(ch)}</div>
            <div class="cv-desc">${channelDesc(ch)}</div>
          </div>
        </header>

        <div class="cv-stream chat-messages" onClick=${handleMsgRefClick}>
          ${oldestLoadedId.value > 0 && hasMoreOlder.value && html`
            <div class="chat-load-older">
              <button
                type="button"
                disabled=${loadingOlder.value}
                onClick=${loadOlder}
              >
                ${loadingOlder.value ? 'Loading...' : 'Load older messages'}
              </button>
            </div>
          `}
          ${oldestLoadedId.value > 0 && !hasMoreOlder.value && visibleMessages.length > 0 && html`
            <div class="chat-load-older chat-load-older-end">Start of history</div>
          `}
          ${visibleMessages.length === 0 && html`<div class="cv-empty">No messages yet.</div>`}
          ${visibleMessages.map(msg => {
            const day = dayLabel(msgAt(msg));
            const showDay = day !== lastDay;
            const compact = !showDay && msg.from === lastFrom;
            lastFrom = msg.from;
            lastDay = day;

            const parentMsg = msg.reply_to ? msgCache[msg.reply_to] : null;
            const isAgentOnly = opIds !== null && !opIds.has(msg.id)
              && msg.from !== 'operator' && msg.type !== 'input';

            return html`
              ${showDay && html`<div class="day-sep">${day}</div>`}
              <${MessageBubble}
                key=${msg.id}
                msg=${msg}
                compact=${compact}
                parentMsg=${parentMsg}
                agentOnly=${isAgentOnly}
              />
            `;
          })}
        </div>

        <${Composer} channel=${ch} onSent=${loadMessages} />
      </section>
    </div>
  `;
}
