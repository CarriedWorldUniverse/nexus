// threads.js — Thread registry (NEX-246 Part 1).
//
// Singleton registry so two views looking at the same thread share one
// Thread instance + one chat-ws subscription. Without this, each view
// would create its own Thread on mount, doubling the bookkeeping and
// firing duplicate WS events.
//
// Usage:
//
//   import { getOrCreateThread, listOpenThreads } from './models/threads.js';
//
//   // open a thread (e.g. on a row click in FeedView)
//   const t = getOrCreateThread(msgId, optionalSeed);
//   await t.load();
//   const off = t.subscribe(() => render(t));
//
//   // somewhere else (Chat.js currently-open thread)
//   const same = getOrCreateThread(msgId);  // returns the same instance
//
// Eviction: not implemented yet — Threads stay in memory for the
// session. v2 may LRU-evict cold threads if memory becomes an issue,
// but typical operator session has < 50 active threads × small msg
// counts, so it's premature.

import { Thread } from './Thread.js';

const registry = new Map(); // rootId → Thread

/**
 * Return the Thread for `rootId`, creating it on first request. If the
 * caller has the root message in hand, pass it as `seed` so the new
 * Thread doesn't have to fetch the root itself.
 *
 * Subsequent calls with the same rootId return the same Thread
 * instance — state and subscribers are shared.
 *
 * @param {number} rootId thread_root_msg_id
 * @param {object} [seed] optional pre-known root message
 * @returns {Thread}
 */
export function getOrCreateThread(rootId, seed) {
  const id = Number(rootId);
  if (!id || id <= 0) {
    throw new Error('getOrCreateThread: rootId required');
  }
  let t = registry.get(id);
  if (!t) {
    t = new Thread(id, seed);
    registry.set(id, t);
  } else if (seed && !t.messages.some((m) => m.id === seed.id)) {
    // Late seed: caller has the root but the Thread doesn't yet.
    // Insert it without triggering a load — load() will merge.
    t.messages.unshift(seed);
    t.messages.sort((a, b) => Number(a.id) - Number(b.id));
  }
  return t;
}

/**
 * Return the currently-registered Thread for `rootId`, or undefined if
 * none has been created. Use this when a view wants to check whether a
 * thread is already open without forcing instantiation.
 */
export function peekThread(rootId) {
  return registry.get(Number(rootId));
}

/**
 * Snapshot of every Thread currently registered. Used by views that
 * render "list of open threads" (e.g. FeedView refactor in Part 4a).
 * Not a live view — call again after registry mutations.
 *
 * @returns {Thread[]}
 */
export function listOpenThreads() {
  return Array.from(registry.values());
}

/**
 * Drop a Thread from the registry. Caller should unsubscribe their
 * listeners first; the registry doesn't track them. Existing in-flight
 * listeners on the dropped Thread will still fire until they
 * unsubscribe — eviction doesn't kill subscriptions.
 *
 * Mainly here for tests + future LRU eviction. Production code
 * generally doesn't need to evict.
 */
export function dropThread(rootId) {
  registry.delete(Number(rootId));
}

/**
 * Clear all Threads. Tests only — never call in production.
 */
export function _resetForTests() {
  registry.clear();
}
