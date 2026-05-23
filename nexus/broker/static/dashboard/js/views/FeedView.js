// FeedView.js — sidebar + focused-thread layout (NEX-246 Part 5,
// docs/2026-05-23-feed-trust-surface-spec.md).
//
// Pre-spec shape: single-thread accordion with role-hint / mentions-me
// filters above a flat row list. Operator could only see one
// conversation at a time and the filter chips were sender-based
// (planner / worker / operator / casual) — meaningless for "what's
// being discussed."
//
// Now: left-rail sidebar lists active threads with per-participant
// activity dots, main area renders one focused thread at a time
// with sticky presence strip + scrollable messages + composer.
// Operator scans the sidebar's dot column to know which agents are
// alive and working across the whole network without having to read
// any messages; opens a thread when they need detail.
//
// What's preserved:
//   - Hydration: pull a recent page of #general, group by thread
//     root, create Threads from the registry.
//   - Live WS: new top-level messages adopt a fresh Thread; existing
//     threads update via their own Thread.subscribe.
//   - Reconnect refetch: missed messages during disconnect surface
//     on next chat-ws reconnect.
//   - URL hash deep-link: #/feed?thread=N focuses that thread on
//     mount and on external hashchange.
//
// What's gone:
//   - Role-hint chips + mentions-me filter (sender-based, meaningless
//     per spec; thread IS the topic).
//   - Single-thread accordion + toggleExpand (replaced by focus model).
//   - ExpandedThread inline component (extracted to
//     components/FocusedThread.js).

import { fetchMessages } from '../api.js';
import { chatWS } from '../chat-ws.js';
import { getOrCreateThread, peekThread } from '../models/threads.js';
import { aspectActivity } from '../models/activity.js';
import { ChatInput } from '../components/ChatInput.js';
import { ThreadSidebar } from '../components/ThreadSidebar.js';
import { FocusedThread } from '../components/FocusedThread.js';
import { persistGet, persistSet } from '../util/persist.js';

const { html, useState, useEffect, useMemo } = window.__preact;

// Threads with no activity for this long don't appear in the sidebar.
// Tunable via spec decision (7d picked as "long enough to catch
// week-long pauses, short enough to keep the list scan-friendly").
const THREAD_AGING_MS = 7 * 24 * 60 * 60 * 1000;

// Prefer thread_root / thread_root_msg_id when the broker stamps it;
// fall back to reply_to chain head (which for a top-level message is
// just the message id). Mirrors the matching logic in
// Thread._wireChatWS.
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

// Truncate the root preview to ~120 chars for the sidebar row.
function previewOf(content) {
  if (!content) return '';
  const flat = String(content).replace(/\s+/g, ' ').trim();
  if (flat.length <= 120) return flat;
  return flat.slice(0, 117) + '...';
}

// Hash plumbing — deep-link to a focused thread via
// #/feed?thread=<rootId> or #/chat?thread=<rootId>. setThreadInHash
// uses replaceState so the back button doesn't fill with focus-switch
// toggles.
function parseThreadFromHash() {
  const hash = window.location.hash || '';
  const q = hash.indexOf('?');
  if (q === -1) return 0;
  const params = new URLSearchParams(hash.slice(q + 1));
  const n = Number(params.get('thread'));
  return Number.isFinite(n) && n > 0 ? n : 0;
}

function setThreadInHash(rootId) {
  const hash = window.location.hash || '#/feed';
  const base = hash.split('?')[0] || '#/feed';
  const next = rootId > 0 ? `${base}?thread=${rootId}` : base;
  if (window.location.hash !== next) {
    history.replaceState(null, '', next);
  }
}

// Snapshot a Thread into the shape ThreadSidebar consumes. Cheap
// (participants and lastSortKey are already maintained on the
// Thread); recomputed on every Thread.subscribe fire.
function snapshotThread(thread) {
  const msgs = thread.messages;
  const root = msgs[0];
  const last = msgs[msgs.length - 1] || root;
  return {
    rootId: thread.rootId,
    preview: previewOf(root && root.content),
    from: (root && (root.from_agent || root.from)) || '',
    participants: thread.participants,
    lastAt: msgAt(last),
    lastSortKey: (last && Number(last.id)) || thread.rootId,
  };
}

export function FeedView() {
  // Map of rootId → snapshot. Object-keyed so Thread.subscribe
  // callbacks can patch a single entry without rebuilding the full
  // list.
  const [rows, setRows] = useState({});
  const [loading, setLoading] = useState(true);
  // Currently-focused thread rootId (0 = none — empty-state in main
  // area). Initial value priority: URL hash (deep-link from another
  // view) → localStorage (last focused before reload) → 0 (auto-focus
  // most-active thread on first hydration kicks in via the effect
  // below).
  const [focusedRoot, setFocusedRoot] = useState(() => {
    return parseThreadFromHash() || persistGet('feed', 'focusedThread', 0);
  });

  // Drive focus changes from a single helper so URL-hash + replyTo
  // teardown stay paired. Switching focus also kicks the new thread
  // into load() — idempotent on the Thread registry, but ensures the
  // FocusedThread component sees a populated thread on first render.
  // Persists to localStorage so a reload lands the operator back on
  // the same thread (URL hash still wins if present).
  function focusThread(rootId) {
    if (rootId === focusedRoot) return;
    setFocusedRoot(rootId);
    setThreadInHash(rootId);
    persistSet('feed', 'focusedThread', rootId);
    if (rootId > 0) {
      const t = peekThread(rootId) || getOrCreateThread(rootId);
      t.load().catch(() => {});
    }
  }

  // External hash change (operator pastes a deep-link, or another
  // view sets #/feed?thread=N) → reflect into focus.
  useEffect(() => {
    function onHashChange() {
      const next = parseThreadFromHash();
      if (next === focusedRoot) return;
      setFocusedRoot(next);
      if (next > 0) {
        const t = peekThread(next) || getOrCreateThread(next);
        t.load().catch(() => {});
      }
    }
    window.addEventListener('hashchange', onHashChange);
    return () => window.removeEventListener('hashchange', onHashChange);
  }, [focusedRoot]);

  // Initial hydration + live updates + reconnect refetch.
  useEffect(() => {
    let cancelled = false;
    const unsubs = new Map(); // rootId → unsubscribe fn

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
      if (seedSnapshot) {
        setRows((cur) => ({ ...cur, [thread.rootId]: seedSnapshot }));
      } else {
        sync(thread);
      }
      thread.load().catch((err) => {
        // eslint-disable-next-line no-console
        console.error('[FeedView] thread.load failed', thread.rootId, err);
      });
    }

    (async () => {
      try {
        const result = await fetchMessages('general', 0);
        const msgs = Array.isArray(result) ? result : (result && result.messages) || [];
        const rootSeeds = new Map();
        for (const m of msgs) {
          const root = rootIdOf(m);
          if (!root) continue;
          if (m.id === root) {
            rootSeeds.set(root, m);
          } else if (!rootSeeds.has(root)) {
            rootSeeds.set(root, null);
          }
        }
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

    chatWS.start();
    const offMsg = chatWS.on('message.created', (ev) => {
      const m = ev && ev.msg;
      if (!m || typeof m.id !== 'number') return;
      if (m.topic && m.topic !== 'general') return;
      const root = rootIdOf(m);
      if (!root) return;
      if (peekThread(root)) return; // existing thread handles its own update
      if (m.id !== root) return;    // skip orphan replies into unknown roots
      const thread = getOrCreateThread(root, m);
      adopt(thread, snapshotThread(thread));
    });

    const offReconnect = chatWS.on('reconnect', () => {
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

  // Derive the sorted, aged thread list. Reads aspectActivity so the
  // "any participant thinking?" sort key recomputes when activity
  // changes — operator's eye follows the live agents to the top.
  const activity = aspectActivity.value;
  const sortedThreads = useMemo(() => {
    const now = Date.now();
    const all = Object.values(rows);
    return all
      .filter((r) => {
        const last = new Date(r.lastAt).getTime();
        // Threads with unparseable / missing timestamps fall through
        // as "old enough to hide" rather than crash the sort.
        return Number.isFinite(last) && (now - last) < THREAD_AGING_MS;
      })
      .sort((a, b) => {
        // Threads where at least one aspect is currently thinking or
        // mid-tool sort above idle threads. Ties broken by last
        // activity (most recent first) so live threads cluster at
        // the top of the rail.
        const aActive = (a.participants || []).some((p) => {
          const ai = activity[p];
          return ai && ai.state !== 'idle';
        });
        const bActive = (b.participants || []).some((p) => {
          const bi = activity[p];
          return bi && bi.state !== 'idle';
        });
        if (aActive !== bActive) return aActive ? -1 : 1;
        return b.lastSortKey - a.lastSortKey;
      });
  }, [rows, activity]);

  // Auto-focus the most-active thread on first hydration so the
  // operator never sees an empty main area when threads exist. Only
  // fires while focusedRoot is still 0 — operator's explicit choice
  // beats auto-focus.
  useEffect(() => {
    if (focusedRoot > 0) return;
    if (sortedThreads.length === 0) return;
    focusThread(sortedThreads[0].rootId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sortedThreads.length, focusedRoot]);

  // Keyboard nav — j/k or ArrowDown/ArrowUp move focus to the
  // adjacent sidebar row. Skips when focus is in a text input so
  // composer typing isn't hijacked.
  useEffect(() => {
    function onKey(e) {
      const tag = (document.activeElement && document.activeElement.tagName) || '';
      if (tag === 'TEXTAREA' || tag === 'INPUT') return;
      let dir = 0;
      if (e.key === 'ArrowDown' || e.key === 'j') dir = 1;
      else if (e.key === 'ArrowUp' || e.key === 'k') dir = -1;
      else return;
      if (sortedThreads.length === 0) return;
      e.preventDefault();
      const idx = sortedThreads.findIndex((t) => t.rootId === focusedRoot);
      // If nothing was focused, ArrowDown picks the first, ArrowUp the last.
      let nextIdx;
      if (idx < 0) {
        nextIdx = dir > 0 ? 0 : sortedThreads.length - 1;
      } else {
        nextIdx = (idx + dir + sortedThreads.length) % sortedThreads.length;
      }
      focusThread(sortedThreads[nextIdx].rootId);
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
    // focusThread closes over focusedRoot; re-bind when either changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sortedThreads, focusedRoot]);

  return html`
    <div class="chat-view feed-trust-view">
      <${ThreadSidebar}
        threads=${sortedThreads}
        focusedRootId=${focusedRoot}
        onFocus=${focusThread}
      />
      <main class="feed-main">
        ${loading && sortedThreads.length === 0 && html`
          <div class="feed-main-empty">Loading threads…</div>
        `}
        ${!loading && sortedThreads.length === 0 && html`
          <div class="feed-main-empty">No active threads in the last 7 days.</div>
        `}
        ${focusedRoot > 0 && html`<${FocusedThread} rootId=${focusedRoot} />`}
        ${focusedRoot === 0 && sortedThreads.length > 0 && html`
          <div class="feed-main-empty">Pick a thread on the left.</div>
        `}
      </main>
      <${ChatInput} />
    </div>
  `;
}
