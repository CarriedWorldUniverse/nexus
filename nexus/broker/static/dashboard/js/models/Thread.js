// Thread.js — client-side first-class Thread primitive (NEX-246 Part 1).
//
// Why this exists: the SPA was lift-and-shifted from agent-network's
// aspect-first model. Every view fetches messages ad-hoc and filters by
// `selectedAgent` or scans a flat chronological list. Threads exist as
// visual structure (reply quote bars) but never as an addressable
// object. Under the peer-substrate model the operator works through
// threads — planner dispatches, worker reports back, the operator joins
// in — and the lack of a Thread primitive forces every view to
// reinvent thread bookkeeping locally.
//
// Thread is the missing primitive. It:
//   - loads the full subtree under a thread_root via chat.replies
//   - keeps an ordered messages[] (root + descendants, newest at tail)
//   - subscribes to live chat.deliver pushes via chat-ws, filtering by
//     thread_root client-side so only messages in THIS thread fire the
//     onChange callback
//   - computes participants and a heuristic role-hint cheaply
//   - exposes send() that posts with reply_to set correctly for the
//     thread
//
// Views consume the Thread instance via getOrCreateThread(rootId) from
// threads.js — registry-style so two views looking at the same thread
// share state and one subscription.
//
// What this does NOT do (yet, by design):
//   - No per-thread WS subscription on the broker side — Part 3 if
//     shadow picks up. v1 subscribes to all chat and filters locally.
//   - No reactions plumbing — Thread.messages[].reactions is populated
//     by the existing reaction-fetch path the views already use; Thread
//     doesn't own reaction state.
//   - No optimistic-send local echo — send() returns the Promise from
//     api.sendMessage and the message arrives back via the chat-ws
//     subscription like everything else. Local echo can layer on later
//     if the latency proves painful.
//   - No write-through to localStorage — Thread is in-memory only.
//     Refresh = re-load. Persistent UI state (which threads are open)
//     lives in app-level state, not in Thread.

import { fetchReplies, sendMessage } from '../api.js';
import { chatWS } from '../chat-ws.js';

const MENTION_RE = /@([a-zA-Z][\w-]*)/g;
const WORKER_RE = /@(anvil|plumb|harrow|wren|maren|forge)\b/i;
const WORKERS = ['anvil', 'plumb', 'harrow', 'wren', 'maren', 'forge'];

export class Thread {
  /**
   * @param {number} rootId thread_root_msg_id — the canonical thread
   *   identity. Must be > 0; zero/falsy throws.
   * @param {object} [seed] optional pre-known root message. When the
   *   caller already has the root in hand (e.g. they just received it
   *   via chat-ws), seeding here lets load() skip a fetch for the root
   *   itself. Shape: a normalised chat message ({id, from, content,
   *   reply_to, created_at}).
   */
  constructor(rootId, seed) {
    if (!rootId || rootId <= 0) {
      throw new Error('Thread: rootId required');
    }
    this.rootId = Number(rootId);
    this.messages = seed ? [seed] : [];
    this.loaded = false;
    this.loading = null; // Promise<void> while a load is in flight
    this._listeners = new Set();
    this._unsubChat = null;
  }

  /**
   * Load the full subtree under this thread root from the broker. Safe
   * to call multiple times — concurrent calls share the same in-flight
   * Promise; a second call after load completes is a no-op. Use
   * reload() to force a refetch.
   *
   * Resolves to the Thread itself for ergonomic chaining:
   *   const t = await new Thread(42).load();
   */
  load() {
    if (this.loaded) return Promise.resolve(this);
    if (this.loading) return this.loading;
    this.loading = fetchReplies(this.rootId).then((replies) => {
      // chat.replies returns the subtree WITHOUT the root. If we
      // weren't seeded with the root, we have to fetch it separately —
      // but for v1 we just live without it when no seed was given. The
      // caller (typically FeedView opening a thread the operator
      // clicked) almost always has the root in the open chat already
      // and seeds us. If they don't, messages[] starts at the first
      // reply, which is fine for the views' "show this thread" use
      // case; the root preview is the FeedView entry the operator
      // clicked through.
      //
      // Edge case: if there are no replies AND no seed, messages[] is
      // empty. Views render an empty thread; the operator sees the
      // empty state and the absence of the root is intentional
      // (degenerate thread of one message that has no actual descent).
      const ordered = replies.slice().sort(byId);
      // Merge with seed (if any). Replace duplicates by id to defend
      // against the seed root reappearing in replies.
      const map = new Map();
      for (const m of this.messages) map.set(m.id, m);
      for (const m of ordered) map.set(m.id, m);
      this.messages = Array.from(map.values()).sort(byId);
      this.loaded = true;
      this.loading = null;
      this._fire();
      return this;
    }).catch((err) => {
      this.loading = null;
      throw err;
    });
    return this.loading;
  }

  /**
   * Force a fresh fetch even if loaded. Used after long-disconnects
   * where the live subscription may have missed messages.
   */
  reload() {
    this.loaded = false;
    return this.load();
  }

  /**
   * Subscribe to live updates on this thread. Returns an unsubscribe
   * function. The first subscriber starts the underlying chat-ws
   * subscription; subsequent subscribers piggy-back. The last to
   * unsubscribe tears the chat-ws subscription down.
   *
   * `fn` is called with the Thread itself (NOT the new message) on
   * every change — load complete, new message arrived, etc. Subscribers
   * read .messages off the Thread rather than receiving a delta, so
   * they always see a consistent snapshot.
   *
   * @param {(t: Thread) => void} fn
   * @returns {() => void} unsubscribe
   */
  subscribe(fn) {
    this._listeners.add(fn);
    if (this._listeners.size === 1) this._wireChatWS();
    return () => {
      this._listeners.delete(fn);
      if (this._listeners.size === 0) this._unwireChatWS();
    };
  }

  /**
   * Post a message to this thread. Replies under the thread's root
   * (NOT under whatever the latest message in the thread is) — that
   * keeps thread_root_msg_id stable broker-side and matches how the
   * funnel-side and other clients address threads.
   *
   * Returns the api.sendMessage Promise. Resolves when the broker
   * acknowledges the chat.send frame; the actual delivery comes back
   * via the chat-ws subscription and lands in this.messages
   * automatically. Callers typically don't need to await — fire and
   * forget, the UI updates when the message round-trips.
   *
   * @param {string} content
   * @param {object} [opts]
   * @param {string} [opts.from] sender identity; defaults to 'operator'
   *   inside sendMessage. Callers should leave this unset for the
   *   normal operator-typing-in-chat path.
   * @param {string} [opts.topic] topic name (currently unused server-
   *   side; passed through for forward-compat).
   */
  send(content, opts = {}) {
    return sendMessage({
      from: opts.from,
      content,
      replyTo: this.rootId,
      topic: opts.topic || '',
    });
  }

  /**
   * Distinct senders + @-mentioned aspects in this thread, sorted by
   * first appearance. Computed lazily on every access — cheap given
   * typical thread sizes (< 100 msgs). If profiling shows this hot,
   * memoise on a version counter.
   *
   * Operator inclusion: operator IS a participant when they sent or
   * were @-mentioned. Under the peer model there's no asymmetric
   * treatment; @keel-cli, @operator, @harrow all count the same.
   */
  get participants() {
    const seen = new Set();
    const out = [];
    for (const m of this.messages) {
      if (m.from && !seen.has(m.from)) {
        seen.add(m.from);
        out.push(m.from);
      }
      if (typeof m.content === 'string') {
        // matchAll returns an iterator of full RegExp matches; spread
        // into an array for a single linear scan. Each match's [1] is
        // the captured name without the leading @.
        const matches = [...m.content.matchAll(MENTION_RE)];
        for (const match of matches) {
          const who = match[1];
          if (who && !seen.has(who) && who !== 'all') {
            seen.add(who);
            out.push(who);
          }
        }
      }
    }
    return out;
  }

  /**
   * Heuristic role hint for the thread. Coarse classification driven
   * by the work-routing policy vocabulary (chat #1011/#1014):
   *
   *   - 'planner-dispatch' : root message is a planner (shadow / keel)
   *     @-mentioning a worker. The thread is a dispatch.
   *   - 'worker-execution' : root message is a worker reporting in.
   *   - 'operator-drive'   : root message is from operator @-mentioning
   *     anyone.
   *   - 'casual'           : anything else (peer-to-peer chatter,
   *     ambient announcements).
   *
   * Heuristic, not authoritative — the broker doesn't tag threads with
   * a role today. Views use this for affordance hints (chip colour,
   * icon, sort order), never for correctness decisions.
   *
   * Returns '' when messages[] is empty.
   */
  get roleHint() {
    if (!this.messages.length) return '';
    const root = this.messages[0];
    const from = (root.from || '').toLowerCase();
    const content = root.content || '';
    const mentionsWorker = WORKER_RE.test(content);
    if (from === 'operator') return 'operator-drive';
    if (from === 'shadow' || from === 'keel' || from === 'keel-cli') {
      return mentionsWorker ? 'planner-dispatch' : 'casual';
    }
    if (WORKERS.includes(from)) {
      return 'worker-execution';
    }
    return 'casual';
  }

  // --- internal ---

  _wireChatWS() {
    if (this._unsubChat) return;
    // The chat-ws message.created event fires for every message in the
    // network. Filter by thread_root locally. Part 3 (broker-side
    // thread_root filter on subscribe.chat) would let us subscribe
    // narrowly, but the client-side filter is fast and works today.
    chatWS.start();
    this._unsubChat = chatWS.on('message.created', (ev) => {
      const m = ev && ev.msg;
      if (!m) return;
      // Match by thread_root if present; else fall back to direct-reply
      // chain rooted at our rootId. The broker stamps thread_root on
      // INSERT post-#228, so any new message has it; legacy rows may
      // not, and the reply_to walk catches those.
      const matches =
        Number(m.thread_root) === this.rootId ||
        Number(m.thread_root_msg_id) === this.rootId ||
        Number(m.reply_to) === this.rootId ||
        Number(m.id) === this.rootId;
      if (!matches) return;
      // Dedup against what we already have (load() + push race-
      // protection: if a push arrives mid-load, we may end up with the
      // same msg twice).
      if (this.messages.some((x) => x.id === m.id)) return;
      this.messages.push(m);
      this.messages.sort(byId);
      this._fire();
    });
  }

  _unwireChatWS() {
    if (this._unsubChat) {
      this._unsubChat();
      this._unsubChat = null;
    }
  }

  _fire() {
    // Snapshot listeners — a listener that unsubscribes inside its
    // callback (legitimate pattern for one-shot subscribes) would
    // mutate the Set mid-iteration otherwise.
    const snap = Array.from(this._listeners);
    for (const fn of snap) {
      try {
        fn(this);
      } catch (e) {
        // Swallow listener errors — one buggy view shouldn't break the
        // others. Console-log for debugging.
        // eslint-disable-next-line no-console
        console.error('Thread listener threw:', e);
      }
    }
  }
}

// byId — stable sort by numeric id ascending. Used to keep messages[]
// ordered as new pushes arrive out of order.
function byId(a, b) {
  return Number(a.id) - Number(b.id);
}
