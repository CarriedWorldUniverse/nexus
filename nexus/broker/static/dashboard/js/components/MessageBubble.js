const { h, html, useState, useEffect } = window.__preact;

import { agentColors, replyTo } from '../state.js';
import { toggleReaction } from '../api.js';
import { marked } from '/js/vendor/marked.js';
import DOMPurify from '/js/vendor/dompurify.js';

const QUICK_EMOJI = ['👍', '✅', '👀', '❤️', '🎉', '🤔'];

function escapeHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

function formatTime(dateStr) {
  if (!dateStr) return '';
  // agent-network sent naive UTC like "2024-01-01 12:00:00" — needed
  // a 'Z' suffix to parse as UTC. nexus emits RFC 3339 like
  // "2024-01-01T12:00:00Z" — already terminated. Append only when
  // missing so both sources round-trip cleanly; otherwise the double-Z
  // produces an Invalid Date.
  const isISO = /Z$|[+-]\d\d:?\d\d$/.test(dateStr);
  const d = new Date(isISO ? dateStr : dateStr + 'Z');
  if (isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

function renderContent(content) {
  // Extract custom tokens before markdown parse so they aren't mangled.
  // Use a UUID-keyed placeholder that can't appear in normal message text.
  // Each token's HTML is sanitized individually before storage so the final
  // restore after DOMPurify doesn't reintroduce unsanitized content.
  const tokens = [];
  const KEY = crypto.randomUUID().replace(/-/g, '');

  let text = content
    // [image:URL]
    .replace(/\[image:([^\]]+)\]/g, (_, url) => {
      let html;
      try {
        const parsed = new URL(url, window.location.origin);
        if (!['http:', 'https:'].includes(parsed.protocol)) throw new Error();
        const safe = url.replace(/"/g, '%22');
        html = DOMPurify.sanitize(`<img src="${safe}" alt="image" loading="lazy" class="msg-img" />`);
      } catch {
        html = '[invalid image]';
      }
      tokens.push(html);
      return `${KEY}${tokens.length - 1}`;
    })
    // [file:ID:name]
    .replace(/\[file:(\d+):([^\]]+)\]/g, (_, id, name) => {
      tokens.push(DOMPurify.sanitize(
        `<a href="/api/files/${id}" download="${escapeHtml(name)}" style="color:#5865f2;text-decoration:underline">${escapeHtml(name)}</a>`,
        { ADD_ATTR: ['download'] }
      ));
      return `${KEY}${tokens.length - 1}`;
    })
    // @mentions — extract so markdown doesn't italicise the @ symbol
    .replace(/@([\w-]+)/g, (_, name) => {
      const colors = agentColors.value;
      const color = colors[name] || '#bb86fc';
      tokens.push(DOMPurify.sanitize(
        `<span style="color:${color};font-weight:600">@${escapeHtml(name)}</span>`
      ));
      return `${KEY}${tokens.length - 1}`;
    })
    // #NNNN message id refs — clickable, navigates to the referenced
    // message via Chat's delegated click handler. 3+ digit threshold so
    // ticket numbers like #86 don't match (those should stay literal).
    // When the referenced message is in window.__msgCache (populated by
    // Chat.js loadMessages), set a tooltip with @from + first 80 chars of
    // content so the operator gets context without having to navigate.
    // Cache miss → no tooltip; click still navigates if the message is in
    // the rendered DOM, falls through otherwise.
    .replace(/(^|\s)#(\d{3,})\b/g, (m, lead, id) => {
      const cache = (typeof window !== 'undefined' && window.__msgCache) || {};
      const cached = cache[Number(id)];
      let titleAttr = '';
      if (cached) {
        const preview = (cached.content || '').replace(/\s+/g, ' ').slice(0, 80);
        const from = cached.from || '?';
        // Encode " and < to prevent attribute-injection; DOMPurify also
        // sanitizes title attrs but defense-in-depth.
        const safe = `@${from}: ${preview}`.replace(/[&<>"']/g, c => ({
          '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
        }[c]));
        titleAttr = ` title="${safe}"`;
      }
      tokens.push(DOMPurify.sanitize(
        `<a href="#msg-${id}" data-msg-ref="${id}" class="msg-id-ref"${titleAttr}>#${id}</a>`,
        { ADD_ATTR: ['data-msg-ref'] }
      ));
      return `${lead}${KEY}${tokens.length - 1}`;
    });

  // Render markdown then sanitize
  const md = marked.parse(text, { breaks: true, gfm: true });
  let out = DOMPurify.sanitize(md, { ADD_ATTR: ['download'] });

  // Restore pre-sanitized custom tokens
  tokens.forEach((tok, i) => {
    out = out.replace(`${KEY}${i}`, tok);
  });

  return out;
}

export function MessageBubble({ msg, compact, parentMsg, onReply, agentOnly }) {
  const colors = agentColors.value;
  const color = colors[msg.from] || '#bb86fc';
  const initials = (msg.from || '??').slice(0, 2).toUpperCase();

  // Local optimistic reactions state; server shape: { emoji: [agent, agent, ...] }
  const [reactions, setReactions] = useState(msg.reactions || {});
  const [pickerOpen, setPickerOpen] = useState(false);
  const [reacting, setReacting] = useState(false);

  // Re-sync when the underlying message changes (poll refresh, etc.)
  useEffect(() => {
    setReactions(msg.reactions || {});
  }, [msg.id, JSON.stringify(msg.reactions || {})]);

  function handleReply() {
    replyTo.value = msg;
  }

  async function react(emoji) {
    if (reacting) return; // ignore rapid double-clicks while a request is in flight
    setPickerOpen(false);
    setReacting(true);
    // Optimistic toggle
    const prev = reactions;
    const cur = { ...prev };
    const list = (cur[emoji] || []).slice();
    const i = list.indexOf('operator');
    if (i >= 0) list.splice(i, 1); else list.push('operator');
    if (list.length === 0) delete cur[emoji]; else cur[emoji] = list;
    setReactions(cur);
    try {
      const res = await toggleReaction(msg.id, 'operator', emoji);
      if (res && res.agents) setReactions(res.agents);
    } catch (e) {
      // Roll back on failure
      setReactions(prev);
      console.error('reaction failed', e);
    } finally {
      setReacting(false);
    }
  }

  function scrollToParent() {
    if (!parentMsg) return;
    const el = document.getElementById(`msg-${parentMsg.id}`);
    if (el) el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }

  const rendered = renderContent(msg.content || '');
  const reactionEntries = Object.entries(reactions || {}).filter(([, agents]) => agents && agents.length);
  const isReplying = replyTo.value?.id === msg.id;

  // Tap whole bubble to set reply target (toggle: tap again to cancel).
  // Inner controls (reactions/buttons/replies-link/content) call
  // stopPropagation so they don't also trigger reply. agentOnly mode keeps
  // its own click-to-expand behaviour.
  function handleBubbleClick(e) {
    if (agentOnly) {
      e.currentTarget.classList.toggle('expanded');
      return;
    }
    if (replyTo.value && replyTo.value.id === msg.id) {
      replyTo.value = null;
    } else {
      handleReply();
    }
  }
  const stop = (e) => e.stopPropagation();

  return html`
    <div class=${'msg' + (compact ? ' compact' : '') + (agentOnly ? ' agent-only' : '') + (!agentOnly ? ' tappable' : '') + (isReplying ? ' replying' : '')} id=${`msg-${msg.id}`}
      role=${agentOnly ? undefined : 'button'}
      tabindex=${agentOnly ? undefined : '0'}
      aria-label=${agentOnly ? undefined : `Reply to ${msg.from}`}
      onClick=${handleBubbleClick}
      onKeyDown=${agentOnly ? undefined : (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handleReply(); } }}>
      <div class="msg-avatar" style=${{ background: color }}>
        ${initials}
      </div>
      <div class="msg-body">
        ${!compact && html`
          <div class="msg-head">
            <span class="msg-from" style=${{ color }}>${msg.from}</span>
            <span class="msg-time">${formatTime(msg.at)}</span>
          </div>
        `}
        ${parentMsg && html`
          <div class="msg-quote" onClick=${(e) => { stop(e); scrollToParent(); }}>
            ${parentMsg.from}: ${(parentMsg.content || '').slice(0, 80)}
          </div>
        `}
        <div
          class="msg-content"
          onClick=${stop}
          dangerouslySetInnerHTML=${{ __html: rendered }}
        />
        ${msg.reply_count > 0 && html`
          <span class="msg-replies" onClick=${(e) => { stop(e); onReply && onReply(msg); }}>
            ${msg.reply_count} ${msg.reply_count === 1 ? 'reply' : 'replies'}
          </span>
        `}
        ${reactionEntries.length > 0 && html`
          <div class="msg-reactions" onClick=${stop}>
            ${reactionEntries.map(([emoji, agents]) => {
              const mine = agents.includes('operator');
              return html`
                <button
                  class=${'reaction-chip' + (mine ? ' mine' : '')}
                  title=${agents.join(', ')}
                  onClick=${(e) => { stop(e); react(emoji); }}
                >
                  <span class="reaction-emoji">${emoji}</span>
                  <span class="reaction-count">${agents.length}</span>
                </button>
              `;
            })}
          </div>
        `}
      </div>
      <div class=${'msg-actions' + (pickerOpen ? ' picker-open' : '')} onClick=${stop}>
        <button onClick=${(e) => { stop(e); handleReply(); }}>Reply</button>
        <button class="msg-react-btn" onClick=${(e) => { stop(e); setPickerOpen(p => !p); }}>☺</button>
        ${pickerOpen && html`
          <div class="reaction-picker" onClick=${stop}>
            ${QUICK_EMOJI.map(em => html`
              <button class="reaction-picker-item" onClick=${(e) => { stop(e); react(em); }}>${em}</button>
            `)}
          </div>
        `}
      </div>
    </div>
  `;
}
