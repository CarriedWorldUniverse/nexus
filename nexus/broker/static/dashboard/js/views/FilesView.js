const { html, useState } = window.__preact;
import { Files } from './Files.js';
import { Knowledge } from './Knowledge.js';

export function FilesView() {
  const [tab, setTab] = useState('files');

  return html`
    <div class="files-view">
      <div class="files-view-tabs">
        <button
          class=${'files-view-tab' + (tab === 'files' ? ' active' : '')}
          onClick=${() => setTab('files')}
        >Shared Files</button>
        <button
          class=${'files-view-tab' + (tab === 'knowledge' ? ' active' : '')}
          onClick=${() => setTab('knowledge')}
        >Knowledge Base</button>
      </div>
      <div class="files-view-content">
        ${tab === 'files' ? html`<${Files} />` : html`<${Knowledge} />`}
      </div>
    </div>
  `;
}
