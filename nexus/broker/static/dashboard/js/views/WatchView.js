const { html, useEffect, useRef, useState } = window.__preact;

import { replyToThread, runsList, runGet, runCancel } from '../api.js';
import { onPushKind, subscribe } from '../comms.js';
import { MessageBubble } from '../components/MessageBubble.js';
import { DispatchComposePanel } from './panels/DispatchComposePanel.js';
import { TeamPanel } from './panels/TeamPanel.js';
import { EnvHealthPanel } from './panels/EnvHealthPanel.js';

function statusClass(status) {
  const map = {
    queued: 'wait',
    running: 'live',
    complete: 'ok',
    failed: 'bad',
    cancelled: 'muted',
  };
  return map[status] || 'muted';
}

function formatTime(ms) {
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '';
  const d = new Date(n);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

function summarizeRun(r) {
  if (r.command) return r.command;
  if (r.pr_url) return r.pr_url;
  return r.status || 'run';
}

function RunFeed({ runs, selected, onSelect, loading, error }) {
  return html`
    <section class="run-feed" aria-label="Runs">
      <div class="run-feed-head">
        <span>Runs</span>
        ${loading ? html`<span class="run-feed-state">loading</span>` : null}
      </div>
      <div class="run-feed-list">
        ${runs.map((r) => html`
          <button
            key=${r.run_id}
            class=${`run-card ${selected === r.run_id ? 'run-card-active' : ''}`}
            onClick=${() => onSelect(r.run_id)}
          >
            <span class=${`run-dot run-dot-${statusClass(r.status)}`} title=${r.status || 'unknown'}></span>
            <span class="run-card-main">
              <span class="run-card-line">
                <span class="run-agent">${r.agent || 'unassigned'}</span>
                <span class="run-ticket">${r.ticket || r.thread || r.run_id}</span>
              </span>
              <span class="run-cmd">${summarizeRun(r)}</span>
            </span>
            <span class="run-status">${r.status || 'unknown'}</span>
          </button>
        `)}
        ${!loading && runs.length === 0 ? html`<div class="run-feed-empty">No runs yet.</div>` : null}
        ${error ? html`<div class="run-feed-error">${error}</div>` : null}
      </div>
    </section>
  `;
}

function activityFromObservePayload(payload) {
  const frame = payload && payload.frame;
  if (!frame || typeof frame !== 'object') return null;
  const p = frame.payload || {};
  return {
    kind: 'activity',
    at: Date.parse(frame.ts || '') || Date.now(),
    run_id: frame.run_id || '',
    activity: {
      type: frame.kind || 'activity',
      text: p.text || p.summary || p.content || p.state || '',
      tool: p.tool || p.name || '',
      state: p.status || p.state || '',
    },
  };
}

function ActivityLine({ item }) {
  const activity = item.activity || {};
  const type = activity.type || 'activity';
  const text = activity.text || activity.tool || activity.state || '';
  return html`
    <div class=${`activity-line activity-${type}`}>
      <span class="activity-at">${formatTime(item.at)}</span>
      <span class="activity-kind">${type}</span>
      ${text ? html`<span class="activity-text">${text}</span>` : null}
    </div>
  `;
}

function CancelControls({ run, onCancelled }) {
  if (!run || run.status !== 'running') return null;
  const stop = (force) => {
    const target = run.ticket || run.run_id || 'this run';
    const msg = force ? `Force-kill ${target}? In-flight work is lost.` : `Stop ${target}?`;
    if (!window.confirm(msg)) return;
    runCancel(run.run_id, force).then(() => onCancelled && onCancelled());
  };
  return html`
    <span class="run-cancel">
      <button class="btn-stop" onClick=${() => stop(false)}>Stop</button>
      <button class="btn-force" onClick=${() => stop(true)}>Force</button>
    </span>
  `;
}

function ThreadReplyBox({ run, onReply }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  if (!run) return null;
  const disabled = busy || !run.dispatch_msg_id || !run.agent;
  const submit = (e) => {
    e.preventDefault();
    const body = text.trim();
    if (!body) return;
    setBusy(true);
    setError('');
    replyToThread(run, body).then((res) => {
      setText('');
      if (onReply) onReply({
        kind: 'chat',
        at: Date.now(),
        chat: {
          msg_id: res.msg_id,
          from: 'operator',
          content: body.startsWith(`@${run.agent}`) ? body : `@${run.agent} ${body}`,
          reply_to: run.dispatch_msg_id,
        },
      });
    }).catch((err) => {
      setError(err.message || 'reply failed');
    }).finally(() => setBusy(false));
  };
  return html`
    <form class="timeline-reply" onSubmit=${submit}>
      <textarea
        value=${text}
        rows="3"
        disabled=${disabled}
        placeholder=${run.dispatch_msg_id ? `Reply to ${run.agent || 'agent'}` : 'Thread root unavailable'}
        onInput=${(e) => setText(e.currentTarget.value)}
      />
      <div class="timeline-reply-actions">
        ${error ? html`<span class="timeline-reply-error">${error}</span>` : null}
        <button type="submit" disabled=${disabled || !text.trim()}>Reply</button>
      </div>
    </form>
  `;
}

function Timeline({ items, selected, selectedRun, loading, partial, error, onReply }) {
  if (!selected) {
    return html`<section class="timeline timeline-empty">Select a run.</section>`;
  }
  return html`
    <section class="timeline" aria-label="Run timeline">
      <div class="timeline-head">
        <span>Timeline</span>
        <span class="timeline-head-actions">
          ${loading ? html`<span class="timeline-state">loading</span>` : null}
          ${partial ? html`<span class="timeline-state warn">partial</span>` : null}
          <${CancelControls} run=${selectedRun} onCancelled=${() => {}} />
        </span>
      </div>
      <div class="timeline-scroll">
        ${items.length === 0 && !loading ? html`<div class="timeline-empty">No timeline items yet.</div>` : null}
        ${items.map((it, i) => {
          if (it.kind === 'chat' && it.chat) {
            const msg = {
              id: it.chat.msg_id,
              from: it.chat.from,
              content: it.chat.content || '',
              reply_to: it.chat.reply_to || undefined,
              created_at: it.at ? new Date(it.at).toISOString() : '',
              reactions: {},
            };
            return html`<${MessageBubble} key=${`chat-${msg.id || i}`} msg=${msg} readOnly=${true} />`;
          }
          return html`<${ActivityLine} key=${`activity-${it.at || i}-${i}`} item=${it} />`;
        })}
        ${error ? html`<div class="timeline-error">${error}</div>` : null}
      </div>
      <${ThreadReplyBox} run=${selectedRun} onReply=${onReply} />
    </section>
  `;
}

export function WatchView() {
  const [runs, setRuns] = useState([]);
  const [selected, setSelected] = useState(null);
  const [items, setItems] = useState([]);
  const [loadingRuns, setLoadingRuns] = useState(true);
  const [loadingRun, setLoadingRun] = useState(false);
  const [partial, setPartial] = useState(false);
  const [feedError, setFeedError] = useState('');
  const [timelineError, setTimelineError] = useState('');
  const [showDispatch, setShowDispatch] = useState(false);
  const [showTeam, setShowTeam] = useState(false);
  const [showEnv, setShowEnv] = useState(false);
  const selectedRef = useRef(null);
  selectedRef.current = selected;
  const selectedRun = runs.find((r) => r.run_id === selected) || null;

  useEffect(() => {
    let alive = true;
    setLoadingRuns(true);
    runsList().then((rs) => {
      if (!alive) return;
      setRuns(rs);
      setFeedError('');
      if (rs.length && !selectedRef.current) setSelected(rs[0].run_id);
    }).catch((e) => {
      if (alive) setFeedError(e.message || 'runs.list failed');
    }).finally(() => {
      if (alive) setLoadingRuns(false);
    });

    const off = onPushKind('runs.update', (payload) => {
      const r = payload && (payload.run || payload);
      if (!r || !r.run_id) return;
      setRuns((prev) => {
        const idx = prev.findIndex((x) => x.run_id === r.run_id);
        if (idx >= 0) {
          const next = prev.slice();
          next[idx] = { ...next[idx], ...r };
          return next;
        }
        return [r, ...prev];
      });
      if (!selectedRef.current) setSelected(r.run_id);
    });
    return () => {
      alive = false;
      off();
    };
  }, []);

  useEffect(() => {
    if (!selected) {
      setItems([]);
      return;
    }
    let alive = true;
    let unsubscribe = null;
    setLoadingRun(true);
    setTimelineError('');
    setPartial(false);

    runGet(selected).then((res) => {
      if (!alive) return;
      const timeline = res.timeline || [];
      setItems(timeline);
      setPartial(!!res.partial);
      const agent = res.run && res.run.agent;
      if (agent) {
        unsubscribe = subscribe('subscribe.observe', { aspect: agent }, (payload) => {
          if (!alive || !payload || payload.aspect !== agent) return;
          const item = activityFromObservePayload(payload);
          if (!item) return;
          if (item.run_id && item.run_id !== selected) return;
          setItems((prev) => [...prev, item]);
        });
      }
    }).catch((e) => {
      if (alive) {
        setItems([]);
        setTimelineError(e.message || 'run.get failed');
      }
    }).finally(() => {
      if (alive) setLoadingRun(false);
    });

    return () => {
      alive = false;
      if (unsubscribe) unsubscribe();
    };
  }, [selected]);

  return html`
    <div class="watch">
      <div class="watch-toolbar">
        <div class="watch-title">
          <span class="watch-kicker">Watch</span>
          <span class="watch-selected">${selected || 'No run selected'}</span>
        </div>
        <div class="watch-actions">
          <button class=${showDispatch ? 'panel-toggle on' : 'panel-toggle'} onClick=${() => setShowDispatch((v) => !v)}>+ Dispatch</button>
          <button class=${showTeam ? 'panel-toggle on' : 'panel-toggle'} onClick=${() => setShowTeam((v) => !v)}>Team</button>
          <button class=${showEnv ? 'panel-toggle on' : 'panel-toggle'} onClick=${() => setShowEnv((v) => !v)}>Env</button>
        </div>
      </div>
      <div class="watch-body">
        ${showDispatch ? html`<${DispatchComposePanel} onClose=${() => setShowDispatch(false)} onPosted=${() => runsList().then(setRuns).catch(() => {})} />` : null}
        ${showTeam ? html`<${TeamPanel} onClose=${() => setShowTeam(false)} />` : null}
        <${RunFeed} runs=${runs} selected=${selected} onSelect=${setSelected} loading=${loadingRuns} error=${feedError} />
        <${Timeline}
          items=${items}
          selected=${selected}
          selectedRun=${selectedRun}
          loading=${loadingRun}
          partial=${partial}
          error=${timelineError}
          onReply=${(item) => setItems((prev) => [...prev, item])}
        />
        ${showEnv ? html`<${EnvHealthPanel} onClose=${() => setShowEnv(false)} />` : null}
      </div>
    </div>
  `;
}
