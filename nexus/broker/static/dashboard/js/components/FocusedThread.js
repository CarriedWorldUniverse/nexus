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
//
// PR 5 additions: autoscroll-on-bottom + since-you-left divider +
// per-thread lastSeen persistence. The autoscroll target is the
// .feed-main scroll container (FeedView owns it, FocusedThread
// reaches it via closest()); the lastSeen baseline is captured
// once per focus so the divider doesn't slide as new messages
// arrive.

const { html, useState, useEffect, useRef, useMemo } = window.__preact;

import { peekThread } from '../models/threads.js';
import { replyTo } from '../state.js';
import { persistGet, persistSet } from '../util/persist.js';
import { MessageBubble } from './MessageBubble.js';
import { PresenceStrip } from './PresenceStrip.js';

// AT_BOTTOM_SLACK_PX is the tolerance for "scrolled to bottom" —
// browsers can leave a sub-pixel gap after auto-scroll on high-DPR
// displays, so an exact == 0 check would miss the case.
const AT_BOTTOM_SLACK_PX = 32;

export function FocusedThread({ rootId }) {
  const thread = peekThread(rootId);
  // version bump → rerender on Thread change. Cheap: Thread.subscribe
  // fires only when this thread's messages change.
  const [, setTick] = useState(0);

  // anchorRef sits inside FocusedThread; we walk up to find the
  // scroll container (.feed-main, owned by FeedView). Refs on
  // siblings break cleanly across remounts; closest() is cheap.
  const anchorRef = useRef(null);
  // Tracked via ref (not state) so per-scroll updates don't trigger
  // re-renders. Read inside the message-change effect to decide
  // whether to pin.
  const wasAtBottom = useRef(true);

  // Baseline lastSeen captured once per focus mount. The divider
  // renders above the first message with id > this value, and stays
  // put as new messages arrive (because baseline doesn't update —
  // only the persisted value does). Reading from LS so the divider
  // shows everything that landed since the operator last had this
  // thread focused, not just since this session started.
  const [baselineLastSeen] = useState(() => {
    const map = persistGet('feed', 'lastSeen', {});
    return Number(map[rootId]) || 0;
  });

  useEffect(() => {
    const t = peekThread(rootId);
    if (!t) return undefined;
    // Make sure the subtree is loaded (idempotent if already loaded).
    t.load().catch(() => {});
    const off = t.subscribe(() => setTick((n) => n + 1));
    return off;
  }, [rootId]);

  // Bind scroll listener to the .feed-main container. Re-bind when
  // rootId changes — a focus switch may have remounted FocusedThread
  // with a new anchor parent (the FeedView container is stable, but
  // re-binding on every rootId keeps the listener tied to whatever
  // we can reach right now).
  useEffect(() => {
    const anchor = anchorRef.current;
    if (!anchor) return undefined;
    const scrollEl = anchor.closest('.feed-main');
    if (!scrollEl) return undefined;
    function onScroll() {
      wasAtBottom.current =
        (scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight) < AT_BOTTOM_SLACK_PX;
    }
    // Seed the ref once so the very first render-triggered pin uses a
    // sane value before any scroll event has fired.
    onScroll();
    scrollEl.addEventListener('scroll', onScroll, { passive: true });
    return () => scrollEl.removeEventListener('scroll', onScroll);
  }, [rootId]);

  if (!thread) {
    return html`<div class="feed-thread-expanded-empty">Loading thread…</div>`;
  }

  const msgs = thread.messages;
  const root = msgs[0];
  const replies = msgs.slice(1);
  const stillLoading = !thread.loaded && replies.length === 0;

  // After every render that changes message count, pin to bottom if
  // the operator was at the bottom beforehand. requestAnimationFrame
  // so the browser settles new layout (MessageBubble heights etc.)
  // before we measure scrollHeight.
  useEffect(() => {
    if (!wasAtBottom.current) return;
    const anchor = anchorRef.current;
    if (!anchor) return;
    const scrollEl = anchor.closest('.feed-main');
    if (!scrollEl) return;
    requestAnimationFrame(() => {
      if (!scrollEl.isConnected) return;
      scrollEl.scrollTop = scrollEl.scrollHeight;
    });
  }, [msgs.length, rootId]);

  // Bump persisted lastSeen on every new message arrival while this
  // thread is focused. Uses the latest-known message id so a future
  // focus mount reads a fresh baseline. Does NOT update
  // baselineLastSeen — the divider stays anchored at the value
  // captured at mount so it doesn't slide.
  useEffect(() => {
    if (msgs.length === 0) return;
    const lastId = Number(msgs[msgs.length - 1].id) || 0;
    if (lastId <= 0) return;
    const map = persistGet('feed', 'lastSeen', {});
    if ((Number(map[rootId]) || 0) >= lastId) return;
    map[rootId] = lastId;
    persistSet('feed', 'lastSeen', map);
  }, [msgs.length, rootId]);

  // Index of the first reply whose id crosses the baseline — divider
  // sits above this message. -1 means "all replies are seen" (no
  // divider). Memoised to avoid recomputing during scroll-driven
  // renders.
  const dividerIndex = useMemo(() => {
    if (baselineLastSeen <= 0) return -1; // no prior visit, suppress divider
    for (let i = 0; i < replies.length; i++) {
      if (Number(replies[i].id) > baselineLastSeen) return i;
    }
    return -1;
  }, [replies.length, baselineLastSeen]);

  function setReplyTarget(e) {
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
    <div class="feed-thread-expanded" onClick=${(e) => e.stopPropagation()} ref=${anchorRef}>
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
              ${i === dividerIndex && html`
                <hr class="feed-since-divider" data-label="new since you left" key=${'div-' + rootId} />
              `}
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
