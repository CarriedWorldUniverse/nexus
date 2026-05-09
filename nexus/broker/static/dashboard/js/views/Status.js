const { html, useState, useEffect } = window.__preact;

import { fetchAgents, fetchUsage, BASE } from '../api.js';
import { RegisterDeviceButton } from '../components/Auth.js';
import { agentColors, usageData } from '../state.js';

function fmtTokens(n) {
  if (!n || n === 0) return '0';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000)     return (n / 1_000).toFixed(0) + 'k';
  return String(n);
}

function useConfirm(timeout = 3000) {
  const [confirming, setConfirming] = useState(null);

  function request(key, action) {
    if (confirming === key) {
      setConfirming(null);
      action();
    } else {
      setConfirming(key);
      setTimeout(() => setConfirming(c => c === key ? null : c), timeout);
    }
  }

  return { confirming, request };
}

function authHeaders() {
  const token = localStorage.getItem('auth_token');
  return token ? { 'Authorization': `Bearer ${token}` } : {};
}

async function authedGet(path) {
  const res = await fetch(`${BASE}${path}`, { headers: authHeaders() });
  if (!res.ok) throw new Error(`${res.status}`);
  return res.json();
}

async function postAction(path) {
  const res = await fetch(`${BASE}${path}`, { method: 'POST', headers: authHeaders() });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json().catch(() => ({}));
}

function renderAgentGroup(title, agents, colors, confirming, request, restartAgent, usage) {
  if (!agents || agents.length === 0) return null;
  return html`
    <div class="status-section">
      <div class="section-title">${title}</div>
      ${agents.map(agent => {
        const id = typeof agent === 'string' ? agent : agent.id;
        const color = colors[id] || '#888';
        const alive = agent.alive === true;
        const key = `agent-${id}`;
        const nexus = agent.nexus || {};
        const details = [];
        if (nexus.title) details.push(nexus.title);
        if (agent.port) details.push(`port ${agent.port}`);
        if (agent.pid) details.push(`pid ${agent.pid}`);
        if (agent.escalated && agent.tier) details.push(`tier: ${agent.tier}`);
        if (agent.rate_limited) details.push('rate-limited');
        if (agent.buffer_lines != null) details.push(`buf: ${agent.buffer_lines}`);
        const taskStr = agent.task || '';
        const u = usage?.by_agent?.[id];

        return html`
          <div class="status-card" key=${id}>
            <div class="card-body">
              <div class="card-row">
                <div class=${'card-pip ' + (alive ? 'alive' : 'dead')}></div>
                <div class="card-name" style=${'color:' + color}>@${id}</div>
                <div class="card-detail">${details.join(' · ')}</div>
                ${nexus.domain && html`<span class="badge-domain">${nexus.domain}</span>`}
                ${agent.rate_limited && html`<span class="badge-ratelimit">rate limited</span>`}
                ${agent.escalated && html`<span class="badge-escalated">${agent.tier || 'escalated'}</span>`}
              </div>
              ${taskStr && html`<div class="card-task">${taskStr}</div>`}
              ${u && html`
                <div class="card-tokens">
                  <span class="token-stat out" title="Output tokens (7d)">${fmtTokens(u.output)} out</span>
                  <span class="token-sep">·</span>
                  <span class="token-stat cache" title="Cache read tokens (7d)">${fmtTokens(u.cache_read)} cached</span>
                  <span class="token-sep">·</span>
                  <span class="token-stat turns" title="Turns this week">${u.turns} turns</span>
                </div>
              `}
            </div>
            <div class="card-actions">
              <button
                class=${'action-btn' + (confirming === key ? ' confirming' : '')}
                onClick=${() => request(key, () => restartAgent(id))}
              >${confirming === key ? 'Confirm?' : 'Restart'}</button>
            </div>
          </div>
        `;
      })}
    </div>
  `;
}

export function Status() {
  const [broker, setBroker] = useState(null);
  const [alarms, setAlarms] = useState(null);
  const [agentList, setAgentList] = useState([]);
  const [error, setError] = useState(null);
  const [lastRefresh, setLastRefresh] = useState(null);
  const [usagePeriod, setUsagePeriod] = useState('7d');
  const [networkMode, setNetworkMode] = useState(null); // { mode, since }
  const colors = agentColors.value;
  const { confirming, request } = useConfirm(3000);
  const usage = usageData.value;

  async function refresh() {
    try {
      const [brokerRes, alarmsRes, agentsRes, modeRes] = await Promise.allSettled([
        fetch(`${BASE}/health`).then(r => r.json()),
        authedGet('/api/alarms'),
        fetchAgents(),
        authedGet('/api/network/mode'),
      ]);
      setBroker(brokerRes.status === 'fulfilled' ? brokerRes.value : null);
      setAlarms(alarmsRes.status === 'fulfilled' ? alarmsRes.value : null);
      setAgentList(agentsRes.status === 'fulfilled' ? (agentsRes.value || []) : []);
      setNetworkMode(modeRes.status === 'fulfilled' ? modeRes.value : null);
      setLastRefresh(new Date());
      setError(null);
    } catch (e) {
      setError(e.message);
    }
  }

  async function refreshUsage(period) {
    try {
      const data = await fetchUsage(period || usagePeriod);
      usageData.value = data;
    } catch (e) {
      console.error('[Status] fetchUsage failed', e);
    }
  }

  useEffect(() => {
    refresh();
    const iv = setInterval(refresh, 8000);
    return () => clearInterval(iv);
  }, []);

  useEffect(() => {
    refreshUsage(usagePeriod);
    // Refresh usage every 5 minutes — it reads files so no need to hammer it
    const iv = setInterval(() => refreshUsage(usagePeriod), 5 * 60 * 1000);
    return () => clearInterval(iv);
  }, [usagePeriod]);

  async function restartAgent(id) {
    try {
      const result = await postAction(`/api/agents/${encodeURIComponent(id)}/restart`);
      // Harness restart kills the process and relies on network.js respawn
      // (~3s delay + spawn/listen time). Proxy restart reuses the wrapper
      // and is near-instant.
      const delay = result && result.runtime === 'harness' ? 5000 : 1000;
      setTimeout(refresh, delay);
    } catch (e) {
      console.error('[Status] restart agent failed', e);
    }
  }

  async function restartService(name) {
    try {
      await postAction(`/api/service/${encodeURIComponent(name)}/restart`);
      setTimeout(refresh, 1500);
    } catch (e) {
      console.error('[Status] restart service failed', e);
    }
  }

  async function restartNetwork() {
    try {
      await postAction('/api/network/restart');
      setTimeout(refresh, 2000);
    } catch (e) {
      console.error('[Status] restart network failed', e);
    }
  }

  async function shutdownNetwork() {
    try {
      await postAction('/api/network/shutdown');
    } catch (e) {
      console.error('[Status] shutdown failed', e);
    }
  }

  async function enterMaintenance() {
    try {
      await postAction('/api/network/maintenance');
      setTimeout(refresh, 3000);
    } catch (e) {
      console.error('[Status] enter maintenance failed', e);
      setError(`Maintenance mode failed: ${e.message}`);
    }
  }

  async function restartFrame() {
    try {
      await postAction('/api/network/restart-frame');
      setTimeout(refresh, 3000);
    } catch (e) {
      console.error('[Status] restart frame failed', e);
      setError(`Restart frame failed: ${e.message}`);
    }
  }

  const brokerOk = broker && broker.status === 'ok';
  const orchOk = alarms !== null && Array.isArray(alarms);
  const inMaintenance = networkMode && networkMode.mode === 'maintenance';

  return html`
    <div class="status-view">
      ${inMaintenance && html`
        <div class="maintenance-banner">
          <span class="maintenance-icon">⚠</span>
          <span class="maintenance-text">
            Frame in maintenance mode — broker only.
            ${networkMode.since && html` <span class="maintenance-since">(since ${new Date(networkMode.since).toLocaleString()})</span>`}
          </span>
          <button
            class=${'maintenance-restart-btn' + (confirming === 'restart-frame' ? ' confirming' : '')}
            onClick=${() => request('restart-frame', restartFrame)}
          >${confirming === 'restart-frame' ? 'Confirm restart?' : 'Restart Network'}</button>
        </div>
      `}

      <div class="status-header">
        <h2>The Nexus — Status</h2>
        <div class="status-header-right">
          ${lastRefresh && html`<span class="status-timestamp">${lastRefresh.toLocaleTimeString()}</span>`}
          <button class="status-refresh-btn" onClick=${refresh}>Refresh</button>
        </div>
      </div>

      <div class="status-grid">

        <div class="status-section">
          <div class="section-title">Infrastructure</div>

          <div class="status-card">
            <div class=${'card-pip ' + (brokerOk ? 'alive' : 'dead')}></div>
            <div class="card-name">Broker</div>
            <div class="card-detail">
              ${brokerOk
                ? `port ${broker.port} — ok`
                : broker === null ? 'unreachable' : `status: ${broker.status}`}
            </div>
            <div class="card-actions">
              <button
                class=${'action-btn' + (confirming === 'broker' ? ' confirming' : '')}
                onClick=${() => request('broker', () => restartService('broker'))}
              >${confirming === 'broker' ? 'Confirm?' : 'Restart'}</button>
            </div>
          </div>

          <div class="status-card">
            <div class=${'card-pip ' + (orchOk ? 'alive' : 'dead')}></div>
            <div class="card-name">Orchestrator</div>
            <div class="card-detail">
              ${orchOk
                ? `${alarms.length} alarm${alarms.length !== 1 ? 's' : ''} active`
                : 'not responding'}
            </div>
            <div class="card-actions">
              <button
                class=${'action-btn' + (confirming === 'orchestrator' ? ' confirming' : '')}
                onClick=${() => request('orchestrator', () => restartService('orchestrator'))}
              >${confirming === 'orchestrator' ? 'Confirm?' : 'Restart'}</button>
            </div>
          </div>
        </div>

        ${renderAgentGroup('The Frame', agentList.filter(a => a.nexus?.classification === 'frame'), colors, confirming, request, restartAgent, usage)}
        ${renderAgentGroup('Aspects', agentList.filter(a => a.nexus?.classification === 'aspect'), colors, confirming, request, restartAgent, usage)}
        ${renderAgentGroup('Hands', agentList.filter(a => a.nexus?.classification === 'hand'), colors, confirming, request, restartAgent, usage)}
        ${renderAgentGroup('Other Agents', agentList.filter(a => !a.nexus), colors, confirming, request, restartAgent, usage)}

        ${usage && html`
          <div class="status-section">
            <div class="section-title">
              Token Usage
              <span class="usage-period-picker">
                ${['7d','30d','all'].map(p => html`
                  <button
                    key=${p}
                    class=${'usage-period-btn' + (usagePeriod === p ? ' active' : '')}
                    onClick=${() => setUsagePeriod(p)}
                  >${p}</button>
                `)}
              </span>
            </div>
            <div class="usage-totals">
              <div class="usage-total-item">
                <span class="usage-total-label">Output</span>
                <span class="usage-total-value">${fmtTokens(usage.totals?.output)}</span>
              </div>
              <div class="usage-total-item">
                <span class="usage-total-label">Cache read</span>
                <span class="usage-total-value cache">${fmtTokens(usage.totals?.cache_read)}</span>
              </div>
              <div class="usage-total-item">
                <span class="usage-total-label">Cache created</span>
                <span class="usage-total-value">${fmtTokens(usage.totals?.cache_creation)}</span>
              </div>
              <div class="usage-total-item">
                <span class="usage-total-label">Turns</span>
                <span class="usage-total-value">${usage.totals?.turns?.toLocaleString()}</span>
              </div>
            </div>
          </div>
        `}

        <div class="status-section">
          <div class="section-title">Network Controls</div>
          <div class="network-controls">
            <button
              class=${'network-btn restart-btn' + (confirming === 'network-restart' ? ' confirming' : '')}
              onClick=${() => request('network-restart', restartNetwork)}
              disabled=${inMaintenance}
            >${confirming === 'network-restart' ? 'Confirm restart?' : 'Restart Network'}</button>
            <button
              class=${'network-btn maintenance-btn' + (confirming === 'network-maintenance' ? ' confirming' : '')}
              onClick=${() => request('network-maintenance', enterMaintenance)}
              disabled=${inMaintenance}
              title="Shut down frame (agents + orchestrator), keep broker and dashboard running"
            >${confirming === 'network-maintenance' ? 'Confirm maintenance?' : 'Maintenance Mode'}</button>
            <button
              class=${'network-btn shutdown-btn' + (confirming === 'network-shutdown' ? ' confirming' : '')}
              onClick=${() => request('network-shutdown', shutdownNetwork)}
            >${confirming === 'network-shutdown' ? 'Confirm shutdown?' : 'Shutdown'}</button>
            <${RegisterDeviceButton} />
          </div>
        </div>

        ${error && html`<div class="status-error">${error}</div>`}

      </div>
    </div>
  `;
}
