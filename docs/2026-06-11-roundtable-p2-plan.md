# Roundtable P2 — Aspect-Owned Hands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An aspect can spawn fresh-context instances of *itself* (hands) mid-turn — same soul, derived identity, fully audit-threaded — and keep conversing while they work (NEX-571; spec component 2; recovers Hand Dispatch v0.1).

**Architecture:** A `spawn` MCP tool on the aspect's comms bridge → `spawn.request` WS frame → broker `Runner.SubmitSpawn` creates worker Jobs running the parent's image/persona under derived names (`<parent>.sub-N`, broker-minted scoped sessions, persona lookup falls back to the parent). Audit rides the proven dispatch thread-root pattern; results re-enter the thread asynchronously. Branch `feat/aspect-hands` off main (independent of P1; minor chat_send.go rebase expected).

**Key identity decision (locked):** the broker enforces one live session per name, and the parent is awake while its hands run — so hands register under derived names. **Naming (operator, 2026-06-11): no numbers.** Derived names are `<parent>.<word>` drawn from a per-aspect pool of kindred words (`AspectHandNames` config + built-in defaults): shadow → umbra, gloam, shade, dusk, silhouette, penumbra, murk, tenebra; plumb → bob, fathom, sound, level, datum, line; anvil → horn, hardy, temper, face, strike, pritchel; keel → ballast, skeg, stem, draft, hull; maren → brine, spume, swell, foam, pearl; harrow → tine, furrow, loam, glebe. A hand's name is leased for the Job's lifetime and returns to the pool on completion; `<parent>.hand-N` is the overflow fallback only when a pool is exhausted (cap 4 concurrent makes this unreachable in practice). The parent prefix keeps lineage parsing a string-split. v1 posts attribute `from: <parent>.<word>` (truthful audit; P4 mediation/UI may collapse lineage visually). Hands never mount the parent's session PVC (RWO, and hands are fresh-context by definition).

---

### Task A: Frames + spawn.request surface

**Files:** `nexus/frames/frames.go` (+payloads.go), `nexus/broker/ws.go` (aspect kind switch), new `nexus/broker/spawn.go` + test.

- [ ] Kinds `KindSpawnRequest "spawn.request"` / `KindSpawnResult "spawn.result"` — REGISTER BOTH IN `IsKnown` (known trap). Payloads:
  `SpawnRequestPayload{Brief string; Count int; Thread string}` (Count default 1; Thread optional — empty means root a fresh audit thread), `SpawnResultPayload{Hands []SpawnHandle}` with `SpawnHandle{RunID, Name string}`.
- [ ] Handler (aspect path only, registered aspects only): validate Brief non-empty, Count 1..`SpawnMaxPerRequest` (config, default 4); reject derived names spawning (no `sub-of-sub` in v1 — parent must be a base aspect). Respond `spawn.result` with handles or an error response. TDD with the broker's WS test fixtures.

### Task B: Derived-identity sessions + persona fallback

**Files:** `nexus/broker/` (the keyfile-validate / register path — read `aspect_jwt_verify.go`, the validate endpoint, and how SOUL/nexus.md are served on validate), `nexus/roster/roster.go`.

- [ ] Broker mints a scoped session credential for `<parent>.sub-N` at spawn time and injects it into the Job (env or projected secret — follow how dispatch Jobs receive `aspect-keyfile-<agent>` today and add the mint-injection path beside it; the derived credential must NOT be the parent's keyfile).
- [ ] Persona/config lookup: for names matching `<base>.sub-*`, serve the BASE aspect's SOUL.md/nexus.md/personality on validate. Roster: derived names register without the discovery-map WARN (lineage-aware accept), are excluded from wake policies/idle reaping (their lifecycle is the Job's), and show with a `lineage: <base>` marker in roster listings.
- [ ] TDD: derived mint round-trip (validate as sub → persona = parent's), roster accept + lineage, one-session-per-name still enforced per derived name.

### Task C: Runner.SubmitSpawn + audit threading

**Files:** `runtime/dispatch/runner.go` (+jobspec), test alongside.

- [ ] `SubmitSpawn(ctx, parent string, brief string, count int, thread string) ([]SpawnHandle, error)`: for N hands — lease derived names from the parent's hand-name pool (free names first, overflow fallback per the naming decision), build Jobs with the PARENT's image (JobConfig per-aspect image seam) + the derived credential + the brief; per-hand concurrency cap (`SpawnMaxConcurrent` per parent, config default 4) and the existing global MaxConc both apply; queue overflow behaves like Submit's queueing.
- [ ] Audit: if Thread empty, post (store) a root message `from=<parent>` summarizing the spawn (count + brief head) — reuse the !dispatch post-as-thread-root machinery; per hand, post its brief under the root on creation; OnJobDone already posts results to the thread — extend the completion summary with the hand's lineage. RunsStore rows: agent=derived name, ParentRunID semantics unchanged, DispatchMsgID=the audit root.
- [ ] TDD with the runner's existing fake K8s/Poster fixtures: N jobs, derived identities, caps, audit posts in order, results threaded.

### Task D: spawn tool on the aspect MCP bridge

**Files:** wherever the aspect's comms tools live (find `send_chat` in the MCP bridge the funnel materialises — `runtime/cmd/nexus-comms-mcp` or the broker MCP-bridge profile; follow `jira.go`'s url-escape pattern note if HTTP is involved).

- [ ] Tool `spawn{brief, count?, thread?}` → sends `spawn.request` over the aspect's existing WS → returns handles. Tool description teaches the contract: fire-and-forget, results arrive as thread messages, keep your turn short.
- [ ] TDD per that package's conventions.

### Task E: observe lineage + acceptance

- [ ] Verify (tests, minimal code): observe frames from `<parent>.sub-N` flow per-aspect as-is (GrouperFor keyed by name); `subscribe.observe` on a derived name works from the dashboard/agora trace pane.
- [ ] e2e on dMon after merge+deploy: shadow spawns 2 hands mid-conversation (probe brief: "summarize file X" class), keeps answering DMs while they run; briefs + results land threaded under the audit root; runs table shows lineage; pods exit on completion.

## Out of scope

Convene (P3), digest/mediation (P4), sub-of-sub spawning, cost ceilings beyond the concurrency caps (ticket if needed), herald DeriveAgentKey (the mint is broker-local until herald-rooted boot lands — note the seam).
