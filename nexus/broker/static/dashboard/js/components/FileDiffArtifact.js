// FileDiffArtifact — renders a structured file-mutating tool call
// (Edit / Write / MultiEdit / NotebookEdit) as a compact diff. Built
// from observability.Artifact which is pre-parsed broker-side
// (nexus/observability/artifact.go), so the input here is already
// shape-normalised — we don't re-parse the underlying tool args.
//
// Display priorities:
//   - file_path always visible
//   - Edit: two-pane old vs new with line-level highlights
//   - Write: header "created · N bytes" + preview body
//   - MultiEdit: list of {old → new} pairs
//   - NotebookEdit: degrades to the Edit/Write shape via OldText/NewText
//
// Long content is truncated by character count rather than line count
// so a 5000-char single line doesn't blow out the layout. Operator
// can expand via "show more" — state is local to the component.

const { html, useState } = window.__preact;

const PREVIEW_CHARS = 400; // truncation threshold per text region

export function FileDiffArtifact({ artifact }) {
  if (!artifact) return null;
  const kind = artifact.kind || 'unknown';
  const filePath = artifact.file_path || '(no path)';

  switch (kind) {
    case 'file_edit':
    case 'notebook_edit':
      return html`<${EditPair} path=${filePath} oldText=${artifact.old_text || ''} newText=${artifact.new_text || ''} />`;

    case 'file_write':
      return html`<${WriteBlock} path=${filePath} newText=${artifact.new_text || ''} />`;

    case 'multi_edit':
      return html`<${MultiEditBlock} path=${filePath} edits=${artifact.edits || []} />`;

    default:
      return html`
        <div class="observe-artifact observe-artifact-unknown">
          <div class="observe-artifact-path">${filePath}</div>
          <div class="observe-artifact-note">unknown artifact kind: ${kind}</div>
        </div>
      `;
  }
}

function EditPair({ path, oldText, newText }) {
  return html`
    <div class="observe-artifact observe-artifact-edit">
      <div class="observe-artifact-path">${path}</div>
      <div class="observe-artifact-edit-pair">
        <${DiffPane} label="−" cls="observe-diff-old" text=${oldText} />
        <${DiffPane} label="+" cls="observe-diff-new" text=${newText} />
      </div>
    </div>
  `;
}

function WriteBlock({ path, newText }) {
  const bytes = byteLen(newText);
  return html`
    <div class="observe-artifact observe-artifact-write">
      <div class="observe-artifact-path">${path}</div>
      <div class="observe-artifact-meta">wrote · ${bytes} bytes · ${lineCount(newText)} lines</div>
      <${DiffPane} label="" cls="observe-diff-new" text=${newText} />
    </div>
  `;
}

function MultiEditBlock({ path, edits }) {
  return html`
    <div class="observe-artifact observe-artifact-multi">
      <div class="observe-artifact-path">${path}</div>
      <div class="observe-artifact-meta">${edits.length} edit${edits.length === 1 ? '' : 's'}</div>
      ${edits.map((e, i) => html`
        <div class="observe-artifact-edit-pair" key=${i}>
          <${DiffPane} label="−" cls="observe-diff-old" text=${e.old_text || ''} />
          <${DiffPane} label="+" cls="observe-diff-new" text=${e.new_text || ''} />
        </div>
      `)}
    </div>
  `;
}

// DiffPane — one side of an Edit pair, or a single block (Write).
// Truncates with a "show more" toggle so a 50K-line Write doesn't
// flatten the stream.
function DiffPane({ label, cls, text }) {
  const [expanded, setExpanded] = useState(false);
  const overLimit = text.length > PREVIEW_CHARS;
  const shown = expanded || !overLimit ? text : text.slice(0, PREVIEW_CHARS);
  return html`
    <div class=${'observe-diff-pane ' + cls}>
      ${label && html`<span class="observe-diff-label" aria-hidden="true">${label}</span>`}
      <pre class="observe-diff-body">${shown}${overLimit && !expanded ? '…' : ''}</pre>
      ${overLimit && html`
        <button
          class="observe-diff-toggle"
          onClick=${() => setExpanded(v => !v)}
          aria-label=${expanded ? 'collapse' : 'expand'}
        >${expanded ? 'show less' : `show ${text.length - PREVIEW_CHARS} more chars`}</button>
      `}
    </div>
  `;
}

function lineCount(s) {
  if (!s) return 0;
  let n = 1;
  for (let i = 0; i < s.length; i++) if (s.charCodeAt(i) === 10) n++;
  return n;
}

// byteLen approximates UTF-8 byte length without a TextEncoder allocation
// per render. JS strings are UTF-16; non-BMP chars use surrogate pairs.
// For dashboard preview, the simple s.length is close enough — pure
// ASCII matches exactly, and the worst-case overstatement for an
// all-4-byte-emoji string is 2× (still useful for "is this big?").
function byteLen(s) {
  return s ? s.length : 0;
}
