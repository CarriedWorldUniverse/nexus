const { h, html, useRef, useEffect, useState } = window.__preact;

import { replyTo, currentChannel, agents, agentColors } from '../state.js';
import { sendMessage, uploadImage, BASE } from '../api.js';

let pendingImageUrl = null;
const draftText = {}; // keyed by channel, persists across tab switches

function getMentionState(ta) {
  // Returns { active, query, start } if cursor sits inside an @-mention token, else { active: false }.
  if (!ta) return { active: false };
  const pos = ta.selectionStart;
  if (pos !== ta.selectionEnd) return { active: false };
  const text = ta.value;
  const before = text.slice(0, pos);
  // Find last @ that's at start or after whitespace, with no whitespace between it and cursor.
  const m = before.match(/(^|\s)@([A-Za-z0-9_-]*)$/);
  if (!m) return { active: false };
  const query = m[2];
  const start = pos - query.length - 1;
  return { active: true, query, start };
}

export function ChatInput({ onSent }) {
  const textareaRef = useRef(null);
  const fileInputRef = useRef(null);
  const [mention, setMention] = useState({ active: false, query: '', start: 0, index: 0 });
  const [sendError, setSendError] = useState(null);
  const agentList = agents.value;
  const colors = agentColors.value;

  // Build mention targets: agents + @all + @operator. Dedupe.
  const mentionTargets = (() => {
    const ids = new Set(['all', 'operator']);
    agentList.forEach(a => {
      const id = typeof a === 'string' ? a : a?.id;
      if (id) ids.add(id);
    });
    return Array.from(ids);
  })();

  const filtered = mention.active
    ? mentionTargets.filter(id => id.toLowerCase().startsWith(mention.query.toLowerCase()))
    : [];

  function updateMentionFromTextarea() {
    const ta = textareaRef.current;
    const state = getMentionState(ta);
    setMention(prev => state.active
      ? { ...state, index: 0 }
      : { active: false, query: '', start: 0, index: 0 });
  }

  function insertMention(id) {
    const ta = textareaRef.current;
    if (!ta || !mention.active) return;
    const before = ta.value.slice(0, mention.start);
    const after = ta.value.slice(ta.selectionStart);
    const insert = `@${id} `;
    ta.value = before + insert + after;
    const newPos = before.length + insert.length;
    ta.setSelectionRange(newPos, newPos);
    setMention({ active: false, query: '', start: 0, index: 0 });
    ta.focus();
  }

  function handleSend() {
    const ta = textareaRef.current;
    let content = ta ? ta.value.trim() : '';
    if (!content && !pendingImageUrl) return;

    setSendError(null);

    const ch = currentChannel.value;
    const inDM = ch && ch !== 'general' && !ch.startsWith('topic:');
    const replying = !!replyTo.value;

    // Validation: in #general or topic channels with no reply target, the
    // message must address at least one known agent. DM channels auto-prefix
    // the @mention so validation is unnecessary there.
    if (!inDM && !replying) {
      const mentions = [...content.matchAll(/(?:^|\s)@([\w-]+)/g)].map(m => m[1]);
      if (mentions.length === 0) {
        setSendError(`No target. Address an agent (e.g. @${mentionTargets[2] || 'forge'}) or reply to a message — pick a bubble to set reply.`);
        return;
      }
      const validSet = new Set(mentionTargets);
      const unknown = mentions.find(name => !validSet.has(name));
      if (unknown) {
        setSendError(`Unknown agent: @${unknown}. Known: ${mentionTargets.join(', ')}.`);
        return;
      }
    }

    if (pendingImageUrl) {
      content = content ? `${content} [image:${pendingImageUrl}]` : `[image:${pendingImageUrl}]`;
    }

    if (!inDM) {
      const msgData = { from: 'operator', content, replyTo: replyTo.value?.id };
      if (ch && ch.startsWith('topic:')) {
        msgData.topic = ch.slice(6);
      }
      sendMessage(msgData);
    } else {
      // Agent DM — send as chat message with dm: topic and auto @mention
      const dmContent = content.startsWith(`@${ch}`) ? content : `@${ch} ${content}`;
      sendMessage({ from: 'operator', content: dmContent, topic: `dm:${ch}`, replyTo: replyTo.value?.id });
    }

    if (ta) {
      ta.value = '';
      ta.style.height = 'auto';
    }
    delete draftText[currentChannel.value || 'general'];
    pendingImageUrl = null;
    replyTo.value = null;
    const preview = document.getElementById('chatImgPreview');
    if (preview) preview.remove();
    if (onSent) onSent();
  }

  async function handleImageAttach(e) {
    const file = e.target.files[0];
    if (!file) return;
    try {
      const result = await uploadImage(file);
      pendingImageUrl = BASE + result.url;

      let preview = document.getElementById('chatImgPreview');
      const inputArea = textareaRef.current?.closest('.chat-input-area');
      if (!preview && inputArea) {
        preview = document.createElement('div');
        preview.id = 'chatImgPreview';
        preview.className = 'chat-img-preview';
        const row = inputArea.querySelector('.chat-input-row');
        if (row) inputArea.insertBefore(preview, row);
      }
      if (preview) {
        preview.innerHTML = `Image attached <span style="cursor:pointer" onclick="this.closest('.chat-img-preview').remove(); window.__clearPendingImage && window.__clearPendingImage()">✕</span>`;
      }
    } catch (err) {
      console.error('[ChatInput] uploadImage failed', err);
    }
    if (fileInputRef.current) fileInputRef.current.value = '';
  }

  // Expose clear so the ✕ button inline handler works
  window.__clearPendingImage = () => { pendingImageUrl = null; };

  function handleKeydown(e) {
    if (mention.active && filtered.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setMention(m => ({ ...m, index: (m.index + 1) % filtered.length }));
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        setMention(m => ({ ...m, index: (m.index - 1 + filtered.length) % filtered.length }));
        return;
      }
      if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault();
        insertMention(filtered[mention.index] || filtered[0]);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        setMention({ active: false, query: '', start: 0, index: 0 });
        return;
      }
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    } else if (e.key === 'Enter' && e.shiftKey) {
      // Let the newline insert, then resize on next tick
      setTimeout(() => autoResize(e.target), 0);
    }
  }

  function autoResize(e) {
    const ta = e.target || e;
    ta.style.height = 'auto';
    ta.style.height = Math.min(ta.scrollHeight, 200) + 'px';
    updateMentionFromTextarea();
    if (sendError) setSendError(null);
  }

  async function handlePaste(e) {
    const items = e.clipboardData?.items;
    if (!items) return;
    for (const item of items) {
      if (item.type.startsWith('image/')) {
        e.preventDefault();
        const file = item.getAsFile();
        if (!file) continue;
        try {
          const result = await uploadImage(file);
          pendingImageUrl = BASE + result.url;

          let preview = document.getElementById('chatImgPreview');
          const inputArea = textareaRef.current?.closest('.chat-input-area');
          if (!preview && inputArea) {
            preview = document.createElement('div');
            preview.id = 'chatImgPreview';
            preview.className = 'chat-img-preview';
            const row = inputArea.querySelector('.chat-input-row');
            if (row) inputArea.insertBefore(preview, row);
          }
          if (preview) {
            preview.innerHTML = `Image attached <span style="cursor:pointer" onclick="this.closest('.chat-img-preview').remove(); window.__clearPendingImage && window.__clearPendingImage()">✕</span>`;
          }
        } catch (err) {
          console.error('[ChatInput] paste uploadImage failed', err);
        }
        return;
      }
    }
  }

  // Save draft on unmount, restore on mount — survives tab switches.
  // Also clears any stale send error from the previous channel — an error
  // message about missing mentions doesn't apply once you're in a DM.
  const ch = currentChannel.value;
  useEffect(() => {
    const ta = textareaRef.current;
    if (!ta) return;
    const key = ch || 'general';
    setSendError(null);
    if (draftText[key]) {
      ta.value = draftText[key];
      autoResize(ta);
    }
    return () => {
      draftText[key] = ta.value;
    };
  }, [ch]);

  // Focus textarea when reply is set; clear any send error so the new
  // reply target supersedes a "no target" error from a prior attempt.
  useEffect(() => {
    if (replyTo.value) {
      textareaRef.current?.focus();
      setSendError(null);
    }
  }, [replyTo.value]);
  let placeholder = 'Message #general…';
  if (ch && ch.startsWith('topic:')) placeholder = `Message #${ch.slice(6)}…`;
  else if (ch && ch !== 'general') placeholder = `Message @${ch}…`;

  const reply = replyTo.value;

  return html`
    <div class="chat-input-area">
      ${mention.active && filtered.length > 0 && html`
        <div class="mention-picker">
          ${filtered.map((id, i) => html`
            <div
              key=${id}
              class=${'mention-item' + (i === mention.index ? ' active' : '')}
              onMouseDown=${e => { e.preventDefault(); insertMention(id); }}
            >
              <span class="mention-dot" style=${'background:' + (colors[id] || '#888')}></span>
              <span class="mention-id">@${id}</span>
            </div>
          `)}
        </div>
      `}
      ${reply && html`
        <div class="chat-reply-bar" role="status" aria-live="polite">
          <div class="chat-reply-bar-text">
            <span class="chat-reply-bar-label">Replying to <strong>@${reply.from}</strong>:</span>
            <span class="chat-reply-bar-preview">${(reply.content || '').replace(/\s+/g, ' ').slice(0, 120)}</span>
          </div>
          <button class="chat-reply-bar-cancel" aria-label="Cancel reply" onClick=${() => { replyTo.value = null; }}>✕</button>
        </div>
      `}
      ${sendError && html`
        <div class="chat-send-error" role="alert">${sendError}</div>
      `}
      <div class="chat-input-row">
        <button
          class="chat-attach-btn"
          title="Attach image"
          onClick=${() => fileInputRef.current?.click()}
        >📎</button>
        <input
          type="file"
          accept="image/*"
          ref=${fileInputRef}
          style="display:none"
          onChange=${handleImageAttach}
        />
        <textarea
          ref=${textareaRef}
          placeholder=${placeholder}
          rows="1"
          onKeyDown=${handleKeydown}
          onInput=${autoResize}
          onClick=${updateMentionFromTextarea}
          onBlur=${() => setTimeout(() => setMention({ active: false, query: '', start: 0, index: 0 }), 150)}
          onPaste=${handlePaste}
        ></textarea>
        <button class="chat-send-btn" onClick=${handleSend}>Send</button>
      </div>
    </div>
  `;
}
