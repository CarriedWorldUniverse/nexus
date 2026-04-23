# BUILD.md — Nexus v0.4

Build log and next-steps for the greenfield Nexus at `C:\src\nexus`.

Primary design reference: [`docs/2026-04-22-nexus-registration-spec.md`](docs/2026-04-22-nexus-registration-spec.md) (v0.4).

**v0.4 delta:** Tree-structured session JSONL (`id`/`parentId`), proactive compaction formula (`tokens > window - reserve`), rewind/fork/branch-summary as first-class ops. See spec header + §2.6, §2.7, §4.6.

**v0.3 delta:** JSONL-owns-state retained, enrichment-fiber registration adopted, thread TTL resolved (30-min idle reap / 5-min sweep), tool-authority runtime modes and plan/execute mode logged as future work.

## Status

**Current step:** §6.1 scaffold complete. Directory structure + stubs in place. No runtime code yet.

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
- [ ] **§6.2 Nexus core + registration endpoints** — broker skeleton, HTTPS, in-memory roster, `/aspects/register|heartbeat|deregister|list`. Smoke test with synthetic client.
- [ ] **§6.3 Single agent runtime + Claude API provider** — `agent.exe` reads aspect home, registers, heartbeats, handles comms dispatch. `claude-api` provider implements invoke/tokenCount/compact. Context persistence for all three modes.
- [ ] **§6.4 Hands end-to-end** — `kind:"hand"` dispatch. wren `verify-canon` as first cross-aspect test.
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
