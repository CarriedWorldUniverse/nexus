import { MessageBubble } from './MessageBubble.js';
import { fetchReplies } from '../api.js';

const { html, useState, useEffect } = window.__preact;

export function ThreadView({ parentId }) {
  const [replies, setReplies] = useState([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    function load() {
      fetchReplies(parentId).then(items => {
        if (cancelled) return;
        setReplies(items.map(r => ({
          id: r.id,
          from: r.from_agent || r.from,
          content: r.content,
          at: r.created_at || r.at,
          reply_to: r.reply_to,
          reply_count: 0,
        })));
        setLoading(false);
      });
    }
    load();
    const interval = setInterval(load, 4000);
    return () => { cancelled = true; clearInterval(interval); };
  }, [parentId]);

  if (loading) {
    return html`<div class="msg-thread" style="color:var(--text-muted);font-size:11px;padding:4px 0 4px 48px;">Loading replies...</div>`;
  }

  return html`
    <div class="msg-thread">
      ${replies.map(r => html`
        <${MessageBubble} key=${r.id} msg=${r} compact=${false} />
      `)}
    </div>
  `;
}
