// ToolCall — renders one model tool invocation + its result.
//
// Hierarchy:
//   - header line: 🔧 name · arg-preview · result-state-pill
//   - body (expandable): full input JSON + full result preview
//   - artifact (when present, see FileDiffArtifact): replaces raw
//     input rendering for the file-mutating built-ins
//
// In-flight calls (no Result yet) show a pulsing dot to make
// long-running tools visually distinct from completed ones — the
// "is something happening?" signal Lock 5 cares about.

const { html, useState } = window.__preact;

import { FileDiffArtifact } from './FileDiffArtifact.js';

const ARG_PREVIEW_CHARS = 90;
const FULL_INPUT_CAP = 4000;

export function ToolCall({ tool, isOrphan }) {
  if (!tool) return null;
  const [open, setOpen] = useState(false);

  const name = tool.name || (isOrphan ? '(orphan result)' : '?');
  const result = tool.result;
  const hasArtifact = !!tool.artifact;

  // Header: name + short preview of input (or "(no args)") + result pill.
  const argPreview = previewArgs(tool.input, hasArtifact ? tool.artifact : null);
  const resultState = resultStateOf(tool, isOrphan);

  return html`
    <div class=${'observe-toolcall observe-toolcall-' + resultState.cls}>
      <button class="observe-toolcall-head" onClick=${() => setOpen(v => !v)} aria-expanded=${open}>
        <span class="observe-toolcall-icon" aria-hidden="true">${isOrphan ? '⚠' : '🔧'}</span>
        <span class="observe-toolcall-name">${name}</span>
        ${argPreview && html`<span class="observe-toolcall-args">${argPreview}</span>`}
        <span class=${'observe-toolcall-pill observe-toolcall-pill-' + resultState.cls}>${resultState.label}</span>
        <span class="observe-toolcall-chevron" aria-hidden="true">${open ? '▾' : '▸'}</span>
      </button>
      ${open && html`
        <div class="observe-toolcall-body">
          ${hasArtifact
            ? html`<${FileDiffArtifact} artifact=${tool.artifact} />`
            : html`<${InputBlock} input=${tool.input} parseErr=${tool.artifact_parse_err} />`}
          ${result && html`<${ResultBlock} result=${result} />`}
        </div>
      `}
    </div>
  `;
}

function InputBlock({ input, parseErr }) {
  if (!input) return html`<div class="observe-toolcall-noargs">no args</div>`;
  // input is json.RawMessage — preact gives us either an object (if the
  // outer envelope parser already walked into it) or a string.
  const text = typeof input === 'string' ? input : safeStringify(input);
  const truncated = text.length > FULL_INPUT_CAP;
  return html`
    <div class="observe-toolcall-input">
      ${parseErr && html`<div class="observe-toolcall-parse-err">artifact parse failed: ${parseErr}</div>`}
      <pre>${truncated ? text.slice(0, FULL_INPUT_CAP) + '…' : text}</pre>
    </div>
  `;
}

function ResultBlock({ result }) {
  const isError = !!result.is_error;
  const cls = isError ? 'observe-toolcall-result observe-toolcall-result-error' : 'observe-toolcall-result';
  // Preview is preview-only by intent (broker truncates to ≤200 chars).
  // The "full" field is reserved for the future operator-expand path.
  const body = result.preview || '';
  return html`
    <div class=${cls}>
      <div class="observe-toolcall-result-label">${isError ? 'error' : 'result'}</div>
      <pre>${body}</pre>
    </div>
  `;
}

function previewArgs(input, artifact) {
  // Artifact case: show the file_path; that's the salient arg for
  // Edit/Write/MultiEdit and matches what the operator scans for.
  if (artifact && artifact.file_path) return artifact.file_path;
  if (!input) return '';
  const text = typeof input === 'string' ? input : safeStringify(input);
  if (text.length <= ARG_PREVIEW_CHARS) return text;
  return text.slice(0, ARG_PREVIEW_CHARS) + '…';
}

function resultStateOf(tool, isOrphan) {
  if (isOrphan) return { cls: 'orphan', label: 'orphan' };
  if (!tool.result) return { cls: 'pending', label: 'running' };
  if (tool.result.is_error) return { cls: 'error', label: 'error' };
  return { cls: 'ok', label: 'ok' };
}

function safeStringify(o) {
  try { return JSON.stringify(o); } catch (_) { return String(o); }
}
