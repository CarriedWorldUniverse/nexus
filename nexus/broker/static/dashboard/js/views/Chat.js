const { h, html, signal, useEffect, useRef } = window.__preact;

import { currentChannel, messages, lastMessageId, replyTo, agents, agentColors } from '../state.js';
import { fetchMessages, fetchOlderMessages, fetchTopics, fetchReactionsForIds } from '../api.js';
import { checkForMentions } from '../notifications.js';
import { chatWS } from '../chat-ws.js';
import { MessageBubble } from '../components/MessageBubble.js';
import { ChatInput } from '../components/ChatInput.js';
import { ThreadView } from '../components/ThreadView.js';

const topics = signal([]);
const expandedThreads = signal({});

// Oldest currently-loaded message id per channel — anchor for backward
// pagination ("Load older"). Set on first successful loadMessages, cleared
// on channel switch. `0` = nothing loaded yet.
const oldestLoadedId = signal(0);
// hasMoreOlder: true unless a fetchOlderMessages returned an empty page,
// proving we've seen the start of history. Reset on channel switch.
const hasMoreOlder = signal(true);
// Set while a "Load older" fetch is in flight, to disable the button and
// show a spinner without racing duplicate requests.
const loadingOlder = signal(false);

// Module-scoped cache of recently-seen messages, keyed by id. Exposed on
// window so MessageBubble's renderContent can look up preview text for
// #NNNN refs without importing this view (avoids a circular dependency
// since Chat.js already imports MessageBubble).
const msgCache = {};
if (typeof window !== 'undefined') window.__msgCache = msgCache;

async function loadMessages() {
  try {
    // fetchMessages returns {messages, has_more}; pull the array.
    // Tolerate both shapes for callers that might not have migrated.
    const result = await fetchMessages(currentChannel.value, lastMessageId.value);
    const rows = Array.isArray(result) ? result : (result?.messages || []);
    const haveRows = rows.length > 0;

    // Anchor oldestLoadedId so the Load Older button can appear even on a
    // reconnect that returned zero new rows (history exists, we just got a
    // valid empty catch-up). When rows arrive, use the min id; otherwise
    // fall back to lastMessageId so the first Load Older click fetches the
    // right set.
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
    console.error('[Chat] loadMessages failed', e);
  }
}

// After a WebSocket reconnect, loadMessages() (which uses ?after=lastId) only
// fetches *new* rows — it misses reactions added to already-rendered messages
// while we were offline. Batch-refresh reactions for everything currently in
// the signal so the UI catches up.
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
    // Keep msgCache in sync so thread views don't render stale reactions.
    for (const m of next) msgCache[m.id] = m;
    messages.value = next;
  } catch (e) {
    console.error('[Chat] refreshReactions failed', e);
  }
}

async function loadTopics() {
  try {
    const rows = await fetchTopics(15);
    topics.value = Array.isArray(rows) ? rows : [];
  } catch (e) {
    console.error('[Chat] loadTopics failed', e);
  }
}

function switchChannel(ch) {
  currentChannel.value = ch;
  messages.value = [];
  lastMessageId.value = 0;
  oldestLoadedId.value = 0;
  hasMoreOlder.value = true;
  loadingOlder.value = false;
  expandedThreads.value = {};
  loadMessages();
  window.location.hash = ch === 'general' ? '#/chat' : `#/chat/${ch}`;
}

// Backward pagination — fetch the page of messages preceding oldestLoadedId,
// prepend to the signal, preserve scroll position so the user's view doesn't
// jump. Empty result = we've reached the start of history; lock the button.
async function loadOlder() {
  if (loadingOlder.value || !hasMoreOlder.value || oldestLoadedId.value <= 0) return;
  loadingOlder.value = true;
  const ch = currentChannel.value;
  const beforeId = oldestLoadedId.value;
  // Snapshot scroll anchor so we can restore the operator's place after
  // the prepended rows expand the scroll height.
  const el = document.querySelector('.chat-messages');
  const prevScrollHeight = el ? el.scrollHeight : 0;
  const prevScrollTop = el ? el.scrollTop : 0;
  try {
    // fetchOlderMessages returns {messages, has_more}; pull the array.
    const result = await fetchOlderMessages(ch, beforeId);
    const rows = Array.isArray(result) ? result : (result?.messages || []);
    // Discard if the operator switched channels during the fetch — without
    // this guard, channel A's older messages would prepend onto channel B's
    // (now-empty) message list.
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
    // Restore scroll position: new rows added at top expanded scrollHeight,
    // so anchor on the delta to keep the previously-visible row in place.
    requestAnimationFrame(() => {
      if (!el) return;
      el.scrollTop = prevScrollTop + (el.scrollHeight - prevScrollHeight);
    });
  } catch (e) {
    console.error('[Chat] loadOlder failed', e);
  } finally {
    loadingOlder.value = false;
  }
}

// True if an incoming WS message.created belongs in the currently-displayed
// channel. The /api/chat GET already filters server-side, but the WS broadcasts
// every insert to every client, so we filter here.
function messageBelongsToChannel(msg, channel) {
  if (!channel || channel === 'general') {
    // #general shows messages with no topic OR topic === 'general'
    return !msg.topic || msg.topic === 'general';
  }
  if (channel.startsWith('topic:')) {
    return msg.topic === channel.slice(6);
  }
  // Agent DM channel — match dm:<agent> topic
  return msg.topic === `dm:${channel}`;
}

// Handler: WS message.created — append to messages signal if it belongs here
// and isn't already present. Matches the dedup / reply_count bookkeeping that
// loadMessages() does so the two paths stay consistent.
function onWsMessageCreated(ev) {
  const msg = ev && ev.msg;
  if (!msg || typeof msg.id !== 'number') return;
  if (!messageBelongsToChannel(msg, currentChannel.value)) return;
  msgCache[msg.id] = msg;
  if (messages.value.some(m => m.id === msg.id)) return;

  checkForMentions([msg]);
  lastMessageId.value = Math.max(lastMessageId.value, msg.id);

  // Update reply_count on parent when a reply arrives
  let updated = messages.value;
  if (msg.reply_to) {
    const idx = updated.findIndex(p => p.id === msg.reply_to);
    if (idx !== -1) {
      updated = [...updated];
      updated[idx] = { ...updated[idx], reply_count: (updated[idx].reply_count || 0) + 1 };
    }
  }
  messages.value = [...updated, msg];

  // Auto-scroll if near bottom
  const el = document.querySelector('.chat-messages');
  if (el) {
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 150;
    if (nearBottom) {
      requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
    }
  }
}

// Handler: WS reaction.changed — merge new reactions into the matching
// message in place. Reactions apply across channels, no filter needed.
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
  // The broker doesn't surface reply_count in chat.list / chat.deliver,
  // so derive it from what we have loaded. This keeps the thread expander
  // visible even if the parent's row was returned without the field.
  // The WS-bump path in onWsMessageCreated still bumps an explicit
  // reply_count on parents already in the signal — we max() so that a
  // fresh derivation from replyMap doesn't undercount stale state.
  const decorated = topLevel.map(t => {
    const replies = replyMap[t.id]?.length || 0;
    const existing = t.reply_count || 0;
    if (replies <= existing) return t;
    return { ...t, reply_count: replies };
  });
  return { topLevel: decorated, replyMap };
}

function channelLabel(ch) {
  if (!ch || ch === 'general') return '#general';
  if (ch.startsWith('topic:')) return `#${ch.slice(6)}`;
  return `@${ch}`;
}

function channelDesc(ch) {
  if (!ch || ch === 'general') return 'All messages';
  if (ch.startsWith('topic:')) return `Topic: ${ch.slice(6)}`;
  return `Direct message`;
}

function dayLabel(dateStr) {
  if (!dateStr) return '';
  // Match formatTime's tolerance: nexus emits RFC 3339 (already terminated
  // with Z), agent-network's legacy shape was naive UTC needing Z appended.
  const isISO = /Z$|[+-]\d\d:?\d\d$/.test(dateStr);
  const d = new Date(isISO ? dateStr : dateStr + 'Z');
  if (isNaN(d.getTime())) return '';
  return d.toLocaleDateString([], { weekday: 'long', month: 'short', day: 'numeric' });
}

// Pick the timestamp field for a message regardless of which path delivered
// it. chat.list (REST-via-WS) normalizes to created_at; WS shim does the
// same; legacy code paths used `at`. Try in that order.
function msgAt(msg) {
  return msg && (msg.created_at || msg.received_at || msg.at) || '';
}

// Delegated handler for #NNNN msg id refs rendered as <a class="msg-id-ref">.
// Intercepts the click, scrolls the referenced message into view if it's
// already loaded, briefly highlights it, and prevents the default
// hash-navigation (which would jump abruptly without highlight).
// If the target isn't loaded, falls through — the operator can scroll up
// and Load Older to find it. Future: trigger a fetch by id.
function handleMsgRefClick(e) {
  const a = e.target.closest('a.msg-id-ref');
  if (!a) return;
  const id = a.getAttribute('data-msg-ref');
  if (!id) return;
  const target = document.getElementById(`msg-${id}`);
  if (!target) return; // not loaded; let default href fire (no-op, hash-only)
  e.preventDefault();
  target.scrollIntoView({ behavior: 'smooth', block: 'center' });
  target.classList.add('msg-flash');
  setTimeout(() => target.classList.remove('msg-flash'), 1600);
}

export function Chat() {
  const initialized = useRef(false);

  useEffect(() => {
    if (!initialized.current) {
      initialized.current = true;
      loadMessages();
      loadTopics();
    }

    // Wire WS handlers — unsubscribe on unmount so FeedView / AgentsView
    // don't inherit Chat's handlers when the route changes.
    chatWS.start();
    const offMsg = chatWS.on('message.created', onWsMessageCreated);
    const offReact = chatWS.on('reaction.changed', onWsReactionChanged);
    const offReconnect = chatWS.on('reconnect', () => {
      loadMessages();
      refreshReactions();
    });

    // Polling stays as a safety net at 30s (was 3s) — covers any event the
    // WebSocket misses and any time the socket is disconnected. Hot path is
    // now the WS.
    const msgInterval = setInterval(loadMessages, 30000);
    const topicInterval = setInterval(loadTopics, 15000);
    return () => {
      clearInterval(msgInterval);
      clearInterval(topicInterval);
      offMsg();
      offReact();
      offReconnect();
    };
  }, []);

  const ch = currentChannel.value;
  const { topLevel, replyMap } = getTopLevelMessages();

  // Build operator-involved thread set for agent-only collapsing
  const opThreads = new Set();
  if (ch === 'general') {
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
  }

  let lastFrom = null;
  let lastDay = null;

  function toggleThread(msg) {
    const cur = expandedThreads.value;
    expandedThreads.value = { ...cur, [msg.id]: !cur[msg.id] };
  }

  return html`
    <div class="chat-view">
      <div class="chat-header">
        <span class="chat-header-title">${channelLabel(ch)}</span>
        <span class="chat-header-desc">${channelDesc(ch)}</span>
      </div>

      <div class="chat-channels">
        <button
          class=${'chat-tab' + (ch === 'general' ? ' active' : '')}
          onClick=${() => switchChannel('general')}
        >#general</button>

        ${topics.value
          .filter(t => {
            const name = t.topic || t.name || t;
            return !String(name).startsWith('dm:');
          })
          .map(t => {
          const key = `topic:${t.topic || t.name || t}`;
          return html`
            <button
              key=${key}
              class=${'chat-tab' + (ch === key ? ' active' : '')}
              onClick=${() => switchChannel(key)}
            >#${t.topic || t.name || t}</button>
          `;
        })}

        <span style="width:1px;height:16px;background:#333;margin:0 4px;flex-shrink:0;align-self:center;"></span>

        ${agents.value.map(agent => {
          const id = typeof agent === 'string' ? agent : agent.id;
          const color = (agentColors.value || {})[id] || '#888';
          return html`
            <button
              key=${`dm:${id}`}
              class=${'chat-tab' + (ch === id ? ' active' : '')}
              style=${{ color: ch === id ? color : undefined }}
              onClick=${() => switchChannel(id)}
            >@${id}</button>
          `;
        })}

      </div>

      <div class="chat-messages" onclick=${handleMsgRefClick}>
        ${oldestLoadedId.value > 0 && hasMoreOlder.value && html`
          <div class="chat-load-older">
            <button
              type="button"
              disabled=${loadingOlder.value}
              onClick=${loadOlder}
            >
              ${loadingOlder.value ? 'Loading…' : 'Load older messages'}
            </button>
          </div>
        `}
        ${oldestLoadedId.value > 0 && !hasMoreOlder.value && messages.value.length > 0 && html`
          <div class="chat-load-older chat-load-older-end">Start of history</div>
        `}
        ${topLevel.map(msg => {
          const day = dayLabel(msgAt(msg));
          const showDay = day !== lastDay;
          const compact = !showDay && msg.from === lastFrom;
          lastFrom = msg.from;
          lastDay = day;

          const parentMsg = msg.reply_to ? msgCache[msg.reply_to] : null;
          const threadMsgs = replyMap[msg.id] || [];
          const threadOpen = !!expandedThreads.value[msg.id];

          const isAgentOnly = ch === 'general' && msg.from !== 'operator' && msg.type !== 'input'
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
