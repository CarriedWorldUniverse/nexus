// ObserveView — per-aspect observability surface.
//
// Replaces the deleted AgentsView (per-agent DM panel that only
// showed dm:<id> topic traffic — operator-confusing because most
// aspect chat happens on #general or @-addressed threads, not in
// DMs). The new view consumes the Phase B `subscribe.observe` WS
// frame surface: pick one aspect, get a chronological stream of
// observability Frames (ChatFrame in v0.1; TurnFrame stub already
// wired so Phase E/F drops in without view changes).
//
// WHY direct comms.send + onPushKind instead of comms.subscribe:
// the existing subscribe() helper keys handlers by push-kind only,
// not by (kind+params). Calling subscribe('subscribe.observe',
// {aspect:'plumb'}) then subscribe('subscribe.observe',
// {aspect:'keel'}) would emit only one subscribe frame and route
// every observe.frame push to both handlers. The lower-level
// primitives let us own subscribe/unsubscribe lifecycle per-aspect.
// TODO(comms): extend comms.subscribe to support per-param
// multi-subscription so views like this can use it normally.
//
// History: the broker's Observability Hub keeps a per-aspect ring
// buffer and replays it on every subscribe. We hold a small
// in-component buffer (HISTORY_CAP) so the stream doesn't grow
// unbounded over a long session; replay covers refresh, the cap
// covers liveness.

const { html, useEffect, useState } = window.__preact;

import { agents, agentColors } from '../state.js';
import { send, onPushKind } from '../comms.js';
import { MessageBubble } from '../components/MessageBubble.js';
import { PresenceMarker } from '../components/PresenceMarker.js';
import { TurnBlock } from '../components/TurnBlock.js';

const HISTORY_CAP = 200;

function aspectFromHash() {
  const hash = window.location.hash;
  if (hash.startsWith('#/agents/')) return hash.slice('#/agents/'.length);
  return null;
}

function defaultAspect(list) {
  if (!list || list.length === 0) return null;
  const a = list[0];
  return typeof a === 'string' ? a : a.id;
}

export function ObserveView() {
  const agentList = agents.value;
  const colors = agentColors.value;

  const [aspect, setAspectState] = useState(() => aspectFromHash() || defaultAspect(agentList));
  const [frames, setFrames] = useState([]);

  // Track hash changes so back/forward + sidebar nav update the picker.
  useEffect(() => {
    function onHash() {
      const next = aspectFromHash();
      if (next) setAspectState(next);
    }
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  // If we mounted before agents loaded, pick a default once they arrive.
  useEffect(() => {
    if (aspect) return;
    const d = defaultAspect(agentList);
    if (d) setAspectState(d);
  }, [agentList, aspect]);

  // Subscribe + push-handler lifecycle. Reset frames on every aspect
  // switch — the buffer replay on the new subscribe will repopulate.
  useEffect(() => {
    if (!aspect) return;
    setFrames([]);
    send('subscribe.observe', { aspect });
    const off = onPushKind('observe.frame', (payload) => {
      if (!payload || payload.aspect !== aspect) return; // not for this view
      // ObserveFramePayload.Frame is json.RawMessage on the wire, so it
      // already arrives as a parsed object here — no extra JSON.parse.
      const frame = payload.frame;
      if (!frame || typeof frame !== 'object') return;
      setFrames(prev => {
        // Drop out-of-order arrivals (replay + live race window can
        // occasionally re-deliver an old seq). Sequence is monotonic
        // per-aspect on the server side; trust it.
        if (prev.length > 0 && prev[prev.length - 1].seq >= frame.seq) return prev;

        // Turn-snapshot collapse: per types.go, each TurnFrame is a
        // full snapshot — every event the Grouper sees re-emits the
        // whole turn. Rendering each one stacks duplicates ("turn X
        // appears 4 times"). Replace prior frames of the same turn_id
        // in place so the operator sees one row per turn that updates
        // live. Non-turn frames (chat / presence) append normally.
        const turnID = (frame.kind === 'turn' && frame.payload && frame.payload.turn_id) || null;
        let next;
        if (turnID) {
          const idx = prev.findIndex(f =>
            f.kind === 'turn' && f.payload && f.payload.turn_id === turnID
          );
          if (idx >= 0) {
            next = prev.slice();
            next[idx] = frame;
          } else {
            next = [...prev, frame];
          }
        } else {
          next = [...prev, frame];
        }
        if (next.length > HISTORY_CAP) {
          next = next.slice(next.length - HISTORY_CAP);
        }
        return next;
      });
    });
    return () => {
      send('unsubscribe.observe', { aspect });
      off();
    };
  }, [aspect]);

  function selectAspect(id) {
    setAspectState(id);
    window.location.hash = '#/agents/' + id;
  }

  return html`
    <div class="observe-view">
      <div class="observe-bar">
        ${agentList.map(a => {
          const id = typeof a === 'string' ? a : a.id;
          const alive = typeof a === 'object' ? a.alive : true;
          const color = colors[id] || '#888';
          return html`
            <button
              key=${id}
              class=${'observe-btn' + (aspect === id ? ' active' : '')}
              style=${{ '--observe-color': color }}
              onClick=${() => selectAspect(id)}
            >
              <span class=${'observe-dot' + (alive ? ' alive' : '')} style=${{ background: alive ? color : '#444' }}></span>
              <span class="observe-btn-name">${id}</span>
            </button>
          `;
        })}
      </div>
      <div class="observe-stream">
        ${frames.length === 0
          ? html`<div class="observe-empty">${aspect ? `Waiting for observability frames from @${aspect}…` : 'Pick an aspect to observe.'}</div>`
          : frames.map(frame => renderFrame(frame))
        }
      </div>
    </div>
  `;
}

function renderFrame(frame) {
  const key = `${frame.aspect}:${frame.seq}`;
  switch (frame.kind) {
    case 'chat':     return html`<${ChatRow}     key=${key} frame=${frame} />`;
    case 'presence': return html`<${PresenceMarker} key=${key} aspect=${frame.aspect} payload=${frame.payload || {}} ts=${frame.ts} />`;
    case 'turn':     return html`<${TurnBlock}      key=${key} payload=${frame.payload || {}} seq=${frame.seq} ts=${frame.ts} />`;
    default:         return null; // unknown frame kind — drop quietly
  }
}

// ChatRow — wraps MessageBubble with a direction badge. WHY inline
// here rather than a separate component file: the only thing it adds
// over MessageBubble is the badge + flex shell; pulling it into its
// own component would just spread the same 15 lines across two
// modules.
function ChatRow({ frame }) {
  const p = frame.payload || {};
  const direction = p.direction === 'outbound' ? 'outbound' : 'inbound';
  // Synthesise the message shape MessageBubble expects (id, from,
  // content, reply_to, topic, created_at). reactions intentionally
  // empty — observability frames don't carry them and the view is
  // read-only anyway (operator reactions live in chat-view).
  const msg = {
    id: p.msg_id,
    from: p.from,
    content: p.content || '',
    reply_to: p.reply_to || undefined,
    topic: p.topic || undefined,
    created_at: p.created_at || frame.ts,
    reactions: {},
  };
  const arrow = direction === 'outbound' ? '▶' : '◀';
  const label = direction === 'outbound' ? 'sent' : 'received';
  return html`
    <div class=${'observe-chatrow observe-chatrow-' + direction}>
      <div class=${'observe-direction ' + direction} title=${label} aria-label=${label}>${arrow}</div>
      ${/* observe pane is read-only by intent: no Reply, no reactions */ ''}
      <${MessageBubble} msg=${msg} readOnly=${true} />
    </div>
  `;
}
