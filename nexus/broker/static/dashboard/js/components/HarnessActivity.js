const { html, useState, useEffect, useRef } = window.__preact;
import { subscribe, getSnapshot } from '../harness-stream-store.js';

/**
 * Live activity pane for harness-runtime agents.
 *
 * Reads from harness-stream-store, which owns one persistent EventSource per
 * agent at module scope. Events buffered there survive view unmounts, so
 * navigating Terminal → Feed → Terminal no longer drops history.
 */
export function HarnessActivity({ agentId }) {
  const [, forceRender] = useState(0);
  const scrollRef = useRef(null);
  // Sticky-bottom: default true (stay pinned). Flip to false only when the
  // user deliberately scrolls up; flip back when they return near the bottom.
  // Without this, tabbing back to Terminal mounts at scrollTop=0 and the
  // nearBottom check never passes, so history never auto-scrolls into view.
  const stickBottomRef = useRef(true);

  useEffect(() => {
    if (!agentId) return;
    stickBottomRef.current = true;
    const unsub = subscribe(agentId, () => forceRender(v => v + 1));
    return unsub;
  }, [agentId]);

  const snap = getSnapshot(agentId);
  const { events, connected, error } = snap;

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const onScroll = () => {
      const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
      stickBottomRef.current = nearBottom;
    };
    el.addEventListener('scroll', onScroll, { passive: true });
    return () => el.removeEventListener('scroll', onScroll);
  }, []);

  // Auto-scroll after render — on mount (covers tab-back) and on every new
  // event — unless the user has deliberately scrolled up.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (stickBottomRef.current) el.scrollTop = el.scrollHeight;
  });

  return html`
    <div class="harness-activity">
      <div class="harness-activity-header">
        <span class=${'harness-activity-dot ' + (connected ? 'connected' : 'disconnected')}></span>
        <span class="harness-activity-label">Harness activity — @${agentId}</span>
        ${error ? html`<span class="harness-activity-error">${error}</span>` : null}
      </div>
      <div class="harness-activity-feed" ref=${scrollRef}>
        ${events.length === 0
          ? html`<div class="harness-activity-empty">waiting for turn…</div>`
          : renderWithSeededDivider(events)}
      </div>
    </div>
  `;
}

// Inserts a "— live —" divider between seeded (history) events and the first
// non-seeded (live) event, so the operator can see where replay ends and
// real-time begins.
function renderWithSeededDivider(events) {
  const out = [];
  let dividerInserted = false;
  const anySeeded = events.some(e => e._seeded);
  for (let i = 0; i < events.length; i++) {
    const ev = events[i];
    if (anySeeded && !dividerInserted && !ev._seeded) {
      out.push(html`<div key="live-divider" class="harness-activity-row live-divider">
        <span class="har-sep">╌╌╌</span>
        <span class="har-label">live</span>
      </div>`);
      dividerInserted = true;
    }
    out.push(html`<${ActivityRow} key=${ev._seq} ev=${ev} />`);
  }
  return out;
}

function ActivityRow({ ev }) {
  const seeded = ev._seeded ? '1' : null;
  if (ev.kind === 'turn_start') {
    return html`
      <div class="harness-activity-row turn-start" data-seeded=${seeded}>
        <span class="har-sep">━━━</span>
        <span class="har-label">turn start</span>
        <span class="har-meta">thread #${ev.threadId}${ev.msgId ? ` · msg #${ev.msgId}` : ''}</span>
      </div>
    `;
  }
  if (ev.kind === 'turn_end') {
    return html`
      <div class="harness-activity-row turn-end" data-seeded=${seeded}>
        <span class="har-sep">━━━</span>
        <span class="har-label">turn end</span>
      </div>
    `;
  }
  if (ev.kind === 'tool_use') {
    return html`
      <div class="harness-activity-row tool-use" data-seeded=${seeded}>
        <span class="har-icon">🔧</span>
        <span class="har-tool-name">${ev.name}</span>
        <code class="har-tool-input">${ev.input || ''}</code>
      </div>
    `;
  }
  if (ev.kind === 'tool_result') {
    return html`
      <div class=${'harness-activity-row tool-result' + (ev.is_error ? ' is-error' : '')} data-seeded=${seeded}>
        <span class="har-icon">${ev.is_error ? '❌' : '✅'}</span>
        <span class="har-tool-preview">${ev.preview || ''}</span>
      </div>
    `;
  }
  if (ev.kind === 'text') {
    return html`
      <div class="harness-activity-row text" data-seeded=${seeded}>
        <span class="har-icon">💭</span>
        <span class="har-text">${ev.text || ''}</span>
      </div>
    `;
  }
  return null;
}
