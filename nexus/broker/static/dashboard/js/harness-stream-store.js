// Persistent per-agent harness activity streams.
//
// Lives at module scope so EventSources + event buffers survive view
// unmounts (e.g. navigating Terminal → Feed → Terminal). Without this, every
// tab-away closed the SSE and dropped history; operators saw the activity
// pane reset on return.
//
// Auto-starts a stream on first subscription, keeps it open forever, caps the
// per-agent buffer at 500 events.

import { BASE } from './api.js';

const BUFFER_CAP = 500;
const BUFFER_TRIM_TO = 399;

const streams = new Map(); // agentId -> { es, events, subscribers:Set, connected, error, seq }

function ensureStream(agentId) {
  let s = streams.get(agentId);
  if (s) return s;

  s = {
    es: null,
    events: [],
    subscribers: new Set(),
    connected: false,
    error: null,
    seq: 0,
    seeded: false,
  };
  streams.set(agentId, s);
  seedFromTail(agentId, s);
  openEventSource(agentId, s);
  return s;
}

// One-shot fetch of the most-recent session tail. Runs in parallel with the
// SSE connect — live events always win via eventKey dedup would be ideal, but
// the seed lands first (usually) and the SSE turn_start / new events simply
// append. On rare reordering the worst case is a duplicated row, which is
// acceptable for a context-seeding UX and preferable to blocking.
async function seedFromTail(agentId, s) {
  try {
    const token = localStorage.getItem('auth_token') || '';
    const resp = await fetch(`${BASE}/harness/${encodeURIComponent(agentId)}/tail?limit=15`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!resp.ok) return;
    const body = await resp.json();
    if (!body || !Array.isArray(body.events) || body.events.length === 0) return;
    // Only seed if nothing has streamed in yet — avoids clobbering a live turn
    // that started between fetch dispatch and resolve.
    if (s.events.length > 0) return;
    const seeded = body.events.map((ev) => ({
      ...ev,
      _seq: ++s.seq,
      _receivedAt: Date.now(),
      _seeded: true,
    }));
    s.events = seeded;
    s.seeded = true;
    notify(s);
  } catch {
    // Non-fatal — the pane just starts empty, same as before.
  }
}

// Reconnect backoff: 1s, 2s, 4s, capped at 10s. Resets to 1s on successful open.
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 10000;

function openEventSource(agentId, s) {
  const token = localStorage.getItem('auth_token') || '';
  const url = `${BASE}/harness/${encodeURIComponent(agentId)}/stream${token ? `?token=${encodeURIComponent(token)}` : ''}`;
  const es = new EventSource(url);
  s.es = es;
  if (s.reconnectDelay == null) s.reconnectDelay = RECONNECT_MIN_MS;

  es.onopen = () => {
    s.connected = true;
    s.error = null;
    s.reconnectDelay = RECONNECT_MIN_MS;
    notify(s);
  };
  es.onmessage = (ev) => {
    let parsed;
    try { parsed = JSON.parse(ev.data); } catch { return; }
    if (s.events.length >= BUFFER_CAP) {
      s.events = s.events.slice(-BUFFER_TRIM_TO);
    }
    s.events.push({ ...parsed, _seq: ++s.seq, _receivedAt: Date.now() });
    notify(s);
  };
  es.onerror = () => {
    s.connected = false;
    // The broker responds 502 when the agent process is momentarily gone
    // (e.g. during restart). EventSource treats a non-200 close as terminal
    // and moves to CLOSED without retrying — so we reopen manually with
    // backoff. If it's still CONNECTING, the native retry is in flight;
    // don't interfere.
    if (es.readyState === EventSource.CLOSED) {
      const delay = s.reconnectDelay;
      s.error = `disconnected — reconnecting in ${Math.round(delay / 1000)}s…`;
      notify(s);
      try { es.close(); } catch {}
      if (s.es === es) s.es = null;
      // Bump backoff for the *next* failure; the current schedule fires at `delay`.
      s.reconnectDelay = Math.min(delay * 2, RECONNECT_MAX_MS);
      if (s.reconnectTimer) clearTimeout(s.reconnectTimer);
      s.reconnectTimer = setTimeout(() => {
        s.reconnectTimer = null;
        openEventSource(agentId, s);
      }, delay);
    } else {
      s.error = 'disconnected — retrying…';
      notify(s);
    }
  };
}

function notify(s) {
  for (const cb of s.subscribers) {
    try { cb(); } catch {}
  }
}

// Subscribe to an agent's stream. The callback fires on every state change
// (new event, connection flip, error). Returns an unsubscribe fn.
//
// When the subscriber count drops to zero, the underlying EventSource is
// closed after a short grace period. This releases the per-host HTTP/1.1
// connection slot — without it, every aspect we ever viewed kept its
// stream open forever, leaving two tabs of the dashboard unable to open
// new connections (browsers cap at ~6 per host).
const TEARDOWN_GRACE_MS = 5000;

export function subscribe(agentId, cb) {
  const s = ensureStream(agentId);
  s.subscribers.add(cb);
  if (s.teardownTimer) {
    clearTimeout(s.teardownTimer);
    s.teardownTimer = null;
  }
  return () => {
    s.subscribers.delete(cb);
    if (s.subscribers.size === 0 && !s.teardownTimer) {
      s.teardownTimer = setTimeout(() => {
        s.teardownTimer = null;
        if (s.subscribers.size > 0) return; // resubscribed inside the grace window
        try { s.es?.close(); } catch {}
        s.es = null;
        if (s.reconnectTimer) {
          clearTimeout(s.reconnectTimer);
          s.reconnectTimer = null;
        }
        s.connected = false;
        // Drop the entry entirely — next subscribe rebuilds via
        // ensureStream(), which re-seeds history from /tail. Costs one
        // fetch but releases the connection slot immediately.
        streams.delete(agentId);
      }, TEARDOWN_GRACE_MS);
    }
  };
}

export function getSnapshot(agentId) {
  const s = streams.get(agentId);
  if (!s) return { events: [], connected: false, error: null };
  return { events: s.events, connected: s.connected, error: s.error };
}
