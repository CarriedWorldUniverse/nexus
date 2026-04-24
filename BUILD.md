# BUILD.md — Nexus v0.4

Build log and next-steps for the greenfield Nexus at `C:\src\nexus`.

Primary design reference: [`docs/2026-04-22-nexus-registration-spec.md`](docs/2026-04-22-nexus-registration-spec.md) (v0.4).

**v0.4 delta:** Tree-structured session JSONL (`id`/`parentId`), proactive compaction formula (`tokens > window - reserve`), rewind/fork/branch-summary as first-class ops. See spec header + §2.6, §2.7, §4.6.

**v0.3 delta:** JSONL-owns-state retained, enrichment-fiber registration adopted, thread TTL resolved (30-min idle reap / 5-min sweep), tool-authority runtime modes and plan/execute mode logged as future work.

## Status

**Current step:** §6.3 single-agent runtime complete. Nexus broker + agent runtime + Claude provider + Ollama embedding + knowledge store + tree JSONL + compaction all landed in 8 PRs over a single session. Live smoke script at `scripts/smoke-e2e` drives a real Nexus + agent + Claude round-trip. No TLS yet (plain HTTP for local dev).

**Running in parallel:** `C:\src\agent-network` (current production) continues to run. Nexus is built alongside; cutover happens at §6.8.

## Directory map

```
nexus/                    # central process: broker + orchestrator + frame-agent (embedded keel)
  broker/                 # MCP + REST on port 7888 successor
  orchestrator/           # polling, watches, alarms, stale-reaping
  frame/                  # keel-as-embedded-harness (§6 step 5 — populates later)
  admin/                  # /nexus/* admin endpoints (rewind, compact, shutdown)

runtime/                  # the single agent executable
  providers/              # AI backend plug-ins (contract: invoke/tokenCount/compact)
    claude-api/           # v1 primary
    claude-code/          # optional wrapper (deprecated path, keep stub)
    openai-api/           # v2 stub
  context/                # session persistence (global/thread/stateless JSONL)
  dispatch/               # comms message → invoke pipeline
  hands/                  # kind:"hand" dispatch + concurrency + cost attribution

agents/                   # per-aspect home folders (empty at scaffold time)
  # populated by §6 step 6 — each contains aspect.json, CLAUDE.md, SOUL.md,
  # PRIMER.md, .credentials/, session/, memory/, logs/

shared/                   # cross-cutting
  paths/                  # path resolution (successor to code/paths.js)
  auth/                   # NEXUS_TOKEN v1; per-aspect tokens v2
  schemas/                # aspect.json, registration, hand envelopes

docs/                     # specs, decisions, design notes
scripts/                  # build/launch/migrate helpers
tests/                    # end-to-end test harness (wren verify-canon = first)
```

## Next steps

Per spec §9 and §6:

- [x] **§6.1 Scaffold** — directory structure, stubs, BUILD.md, spec copied in.
- [x] **§6.2 Nexus core + registration endpoints** — broker (`nexus/broker`), in-memory roster (`nexus/roster`), entry point (`nexus/cmd/nexus`), smoke test (`scripts/smoke-register`). Endpoints: `/health`, `/aspects/register`, `/aspects/heartbeat`, `/aspects/deregister`, `/aspects`. Bearer-token auth. Stale-reap sweeper in `main.go`. **TLS deferred** — v1 runs plain HTTP on loopback; TLS lands when we wire the first real aspect.
- [x] **§6.3 Single agent runtime + Claude API provider** — delivered in 8 sub-parts, each branch → code → review → test → merge:
  - 1. Storage bootstrap (`nexus/storage`) — SQLite + FTS5, `sqlite-vec` deferred.
  - 2. Provider interface + `claude-api` adapter (invoke, token count, capabilities).
  - 3. `ollama-local` embeddings adapter (`nomic-embed-text`, 768-dim).
  - 4. Knowledge store (`nexus/knowledge`) — FTS5 retrieval with scope flags.
  - 5. Tree-structured session JSONL (`runtime/context/tree`) — head sidecar, fsync, fork.
  - 6. Compaction (`runtime/compactor`) — active-system-prompt preservation; real `claude-api` Compact via Haiku.
  - 7. Agent runtime (`runtime/agent` + `runtime/cmd/agent`) — register/heartbeat/deregister + POST /turn dispatch.
  - 8. E2E smoke (`scripts/smoke-e2e`) — `-live` mode builds both binaries, spawns them, hits Claude for a real turn.

  Deferred to follow-ups (flagged with TODO in code):
  - Thread + stateless context modes (warn-only at startup; global-mode served).
  - Tool-call execution end-to-end (runtime rejects tool-result entries in `buildMessages` with ErrUnsupported).
  - Compaction trigger wired into dispatch loop (compactor.Run exists; not yet called automatically).
  - Per-write / per-search telemetry counters (§2.8).
  - SOUL.md / CLAUDE.md / PRIMER composition into SystemPrompt.
  - §2.8 active-retrieval injection at thread start.
  - Cost accounting attribution.
  - sqlite-vec activation (columns reserved; extension load deferred pending upstream binding fix).
- [x] **§6.4 Hands end-to-end** — delivered via the WS-first transport reshape per `docs/2026-04-25-nexus-transport-spec.md`, 11 sub-parts:
  - 0. Transport spec v0.1 committed.
  - 1. `nexus/frames` — Envelope, Kind constants, payloads, coder/websocket locked.
  - 2. Nexus WS endpoint at `/connect`, register/deregister as WS frames.
  - 3. Aspect WS client (`runtime/wsclient`) + agent reshape — HTTP server replaced with WS handler.
  - 4. Turn dispatch (server side) — `Broker.SendTurn`, response correlation via Dispatcher.
  - 5. Session projection upward — `session.entry.appended` frames; `nexus/sessions` projection table.
  - 6. `nexus/outpost` — per-host relay binary + cmd/outpost; register forwarding with via_outpost stamp.
  - 7. Dispatch queue (`nexus/handqueue`) + spawn mechanics (`runtime/handexec`) — harness `--hand` mode; SpawnExecutor.
  - 8. Auto-spawn on startup (`nexus/autospawn`) — scan aspect dir, fire-and-forget harness subprocesses.
  - 9. First real hand — `agents/wren` with verify-canon declaration + subprocess-level spawn test.
  - 10. Cross-aspect e2e smoke — WS-first smoke script with default (fake harness) and -live (real Claude) modes.

  Deferred / known gaps:
  - Outpost-side dispatch queue (cross-host hand routing): v1 only Nexus has a queue; hands only run on aspects whose home is on the Nexus host.
  - Turn frame routing downward THROUGH an Outpost: spec gap (frame doesn't carry a target aspect identifier for routing); needs a dedicated routing header. Direct-aspect turn dispatch works.
  - Shutdown frame handling beyond log-and-drop.
  - Per-aspect concurrency caps vs global handqueue max.
- [ ] **§6.5 keel embedded** — keel folds into Nexus process as global-context harness. No PTY. `@keel` preserved.
- [ ] **§6.6 Migrate remaining aspects** — home folders populated; old proxies stood down one-by-one.
- [ ] **§6.7 Dashboard migration** — live-feed from `/aspects`, views re-pointed.
- [ ] **§6.8 Retire `C:\src\agent-network`** — archive + remove from startup.

## Open questions (from spec §7)

Tracked in the spec. Notable blockers-if-unresolved:
- Context mode naming (`global`/`thread`/`stateless` placeholders — keeping unless better surfaces)
- CLAUDE.md vs AGENT.md — align with harness-v2
- Port allocation under no-PTY — leaning drop, heartbeat only
- Thread-session TTL/eviction

## Tech choices

- **Language / runtime: Go** (locked 2026-04-24, #7613). Single static binary per platform, trivial cross-compile (`GOOS=linux GOARCH=amd64`), already have Go comms/orchestrator to port concepts from. Two executables: `nexus` and `agent`.
- **HTTPS certs.** Tailscale MagicDNS cert reuse from agent-network, or self-signed fresh. Decide before §6.2.
- **DB.** SQLite for comms + roster, same as current. Fresh `comms.db` or migrate. Decide before §6.2.

## Platform constraints

- **Cross-platform mandatory** — Windows + Linux at minimum, Mac preferred. Ruled out: Windows-only tech (Task Scheduler-specific hooks, `.exe`-only distribution paths, win32 APIs). Process-launch and home-folder resolution must work with both `\` and `/` path separators.

## Ground rules

- No PTY. Ever. Comms-first, headless only.
- Single runtime. Context mode is an aspect property, not a runtime variant.
- Runtime owns session state; providers are swappable.
- Greenfield — don't import code from `C:\src\agent-network` wholesale; port concepts, re-implement.
