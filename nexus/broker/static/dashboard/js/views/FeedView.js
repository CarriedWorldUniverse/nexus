// FeedView.js — thread-centric feed (NEX-246 Part 4a).
//
// Pre-NEX-246 shape: a flat firehose of #general messages with a
// sender-filter dropdown (agent chips) and inline expandable replies.
// The unit of attention was the message, and the only "where am I in
// the network" lens was "who sent this?". That model came from
// agent-network where aspects were the substrate; under the peer
// substrate the substrate IS the thread.
//
// Now: each row is a Thread. The operator scans for active threads,
// filters by role hint or "mentions me", clicks through to ChatView
// for the full conversation. Live updates land via Thread.subscribe —
// no local thread bookkeeping, no replyMap, no msgCache.
//
// What's preserved:
//   - Route mount points (#/feed, #/chat, default) keep working.
//   - WS-driven liveness — new top-level messages prepend, replies
//     re-render their row.
//   - Channel scope: #general only (matches the legacy behaviour).
//   - ChatInput at the bottom so the operator can still post a new
//     top-level message without leaving the feed.
//
// What's replaced:
//   - Agent-filter dropdown → role-hint filter + mentions-me toggle.
//   - Flat message list → thread row list with participants, role
//     chip, reply count, last-activity stamp.
//   - Inline reply expansion (ThreadView nested under MessageBubble)
//     → click-through to ChatView via #/chat?thread=<rootId>.
//     Chat.js doesn't yet consume the ?thread= query (Part 4b/4c);
//     this PR sets the affordance, the consumer lands later.
//   - Local replyMap / msgCache → Thread instances from the registry.
//     Each thread owns its own state.

import { fetchMessages } from '../api.js';
import { chatWS } from '../chat-ws.js';
import { getOrCreateThread, peekThread } from '../models/threads.js';
import { agentColors } from '../state.js';
import { ChatInput } from '../components/ChatInput.js';

const { html, useState, useEffect } = window.__preact;

const ROLE_LABELS = {
  '':                  'All',
  'planner-dispatch':  'Planner dispatch',
  'worker-execution':  'Worker execution',
  'operator-drive':    'Operator drive',
  'casual':            'Casual',
};

// Order the chip row deterministically — All on the left, then the
// canonical role progression (planner → worker → operator → casual).
const ROLE_ORDER = ['', 'planner-dispatch', 'worker-execution', 'operator-drive', 'casual'];

// Match Thread.roleHint's mention vocabulary. The operator is a
// participant when they sent, were @-mentioned, or the message
// @-mentions @all. Thread.participants already folds @-mentions in;
// this helper additionally treats @all as an operator mention since
// @all is "everyone including the operator".
function threadMentionsOperator(thread) {
  const parts = thread.participants;
  if (parts.includes('operator')) return true;
  for (const m of thread.messages) {
    const c = (m.content || '').toLowerCase();
    if (c.includes('@all')) return true;
  }
  return false;
}

// Prefer thread_root / thread_root_msg_id when the broker stamps it;
// fall back to reply_to chain head (which for a top-level message is
// just the message id). Mirrors the matching logic in Thread._wireChatWS.
function rootIdOf(msg) {
  if (!msg) return 0;
  const r =
    Number(msg.thread_root) ||
    Number(msg.thread_root_msg_id) ||
    Number(msg.reply_to) ||
    Number(msg.id);
  return r > 0 ? r : 0;
}

// msgAt — same tolerant timestamp read as the rest of the SPA. The
// broker emits created_at; older paths used received_at / `at`.
function msgAt(msg) {
  return (msg && (msg.created_at || msg.received_at || msg.at)) || '';
}

// Truncate the root preview to ~120 chars for the row. Strips leading
// whitespace and collapses runs of newlines so the preview stays on
// one visual line even when the source has hard breaks.
function previewOf(content) {
  if (!content) return '';
  const flat = String(content).replace(/\s+/g, ' ').trim();
  if (flat.length <= 120) return flat;
  return flat.slice(0, 117) + '...';
}

// Relative-time pretty-print, hour-floor. Mirrors the dashboard's
// existing time vocabulary loosely — exact format isn't load-bearing,
// just needs to be glanceable.
function relTime(dateStr) {
  if (!dateStr) return '';
  const isISO = /Z$|[+-]\d\d:?\d\d$/.test(dateStr);
  const d = new Date(isISO ? dateStr : dateStr + 'Z');
  if (isNaN(d.getTime())) return '';
  const diffMs = Date.now() - d.getTime();
  const sec = Math.floor(diffMs / 1000);
  if (sec < 45) return 'just now';
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  if (day < 7) return `${day}d ago`;
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

// Navigate to the thread in ChatView. Chat.js doesn't yet read the
// ?thread= query (Part 4b/4c will wire that), but setting the hash
// here means the affordance is correct from this PR's side; the
// receiver lands later. Today the click effectively returns the
// operator to the chat firehose — acceptable degraded behaviour.
function openThread(rootId) {
  window.location.hash = `#/chat?thread=${rootId}`;
}

// Pull a row state shape off a Thread. Computed once per render per
// thread; cheap given participants/roleHint are O(messages).
function snapshotThread(thread) {
  const msgs = thread.messages;
  const root = msgs[0];
  const last = msgs[msgs.length - 1] || root;
  return {
    rootId: thread.rootId,
    preview: previewOf(root && root.content),
    from: (root && (root.from_agent || root.from)) || '',
    participants: thread.participants,
    roleHint: thread.roleHint,
    replyCount: Math.max(0, msgs.length - 1),
    lastAt: msgAt(last),
    lastSortKey: (last && Number(last.id)) || thread.rootId,
  };
}

export function FeedView() {
  // Map of rootId → snapshot. Using an object keyed by rootId so
  // Thread.subscribe callbacks can patch a single row without
  // rebuilding the full list.
  const [rows, setRows] = useState({});
  const [roleFilter, setRoleFilter] = useState('');
  const [mentionsMe, setMentionsMe] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    const unsubs = new Map(); // rootId → unsubscribe fn

    // Subscribe a Thread into the rows map. Re-subscribing the same
    // rootId is a no-op (idempotent on the registry side, and we
    // skip if we've already wired it).
    function adopt(thread, seedSnapshot) {
      if (unsubs.has(thread.rootId)) {
        if (seedSnapshot) {
          setRows((cur) => ({ ...cur, [thread.rootId]: seedSnapshot }));
        }
        return;
      }
      const sync = (t) => {
        if (cancelled) return;
        setRows((cur) => ({ ...cur, [t.rootId]: snapshotThread(t) }));
      };
      unsubs.set(thread.rootId, thread.subscribe(sync));
      // Seed immediately so the row renders before load() resolves.
      if (seedSnapshot) {
        setRows((cur) => ({ ...cur, [thread.rootId]: seedSnapshot }));
      } else {
        sync(thread);
      }
      // Lazy load the rest of the subtree so reply_count + participants
      // reflect the full thread, not just the seed root.
      thread.load().catch((err) => {
        // eslint-disable-next-line no-console
        console.error('[FeedView] thread.load failed', thread.rootId, err);
      });
    }

    // Initial hydration: pull a recent page of #general, group by
    // thread root, create/seed Threads for each.
    (async () => {
      try {
        const result = await fetchMessages('general', 0);
        const msgs = Array.isArray(result) ? result : (result && result.messages) || [];
        // Group by root. Earliest-known msg in each group becomes the
        // seed if the root id itself isn't in the page.
        const rootSeeds = new Map();
        for (const m of msgs) {
          const root = rootIdOf(m);
          if (!root) continue;
          if (m.id === root) {
            rootSeeds.set(root, m);
          } else if (!rootSeeds.has(root)) {
            // Placeholder seed; will be replaced if the real root shows
            // up later in the same page.
            rootSeeds.set(root, null);
          }
        }
        // Second pass: if we have the actual root msg, prefer it over
        // null placeholder.
        for (const m of msgs) {
          const root = rootIdOf(m);
          if (m.id === root) rootSeeds.set(root, m);
        }
        if (cancelled) return;
        for (const [rootId, seed] of rootSeeds.entries()) {
          const thread = getOrCreateThread(rootId, seed || undefined);
          adopt(thread, snapshotThread(thread));
        }
        setLoading(false);
      } catch (e) {
        // eslint-disable-next-line no-console
        console.error('[FeedView] initial load failed', e);
        setLoading(false);
      }
    })();

    // Live: every new chat message either belongs to an existing
    // thread (Thread.subscribe handles it) or kicks off a new one
    // (we adopt it here). Topic-filter to #general to match the
    // legacy scope.
    chatWS.start();
    const offMsg = chatWS.on('message.created', (ev) => {
      const m = ev && ev.msg;
      if (!m || typeof m.id !== 'number') return;
      if (m.topic && m.topic !== 'general') return;
      const root = rootIdOf(m);
      if (!root) return;
      const existing = peekThread(root);
      if (existing) {
        // Thread's own chat-ws subscription will push this into its
        // messages[] and fire its listeners; nothing for us to do.
        return;
      }
      // Brand new thread — only adopt top-level messages (root === id).
      // Reply-into-unknown-root is rare and the parent will surface
      // on next poll/reconnect; skipping it avoids creating Threads
      // that lack their root preview.
      if (m.id !== root) return;
      const thread = getOrCreateThread(root, m);
      adopt(thread, snapshotThread(thread));
    });

    const offReconnect = chatWS.on('reconnect', () => {
      // Refetch the recent page so anything we missed during the
      // disconnect window surfaces. Existing Threads pick up their
      // own replies via reload-on-subscribe semantics in Thread.
      fetchMessages('general', 0).then((result) => {
        const msgs = Array.isArray(result) ? result : (result && result.messages) || [];
        for (const m of msgs) {
          const root = rootIdOf(m);
          if (!root || m.id !== root) continue;
          if (peekThread(root)) continue;
          const thread = getOrCreateThread(root, m);
          adopt(thread, snapshotThread(thread));
        }
      }).catch(() => {});
    });

    return () => {
      cancelled = true;
      offMsg();
      offReconnect();
      for (const off of unsubs.values()) off();
    };
  }, []);

  // Render: rows → array, filter, sort by last activity desc.
  const all = Object.values(rows);
  const filtered = all.filter((r) => {
    if (roleFilter && r.roleHint !== roleFilter) return false;
    if (mentionsMe) {
      // Re-derive from the live Thread (participants list lives on
      // the Thread, not the snapshot, to keep snapshots cheap).
      const t = peekThread(r.rootId);
      if (!t || !threadMentionsOperator(t)) return false;
    }
    return true;
  });
  filtered.sort((a, b) => b.lastSortKey - a.lastSortKey);

  return html`
    <div class="chat-view feed-thread-view">
      <div class="feed-thread-filters">
        <div class="feed-thread-filter-group">
          ${ROLE_ORDER.map((role) => html`
            <button
              key=${role || 'all'}
              class=${'feed-filter-btn' + (roleFilter === role ? ' active' : '')}
              onClick=${() => setRoleFilter(role)}
            >${ROLE_LABELS[role]}</button>
          `)}
        </div>
        <label class="feed-thread-mentions">
          <input
            type="checkbox"
            checked=${mentionsMe}
            onChange=${(e) => setMentionsMe(e.target.checked)}
          />
          <span>Mentions me</span>
        </label>
      </div>

      <div class="chat-messages feed-thread-list">
        ${loading && !filtered.length && html`
          <div class="feed-thread-empty">Loading threads...</div>
        `}
        ${!loading && !filtered.length && html`
          <div class="feed-thread-empty">No threads match.</div>
        `}
        ${filtered.map((r) => {
          const fromColor = (agentColors.value || {})[r.from] || '#888';
          return html`
            <div
              key=${r.rootId}
              class="feed-thread-row"
              onClick=${() => openThread(r.rootId)}
              role="button"
              tabIndex=${0}
              onKeyDown=${(e) => { if (e.key === 'Enter' || e.key === ' ') openThread(r.rootId); }}
            >
              <div class="feed-thread-row-head">
                <span class="feed-thread-from" style=${{ '--agent-color': fromColor }}>
                  ${r.from || '(unknown)'}
                </span>
                ${r.roleHint && html`
                  <span class=${'feed-thread-role feed-thread-role-' + r.roleHint}>
                    ${ROLE_LABELS[r.roleHint] || r.roleHint}
                  </span>
                `}
                <span class="feed-thread-time">${relTime(r.lastAt)}</span>
              </div>
              <div class="feed-thread-preview">${r.preview || '(empty)'}</div>
              <div class="feed-thread-row-foot">
                <span class="feed-thread-participants">
                  ${r.participants.length
                    ? r.participants.map((p) => `@${p}`).join(' ')
                    : '(no participants)'}
                </span>
                <span class="feed-thread-replies">
                  ${r.replyCount === 0 ? 'no replies' :
                    r.replyCount === 1 ? '1 reply' :
                    `${r.replyCount} replies`}
                </span>
              </div>
            </div>
          `;
        })}
      </div>
      <${ChatInput} />
    </div>
  `;
}
