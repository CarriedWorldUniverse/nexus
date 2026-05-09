# Crossing Cutover Checklist

**Status:** ready-to-run; use as the operator's go/no-go gate the morning of cutover
**Companion to:** `2026-05-07-crossing-migration-spec.md` (Part 6)

This is the operator's pre-flight + cutover sequence. Crossing parts 1+3+4+5 are merged; the code substrate is complete. This doc names the ordered actions to switch the network from agent-network to nexus, and the rollback path if cutover goes sideways.

## Topology

- **Host:** `agentnetwork.<tailnet>.ts.net` (existing tailnet hostname).
- **Port:** **`7888`** — same port agent-network uses today. Locked in (operator chat 2026-05-10) so the migration is binary-swap, not "binary-swap + update every aspect's nexus_url + dashboard origin + tailnet cert SAN." Cost of changing the port is high (every keyfile + every dashboard reference); cost of keeping it is zero.
- **TLS cert:** existing tailnet cert covers the port; no SAN refresh needed.
- **Local-test convention:** smoke runs against alt ports (`:18888`, `:18889`) to avoid colliding with the running production broker on `:7888`. Production cutover always lands on `:7888`.

## When

Per spec §5.3: **big-bang on a low-traffic window.** Agent-network goes down; nexus comes up; aspects reconnect; smoke passes; operator's daily workflow flips. "Hours, not days."

Pick the window when:

- No aspect has a long-running task in flight (tail current chat for "working on X for the next hour" claims and wait for completion).
- The operator can babysit for ~2 hours straight without interruption.
- The operator has a known-good rollback (agent-network is still installed and runnable).

---

## Pre-flight (do this before the cutover window)

These can land days ahead. None of them require agent-network to be down.

### 1. Nexus identity initialised on the host

```
nexus identity init --host agentnetwork.<tailnet>.ts.net
```

Verifies: `nexus.db` exists at `$NEXUS_DATA_DIR`, `nexus_identity` row populated, server keypair + signing secret stored. Required for everything below — both passkey login and the smoke harness need this row.

Re-running `init` on an already-initialised Nexus is a no-op; safe to verify.

### 2. Tailnet TLS cert in place

```
nexus cert init --host agentnetwork.<tailnet>.ts.net
```

Per spec §3.5. The dashboard SPA's WebAuthn ceremony refuses on a non-https origin (browser policy, not our enforcement); without this the operator can't log in.

Verify the cert applies to the hostname the operator's browser will type — passkeys are bound to the RPID the server advertises, and an RPID mismatch silently fails the assertion.

### 3. Aspect provisioning verified

```
nexus aspect list
```

Should show all 7 aspects (forge, wren, verity, keel, harrow, maren, anvil) plus plumb if registered. Each minted with a current keyfile version. If an aspect isn't there, run `nexus migrate personality-from-disk` then `nexus aspect mint <name>` per Crossing Part 1.

Aspect homes in `<nexus-cw>/nexus/agents/<name>/` should each have:

- `aspect.json`
- `NEXUS.md`
- `SOUL.md`
- `PRIMER.md`

The keyfile artifact lands in the aspect's home as part of mint.

### 4. Operator passkey registered

The first time, this is the bootstrap path. Open `https://agentnetwork.<tailnet>.ts.net:7888/dashboard/` in a browser; the Login overlay should land on the "Register this device" form (because `operator_passkeys` is empty). Complete the WebAuthn ceremony with a label like "<operator-host>" or "dMon".

Verify via:

```
nexus operator list
```

— should show one row, `last_used_at = (never)`. Registering a second device is a follow-up: log in first, then go through the SPA's add-device flow (5e SPA does not yet expose this UI; for now, second-device registration runs through a test browser session sharing the same JWT-bearing Authorization header).

### 5. Operator login round-trip

Click anywhere outside the Login form, refresh — the SPA should land on the "Touch your passkey to sign in…" prompt instead of register. Complete the assertion. The dashboard chrome should mount with at least the Status / Agents views populated (from `roster.list` against the nexus broker; the agent-network aspects haven't moved over yet so the list will be empty — that's fine, the empty result is what we expect pre-cutover).

`nexus operator list` should show `last_used_at` updated to the current time.

### 6. Pre-cutover smoke against the running nexus

```
NEXUS_URL=wss://agentnetwork.<tailnet>.ts.net:7888 \
NEXUS_DATA_DIR=$NEXUS_DATA_DIR \
go run ./scripts/cutover-smoke
```

This runs against a *live* nexus + uses the broker's actual signing secret to forge a smoke-only operator JWT (so it works without WebAuthn). Eight checks land in order:

1. read nexus identity from disk
2. mint operator JWT
3. dial /connect over WS
4. roster.list — current aspect set (likely empty pre-cutover)
5. subscribe.chat
6. chat.send → chat.deliver round-trip (verifies persist + push)
7. knowledge.store → knowledge.list round-trip
8. aspect.say (skipped when roster is empty)

If this prints `PASS — every operator-WS frame round-tripped clean…`, the wire is healthy. If it prints `FAIL: <step>`, do NOT cut over until the failed step is understood. The most common failures are:

- `identity.Load` → `nexus identity init` hasn't run.
- `ws dial` → broker not running, wrong URL, TLS hostname mismatch.
- `chat.deliver round-trip never arrived` → operator JWT was minted but the broker's TokenInfo.Operator flag isn't set; check 5b2 (resolveUpgradeAuth's JWT fallback).
- `knowledge.list: probe entry not visible` → operator-frame Knowledge handlers are misconfigured; the handler defaults to `agent="operator"` which should match the store call.

---

## Cutover sequence (the actual switch)

Time-budget guidance: ~30 minutes if everything goes clean; up to 2 hours if any aspect's startup needs a tweak.

### Step C1 — announce in chat

Post to operator chat (in agent-network — this is the last thing the agent-network broker will deliver):

> "🚪 cutover starting. agent-network shutting down in 5 minutes. nexus comes up next. expect 5–10 minutes of network-quiet then aspects reconnect there."

Five-minute warning gives any aspect mid-thought time to wrap up.

### Step C2 — agent-network down

On the host running agent-network:

```
# stop the broker (whatever supervisor you're using — pm2, systemd, foreground)
pm2 stop agent-network        # or your equivalent
```

Verify: `curl -k https://agentnetwork.<tailnet>.ts.net:7888/health` returns connection refused. No process named `node` or `comms-mcp.exe` still bound to 7888.

### Step C3 — nexus up

```
NEXUS_OPERATOR_RPID=agentnetwork.<tailnet>.ts.net \
NEXUS_OPERATOR_ORIGINS=https://agentnetwork.<tailnet>.ts.net:7888 \
NEXUS_DATA_DIR=$NEXUS_DATA_DIR \
NEXUS_TLS_CERT=$NEXUS_TLS_CERT \
NEXUS_TLS_KEY=$NEXUS_TLS_KEY \
nexus
```

Watch the startup log for:

- `frame: …` lines confirming the embedded Frame loaded with the right aspect.
- `aspect bootstrap: spawned …` lines for each auto_spawn aspect.
- `operator login enabled` (if RPID was set).

If any aspect fails to spawn, capture the error and decide: fix on the spot, or roll back per "Rollback" below.

### Step C4 — verify aspects reconnect

```
nexus operator list   # confirms operator login still works
curl -k https://agentnetwork.<tailnet>.ts.net:7888/api/aspects   # JSON roster
```

Or open the dashboard at /dashboard/. The Status view should show every auto_spawn aspect transition through "live" within ~30 seconds.

### Step C5 — operator chat round-trip

Through the dashboard:

1. Type a message in the chat view.
2. Verify it lands (your own message renders).
3. Wait for at least one aspect to react / reply (👀 reaction is enough).

Round-trip latency under ~5s on a healthy network. If your message never delivers, the most likely cause is the operator JWT WS path (5b2) — check the broker log for token-resolution errors at the /connect upgrade.

### Step C6 — announce completion

Post to operator chat (in nexus now):

> "✅ cutover complete. nexus is live. let me know if anything looks off."

---

## Rollback

If cutover goes sideways at any step, the rollback is symmetric:

1. Stop nexus (`Ctrl-C` or supervisor stop).
2. Start agent-network (`pm2 start agent-network` or equivalent).
3. Wait for aspects to reconnect to agent-network.
4. Announce: "↩ rolling back to agent-network; <reason>. will retry cutover later."

Aspect homes are unchanged by cutover — both nexus and agent-network read the same per-aspect directories. Rollback doesn't lose state on the aspect side. The only thing nexus mutated during the failed cutover is its own DB (chat history, knowledge entries written during the smoke + early operator activity), and that just becomes data that's only visible from nexus until the next cutover succeeds.

---

## What is NOT in this checklist

- **Chat history migration.** Per spec §5.1: summarize-then-fresh. The agent-network chat timeline is dropped; cutover lands on a clean substrate. If a specific decision or rationale needs to survive, the operator captures it as a `knowledge.store` entry on either side BEFORE cutover.
- **Tickets.** Crossing Part 2 is deferred (folds into cairn). The Tickets view in the dashboard renders an empty placeholder.
- **Files / Usage / Terminal views.** Spec §3.5 deferred placeholders.
- **Dashboard rewrite.** Post-Crossing per #173.

---

## After cutover

- **Day 0:** Babysit. Tail the chat. If an aspect starts behaving strangely, the most likely cause is a personality file referencing an agent-network-specific topology (port number, comms-mcp tool name, broker URL). Fix opportunistically (per spec §5.4 — "straight copy then opportunistic edits as aspects onboard").
- **Day 1:** Confirm overnight idle behavior — no spurious reconnect storms, no log spam, no aspect stuck in "stale" status indefinitely.
- **Week 1:** Monitor passkey + login UX. The dashboard's "register a second device" path doesn't yet have UI in 5e; if the operator wants a second device, that's a 5e follow-up.

---

## Open follow-ups (post-cutover, not blocking)

- Topics persistence (chat_messages.topic column + ListTopics method) — defers the dashboard's Topics view from the placeholder.
- Reaction live updates (currently SPA refetches on demand; a fan-out frame for chat_reactions could light the live counter).
- Add-device flow in the SPA (operator-managed via CLI today).
- Mobile layout passes per #104-#108.
- Status pulse origin (#118) — the WS frame is wired (5d aspect.status_pulse) but no aspect emits one yet.
