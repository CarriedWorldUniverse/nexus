const { html, useState, useEffect } = window.__preact;

import { fetchFiles, BASE, getAuthToken } from '../api.js';
import { agents, agentColors } from '../state.js';

function formatSize(bytes) {
  if (bytes == null) return '—';
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function formatDate(str) {
  if (!str) return '';
  const d = new Date(str + 'Z');
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
}

function fileIcon(filename) {
  if (!filename) return '📄';
  const ext = filename.split('.').pop().toLowerCase();
  const map = {
    png: '🖼', jpg: '🖼', jpeg: '🖼', gif: '🖼', webp: '🖼', svg: '🖼',
    mp3: '🎵', wav: '🎵', ogg: '🎵',
    mp4: '🎬', mov: '🎬', avi: '🎬',
    pdf: '📋',
    json: '📦', js: '📦', ts: '📦',
    txt: '📝', md: '📝',
    zip: '🗜', gz: '🗜', tar: '🗜',
  };
  return map[ext] || '📄';
}

async function downloadFile(id, filename) {
  const token = getAuthToken();
  const headers = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(`${BASE}/api/files/${id}`, { headers });
  if (!res.ok) throw new Error(`Download failed: ${res.status}`);
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename || 'download';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

export function Files() {
  const [files, setFiles] = useState([]);
  const [filter, setFilter] = useState('');
  const [loading, setLoading] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState(null);

  const agentList = agents.value || [];
  const colors = agentColors.value || {};

  async function load() {
    setLoading(true);
    setError(null);
    try {
      const result = await fetchFiles(filter || undefined);
      setFiles(Array.isArray(result) ? result : (result.files || []));
    } catch (e) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { load(); }, [filter]);

  async function handleUpload(e) {
    const file = e.target.files[0];
    if (!file) return;
    setUploading(true);
    try {
      const form = new FormData();
      form.append('file', file);
      form.append('owner', 'operator');
      form.append('description', '');
      const token = getAuthToken();
      const headers = {};
      if (token) headers["Authorization"] = `Bearer ${token}`;
      const res = await fetch(`${BASE}/api/files`, { method: 'POST', body: form, headers });
      if (!res.ok) throw new Error(`Upload failed: ${res.status}`);
      await load();
    } catch (e) {
      setError(e.message);
    } finally {
      setUploading(false);
      e.target.value = '';
    }
  }

  async function handleDelete(file) {
    if (!confirm(`Delete "${file.filename}"?`)) return;
    try {
      const token = getAuthToken();
      const headers = {};
      if (token) headers["Authorization"] = `Bearer ${token}`;
      const res = await fetch(`${BASE}/api/files/${file.id}?owner=${encodeURIComponent(file.owner || 'operator')}`, { method: 'DELETE', headers });
      if (!res.ok) throw new Error(`Delete failed: ${res.status}`);
      await load();
    } catch (e) {
      setError(e.message);
    }
  }

  const ownerOptions = ['', 'operator', ...agentList.map(a => typeof a === 'string' ? a : a.id).filter(Boolean)];
  const uniqueOwners = [...new Set(ownerOptions)];

  return html`
    <div class="files-view">
      <div class="files-header">
        <div class="files-header-left">
          <h2 class="files-title">Shared Files</h2>
          ${!loading && html`<span class="files-count">${files.length} file${files.length !== 1 ? 's' : ''}</span>`}
          ${loading && html`<span class="files-count">loading…</span>`}
        </div>
        <label class="files-upload-btn ${uploading ? 'uploading' : ''}">
          ${uploading ? 'Uploading…' : '+ Upload'}
          <input type="file" style="display:none" onChange=${handleUpload} disabled=${uploading} />
        </label>
      </div>

      <div class="files-controls">
        <select class="files-filter" value=${filter} onChange=${e => setFilter(e.target.value)}>
          <option value="">All agents</option>
          ${uniqueOwners.filter(o => o).map(o => html`<option value=${o}>${o}</option>`)}
        </select>
      </div>

      ${error && html`<div class="files-error">${error}</div>`}

      <div class="files-list">
        ${files.length === 0 && !loading && html`
          <div class="files-empty">No files${filter ? ` from ${filter}` : ''}</div>
        `}
        ${files.map(f => html`
          <div class="file-item" key=${f.id}>
            <div class="file-icon">${fileIcon(f.filename)}</div>
            <div class="file-info">
              <div class="file-name">${f.filename || 'unnamed'}</div>
              <div class="file-meta">
                <span class="file-owner" style="color: ${colors[f.owner] || 'var(--text-muted)'}">
                  ${f.owner || 'unknown'}
                </span>
                <span class="file-sep">·</span>
                <span>${formatSize(f.size)}</span>
                <span class="file-sep">·</span>
                <span>${formatDate(f.created_at)}</span>
                ${f.description && html`<span class="file-sep">·</span><span class="file-desc">${f.description}</span>`}
              </div>
            </div>
            <div class="file-actions">
              <button class="file-btn" title="Download" onClick=${() => downloadFile(f.id, f.filename)}>
                ↓
              </button>
              <button class="file-btn file-btn-delete" title="Delete" onClick=${() => handleDelete(f)}>
                ✕
              </button>
            </div>
          </div>
        `)}
      </div>
    </div>
  `;
}
