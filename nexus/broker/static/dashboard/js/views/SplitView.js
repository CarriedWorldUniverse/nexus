const { html, useState, useEffect, useRef } = window.__preact;
import { ConverseView } from './ConverseView.js';
import { ObserveView } from './ObserveView.js';
import { FilesView } from './FilesView.js';
import { Tickets } from './Tickets.js';
import { Status } from './Status.js';
import { DocsView } from './DocsView.js';

// Views hostable inside a split pane. Same component instances as the main
// routes — they listen to the same singleton signals, so one WS / SSE / poll
// stack feeds both panes. Chat-like views (Converse) share currentChannel
// globally; using the same view in both panes will keep them in sync.
const PANE_VIEWS = [
  { id: 'converse', label: 'Converse', Component: ConverseView },
  { id: 'docs',     label: 'Docs',     Component: DocsView },
  { id: 'status',   label: 'Status',   Component: Status },
  { id: 'agents',   label: 'Activity', Component: ObserveView },
  { id: 'tickets',  label: 'Tickets',  Component: Tickets },
  { id: 'files',    label: 'Files',    Component: FilesView },
];

const VIEW_BY_ID = Object.fromEntries(PANE_VIEWS.map(v => [v.id, v]));

const LS_LEFT = 'split.left.view';
const LS_RIGHT = 'split.right.view';
const LS_RATIO = 'split.ratio';

function loadRatio() {
  const raw = parseFloat(localStorage.getItem(LS_RATIO));
  if (!Number.isFinite(raw) || raw < 0.1 || raw > 0.9) return 0.5;
  return raw;
}

function Pane({ viewId, onChangeView, onSwap }) {
  const entry = VIEW_BY_ID[viewId] || PANE_VIEWS[0];
  const View = entry.Component;
  return html`
    <div class="split-pane">
      <div class="split-pane-header">
        <select
          class="split-pane-picker"
          value=${entry.id}
          onChange=${e => onChangeView(e.target.value)}
          aria-label="Pane view"
        >
          ${PANE_VIEWS.map(v => html`<option value=${v.id}>${v.label}</option>`)}
        </select>
        <button class="split-pane-swap" onClick=${onSwap} title="Swap panes" aria-label="Swap panes">⇄</button>
      </div>
      <div class="split-pane-body">
        <${View} />
      </div>
    </div>
  `;
}

export function SplitView() {
  const [left, setLeft] = useState(() => {
    const v = localStorage.getItem(LS_LEFT) || 'converse';
    return (v === 'terminal' || v === 'feed') ? 'converse' : v;
  });
  const [right, setRight] = useState(() => {
    const v = localStorage.getItem(LS_RIGHT) || 'agents';
    // Heal saved 'terminal' references — Terminal view was removed in Phase D.
    return v === 'terminal' ? 'agents' : (v === 'feed' ? 'converse' : v);
  });
  const [ratio, setRatio] = useState(loadRatio);
  const [vertical, setVertical] = useState(() => (typeof window !== 'undefined' && window.innerWidth < 720));
  const containerRef = useRef(null);
  const dragging = useRef(false);

  useEffect(() => { localStorage.setItem(LS_LEFT, left); }, [left]);
  useEffect(() => { localStorage.setItem(LS_RIGHT, right); }, [right]);
  useEffect(() => { localStorage.setItem(LS_RATIO, String(ratio)); }, [ratio]);

  useEffect(() => {
    if (!containerRef.current) return;
    const ro = new ResizeObserver(() => {
      const w = containerRef.current?.getBoundingClientRect().width ?? window.innerWidth;
      setVertical(w < 720);
    });
    ro.observe(containerRef.current);
    return () => ro.disconnect();
  }, []);

  function onDividerPointerDown(e) {
    e.preventDefault();
    const container = containerRef.current;
    if (!container) return;
    const target = e.currentTarget;
    try { target.setPointerCapture(e.pointerId); } catch {}
    dragging.current = true;

    function move(ev) {
      if (!dragging.current) return;
      const r = container.getBoundingClientRect();
      const vertical = r.width < 720;
      const pos = vertical
        ? (ev.clientY - r.top) / r.height
        : (ev.clientX - r.left) / r.width;
      const clamped = Math.max(0.1, Math.min(0.9, pos));
      setRatio(clamped);
    }
    function cleanup() {
      dragging.current = false;
      try { target.releasePointerCapture(e.pointerId); } catch {}
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', cleanup);
      target.removeEventListener('pointercancel', cleanup);
      target.removeEventListener('lostpointercapture', cleanup);
    }
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', cleanup);
    target.addEventListener('pointercancel', cleanup);
    target.addEventListener('lostpointercapture', cleanup);
  }

  function swap() {
    setLeft(right);
    setRight(left);
  }

  const leftFlex = ratio;
  const rightFlex = 1 - ratio;

  return html`
    <div class="split-view" ref=${containerRef}>
      <div class="split-pane-wrap" style=${`flex: ${leftFlex} 1 0;`}>
        <${Pane} viewId=${left} onChangeView=${setLeft} onSwap=${swap} />
      </div>
      <div
        class="split-divider"
        onPointerDown=${onDividerPointerDown}
        role="separator"
        aria-orientation=${vertical ? 'horizontal' : 'vertical'}
        aria-valuenow=${Math.round(ratio * 100)}
        aria-valuemin="10"
        aria-valuemax="90"
      ></div>
      <div class="split-pane-wrap" style=${`flex: ${rightFlex} 1 0;`}>
        <${Pane} viewId=${right} onChangeView=${setRight} onSwap=${swap} />
      </div>
    </div>
  `;
}
