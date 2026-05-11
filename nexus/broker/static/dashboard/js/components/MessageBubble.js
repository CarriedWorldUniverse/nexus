const { h, html, useState, useEffect } = window.__preact;

import { agentColors, replyTo } from '../state.js';
import { toggleReaction } from '../api.js';
import { marked } from '/js/vendor/marked.js';
import DOMPurify from '/js/vendor/dompurify.js';

const QUICK_EMOJI = ['👍', '✅', '👀', '❤️', '🎉', '🤔'];

// Display priority for reaction emojis. Earlier = shown first in the
// reaction strip. The first two are the "work signal" emojis (per
// #189 decision 2026-05-12) — they get top-row visibility so the
// operator can scan who's actively working vs. just acknowledged.
// Anything not listed sorts to the end in insertion order.
const REACTION_PRIORITY = ['👀', '👍'];
function reactionRank(emoji) {
  const i = REACTION_PRIORITY.indexOf(emoji);
  return i === -1 ? REACTION_PRIORITY.length : i;
}

// normalizeReactions accepts the server's ReactionRow[] shape
// ([{aspect, emoji}, ...]) and returns the same array, sorted by
// priority. Tolerates the legacy grouped {emoji: [agents]} shape too
// (one-time during cutover) by flattening it back to the row shape —
// the moment any new reaction lands via WS push, the legacy data is
// replaced.
function normalizeReactions(raw) {
  if (!raw) return [];
  if (Array.isArray(raw)) {
    return [...raw].sort((a, b) => {
      const ar = reactionRank(a.emoji);
      const br = reactionRank(b.emoji);
      if (ar !== br) return ar - br;
      return (a.emoji || '').localeCompare(b.emoji || '');
    });
  }
  // Legacy grouped shape — flatten. Each (emoji, [agents]) becomes
  // one row per agent. Stable enough for the brief cutover window.
  const out = [];
  for (const [emoji, agents] of Object.entries(raw)) {
    if (!Array.isArray(agents)) continue;
    for (const agent of agents) {
      out.push({ aspect: agent, emoji });
    }
  }
  return out.sort((a, b) => {
    const ar = reactionRank(a.emoji);
    const br = reactionRank(b.emoji);
    if (ar !== br) return ar - br;
    return (a.emoji || '').localeCompare(b.emoji || '');
  });
}

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

  // Server shape (post-2026-05-12 single-emoji-per-reactor rule):
  // ReactionRow[] = [{aspect, emoji}, ...]. Each reactor has at most
  // one entry per msg. Display is per-row (one chip per row) so the
  // operator can see WHO reacted with WHAT at a glance — that's the
  // observability point of reactions in this network ("eyes-on by
  // keel, thumbs-up by plumb").
  //
  // Legacy {emoji: [agents]} shape is gone — that was a count-style
  // render for stacked human reactions. With single-emoji-per-reactor
  // there's nothing to count; attribution IS the display.
  const [reactions, setReactions] = useState(normalizeReactions(msg.reactions));
  const [pickerOpen, setPickerOpen] = useState(false);
  const [reacting, setReacting] = useState(false);

  // Re-sync when the underlying message changes (poll refresh, etc.)
  useEffect(() => {
    setReactions(normalizeReactions(msg.reactions));
  }, [msg.id, JSON.stringify(msg.reactions)]);

  function handleReply() {
    replyTo.value = msg;
  }

  async function react(emoji) {
    if (reacting) return; // ignore rapid double-clicks while a request is in flight
    setPickerOpen(false);
    setReacting(true);

    // Optimistic update under single-emoji-per-reactor rule (#189):
    //   - operator already has THIS emoji → remove their row
    //   - operator already has a DIFFERENT emoji → replace it
    //   - operator has no row → add this one
    // The server enforces the same rule on commit; we mirror locally
    // so the UI feels instant. Roll back on failure.
    const prev = reactions;
    const myExisting = prev.find(r => r.aspect === 'operator');
    let next;
    if (myExisting && myExisting.emoji === emoji) {
      next = prev.filter(r => r.aspect !== 'operator');
    } else {
      next = prev.filter(r => r.aspect !== 'operator');
      next.push({ aspect: 'operator', emoji });
    }
    setReactions(normalizeReactions(next));

    try {
      // Server returns confirmed state via chat.reaction.update push,
      // not via the toggle response. Just fire and trust the rule.
      await toggleReaction(msg.id, 'operator', emoji);
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
  const isReplying = replyTo.value?.id === msg.id;

  // Tap whole bubble to set reply target (toggle: tap again to cancel).
  // Inner controls (reactions/buttons/replies-link/content) call
  // stopPropagation so they don't also trigger reply. Operator may want
  // to interject into agent-only chatter, so tap-to-reply works there too.
  function handleBubbleClick(e) {
    if (replyTo.value && replyTo.value.id === msg.id) {
      replyTo.value = null;
    } else {
      handleReply();
    }
  }
  const stop = (e) => e.stopPropagation();

  return html`
    <div class=${'msg' + (compact ? ' compact' : '') + (agentOnly ? ' agent-only' : '') + ' tappable' + (isReplying ? ' replying' : '')} id=${`msg-${msg.id}`}
      role="button"
      tabindex="0"
      aria-label=${`Reply to ${msg.from}`}
      onClick=${handleBubbleClick}
      onKeyDown=${(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handleReply(); } }}>
      <div class="msg-avatar" style=${{ background: color }}>
        ${initials}
      </div>
      <div class="msg-body">
        ${!compact && html`
          <div class="msg-head">
            <span class="msg-from" style=${{ color }}>${msg.from}</span>
            <span class="msg-time">${formatTime(msg.created_at || msg.received_at || msg.at)}</span>
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
        ${msg.reply_count > 0 && onReply && html`
          <span class="msg-replies" onClick=${(e) => { stop(e); onReply(msg); }}>
            ${msg.reply_count} ${msg.reply_count === 1 ? 'reply' : 'replies'}
          </span>
        `}
        ${reactions.length > 0 && html`
          <div class="msg-reactions" onClick=${stop}>
            ${reactions.map((r) => {
              const mine = r.aspect === 'operator';
              return html`
                <button
                  class=${'reaction-chip' + (mine ? ' mine' : '')}
                  title=${r.aspect}
                  onClick=${(e) => { stop(e); react(r.emoji); }}
                >
                  <span class="reaction-emoji">${r.emoji}</span>
                  <span class="reaction-attr">${r.aspect}</span>
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
