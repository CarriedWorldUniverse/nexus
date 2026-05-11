// TurnBlock — v0.1 stub for TurnFrame rendering.
//
// Phase D ships chat-only observability; bridle events that drive rich
// TurnFrame content (tool calls, thinking, artifacts) don't land until
// Phase E/F. The stub still renders something whenever a TurnFrame
// arrives so the operator gets a visible signal that a turn happened
// — header + footer dividers with the essentials (turn id, status,
// model). Rich event/tool detail is deliberately omitted until the
// renderers around it are ready. WHY a stub at all: the wire shape is
// already on the broker side, and silently dropping turn frames would
// hide observability bugs during Phase D bring-up.

const { html } = window.__preact;

export function TurnBlock({ payload, seq, ts }) {
  const turnID = payload.turn_id || '?';
  const status = payload.status || 'unknown';
  const model = payload.model || '';
  const provider = payload.provider || '';
  const trigger = payload.trigger_msg ? `triggered by #${payload.trigger_msg}` : '';

  const headBits = [`turn ${shortID(turnID)}`, status, model, provider, trigger].filter(Boolean);
  const footBits = footerBits(payload);

  return html`
    <div class=${'observe-turn observe-turn-status-' + status} data-seq=${seq}>
      <div class="observe-turn-head">
        <span class="observe-turn-rule" aria-hidden="true">┌──</span>
        <span class="observe-turn-bits">${headBits.join(' · ')}</span>
        <span class="observe-turn-rule observe-turn-rule-fill" aria-hidden="true"></span>
      </div>
      <div class="observe-turn-body">
        <span class="observe-turn-stub">turn detail deferred to Phase F</span>
      </div>
      <div class="observe-turn-foot">
        <span class="observe-turn-rule" aria-hidden="true">└──</span>
        ${footBits && html`<span class="observe-turn-bits">${footBits}</span>`}
        <span class="observe-turn-rule observe-turn-rule-fill" aria-hidden="true"></span>
      </div>
    </div>
  `;
}

function shortID(id) {
  if (!id) return '?';
  return id.length > 8 ? id.slice(0, 8) : id;
}

function footerBits(p) {
  const out = [];
  if (p.usage) {
    if (typeof p.usage.input_tokens === 'number')  out.push(p.usage.input_tokens + ' in');
    if (typeof p.usage.output_tokens === 'number') out.push(p.usage.output_tokens + ' out');
  }
  if (p.error) out.push('error: ' + p.error);
  return out.join(' · ');
}
