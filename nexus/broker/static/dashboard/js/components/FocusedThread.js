// FocusedThread.js — renders a single thread as the operator's main
// view target. Owns its own Thread subscription so live messages
// re-render without forcing the parent FeedView to redraw every row,
// and mounts the sticky PresenceStrip at the top so the operator's
// trust signal stays visible while scrolling the message history.
//
// Was inline as ExpandedThread inside FeedView.js prior to the
// sidebar-layout PR (#119); extracted here so the sidebar layout's
// FeedView can stay focused on orchestration (sidebar + composer +
// main-area routing) rather than carrying ~90 lines of
// per-thread render logic.

const { html, useState, useEffect } = window.__preact;

import { peekThread } from '../models/threads.js';
import { replyTo } from '../state.js';
import { MessageBubble } from './MessageBubble.js';
import { PresenceStrip } from './PresenceStrip.js';

export function FocusedThread({ rootId }) {
  const thread = peekThread(rootId);
  // version bump → rerender on Thread change. Cheap: Thread.subscribe
  // fires only when this thread's messages change.
  const [, setTick] = useState(0);

  useEffect(() => {
    const t = peekThread(rootId);
    if (!t) return undefined;
    // Make sure the subtree is loaded (idempotent if already loaded).
    t.load().catch(() => {});
    const off = t.subscribe(() => setTick((n) => n + 1));
    return off;
  }, [rootId]);

  if (!thread) {
    return html`<div class="feed-thread-expanded-empty">Loading thread…</div>`;
  }

  const msgs = thread.messages;
  const root = msgs[0];
  const replies = msgs.slice(1);
  const stillLoading = !thread.loaded && replies.length === 0;

  function setReplyTarget(e) {
    // Stop the click from bubbling to the row (which would collapse it).
    e.stopPropagation();
    replyTo.value = root;
  }

  function clearReplyTarget(e) {
    e.stopPropagation();
    if (replyTo.value && replyTo.value.id === rootId) {
      replyTo.value = null;
    }
  }

  return html`
    <div class="feed-thread-expanded" onClick=${(e) => e.stopPropagation()}>
      <${PresenceStrip} participants=${thread.participants} />
      ${root && html`
        <div class="feed-thread-expanded-root">
          <${MessageBubble} msg=${root} />
        </div>
      `}
      ${replies.length > 0 && html`
        <div class="feed-thread-expanded-replies">
          ${replies.map((m, i) => {
            const parent = (m.reply_to && msgs.find((p) => p.id === m.reply_to)) || null;
            const compact = i > 0 && replies[i - 1].from === m.from;
            return html`
              <${MessageBubble}
                key=${m.id}
                msg=${m}
                parentMsg=${parent}
                compact=${compact}
              />
            `;
          })}
        </div>
      `}
      ${stillLoading && html`
        <div class="feed-thread-expanded-loading">Loading replies…</div>
      `}
      ${!stillLoading && replies.length === 0 && html`
        <div class="feed-thread-expanded-noreplies">No replies yet — be the first.</div>
      `}
      <div class="feed-thread-expanded-composer">
        ${replyTo.value && replyTo.value.id === rootId
          ? html`
            <div class="feed-thread-replying">
              <span>Replying in thread…</span>
              <button
                type="button"
                class="feed-thread-replying-cancel"
                onClick=${clearReplyTarget}
              >Cancel</button>
            </div>
          `
          : html`
            <button
              type="button"
              class="feed-thread-reply-cta"
              onClick=${setReplyTarget}
            >Reply in thread…</button>
          `}
      </div>
    </div>
  `;
}
