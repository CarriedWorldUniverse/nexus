// ThreadView.js — per-thread replies pane (NEX-246 Part 2).
//
// Pre-NEX-246: this view called fetchReplies(parentId) on mount and
// every 4 seconds, scraping the chat.replies endpoint to surface new
// replies. Worked, but had three problems:
//
//   1. 4s polling floor — replies could lag visibly even when the
//      operator and the aspect were both online and pushing.
//   2. Refetched the entire subtree every tick — bandwidth + broker
//      cost scaled with thread depth and tick rate.
//   3. Reinvented thread bookkeeping locally — no shared state with
//      anyone else looking at the same thread (e.g. FeedView's row
//      preview and ThreadView's expansion held duplicate copies).
//
// Now: consume the shared Thread primitive from models/Thread.js.
// load() runs once; live updates arrive via chat-ws push, filtered
// to this thread_root client-side. Other views opening the same
// thread share the instance via the threads.js registry.
//
// What changed visibly: nothing. Same MessageBubble rendering, same
// "Loading replies..." placeholder until the first load completes.
// Pure under-the-hood refactor.

import { MessageBubble } from './MessageBubble.js';
import { getOrCreateThread } from '../models/threads.js';

const { html, useState, useEffect } = window.__preact;

export function ThreadView({ parentId }) {
  // `replies` mirrors thread.messages but rendered through a Preact-
  // friendly state setter so re-renders fire when the Thread changes.
  // We don't store the Thread instance directly in component state —
  // it never identity-changes for a given parentId, so storing it
  // would trigger no re-renders even though its internal messages[]
  // mutates.
  const [replies, setReplies] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const thread = getOrCreateThread(parentId);

    // Bridge Thread.messages → component state. Called on subscribe
    // and on every Thread mutation (load complete, push received).
    const sync = (t) => {
      // Normalise to the shape MessageBubble expects. The shim layer
      // in api.js already maps received_at → created_at on the
      // chat.replies path, so most fields land correct; the
      // legacy-style `at` fallback is here for safety in case any
      // pre-Crossing rows surface without created_at populated.
      setReplies(t.messages.map((r) => ({
        id: r.id,
        from: r.from_agent || r.from,
        content: r.content,
        at: r.created_at || r.at,
        created_at: r.created_at || r.at,
        reply_to: r.reply_to,
        reply_count: r.reply_count || 0,
        reactions: r.reactions,
      })));
      setLoading(false);
    };

    // subscribe() returns the unsub function; first subscriber starts
    // the underlying chat-ws push wiring. Calling load() after
    // subscribe() means the load-complete event arrives via the
    // sync() callback rather than the load() promise — clean single
    // path for state propagation.
    const unsub = thread.subscribe(sync);
    thread.load().catch((err) => {
      // load failure → leave the loading placeholder up briefly, then
      // show empty state. Don't crash the bubble tree.
      // eslint-disable-next-line no-console
      console.error('[ThreadView] thread.load failed', err);
      setLoading(false);
    });

    // If the Thread was already loaded by another view (e.g. FeedView
    // opened it first), sync() won't have fired yet because subscribe
    // doesn't replay state. Sync explicitly so we render the existing
    // messages immediately rather than flashing the placeholder.
    if (thread.loaded) sync(thread);

    return () => {
      unsub();
    };
  }, [parentId]);

  if (loading) {
    return html`<div class="msg-thread" style="color:var(--text-muted);font-size:11px;padding:4px 0 4px 48px;">Loading replies...</div>`;
  }

  return html`
    <div class="msg-thread">
      ${replies.map((r) => html`
        <${MessageBubble} key=${r.id} msg=${r} compact=${false} />
      `)}
    </div>
  `;
}
