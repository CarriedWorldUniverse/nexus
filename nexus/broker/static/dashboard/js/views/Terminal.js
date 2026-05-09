const { html, useState, useEffect, useRef } = window.__preact;
import { agents, agentColors } from '../state.js';
import { BASE } from '../api.js';
import { HarnessActivity } from '../components/HarnessActivity.js';

function agentRuntime(agent) {
  // agents.value items can be strings (id only) during early state — default to proxy.
  // agent may also be undefined when activeAgent isn't in the list yet — also default.
  if (!agent) return 'proxy';
  if (typeof agent === 'string') return 'proxy';
  return agent.runtime || 'proxy';
}

const XTERM_THEME = {
  background: '#0c1220',
  foreground: '#d8dfe8',
  cursor: '#e94560',
  selectionBackground: '#1e2a42',
};

export function Terminal() {
  const agentList = agents.value;
  const colors = agentColors.value;

  const [activeAgent, setActiveAgent] = useState(null);
  const [unlocked, setUnlocked] = useState({}); // agentId -> boolean (default: true)
  const mountRef = useRef(null);
  const activeAgentRef = useRef(null);
  activeAgentRef.current = activeAgent;

  // Persistent refs for terminal instances and WebSockets per agent
  const terminals = useRef({});      // agentId -> xterm Terminal instance
  const sockets = useRef({});        // agentId -> WebSocket
  const fitAddons = useRef({});      // agentId -> FitAddon instance
  const unlockedRef = useRef({});    // mirror of unlocked state for use in callbacks
  const reconnectTimers = useRef({}); // agentId -> setTimeout handle
  const reconnectDelays = useRef({}); // agentId -> current backoff ms
  const [xtermLoaded, setXtermLoaded] = useState(false);
  const TerminalClass = useRef(null);
  const FitAddonClass = useRef(null);

  // Load xterm modules once
  useEffect(() => {
    (async () => {
      try {
        const [xtermMod, fitMod] = await Promise.all([
          import('/js/vendor/xterm-esm.js'),
          import('/js/vendor/addon-fit-esm.js'),
        ]);
        TerminalClass.current = xtermMod.Terminal;
        FitAddonClass.current = fitMod.FitAddon;
        setXtermLoaded(true);
      } catch (e) {
        console.error('[Terminal] Failed to load xterm:', e);
      }
    })();
  }, []);

  // Set default active agent when agent list loads
  useEffect(() => {
    if (agentList.length > 0 && activeAgent === null) {
      const first = typeof agentList[0] === 'string' ? agentList[0] : agentList[0].id;
      setActiveAgent(first);
    }
  }, [agentList]);

  // Sync PTY size with terminal dimensions
  function syncResize(agentId, term) {
    const { cols, rows } = term;
    const token = localStorage.getItem('auth_token');
    const headers = { 'Content-Type': 'application/json' };
    if (token) headers['Authorization'] = `Bearer ${token}`;
    fetch(`${BASE}/proxy/${agentId}/resize`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ cols, rows }),
    }).catch(() => {});
  }

  // Open (or reopen) the output WebSocket for a proxy agent. On unexpected
  // close — including agent restart, where the proxy's PTY socket goes away
  // momentarily — schedule a backoff reconnect so the operator doesn't have
  // to tab-click to get the stream back.
  function openProxyWs(agentId) {
    const existingWs = sockets.current[agentId];
    if (existingWs && existingWs.readyState !== WebSocket.CLOSED && existingWs.readyState !== WebSocket.CLOSING) {
      return;
    }
    const wsProto = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const wsUrl = `${wsProto}://${window.location.host}/proxy/${agentId}/output`;
    const ws = new WebSocket(wsUrl);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => { reconnectDelays.current[agentId] = 1000; };
    ws.onmessage = (evt) => {
      const t = terminals.current[agentId];
      if (!t) return;
      if (typeof evt.data === 'string') t.write(evt.data);
      else t.write(new Uint8Array(evt.data));
    };
    ws.onerror = (e) => { console.warn(`[Terminal] WS error for ${agentId}:`, e); };
    ws.onclose = () => {
      if (sockets.current[agentId] === ws) delete sockets.current[agentId];
      // Only reconnect if this agent is still the active one. Agent-switch
      // clears any pending timer in the effect cleanup below.
      if (activeAgentRef.current !== agentId) return;
      const delay = reconnectDelays.current[agentId] || 1000;
      reconnectDelays.current[agentId] = Math.min(delay * 2, 10000);
      if (reconnectTimers.current[agentId]) clearTimeout(reconnectTimers.current[agentId]);
      reconnectTimers.current[agentId] = setTimeout(() => {
        reconnectTimers.current[agentId] = null;
        openProxyWs(agentId);
      }, delay);
    };

    sockets.current[agentId] = ws;
  }

  // Switch to agent: attach or create terminal, connect WS
  // Harness agents have no PTY — the mountRef is used by <HarnessActivity/>
  // instead. Detach xterm if the user switches from a proxy to a harness.
  useEffect(() => {
    if (!activeAgent || !mountRef.current || !xtermLoaded) return;
    const activeAgentObj = agentList.find(a => (typeof a === 'string' ? a : a.id) === activeAgent);
    if (agentRuntime(activeAgentObj) === 'harness') {
      // Detach any xterm that might be parented to mountRef — harness pane owns the DOM now.
      for (const term of Object.values(terminals.current)) {
        const el = term.element;
        if (el && el.parentNode === mountRef.current) mountRef.current.removeChild(el);
      }
      return;
    }

    // Detach all other terminals from DOM
    for (const [id, term] of Object.entries(terminals.current)) {
      if (id !== activeAgent) {
        const el = term.element;
        if (el && el.parentNode === mountRef.current) {
          mountRef.current.removeChild(el);
        }
      }
    }

    // Create terminal for this agent if needed
    if (!terminals.current[activeAgent]) {
      const term = new TerminalClass.current({
        theme: XTERM_THEME,
        fontFamily: 'Menlo, Monaco, "Courier New", monospace',
        fontSize: 13,
        cursorBlink: true,
        convertEol: true,
        scrollback: 5000,
      });
      const fit = new FitAddonClass.current();
      term.loadAddon(fit);
      term.open(mountRef.current);
      terminals.current[activeAgent] = term;
      fitAddons.current[activeAgent] = fit;

      // Forward keyboard input to PTY (unlocked by default, locked = explicitly false)
      const agentId = activeAgent;
      term.onData((data) => {
        if (unlockedRef.current[agentId] === false) return;
        const token = localStorage.getItem('auth_token');
        const headers = { 'Content-Type': 'application/json' };
        if (token) headers['Authorization'] = `Bearer ${token}`;
        fetch(`${BASE}/proxy/${agentId}/input`, {
          method: 'POST',
          headers,
          body: JSON.stringify({ text: data }),
        }).catch(() => {});
      });
    } else {
      // Re-attach to DOM
      const term = terminals.current[activeAgent];
      if (term.element && term.element.parentNode !== mountRef.current) {
        mountRef.current.appendChild(term.element);
      }
    }

    // Open WebSocket after fit so incoming data renders into sized terminal
    const agentIdForWs = activeAgent;
    requestAnimationFrame(() => {
      const fit = fitAddons.current[agentIdForWs];
      const term = terminals.current[agentIdForWs];
      if (fit && term) { fit.fit(); syncResize(agentIdForWs, term); }
      openProxyWs(agentIdForWs);
    });

    // Fit on resize. Guard against the observer firing after the terminal
    // has been disposed (e.g., when this view is unmounted from a split pane
    // while a resize is still queued) — fit() on a disposed term throws.
    const ro = new ResizeObserver(() => {
      const term = terminals.current[activeAgent];
      if (!term) return;
      try {
        fitAddons.current[activeAgent]?.fit();
        syncResize(activeAgent, term);
      } catch {}
    });
    if (mountRef.current) ro.observe(mountRef.current);

    return () => {
      ro.disconnect();
      // Cancel any pending reconnect for agents that are no longer active.
      for (const [id, handle] of Object.entries(reconnectTimers.current)) {
        if (id !== activeAgentRef.current && handle) {
          clearTimeout(handle);
          reconnectTimers.current[id] = null;
        }
      }
    };
  }, [activeAgent, xtermLoaded]);

  // Cleanup all on unmount
  useEffect(() => {
    return () => {
      for (const handle of Object.values(reconnectTimers.current)) {
        if (handle) clearTimeout(handle);
      }
      reconnectTimers.current = {};
      for (const ws of Object.values(sockets.current)) {
        ws.close();
      }
      for (const term of Object.values(terminals.current)) {
        term.dispose();
      }
      sockets.current = {};
      terminals.current = {};
      fitAddons.current = {};
    };
  }, []);

  function handleTabClick(agentId) {
    setActiveAgent(agentId);
  }

  function isUnlocked(agentId) {
    return unlocked[agentId] !== false; // default unlocked
  }

  function toggleLock() {
    if (!activeAgent) return;
    const nowUnlocked = !isUnlocked(activeAgent);
    const next = { ...unlocked, [activeAgent]: nowUnlocked };
    setUnlocked(next);
    unlockedRef.current = next;
    if (nowUnlocked) terminals.current[activeAgent]?.focus();
  }

  return html`
    <div class="terminal-view">
      <div class="terminal-header">
        <h2>Terminal</h2>
      </div>
      <div class="terminal-tabs">
        ${agentList.map(agent => {
          const id = typeof agent === 'string' ? agent : agent.id;
          const alive = typeof agent === 'object' ? agent.alive : true;
          const color = colors[id] || '#888';
          return html`
            <button
              key=${id}
              class=${'terminal-tab' + (activeAgent === id ? ' active' : '')}
              style=${{ borderBottomColor: activeAgent === id ? color : undefined, color: activeAgent === id ? color : undefined }}
              onClick=${() => handleTabClick(id)}
            >
              <span style=${{ display: 'inline-block', width: 7, height: 7, borderRadius: '50%', background: alive ? color : '#444', marginRight: 5 }}></span>
              ${id}
            </button>
          `;
        })}
        ${(() => {
          const active = agentList.find(a => (typeof a === 'string' ? a : a.id) === activeAgent);
          const runtime = agentRuntime(active);
          if (runtime === 'harness') return null;
          return html`
            <button
              class=${'terminal-lock-btn' + (isUnlocked(activeAgent) ? ' unlocked' : '')}
              onClick=${toggleLock}
              disabled=${!activeAgent}
              title=${isUnlocked(activeAgent) ? 'Direct input active — click to lock' : 'Click to unlock direct input'}
            >${isUnlocked(activeAgent) ? 'Unlocked' : 'Locked'}</button>
          `;
        })()}
      </div>
      ${(() => {
        const active = agentList.find(a => (typeof a === 'string' ? a : a.id) === activeAgent);
        const runtime = agentRuntime(active);
        if (runtime === 'harness' && activeAgent) {
          return html`<${HarnessActivity} agentId=${activeAgent} />`;
        }
        return html`<div class="terminal-container" ref=${mountRef}></div>`;
      })()}
    </div>
  `;
}
