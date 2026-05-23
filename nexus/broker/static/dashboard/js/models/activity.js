// activity.js — module-scoped per-aspect activity state derived from
// broker observability frames (observe.frame push, see
// nexus/observability/types.go for the wire shape).
//
// Surfaces "trust signal" data for the dashboard:
//
//   - presence: 'online' | 'offline'  (Connected from PresenceFrame)
//   - state:    'idle' | 'thinking' | 'tool'
//                  idle     — no in-flight turn known
//                  thinking — TurnFrame Status=in_flight, no tool open
//                  tool     — last TurnEvent is a tool_call with Result==nil
//   - tool:     name of the tool when state==='tool', else ''
//
// Consumers (PresenceStrip today; sidebar dots in PR 4) read from the
// `aspectActivity` signal and re-render via signal subscription.
//
// Subscription model: this module owns refcounted `subscribe.observe`
// per-aspect via comms.subscribe. Components that want to track an
// aspect call `acquireObserve(aspect)` on mount and the returned
// release fn in cleanup. When the refcount drops to zero, the broker
// subscription is torn down — keeps subscription cardinality bounded
// (the broker fans out per-aspect, so we don't want to subscribe to
// every aspect unconditionally).

const { signal } = window.__preact;
import { subscribe } from '../comms.js';

// aspectActivity.value = { [aspectName]: { presence, state, tool, lastUpdated } }
// Immutable updates: assign a fresh top-level object on every mutator
// so signal subscribers re-render reliably.
export const aspectActivity = signal({});

// Default activity for an aspect we haven't heard from. Presence
// starts 'offline' because nothing has proved otherwise; the first
// observe frame (replayed on subscribe) flips it to 'online'.
function defaultEntry() {
  return { presence: 'offline', state: 'idle', tool: '', lastUpdated: 0 };
}

// Patch a single aspect's entry. Helper to keep mutators terse + the
// immutable-update pattern centralised.
function patch(aspect, fields) {
  if (!aspect) return;
  const cur = aspectActivity.value;
  const prev = cur[aspect] || defaultEntry();
  const next = { ...prev, ...fields, lastUpdated: Date.now() };
  aspectActivity.value = { ...cur, [aspect]: next };
}

// applyObserveFrame interprets one observe.frame payload and updates
// the activity entry for its aspect. Tolerant — unknown frame kinds
// are silently ignored so a server-side kind addition doesn't break
// the UI.
//
// Exported for testability + the comms.js wiring below.
export function applyObserveFrame(frame) {
  if (!frame || !frame.aspect) return;
  const aspect = frame.aspect;
  const payload = frame.payload || {};
  switch (frame.kind) {
    case 'presence': {
      patch(aspect, {
        presence: payload.connected ? 'online' : 'offline',
        // Reset transient state on disconnect — a thinking aspect that
        // drops shouldn't keep showing the spinner.
        ...(payload.connected ? {} : { state: 'idle', tool: '' }),
      });
      return;
    }
    case 'turn': {
      // TurnFrame: derive state from Status + last event.
      // Status === 'in_flight' → thinking or tool (refine via events)
      // Status === 'complete' / 'errored' → idle
      if (payload.status !== 'in_flight') {
        patch(aspect, { state: 'idle', tool: '', presence: 'online' });
        return;
      }
      // Walk events from the end to find the last tool_call. If its
      // Result is still nil, the aspect is mid-tool; otherwise the
      // turn is between events (model is "thinking").
      const events = payload.events || [];
      let toolOpen = null;
      for (let i = events.length - 1; i >= 0; i--) {
        const e = events[i];
        if (e.kind === 'tool_call' && e.tool && !e.tool.result) {
          toolOpen = e.tool;
          break;
        }
        // A non-tool event after a tool means the tool has resolved
        // (the model emitted text or a step boundary).
        if (e.kind === 'text' || e.kind === 'step') break;
      }
      if (toolOpen) {
        patch(aspect, { state: 'tool', tool: toolOpen.name || '', presence: 'online' });
      } else {
        patch(aspect, { state: 'thinking', tool: '', presence: 'online' });
      }
      return;
    }
    // chat / filter_decision / unknown kinds — presence-positive but
    // no state inference (the aspect is producing frames, so it's at
    // least connected).
    default: {
      patch(aspect, { presence: 'online' });
      return;
    }
  }
}

// Refcount table for active subscribe.observe subscriptions. Key is
// the aspect name; value is `{ count, unsubscribe }`. The first
// acquire issues subscribe.observe via comms.subscribe; subsequent
// acquires bump the count. Release decrements; the last release
// fires the captured unsubscribe.
const subscriptions = new Map();

// acquireObserve subscribes this client to observe.frame for the
// named aspect (if not already). Returns a release function the
// caller MUST invoke in cleanup — failing to release leaks the
// subscription for the lifetime of the page.
//
// Multiple components can acquire the same aspect concurrently
// (e.g. two FocusedThread instances when split-view lands in v2).
// They share one server-side subscription via the refcount.
export function acquireObserve(aspect) {
  if (!aspect) return () => {};
  const existing = subscriptions.get(aspect);
  if (existing) {
    existing.count += 1;
  } else {
    const unsubscribe = subscribe('subscribe.observe', { aspect }, (payload) => {
      if (!payload || payload.aspect !== aspect) return;
      applyObserveFrame(payload.frame);
    });
    subscriptions.set(aspect, { count: 1, unsubscribe });
  }
  let released = false;
  return function release() {
    if (released) return; // idempotent — double-release is a no-op
    released = true;
    const entry = subscriptions.get(aspect);
    if (!entry) return;
    entry.count -= 1;
    if (entry.count <= 0) {
      entry.unsubscribe();
      subscriptions.delete(aspect);
    }
  };
}
