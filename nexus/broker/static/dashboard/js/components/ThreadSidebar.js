// ThreadSidebar.js — left-rail thread list. The network glance
// surface: each row shows a thread's preview and an activity-dot
// strip (one dot per participant aspect), with focused-state
// highlighting on the currently-open thread. Operator scans the
// dot column to know "the system is alive" without opening any
// individual thread.
//
// The sidebar IS the trust surface at the cross-thread level;
// PresenceStrip is the same idea at the per-thread level. They
// share data via the aspectActivity signal (models/activity.js),
// so the dots animate live as agents start/stop turns regardless
// of which thread is focused.
//
// Keyboard nav is owned by FeedView (the container); this component
// just dispatches focus changes via onFocus(rootId).

const { html } = window.__preact;
import { aspectActivity } from '../models/activity.js';

// dotStateFor returns the CSS state class for a participant in a
// thread row. Lifted out so the dot rendering can stay terse.
function dotStateFor(activity, name) {
  const a = activity[name];
  if (!a) return 'state-idle pres-offline';
  return `state-${a.state} pres-${a.presence}`;
}

export function ThreadSidebar({ threads, focusedRootId, onFocus }) {
  // Subscribe to the activity signal so the dots animate when
  // anyone's state changes. Per @preact/signals semantics, reading
  // .value inside a component sets up the subscription.
  const activity = aspectActivity.value;

  if (!threads || threads.length === 0) {
    return html`
      <aside class="feed-sidebar" aria-label="Threads">
        <div class="feed-sidebar-empty">No active threads.</div>
      </aside>
    `;
  }

  return html`
    <aside class="feed-sidebar" aria-label="Threads">
      ${threads.map((t) => {
        const isFocused = focusedRootId === t.rootId;
        const cls = 'feed-sidebar-row' + (isFocused ? ' is-focused' : '');
        return html`
          <div
            key=${t.rootId}
            class=${cls}
            role="button"
            tabIndex=${0}
            aria-pressed=${isFocused}
            onClick=${() => onFocus(t.rootId)}
            onKeyDown=${(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onFocus(t.rootId);
              }
            }}
          >
            <div class="feed-sidebar-row-body">
              <div class="feed-sidebar-from">${t.from || '(unknown)'}</div>
              <div class="feed-sidebar-preview">${t.preview || '(empty)'}</div>
            </div>
            <div class="feed-sidebar-dots" aria-hidden="true">
              ${(t.participants || []).map((p) => html`
                <span
                  key=${p}
                  class=${'feed-sidebar-dot ' + dotStateFor(activity, p)}
                  title=${p}
                ></span>
              `)}
            </div>
          </div>
        `;
      })}
    </aside>
  `;
}
