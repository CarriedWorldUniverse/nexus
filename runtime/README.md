# runtime/ — agent.exe

Single agent runtime. Provider-agnostic. Comms-first, no PTY.

Invocation: `agent.exe <aspect-home-folder>`

- `providers/` — AI backend plug-ins. Contract: `invoke / tokenCount / compact`.
- `context/` — session persistence (global.jsonl, threads/<id>.jsonl, or none).
- `dispatch/` — incoming comms → provider invocation pipeline.
- `hands/` — `kind:"hand"` dispatch, concurrency cap, cost attribution.

See spec §2.2–§2.4.
