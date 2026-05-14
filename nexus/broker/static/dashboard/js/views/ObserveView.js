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
//
// NEX-246 Part 4b — thread context on each frame.
// Observability frames today are aspect-scoped ("here's what harrow
// did"). The peer-substrate model has the operator increasingly
// asking "which thread is this turn part of?" or "show me all frames
// from thread #1234". We surface a ThreadChip next to thread-
// attributable frames (chat + turn) that lazily loads thread context
// (participants, role hint) on viewport entry, and a filter input at
// the top to scope the stream to a single thread root. Presence and
// other non-thread frames render unchanged.

const { html, useEffect, useState, useRef, useMemo } = window.__preact;

import { agents, agentColors } from '../state.js';
import { send, onPushKind } from '../comms.js';
import { MessageBubble } from '../components/MessageBubble.js';
import { PresenceMarker } from '../components/PresenceMarker.js';
import { TurnBlock } from '../components/TurnBlock.js';
import { getOrCreateThread, peekThread, listOpenThreads } from '../models/threads.js';

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

// frameThreadRoot — derive the thread-root msg id (if any) for a
// frame. Chat frames carry thread_root post-#228 on the payload; we
// fall back to reply_to (covers legacy pre-#228 rows where
// thread_root wasn't stamped) and finally msg_id (a top-level
// message IS its own thread root). Turn frames carry trigger_msg,
// which is itself a candidate root — getOrCreateThread is forgiving,
// and the Thread's chat-ws filter will resolve the real root via the
// thread_root walk on first matching message. Returns 0 for
// non-thread-attributable kinds (presence, tool-only, unknown).
function frameThreadRoot(frame) {
  if (!frame || !frame.payload) return 0;
  const p = frame.payload;
  if (frame.kind === 'chat') {
    return Number(p.thread_root) || Number(p.thread_root_msg_id) || Number(p.reply_to) || Number(p.msg_id) || 0;
  }
  if (frame.kind === 'turn') {
    return Number(p.trigger_msg) || 0;
  }
  return 0;
}

export function ObserveView() {
  const agentList = agents.value;
  const colors = agentColors.value;

  const [aspect, setAspectState] = useState(() => aspectFromHash() || defaultAspect(agentList));
  const [frames, setFrames] = useState([]);
  // threadFilter — when non-zero, only frames whose thread-root matches
  // are rendered. 0 means "show everything" (default).
  const [threadFilter, setThreadFilter] = useState(0);
  // filterInput — controlled value for the filter text box. Decoupled
  // from threadFilter so the operator can type freely and we only
  // commit on Enter / blur. Avoids re-filtering on every keystroke.
  const [filterInput, setFilterInput] = useState('');

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
    // Don't rewrite the hash when ObserveView is hosted inside SplitView
    // (#/split). The router would interpret an #/agents/<id> hash as
    // "navigate to full ObserveView" and dismiss the split layout — the
    // exact bug operators reported as "split resets to fullscreen on
    // agent change". The hash is for sidebar navigation only; in-view
    // picker state lives in component state.
    if (!window.location.hash.startsWith('#/split')) {
      window.location.hash = '#/agents/' + id;
    }
  }

  // Stable alphabetical order for the aspect picker. The roster.list
  // response order changes when aspects register / deregister; without
  // sorting here the chips visibly reshuffle as presence flips, which
  // makes the picker hard to track while monitoring. Sort by id (case-
  // insensitive) — same identity regardless of registration order or
  // online state. Operator's currently-selected chip stays in place.
  const sortedAgents = [...agentList].sort((a, b) => {
    const ai = (typeof a === 'string' ? a : a.id || '').toLowerCase();
    const bi = (typeof b === 'string' ? b : b.id || '').toLowerCase();
    return ai < bi ? -1 : ai > bi ? 1 : 0;
  });

  // Apply thread filter (if any). frameThreadRoot returns 0 for
  // frames with no thread identity (presence, unknown kinds); those
  // are excluded from filtered view by definition — the operator
  // asked to see one thread, presence flips don't belong there.
  const visibleFrames = threadFilter > 0
    ? frames.filter(f => frameThreadRoot(f) === threadFilter)
    : frames;

  // Snapshot of currently-open threads for the picker dropdown. Not
  // a live binding — recomputed every render, which is fine: listing
  // the registry is O(n) over a small set (< 50 typical).
  const openThreads = listOpenThreads();

  function commitFilter(raw) {
    const n = Number((raw || '').toString().trim().replace(/^#/, ''));
    setThreadFilter(Number.isFinite(n) && n > 0 ? n : 0);
  }

  function clearFilter() {
    setThreadFilter(0);
    setFilterInput('');
  }

  return html`
    <div class="observe-view">
      <div class="observe-bar">
        ${sortedAgents.map(a => {
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
      <div class="observe-filter-bar" style=${{
        display: 'flex', alignItems: 'center', gap: '8px',
        padding: '6px 10px', borderBottom: '1px solid var(--border, #2a2a2a)',
        fontSize: '12px', color: 'var(--text-muted, #888)',
      }}>
        <span>Thread filter:</span>
        <input
          type="text"
          placeholder="paste msg_id (e.g. 1234)"
          value=${filterInput}
          onInput=${(e) => setFilterInput(e.target.value)}
          onKeyDown=${(e) => { if (e.key === 'Enter') commitFilter(filterInput); }}
          onBlur=${() => commitFilter(filterInput)}
          style=${{
            flex: '0 0 180px', padding: '3px 6px',
            background: 'var(--bg-input, #1a1a1a)',
            border: '1px solid var(--border, #333)',
            color: 'var(--text, #ddd)', fontSize: '12px',
          }}
        />
        ${openThreads.length > 0 ? html`
          <select
            value=${threadFilter || ''}
            onChange=${(e) => {
              const v = Number(e.target.value);
              setThreadFilter(v > 0 ? v : 0);
              setFilterInput(v > 0 ? String(v) : '');
            }}
            style=${{
              padding: '3px 6px',
              background: 'var(--bg-input, #1a1a1a)',
              border: '1px solid var(--border, #333)',
              color: 'var(--text, #ddd)', fontSize: '12px',
            }}
          >
            <option value="">— open threads —</option>
            ${openThreads.map(t => html`
              <option key=${t.rootId} value=${t.rootId}>
                #${t.rootId} ${t.roleHint ? `· ${t.roleHint}` : ''} ${t.participants.length ? `(${t.participants.length}p)` : ''}
              </option>
            `)}
          </select>
        ` : null}
        ${threadFilter > 0 ? html`
          <span style=${{ color: 'var(--accent, #6cf)' }}>filtering → #${threadFilter}</span>
          <button
            onClick=${clearFilter}
            style=${{
              padding: '2px 8px', fontSize: '11px',
              background: 'transparent', border: '1px solid var(--border, #333)',
              color: 'var(--text-muted, #888)', cursor: 'pointer',
            }}
          >clear</button>
        ` : null}
      </div>
      <div class="observe-stream">
        ${visibleFrames.length === 0
          ? html`<div class="observe-empty">${
              threadFilter > 0
                ? `No frames for thread #${threadFilter} from @${aspect} in current window.`
                : (aspect ? `Waiting for observability frames from @${aspect}…` : 'Pick an aspect to observe.')
            }</div>`
          : visibleFrames.map(frame => renderFrame(frame))
        }
      </div>
    </div>
  `;
}

function renderFrame(frame) {
  switch (frame.kind) {
    case 'chat': {
      // Chat keyed by msg_id so re-renders (e.g. parent state churn)
      // don't tear down the bubble; seq is also fine but msg_id reads
      // intent clearer when scanning React devtools.
      const key = `${frame.aspect}:chat:${frame.payload?.msg_id ?? frame.seq}`;
      return html`<${ChatRow} key=${key} frame=${frame} />`;
    }
    case 'presence': {
      // Each presence flip is its own event; seq keeps them distinct.
      const key = `${frame.aspect}:presence:${frame.seq}`;
      return html`<${PresenceMarker} key=${key} aspect=${frame.aspect} payload=${frame.payload || {}} ts=${frame.ts} />`;
    }
    case 'turn': {
      // Key by turn_id (NOT seq) so consecutive snapshots of the same
      // turn update the same component instance rather than remounting.
      // Without this the TurnBlock unmount/remount visibly flickered
      // on every event and reset child state (e.g. ToolCall expand
      // toggles). Operator reported the visual churn as the activity
      // page "shuffling order."
      const tid = frame.payload?.turn_id || frame.seq;
      const key = `${frame.aspect}:turn:${tid}`;
      const rootId = frameThreadRoot(frame);
      return html`
        <div class="observe-turnrow" style=${{ display: 'flex', alignItems: 'flex-start', gap: '6px' }}>
          <div style=${{ flex: '1 1 auto', minWidth: 0 }}>
            <${TurnBlock} key=${key} payload=${frame.payload || {}} seq=${frame.seq} ts=${frame.ts} />
          </div>
          ${rootId > 0 ? html`<${ThreadChip} rootId=${rootId} />` : null}
        </div>
      `;
    }
    default:
      return null; // unknown frame kind — drop quietly
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
  const rootId = frameThreadRoot(frame);
  return html`
    <div class=${'observe-chatrow observe-chatrow-' + direction}>
      <div class=${'observe-direction ' + direction} title=${label} aria-label=${label}>${arrow}</div>
      ${/* observe pane is read-only by intent: no Reply, no reactions */ ''}
      <${MessageBubble} msg=${msg} readOnly=${true} />
      ${rootId > 0 ? html`<${ThreadChip} rootId=${rootId} />` : null}
    </div>
  `;
}

// ThreadChip — lazy-loading thread context indicator.
//
// Lazy-load strategy: an IntersectionObserver watches the chip's
// root element. We call getOrCreateThread(rootId) on mount (cheap —
// registry hit if already known, else just constructs the empty
// Thread) and subscribe immediately so participants/roleHint
// re-render when other views populate the same thread. But .load()
// — which hits chat.replies — is gated until the chip enters the
// viewport. With 200 frames in the buffer × many threads, eagerly
// loading every thread on initial render would fan out into dozens
// of chat.replies RPCs in the first paint. Viewport-gated load
// means scrolling pays the cost incrementally and offscreen chips
// stay cheap.
//
// We also short-circuit if peekThread() shows the thread is already
// loaded (another view opened it) — no load needed, just render the
// snapshot we already have. This is the common case once the
// operator has ThreadView open alongside ObserveView.
function ThreadChip({ rootId }) {
  const [tick, setTick] = useState(0); // rerender bumper on thread mutate
  const rootRef = useRef(null);
  const loadedRef = useRef(false);

  // Get / construct the Thread immediately so participants/roleHint
  // are queryable. Wrapped in useMemo so we don't re-call
  // getOrCreateThread on every render (it's cheap but the identity
  // churn would confuse the effect dep array).
  const thread = useMemo(() => {
    try {
      return getOrCreateThread(rootId);
    } catch (e) {
      return null;
    }
  }, [rootId]);

  // Subscribe to thread mutations so participants / roleHint
  // re-render live. The subscription is free if we never call
  // load() — chat-ws only wires up inside Thread on first
  // subscriber, but Thread instances dedupe across views, so the
  // cost is one wire-up per thread regardless of chip count.
  useEffect(() => {
    if (!thread) return;
    const off = thread.subscribe(() => setTick((n) => n + 1));
    return () => off();
  }, [thread]);

  // Viewport-gated load. IntersectionObserver fires on scroll-in;
  // we trigger .load() once per chip lifetime. The Thread itself
  // dedupes concurrent loads, so two chips for the same root racing
  // each other is fine — only one fetch goes out.
  useEffect(() => {
    if (!thread || !rootRef.current) return;
    if (thread.loaded || loadedRef.current) return;
    // Fallback for environments without IntersectionObserver: just
    // load immediately. The lazy path is a perf optimisation, not a
    // correctness requirement.
    if (typeof IntersectionObserver === 'undefined') {
      loadedRef.current = true;
      thread.load().catch(() => {});
      return;
    }
    const el = rootRef.current;
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting && !loadedRef.current) {
          loadedRef.current = true;
          thread.load().catch(() => {});
          io.disconnect();
          break;
        }
      }
    }, { rootMargin: '50px' }); // pre-load slightly before fully visible
    io.observe(el);
    return () => io.disconnect();
  }, [thread]);

  if (!thread) return null;

  // peekThread is the same as `thread` here, but using participants
  // / roleHint pulls the live computed values off the instance.
  // Before .load() completes, participants[] may just hold the
  // seed-or-empty and roleHint may be ''. Render gracefully.
  const participants = thread.participants;
  const role = thread.roleHint;
  const pCount = participants.length;
  const loaded = thread.loaded;

  // Color the chip by role for at-a-glance scanning. Subtle —
  // matches the existing observability palette rather than
  // introducing a new one.
  const roleColor = role === 'planner-dispatch' ? '#7ad'
    : role === 'worker-execution' ? '#a7d'
    : role === 'operator-drive' ? '#d97'
    : '#888';

  function openThread() {
    // Hash-route to the thread. Other views (FeedView, ChatView)
    // honour #/thread/<rootId> for direct thread navigation; if
    // not, this is a no-op route that the operator can copy out.
    window.location.hash = '#/thread/' + rootId;
  }

  return html`
    <div
      ref=${rootRef}
      class="observe-thread-chip"
      onClick=${openThread}
      title=${loaded
        ? `thread #${rootId} · ${role || 'unclassified'} · ${pCount} participant${pCount === 1 ? '' : 's'}: ${participants.join(', ')}`
        : `thread #${rootId} · loading…`}
      style=${{
        display: 'inline-flex', alignItems: 'center', gap: '4px',
        flex: '0 0 auto', alignSelf: 'flex-start',
        marginLeft: '6px', padding: '2px 6px',
        background: 'var(--bg-chip, rgba(255,255,255,0.04))',
        border: `1px solid ${roleColor}33`,
        borderLeft: `2px solid ${roleColor}`,
        borderRadius: '3px',
        fontSize: '10px', fontFamily: 'var(--mono, monospace)',
        color: 'var(--text-muted, #aaa)',
        cursor: 'pointer', whiteSpace: 'nowrap',
        maxWidth: '220px', overflow: 'hidden', textOverflow: 'ellipsis',
      }}
    >
      <span style=${{ color: roleColor }}>◆</span>
      <span>#${rootId}</span>
      ${loaded
        ? html`
            ${pCount > 0 ? html`<span>· ${pCount}p</span>` : null}
            ${role ? html`<span>· ${role}</span>` : null}
          `
        : html`<span style=${{ opacity: 0.5 }}>· …</span>`}
    </div>
  `;
}
