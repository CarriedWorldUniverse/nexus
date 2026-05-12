// TurnBlock — Phase F rich rendering of one observability.TurnFrame.
//
// Wire shape (nexus/observability/types.go):
//   { turn_id, label, status, started, ended?, trigger_msg?, model,
//     provider, events:[ {kind, text?, tool?, step?} ], usage?, error? }
//
// Renderer responsibilities:
//   - header: turn id + label pill + status pill + model · provider · trigger
//   - body: events in order — text runs as prose, tool calls via ToolCall,
//     steps as subtle separators, orphan results highlighted
//   - footer: usage stats (tokens, cache, duration, cost) + error (if any)
//
// Status semantics drive the left border colour:
//   in_flight = accent  ·  complete = subtle  ·  errored = red
//
// The label pill ("main" / "compact" / "filter-judge") lets operators
// see at a glance which call site emitted the turn. Useful when the
// same Grouper carries multiple labels concurrently (rare but legal
// per the audit's §5 concurrency check).

const { html } = window.__preact;

import { ToolCall } from './ToolCall.js';

export function TurnBlock({ payload, seq, ts }) {
  const turnID = payload.turn_id || '?';
  const label = payload.label || 'main';
  const status = payload.status || 'unknown';
  const model = payload.model || '';
  const provider = payload.provider || '';
  const trigger = payload.trigger_msg ? `triggered by #${payload.trigger_msg}` : '';
  const events = Array.isArray(payload.events) ? payload.events : [];

  const headBits = [model, provider, trigger].filter(Boolean);

  return html`
    <div class=${'observe-turn observe-turn-status-' + status} data-seq=${seq}>
      <div class="observe-turn-head">
        <span class="observe-turn-id">${shortID(turnID)}</span>
        <span class=${'observe-turn-label observe-turn-label-' + label}>${label}</span>
        <span class=${'observe-turn-status observe-turn-status-pill-' + status}>${status}</span>
        ${headBits.length > 0 && html`<span class="observe-turn-bits">${headBits.join(' · ')}</span>`}
      </div>
      <div class="observe-turn-body">
        ${events.length === 0
          ? html`<div class="observe-turn-pending">${status === 'in_flight' ? 'waiting on model…' : 'no events recorded'}</div>`
          : events.map((ev, i) => renderEvent(ev, i))}
        ${payload.error && html`<div class="observe-turn-error">${payload.error}</div>`}
      </div>
      <${TurnFooter} usage=${payload.usage} started=${payload.started} ended=${payload.ended} />
    </div>
  `;
}

function renderEvent(ev, idx) {
  switch (ev.kind) {
    case 'text':
      return html`<div class="observe-turn-text" key=${idx}>${ev.text || ''}</div>`;

    case 'tool_call':
      return html`<${ToolCall} key=${idx} tool=${ev.tool} />`;

    case 'tool_result_orphan':
      return html`<${ToolCall} key=${idx} tool=${ev.tool} isOrphan=${true} />`;

    case 'step':
      return html`
        <div class="observe-turn-step" key=${idx} aria-hidden="true">
          <span class="observe-turn-step-rule"></span>
          <span class="observe-turn-step-label">step ${ev.step || '?'}</span>
          <span class="observe-turn-step-rule"></span>
        </div>
      `;

    default:
      return null; // unknown event kind — drop quietly (forward-compat)
  }
}

function TurnFooter({ usage, started, ended }) {
  if (!usage && !ended) return null;
  const bits = [];
  if (usage) {
    if (typeof usage.input_tokens === 'number')  bits.push(usage.input_tokens.toLocaleString() + ' in');
    if (typeof usage.output_tokens === 'number') bits.push(usage.output_tokens.toLocaleString() + ' out');
    if (usage.cache_read)   bits.push(usage.cache_read.toLocaleString() + ' cache↺');
    if (usage.cache_create) bits.push(usage.cache_create.toLocaleString() + ' cache+');
    if (usage.cost_usd)     bits.push('$' + usage.cost_usd.toFixed(4));
    if (typeof usage.duration_ns === 'number') {
      const ms = Math.round(usage.duration_ns / 1e6);
      bits.push(ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's');
    }
  } else if (started && ended) {
    // Fallback: wall-clock duration when usage is absent (provider errored).
    const ms = new Date(ended).getTime() - new Date(started).getTime();
    if (Number.isFinite(ms) && ms >= 0) bits.push(ms + 'ms');
  }
  if (bits.length === 0) return null;
  return html`
    <div class="observe-turn-foot">
      <span class="observe-turn-bits">${bits.join(' · ')}</span>
    </div>
  `;
}

function shortID(id) {
  if (!id) return '?';
  // funnel.newTurnID() shape: "turn-YYYYMMDDTHHMMSS.uuuuuuZ-abc123"
  // — first 10 chars is the constant "turn-YYYYM" prefix which collides
  // across every turn in the same UTC minute. Use the random hex suffix
  // (everything after the final '-') so distinct turns are visually
  // distinct; fall back to the trailing 8 chars for non-conforming ids.
  const i = id.lastIndexOf('-');
  if (i > 0 && i < id.length - 1) return id.slice(i + 1);
  return id.length > 8 ? id.slice(-8) : id;
}
