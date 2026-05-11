// chat-ws.js — compatibility shim over comms.js.
//
// Agent-network had a separate /ws/chat WebSocket purpose-built for
// the SPA. Nexus uses one /connect WS for everything, with frame-
// kind-based routing (comms.js). This shim preserves the agent-
// network chatWS API so AgentsView / Chat / FeedView keep working
// without bulk changes; calls to chatWS.on('message.created', fn)
// translate into comms.subscribe('subscribe.chat', ...) once the
// SPA has authenticated.
//
// Event mapping:
//
//   message.created   ← chat.deliver push
//   reaction.changed  ← chat.reaction.update push (piggy-backs on
//                        subscribe.chat; broker fans both kinds out
//                        to the same subscription gate)
//   ready             ← onConnectionState onOpen
//   reconnect         ← onConnectionState onOpen (after first)

import { subscribe, onPushKind, onConnectionState, isConnected } from './comms.js';

class ChatWS {
  constructor() {
    this.handlers = {
      'message.created': [],
      'reaction.changed': [], // deferred; no fan-out frame today
      'ready': [],
      'reconnect': [],
    };
    this.unsubChat = null;
    this.unsubReact = null;
    this.unsubConn = null;
    this.firstReady = false;
  }

  start() {
    if (this.unsubChat) return; // idempotent

    // chat.deliver → message.created
    this.unsubChat = subscribe('subscribe.chat', {}, (payload) => {
      // Translate the chat.deliver payload back into agent-network's
      // {type, msg} shape so existing views see what they expect.
      const msg = {
        id: payload.id,
        from: payload.from,
        content: payload.content,
        reply_to: payload.reply_to || 0,
        // agent-network used `created_at`; nexus emits `received_at`.
        // Map it across so MessageBubble's existing renderer works.
        created_at: payload.received_at || '',
        topic: payload.topic || '',
      };
      this.fire('message.created', { type: 'message.created', msg });
    });

    // chat.reaction.update → reaction.changed. Server broadcasts this
    // on the same subscribedChat gate, so no extra subscribe frame —
    // onPushKind registers a passive listener for the kind. The
    // payload carries the FULL reactions list for the affected msg
    // (not a delta), shape:
    //   { msg_id, reactor, emoji, op: "added"|"removed",
    //     reactions: [{aspect, emoji}, ...] }
    // Chat.js's onWsReactionChanged reads `ev.reactions` and replaces
    // the message's reactions in place.
    this.unsubReact = onPushKind('chat.reaction.update', (payload) => {
      this.fire('reaction.changed', {
        type: 'reaction.changed',
        msg_id: payload.msg_id,
        reactor: payload.reactor,
        emoji: payload.emoji,
        op: payload.op,
        reactions: payload.reactions || [],
      });
    });

    // Connection-state events
    this.unsubConn = onConnectionState({
      onOpen: () => {
        if (!this.firstReady) {
          this.firstReady = true;
          this.fire('ready', { type: 'ready' });
        } else {
          this.fire('reconnect', { type: 'reconnect' });
        }
      },
      // onClose is intentionally not surfaced — comms.js handles
      // reconnect transparently; views relying on a "disconnected"
      // marker would need to opt in via onConnectionState directly.
    });

    // If comms is already open by the time start() runs, we won't
    // get an onOpen — fire ready synchronously so views unlock.
    if (isConnected() && !this.firstReady) {
      this.firstReady = true;
      // Fire on next tick so handlers attached after start() still
      // see the ready event.
      Promise.resolve().then(() => this.fire('ready', { type: 'ready' }));
    }
  }

  stop() {
    if (this.unsubChat) { this.unsubChat(); this.unsubChat = null; }
    if (this.unsubReact) { this.unsubReact(); this.unsubReact = null; }
    if (this.unsubConn) { this.unsubConn(); this.unsubConn = null; }
    this.firstReady = false;
  }

  on(eventName, handler) {
    if (!this.handlers[eventName]) this.handlers[eventName] = [];
    this.handlers[eventName].push(handler);
    // Views call `const off = on(...)` and use off() inside useEffect
    // cleanup. Returning the unsubscribe matches the agent-network
    // contract; without it, calling off() throws "not a function" on
    // every component unmount.
    return () => this.off(eventName, handler);
  }

  off(eventName, handler) {
    const list = this.handlers[eventName];
    if (!list) return;
    const i = list.indexOf(handler);
    if (i >= 0) list.splice(i, 1);
  }

  fire(eventName, ev) {
    const list = this.handlers[eventName] || [];
    for (const fn of list) {
      try { fn(ev); } catch (e) { console.error(eventName, e); }
    }
  }
}

export const chatWS = new ChatWS();
