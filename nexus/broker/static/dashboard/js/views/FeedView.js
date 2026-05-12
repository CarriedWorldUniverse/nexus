const { h, html, signal, useEffect, useRef } = window.__preact;

import { currentChannel, messages, lastMessageId, replyTo, agents, agentColors } from '../state.js';
import { fetchMessages, fetchReactionsForIds } from '../api.js';
import { checkForMentions } from '../notifications.js';
import { chatWS } from '../chat-ws.js';
import { MessageBubble } from '../components/MessageBubble.js';
import { ChatInput } from '../components/ChatInput.js';
import { ThreadView } from '../components/ThreadView.js';

const expandedThreads = signal({});
const selectedAgent = signal(null);

const msgCache = {};

async function loadMessages() {
  try {
    // fetchMessages returns {messages, has_more}; agent-network's
    // version returned the array directly. Unwrap so the rest of this
    // function (which iterates `rows`) works against the new shape.
    // Without this unwrap the early-return on `!rows.length` always
    // fired (length undefined on the object) and the SPA showed an
    // empty chat history on every load.
    const result = await fetchMessages('general', lastMessageId.value);
    const rows = Array.isArray(result) ? result : (result && result.messages) || [];
    if (!rows.length) return;
    rows.forEach(m => { msgCache[m.id] = m; });

    // Deduplicate — only add messages we haven't seen
    const existingIds = new Set(messages.value.map(m => m.id));
    const newRows = rows.filter(m => !existingIds.has(m.id));
    if (newRows.length === 0) return;

    checkForMentions(newRows);

    const maxId = newRows.reduce((max, m) => Math.max(max, m.id), lastMessageId.value);
    lastMessageId.value = maxId;

    // Update reply_count on parent messages when new replies arrive
    const updated = [...messages.value];
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

    // Auto-scroll: always on first load, then only if near bottom
    const el = document.querySelector('.chat-messages');
    if (el) {
      const isFirstLoad = el.scrollTop === 0 && el.scrollHeight > el.clientHeight;
      const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 150;
      if (isFirstLoad || nearBottom) {
        requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
      }
    }
  } catch (e) {
    console.error('[FeedView] loadMessages failed', e);
  }
}

// Batch-refresh reactions for all currently-rendered messages. Called after
// WS reconnect because loadMessages() uses ?after=lastId and can't recover
// reaction changes on already-rendered rows.
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
    console.error('[FeedView] refreshReactions failed', e);
  }
}

// FeedView only shows #general (no topic / topic === 'general').
function onWsMessageCreated(ev) {
  const msg = ev && ev.msg;
  if (!msg || typeof msg.id !== 'number') return;
  if (msg.topic && msg.topic !== 'general') return;
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

  const el = document.querySelector('.chat-messages');
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

function getTopLevelMessages() {
  const topLevel = [];
  const replyMap = {};
  for (const m of messages.value) {
    if (m.reply_to) {
      if (!replyMap[m.reply_to]) replyMap[m.reply_to] = [];
      replyMap[m.reply_to].push(m);
    } else {
      topLevel.push(m);
    }
  }
  // Broker doesn't surface reply_count today — derive it from the page
  // we already loaded so the thread expander shows up. The WS-bump path
  // above also touches reply_count when a fresh reply arrives, so max()
  // keeps that explicit value if it's higher than what we can see.
  const decorated = topLevel.map(t => {
    const replies = replyMap[t.id]?.length || 0;
    const existing = t.reply_count || 0;
    if (replies <= existing) return t;
    return { ...t, reply_count: replies };
  });
  return { topLevel: decorated, replyMap };
}

function dayLabel(dateStr) {
  if (!dateStr) return '';
  // formatTime in MessageBubble does the same dance: nexus emits RFC 3339
  // (already terminated with Z), agent-network's legacy shape was naive
  // UTC needing Z appended. Tolerate both.
  const isISO = /Z$|[+-]\d\d:?\d\d$/.test(dateStr);
  const d = new Date(isISO ? dateStr : dateStr + 'Z');
  if (isNaN(d.getTime())) return '';
  return d.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' });
}

// chat.list normalizes timestamps to created_at; chat.deliver carries
// received_at; legacy code paths used `at`. Read whichever exists.
function msgAt(msg) {
  return msg && (msg.created_at || msg.received_at || msg.at) || '';
}

function handleMessageTap(e) {
  if (window.innerWidth > 768) return;
  if (e.target.closest('.msg-actions')) return;

  const msg = e.target.closest('.msg');
  if (!msg) return;

  const wasTapped = msg.classList.contains('tapped');
  document.querySelectorAll('.msg.tapped').forEach(m => m.classList.remove('tapped'));
  if (!wasTapped) msg.classList.add('tapped');
}

export function FeedView() {
  const initialized = useRef(false);

  useEffect(() => {
    if (!initialized.current) {
      initialized.current = true;
      // Hydrate messages then reactions. fetchMessages doesn't include
      // reactions inline — without this call reactions only appear after
      // the next live reaction.changed event or a WS reconnect, so a
      // cold reload shows un-reacted messages until traffic resumes.
      loadMessages().then(() => refreshReactions());
    }

    // Scroll to bottom on every mount (including tab re-entry)
    requestAnimationFrame(() => {
      const el = document.querySelector('.chat-messages');
      if (el) el.scrollTop = el.scrollHeight;
    });

    // WS live updates — unsubscribe on unmount so stale handlers don't fire.
    chatWS.start();
    const offMsg = chatWS.on('message.created', onWsMessageCreated);
    const offReact = chatWS.on('reaction.changed', onWsReactionChanged);
    const offReconnect = chatWS.on('reconnect', () => {
      loadMessages();
      refreshReactions();
    });

    // Polling kept as a 30s safety net (was 3s).
    const msgInterval = setInterval(loadMessages, 30000);
    return () => {
      clearInterval(msgInterval);
      offMsg();
      offReact();
      offReconnect();
    };
  }, []);

  const { topLevel, replyMap } = getTopLevelMessages();

  // Build operator-involved thread set for agent-only collapsing
  const opThreads = new Set();
  for (const m of messages.value) {
    const isOp = m.from === 'operator' || m.type === 'input';
    const mentionsOp = (m.content || '').toLowerCase().includes('@operator') || (m.content || '').toLowerCase().includes('@all');
    if (isOp || mentionsOp) {
      opThreads.add(m.id);
      if (m.reply_to) opThreads.add(m.reply_to);
    }
    if (m.reply_to && opThreads.has(m.reply_to)) {
      opThreads.add(m.id);
    }
  }
  // Propagate: if any reply in a thread involves operator, mark the parent
  for (const [pid, reps] of Object.entries(replyMap)) {
    const parentId = Number(pid);
    for (const rep of reps) {
      if (opThreads.has(rep.id)) opThreads.add(parentId);
    }
    if (opThreads.has(parentId)) {
      for (const rep of reps) opThreads.add(rep.id);
    }
  }

  // Apply agent filter
  const agentFilter = selectedAgent.value;
  const filteredTopLevel = agentFilter
    ? topLevel.filter(msg =>
        msg.from === agentFilter ||
        (msg.content || '').includes('@' + agentFilter)
      )
    : topLevel;

  let lastFrom = null;
  let lastDay = null;

  function toggleThread(msg) {
    const cur = expandedThreads.value;
    expandedThreads.value = { ...cur, [msg.id]: !cur[msg.id] };
  }

  return html`
    <div class="chat-view">
      <div class="feed-agent-filter">
        <button
          class=${'feed-filter-btn' + (!selectedAgent.value ? ' active' : '')}
          onClick=${() => selectedAgent.value = null}
        >All</button>
        ${agents.value.map(agent => {
          const id = typeof agent === 'string' ? agent : agent.id;
          const color = (agentColors.value || {})[id] || '#888';
          return html`<button
            key=${id}
            class=${'feed-filter-btn' + (selectedAgent.value === id ? ' active' : '')}
            style=${{ '--agent-color': color }}
            onClick=${() => selectedAgent.value = id}
          >@${id}</button>`;
        })}
      </div>

      <div class="chat-messages" onclick=${handleMessageTap}>
        ${filteredTopLevel.map(msg => {
          const day = dayLabel(msgAt(msg));
          const showDay = day !== lastDay;
          const compact = !showDay && msg.from === lastFrom;
          lastFrom = msg.from;
          lastDay = day;

          const parentMsg = msg.reply_to ? msgCache[msg.reply_to] : null;
          const threadMsgs = replyMap[msg.id] || [];
          const threadOpen = !!expandedThreads.value[msg.id];

          const isAgentOnly = msg.from !== 'operator' && msg.type !== 'input'
            && !(m => (m.content || '').toLowerCase().includes('@operator') || (m.content || '').toLowerCase().includes('@all'))(msg)
            && !opThreads.has(msg.id);

          return html`
            ${showDay && html`<div class="day-sep">${day}</div>`}
            <${MessageBubble}
              key=${msg.id}
              msg=${msg}
              compact=${compact}
              parentMsg=${parentMsg}
              onReply=${toggleThread}
              agentOnly=${isAgentOnly}
            />
            ${threadOpen && html`<${ThreadView} parentId=${msg.id} />`}
          `;
        })}
      </div>
      <${ChatInput} onSent=${loadMessages} />
    </div>
  `;
}
