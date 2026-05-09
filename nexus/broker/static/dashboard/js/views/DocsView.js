const { html, useState, useEffect, useRef } = window.__preact;
import { BASE } from '../api.js';

function authHeaders() {
  const token = localStorage.getItem('auth_token');
  return token ? { Authorization: 'Bearer ' + token } : {};
}

function renderMarkdown(md) {
  let out = md
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

  // Fenced code blocks
  out = out.replace(/```[\s\S]*?```/g, m => {
    const inner = m.slice(3, -3).replace(/^[^\n]*\n/, ''); // strip language line
    return `<pre><code>${inner}</code></pre>`;
  });

  // Headings
  out = out.replace(/^#### (.+)$/gm, '<h4>$1</h4>');
  out = out.replace(/^### (.+)$/gm, '<h3>$1</h3>');
  out = out.replace(/^## (.+)$/gm, '<h2>$1</h2>');
  out = out.replace(/^# (.+)$/gm, '<h1>$1</h1>');

  // Horizontal rules
  out = out.replace(/^---+$/gm, '<hr>');

  // Tables (simple)
  out = out.replace(/((?:^\|.+\|\n)+)/gm, tableBlock => {
    const rows = tableBlock.trim().split('\n');
    let html = '<table>';
    rows.forEach((row, i) => {
      if (/^\|[-: |]+\|$/.test(row)) return; // separator row
      const cells = row.split('|').slice(1, -1).map(c => c.trim());
      const tag = i === 0 ? 'th' : 'td';
      html += '<tr>' + cells.map(c => `<${tag}>${c}</${tag}>`).join('') + '</tr>';
    });
    html += '</table>';
    return html;
  });

  // Bold, inline code
  out = out.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  out = out.replace(/`([^`]+)`/g, '<code>$1</code>');

  // Unordered lists
  out = out.replace(/((?:^[-*] .+\n?)+)/gm, block => {
    const items = block.trim().split('\n').map(l => `<li>${l.replace(/^[-*] /, '')}</li>`).join('');
    return `<ul>${items}</ul>`;
  });

  // Ordered lists
  out = out.replace(/((?:^\d+\. .+\n?)+)/gm, block => {
    const items = block.trim().split('\n').map(l => `<li>${l.replace(/^\d+\. /, '')}</li>`).join('');
    return `<ol>${items}</ol>`;
  });

  // Paragraphs — wrap lines not already wrapped in a block tag
  out = out.replace(/^(?!<[hpuolit]|<hr|<pre|<table)(.+)$/gm, '<p>$1</p>');

  return out;
}

export function DocsView() {
  const [entries, setEntries] = useState([]);
  const [selected, setSelected] = useState(null);
  const [content, setContent] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);
  const [pendingApprovals, setPendingApprovals] = useState(new Set());
  const [editing, setEditing] = useState(false);
  const [editDraft, setEditDraft] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState(null);
  const editorRef = useRef(null);

  useEffect(() => {
    fetch(`${BASE}/api/docs`, { headers: authHeaders() })
      .then(r => r.json())
      .then(data => {
        setEntries(Array.isArray(data) ? data : []);
        if (data.length > 0 && !selected) {
          openDoc(data[0].path);
        }
      })
      .catch(() => setError('Failed to load docs list'));

    // Load open approval tickets to flag docs awaiting review
    fetch(`${BASE}/api/tickets?assignee=operator&status=open`, { headers: authHeaders() })
      .then(r => r.json())
      .then(tickets => {
        if (!Array.isArray(tickets)) return;
        const names = new Set();
        for (const t of tickets) {
          if (t.title && t.title.startsWith('Approval:')) {
            // Extract doc name from title like "Approval: message reactions implementation"
            names.add(t.title.replace(/^Approval:\s*/i, '').toLowerCase());
          }
        }
        setPendingApprovals(names);
      })
      .catch(() => {});
  }, []);

  async function openDoc(docPath) {
    setEditing(false);
    setSaveError(null);
    setSelected(docPath);
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`${BASE}/api/doc?path=${encodeURIComponent(docPath)}`, { headers: authHeaders() });
      if (!res.ok) throw new Error(res.status);
      const text = await res.text();
      setContent(text);
    } catch {
      setError('Failed to load document');
      setContent('');
    } finally {
      setLoading(false);
    }
  }

  function startEdit() {
    setEditDraft(content);
    setSaveError(null);
    setEditing(true);
    // Set textarea value after render via microtask
    Promise.resolve().then(() => {
      if (editorRef.current) editorRef.current.value = content;
    });
  }

  function cancelEdit() {
    setEditing(false);
    setSaveError(null);
  }

  async function saveDoc() {
    const savedPath = selected;
    setSaving(true);
    setSaveError(null);
    try {
      const res = await fetch(
        `${BASE}/api/doc?path=${encodeURIComponent(savedPath)}`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json', ...authHeaders() },
          body: JSON.stringify({ content: editDraft }),
        }
      );
      if (!res.ok) throw new Error(await res.text());
      if (selected === savedPath) {
        setContent(editDraft);
        setEditing(false);
      }
    } catch (e) {
      setSaveError('Save failed: ' + e.message);
    } finally {
      setSaving(false);
    }
  }

  // Group entries by directory (up to 2 levels deep)
  const groups = {};
  for (const e of entries) {
    const parts = e.path.split('/');
    const group = parts.length >= 3 ? `${parts[0]}/${parts[1]}`
                : parts.length === 2 ? parts[0]
                : 'root';
    if (!groups[group]) groups[group] = [];
    groups[group].push(e);
  }

  function groupLabel(g) {
    if (g === 'root') return null;
    const last = g.split('/').pop();
    if (last === 'specs') return 'Specs';
    if (last === 'plans') return 'Plans';
    return last;
  }

  function isAwaitingApproval(e) {
    const nameLower = e.name.toLowerCase();
    for (const pending of pendingApprovals) {
      if (nameLower.includes(pending) || pending.includes(nameLower)) return true;
    }
    return false;
  }

  return html`
    <div class="docs-view">
      <div class="docs-sidebar">
        ${Object.entries(groups).map(([group, items]) => html`
          <div class="docs-group" key=${group}>
            ${groupLabel(group) && html`<div class="docs-group-label">${groupLabel(group)}</div>`}
            ${items.map(e => {
              const awaiting = isAwaitingApproval(e);
              return html`
                <button
                  key=${e.path}
                  class=${'docs-entry' + (selected === e.path ? ' active' : '')}
                  onClick=${() => openDoc(e.path)}
                  title=${e.path}
                >
                  <span class="docs-entry-name">${e.name}</span>
                  ${awaiting && html`<span class="docs-approval-badge" title="Awaiting operator approval">👀</span>`}
                </button>
              `;
            })}
          </div>
        `)}
      </div>
      ${html`<div class="docs-content">
  ${selected && !loading && !error && html`
    <div class="docs-toolbar">
      ${editing
        ? html`
            <button class="docs-btn docs-btn-save" onClick=${saveDoc} disabled=${saving}>
              ${saving ? 'Saving…' : 'Save'}
            </button>
            <button class="docs-btn docs-btn-cancel" onClick=${cancelEdit} disabled=${saving}>Cancel</button>
            ${saveError && html`<span class="docs-save-error">${saveError}</span>`}
          `
        : html`
            <button class="docs-btn docs-btn-edit" onClick=${startEdit}>Edit</button>
          `
      }
    </div>
  `}
  ${loading && html`<div class="docs-loading">Loading…</div>`}
  ${error && html`<div class="docs-error">${error}</div>`}
  ${!loading && !error && content && !editing && html`
    <div
      class="docs-body"
      dangerouslySetInnerHTML=${{ __html: renderMarkdown(content) }}
    />
  `}
  ${!loading && !error && editing && html`
    <textarea
      class="docs-editor"
      ref=${editorRef}
      onInput=${e => setEditDraft(e.target.value)}
    />
  `}
  ${!loading && !error && !content && !editing && html`
    <div class="docs-empty">Select a document</div>
  `}
</div>`}
    </div>
  `;
}
