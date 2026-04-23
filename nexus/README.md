# nexus/ — central process

Broker + orchestrator + embedded frame-agent. Single long-running Node (or Go) process.

- `broker/` — MCP server + REST API. Successor to `agent-network/code/broker/index.js`.
- `orchestrator/` — polling, watches, alarms, stale-aspect reaping.
- `frame/` — keel embedded as global-context harness. Populates at spec §6.5.
- `admin/` — `/nexus/*` admin endpoints (rewind, compact, shutdown).
