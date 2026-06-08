const { html } = window.__preact;

import { Placeholder } from './Placeholder.js';

export function WatchView() {
  return html`<${Placeholder} title="Watch" note="Run timeline arriving in NEX-527" />`;
}
