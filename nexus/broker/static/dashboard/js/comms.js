// comms.js — WS RPC layer between the dashboard SPA and the nexus
// broker. Replaces the REST `request()` path the agent-network
// dashboard used; every view now talks to nexus through a single
// /connect WebSocket.
//
// Three primitives:
//
//   rpc(kind, params)        → Promise<resultPayload>
//   subscribe(kind, params, handler) → unsubscribeFn
//   sendChat({content, ...}) → Promise<{msg_id}>   (sugar over rpc)
//
// Login is HTTP-only (POST /api/operator/login/*); after the SPA
// has a JWT, comms.open(jwt) brings the WS up and resolves once the
// connection is ready. Reconnect is automatic with exponential
// backoff capped at 30s; subscriptions are replayed on reconnect.
//
// Frame catalogue (must match nexus/frames/frames.go):
//
//   request kinds      — roster.list, chat.list, chat.replies,
//                        chat.reactions.fetch, knowledge.list,
//                        knowledge.search, knowledge.store,
//                        aspect.say
//   subscribe kinds    — subscribe.{roster,chat,aspect_status}
//   push kinds         — chat.deliver, roster.update,
//                        aspect.status_pulse
//   result kinds       — <request>.result for each above
//   error kinds        — <request>.error (one-off, not in IsKnown)

// Per-connection state. Module-level singleton; the SPA has one WS.
const state = {
  ws: null,
  jwt: null,
  ready: false,
  // pending RPCs: correlation_id → {resolve, reject, kind, deadline}
  pending: new Map(),
  // active subscriptions: pushKind (e.g. chat.deliver) → array of
  // handler fns. Replayed on reconnect by re-issuing the corresponding
  // subscribe.X frame, looked up via subKinds.
  subs: new Map(),
  // pushKind → subscribe.X (so reconnect replay can issue the right
  // frame). Populated alongside subs in subscribe().
  subKinds: new Map(),
  // outbound queue for frames sent before the WS opened OR during a
  // reconnect window. Drained on open. Bounded by a soft cap so a
  // long offline period doesn't grow the queue unboundedly.
  outbox: [],
  // listeners notified on connection state transitions (so the SPA
  // can render an "offline / reconnecting" banner). { onOpen, onClose }.
  listeners: { onOpen: [], onClose: [] },
  // backoff state for reconnect.
  backoffMs: 1000,
  reconnectTimer: null,
  manualClose: false,
};

const RPC_TIMEOUT_MS = 30_000;
const OUTBOX_CAP = 256;
const MAX_BACKOFF_MS = 30_000;

// crypto.randomUUID is available in every modern browser. We don't
// need RFC4122-strict UUIDs — just collision-resistant strings. Fall
// back to a short random hex on the unlikely browser without it.
function correlationID() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
    return globalThis.crypto.randomUUID();
  }
  return Math.random().toString(36).slice(2) + Date.now().toString(36);
}

// open establishes the WS using the supplied JWT. Returns a Promise
// that resolves when the connection is open (the first frame the
// server may send is a server-driven push, not an ack — we resolve
// on the WS-level open event and let pending rpcs queue until then).
//
// Calling open() while a connection is already alive is a no-op.
// Calling open() with a different JWT closes and reconnects.
export function open(jwt) {
  if (state.jwt === jwt && state.ws && state.ws.readyState === WebSocket.OPEN) {
    return Promise.resolve();
  }
  state.manualClose = false;
  state.jwt = jwt;
  return connect();
}

// close drops the WS without reconnecting. Used by Login.js when
// the operator signs out.
export function close() {
  state.manualClose = true;
  if (state.reconnectTimer) {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
  }
  if (state.ws) {
    state.ws.close(1000, 'operator close');
    state.ws = null;
  }
  state.ready = false;
  // Reject every pending RPC so views surface a clean error rather
  // than dangling Promises after sign-out.
  for (const [id, p] of state.pending) {
    p.reject(new Error('comms: connection closed'));
    state.pending.delete(id);
  }
  state.subs.clear();
  state.outbox.length = 0;
}

function connect() {
  return new Promise((resolve, reject) => {
    const url = wsURL(state.jwt);
    const ws = new WebSocket(url);
    state.ws = ws;

    ws.onopen = () => {
      state.ready = true;
      state.backoffMs = 1000; // reset on successful connect
      // Replay subscriptions over the new connection so server-side
      // state matches what the SPA thinks it has. state.subs is keyed
      // by the inbound push kind (chat.deliver, roster.update, etc) so
      // the per-frame route lookup is cheap. To replay we have to send
      // the subscribe.X frame, not the push kind — the server doesn't
      // accept push kinds inbound (logs "frame kind not yet handled"
      // and never flips subscribedChat). Map back via state.subKinds.
      for (const pushKind of state.subs.keys()) {
        const subKind = state.subKinds.get(pushKind);
        if (!subKind) continue;
        sendFrame({ kind: subKind, id: correlationID(), ts: nowISO(), payload: {} });
      }
      // Drain the outbox.
      while (state.outbox.length > 0) {
        ws.send(state.outbox.shift());
      }
      for (const fn of state.listeners.onOpen) {
        try { fn(); } catch (e) { console.error('comms onOpen', e); }
      }
      resolve();
    };

    ws.onmessage = (ev) => {
      let env;
      try {
        env = JSON.parse(ev.data);
      } catch (e) {
        console.warn('comms: undecodable frame', e);
        return;
      }
      handleFrame(env);
    };

    ws.onclose = (ev) => {
      state.ready = false;
      state.ws = null;
      for (const fn of state.listeners.onClose) {
        try { fn(ev); } catch (e) { console.error('comms onClose', e); }
      }
      if (!state.manualClose && state.jwt) {
        scheduleReconnect();
      }
    };

    ws.onerror = () => {
      // onclose fires after onerror; reconnect handled there.
      // Reject the open() promise on first connection failure.
      if (!state.ready) {
        reject(new Error('comms: WS connect failed'));
      }
    };
  });
}

function scheduleReconnect() {
  if (state.reconnectTimer) return;
  const delay = state.backoffMs;
  state.backoffMs = Math.min(state.backoffMs * 2, MAX_BACKOFF_MS);
  state.reconnectTimer = setTimeout(() => {
    state.reconnectTimer = null;
    connect().catch((e) => {
      console.warn('comms reconnect failed', e);
      // connect rejected — onclose will scheduleReconnect again.
    });
  }, delay);
}

function wsURL(jwt) {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/connect?token=${encodeURIComponent(jwt)}`;
}

function nowISO() {
  return new Date().toISOString();
}

function sendFrame(env) {
  const data = JSON.stringify(env);
  if (state.ws && state.ws.readyState === WebSocket.OPEN) {
    state.ws.send(data);
    return;
  }
  // Queue while disconnected. Drop oldest if over cap so a long
  // offline period doesn't unbound the queue.
  if (state.outbox.length >= OUTBOX_CAP) {
    state.outbox.shift();
  }
  state.outbox.push(data);
}

function handleFrame(env) {
  // Response frame: route to pending by in_reply_to.
  if (env.in_reply_to) {
    const p = state.pending.get(env.in_reply_to);
    if (!p) return; // stale; ignore
    state.pending.delete(env.in_reply_to);
    if (env.kind && env.kind.endsWith('.error')) {
      const errMsg = (env.payload && env.payload.error) || env.kind;
      p.reject(new Error(errMsg));
      return;
    }
    p.resolve(env.payload || {});
    return;
  }
  // Push frame: dispatch to every subscribed handler for the kind.
  // Subscriptions can have multiple handlers per kind (e.g. a Chat
  // view and a Feed view both subscribed to chat.deliver).
  const handlers = state.subs.get(env.kind);
  if (handlers) {
    for (const fn of handlers) {
      try { fn(env.payload || {}); } catch (e) { console.error('handler', env.kind, e); }
    }
  }
  // subscribe.ack frames also carry an in_reply_to; handled above.
  // Server may emit other untracked broadcast kinds in future; drop.
}

// send is fire-and-forget: emits a frame with no Promise + no timeout.
// Use for kinds where the broker's response is observed via a separate
// subscription (chat.send → chat.deliver fan-out) rather than a
// matching .result frame. Calling rpc() on those kinds wedges the
// promise for 30s and rejects with a timeout, which is what made
// chat.send look broken in the SPA even though the server happily
// persisted the message.
export function send(kind, payload = {}) {
  const id = correlationID();
  sendFrame({ kind, id, ts: nowISO(), payload });
  return id;
}

// rpc sends a request frame and awaits the matching .result. Rejects
// with the server's error string on .error, with a timeout error if
// the response doesn't arrive within RPC_TIMEOUT_MS, and with a
// generic close error if the connection closes mid-flight.
export function rpc(kind, payload = {}) {
  const id = correlationID();
  const env = { kind, id, ts: nowISO(), payload };
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => {
      if (state.pending.delete(id)) {
        reject(new Error(`comms: rpc ${kind} timed out`));
      }
    }, RPC_TIMEOUT_MS);
    state.pending.set(id, {
      resolve: (v) => { clearTimeout(t); resolve(v); },
      reject:  (e) => { clearTimeout(t); reject(e); },
      kind,
    });
    sendFrame(env);
  });
}

// onPushKind registers a handler for a server push kind WITHOUT
// issuing a subscribe.X frame. Use when the server fans the kind out
// via another subscription you (or someone else) is already on —
// e.g. chat.reaction.update piggy-backs on the chat subscription, so
// the SPA just needs to listen for the kind, not subscribe to it.
//
// Returns an unsubscribe fn. Multiple handlers per kind are allowed.
export function onPushKind(pushKind, handler) {
  let handlers = state.subs.get(pushKind);
  if (!handlers) {
    handlers = [];
    state.subs.set(pushKind, handlers);
    // Note: no entry in state.subKinds — reconnect won't replay this
    // (there's no subscribe frame to replay). That's correct: the
    // server fans the kind out via the OTHER subscription's gate, so
    // as long as that subscription is replayed, the kind keeps
    // flowing.
  }
  handlers.push(handler);
  return function off() {
    const list = state.subs.get(pushKind);
    if (!list) return;
    const i = list.indexOf(handler);
    if (i >= 0) list.splice(i, 1);
    if (list.length === 0) state.subs.delete(pushKind);
  };
}

// subscribe enrols the connection in a push channel and routes
// matching frames to handler. Returns an unsubscribeFn that:
//
//   - removes handler from the local routing list
//   - sends unsubscribe.<kind> if no handlers remain
//
// Multiple subscribers to the same kind share a single server-side
// subscription; the server doesn't know how many UI components are
// listening.
export function subscribe(kind, params, handler) {
  // pushKind maps "subscribe.X" → "X.update" or similar. The server
  // emits chat.deliver / roster.update / aspect.status_pulse, NOT
  // "subscribe.chat.update" — the subscribe frame is the gate, the
  // push frame is named for the data.
  const pushKind = pushKindFor(kind);

  let handlers = state.subs.get(pushKind);
  const fresh = !handlers;
  if (fresh) {
    handlers = [];
    state.subs.set(pushKind, handlers);
    state.subKinds.set(pushKind, kind);
  }
  handlers.push(handler);

  if (fresh) {
    // First subscriber for this push kind — issue the subscribe
    // frame. We don't await the ack here; the server will start
    // pushing as soon as it processes the frame, and any pre-ack
    // frames are still routable via state.subs.
    sendFrame({ kind, id: correlationID(), ts: nowISO(), payload: params || {} });
  }

  return function unsubscribe() {
    const list = state.subs.get(pushKind);
    if (!list) return;
    const i = list.indexOf(handler);
    if (i >= 0) list.splice(i, 1);
    if (list.length === 0) {
      state.subs.delete(pushKind);
      state.subKinds.delete(pushKind);
      // Tell the server too. Best-effort — if we're already closed,
      // the server's wsConn cleanup tears down the sub anyway.
      sendFrame({
        kind: kind.replace('subscribe.', 'unsubscribe.'),
        id: correlationID(),
        ts: nowISO(),
        payload: {},
      });
    }
  };
}

function pushKindFor(subscribeKind) {
  switch (subscribeKind) {
    case 'subscribe.chat':           return 'chat.deliver';
    case 'subscribe.roster':         return 'roster.update';
    case 'subscribe.aspect_status':  return 'aspect.status_pulse';
    default: return subscribeKind;
  }
}

// onConnectionState lets the SPA's chrome render an "offline" banner
// when the WS drops + reconnect-progress while it's coming back.
// Returns a cleanup fn.
export function onConnectionState({ onOpen, onClose }) {
  if (onOpen)  state.listeners.onOpen.push(onOpen);
  if (onClose) state.listeners.onClose.push(onClose);
  return () => {
    state.listeners.onOpen  = state.listeners.onOpen.filter(f => f !== onOpen);
    state.listeners.onClose = state.listeners.onClose.filter(f => f !== onClose);
  };
}

// isConnected reflects current ready-state for views that want to
// branch on it (e.g. show a Loading affordance until ready).
export function isConnected() {
  return state.ready;
}
