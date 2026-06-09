const { html, useEffect, useRef, useState } = window.__preact;

import { runsList, runGet } from '../../api.js';
import { onPushKind, subscribe } from '../../comms.js';
import { MessageBubble } from '../../components/MessageBubble.js';

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

function summarizeRun(run) {
  if (run.command) return run.command;
  if (run.pr_url) return run.pr_url;
  return run.status || 'run';
}

function activityFromObservePayload(payload, runId) {
  const frame = payload && payload.frame;
  if (!frame || typeof frame !== 'object') return null;
  if (frame.run_id && frame.run_id !== runId) return null;
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

function formatTime(ms) {
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '';
  const d = new Date(n);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

function ActivityLine({ item }) {
  const activity = item.activity || {};
  const type = activity.type || 'activity';
  const text = activity.text || activity.tool || activity.state || '';
  return html`
    <div class=${`m-act m-act-${type}`}>
      <span class="m-act-at">${formatTime(item.at)}</span>
      <span class="m-act-kind">${type}</span>
      ${text ? html`<span class="m-act-text">${text}</span>` : null}
    </div>
  `;
}

export function MobileRuns() {
  const [runs, setRuns] = useState([]);
  const [open, setOpen] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    let alive = true;
    setLoading(true);
    runsList().then((rows) => {
      if (!alive) return;
      setRuns(rows);
      setError('');
    }).catch((e) => {
      if (alive) setError(e.message || 'runs.list failed');
    }).finally(() => {
      if (alive) setLoading(false);
    });

    const off = onPushKind('runs.update', (payload) => {
      const run = payload && (payload.run || payload);
      if (!run || !run.run_id) return;
      setRuns((prev) => {
        const idx = prev.findIndex((x) => x.run_id === run.run_id);
        if (idx >= 0) {
          const next = prev.slice();
          next[idx] = { ...next[idx], ...run };
          return next;
        }
        return [run, ...prev];
      });
    });

    return () => {
      alive = false;
      off();
    };
  }, []);

  if (open) return html`<${MobileRunDetail} runId=${open} onBack=${() => setOpen(null)} />`;

  return html`
    <section class="m-runs" aria-label="Runs">
      <header class="m-view-head">
        <span>Runs</span>
        ${loading ? html`<span>loading</span>` : null}
      </header>
      <div class="m-run-list">
        ${runs.map((run) => html`
          <button key=${run.run_id} type="button" class="m-run-card" onClick=${() => setOpen(run.run_id)}>
            <span class=${`m-run-dot ${statusClass(run.status)}`} title=${run.status || 'unknown'}></span>
            <span class="m-run-main">
              <span class="m-run-line">
                <span class="m-run-agent">${run.agent || 'unassigned'}</span>
                <span class="m-run-ticket">${run.ticket || run.thread || run.run_id}</span>
              </span>
              <span class="m-run-cmd">${summarizeRun(run)}</span>
            </span>
            <span class="m-run-status">${run.status || 'unknown'}</span>
          </button>
        `)}
        ${!loading && runs.length === 0 ? html`<div class="m-empty">No runs yet.</div>` : null}
        ${error ? html`<div class="m-error">${error}</div>` : null}
      </div>
    </section>
  `;
}

function MobileRunDetail({ runId, onBack }) {
  const [items, setItems] = useState([]);
  const [run, setRun] = useState(null);
  const [loading, setLoading] = useState(true);
  const [partial, setPartial] = useState(false);
  const [error, setError] = useState('');
  const streamRef = useRef(null);

  useEffect(() => {
    let alive = true;
    let unsubscribe = null;
    setLoading(true);
    setError('');
    setPartial(false);
    setItems([]);

    runGet(runId).then((res) => {
      if (!alive) return;
      setRun(res.run || null);
      setItems(res.timeline || []);
      setPartial(!!res.partial);
      const agent = res.run && res.run.agent;
      if (agent) {
        unsubscribe = subscribe('subscribe.observe', { aspect: agent }, (payload) => {
          if (!alive || !payload || payload.aspect !== agent) return;
          const item = activityFromObservePayload(payload, runId);
          if (!item) return;
          setItems((prev) => [...prev, item]);
        });
      }
    }).catch((e) => {
      if (alive) setError(e.message || 'run.get failed');
    }).finally(() => {
      if (alive) setLoading(false);
    });

    return () => {
      alive = false;
      if (unsubscribe) unsubscribe();
    };
  }, [runId]);

  useEffect(() => {
    const el = streamRef.current;
    if (!el) return;
    requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
  }, [items.length]);

  return html`
    <section class="m-run-detail" aria-label="Run detail">
      <header class="m-pane-head">
        <button type="button" class="m-back" aria-label="Back to runs" onClick=${onBack}>‹</button>
        <div class="m-pane-title">
          <span>${run ? (run.ticket || run.run_id) : runId}</span>
          ${run ? html`<small>${run.agent || 'unassigned'} · ${run.status || 'unknown'}</small>` : null}
        </div>
      </header>
      <div class="m-pane-stream" ref=${streamRef}>
        ${loading ? html`<div class="m-empty">Loading timeline.</div>` : null}
        ${partial ? html`<div class="m-warn">Partial timeline.</div>` : null}
        ${items.length === 0 && !loading ? html`<div class="m-empty">No timeline items yet.</div>` : null}
        ${items.map((item, i) => {
          if (item.kind === 'chat' && item.chat) {
            const msg = {
              id: item.chat.msg_id || `chat-${i}`,
              from: item.chat.from,
              content: item.chat.content || '',
              reply_to: item.chat.reply_to || 0,
              created_at: item.at ? new Date(item.at).toISOString() : '',
              reactions: {},
            };
            return html`<${MessageBubble} key=${`chat-${msg.id}`} msg=${msg} readOnly=${true} />`;
          }
          return html`<${ActivityLine} key=${`act-${item.at || i}-${i}`} item=${item} />`;
        })}
        ${error ? html`<div class="m-error">${error}</div>` : null}
      </div>
    </section>
  `;
}
