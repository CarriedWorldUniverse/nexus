// PresenceStrip.js — sticky per-thread roster of aspects with their
// current activity state. Operator's trust signal: green dot = aspect
// is online and idle; pulsing green = mid-deliberation; yellow = in a
// tool call. Lets the operator scan the strip and know the network is
// alive without reading any messages.
//
// Mounts inside FocusedThread / ExpandedThread (whichever owns the
// scroll container for the thread's messages). The strip itself uses
// CSS `position: sticky; top: 0` so it locks to the top of the
// thread's scroll area as the messages flow past underneath.
//
// Subscribes to observe.frame for each participant on mount via
// activity.acquireObserve, releases on unmount. The activity model
// refcounts subscriptions so multiple strips for overlapping
// participant sets share one server-side subscription per aspect.

const { html, useEffect } = window.__preact;
import { aspectActivity, acquireObserve } from '../models/activity.js';

export function PresenceStrip({ participants }) {
  // Acquire observe subscriptions for every participant. Re-run when
  // participants change (thread membership shifts as new senders post).
  // Capture the release fns in a closure so cleanup releases exactly
  // what this effect acquired.
  useEffect(() => {
    const releases = (participants || []).map((p) => acquireObserve(p));
    return () => {
      for (const release of releases) release();
    };
  }, [(participants || []).join('|')]);

  // Subscribe to the activity signal so re-renders fire when any
  // participant's state changes. Reading `.value` inside a Preact
  // component does this automatically per @preact/signals semantics.
  const activity = aspectActivity.value;

  if (!participants || participants.length === 0) {
    return html`<div class="presence-strip empty">No participants yet</div>`;
  }

  return html`
    <div class="presence-strip" role="status" aria-label="Thread participants and activity">
      ${participants.map((name) => {
        const a = activity[name] || { presence: 'offline', state: 'idle', tool: '' };
        const cls = `presence-pill state-${a.state} pres-${a.presence}`;
        // Tooltip text — full state + tool name so hovering reveals
        // detail without taking up strip real estate.
        let title = `${name}: ${a.presence}`;
        if (a.state === 'thinking') title += ' · thinking';
        if (a.state === 'tool') title += ` · ${a.tool || 'tool call'}`;
        return html`
          <span class=${cls} key=${name} title=${title}>
            <span class="presence-dot" aria-hidden="true"></span>
            <span class="presence-name">${name}</span>
            ${a.state === 'tool' && a.tool && html`<em class="presence-tool"> ${a.tool}</em>`}
          </span>
        `;
      })}
    </div>
  `;
}
