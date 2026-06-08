# Pre-cutover Diligence Pass — Results

**Status:** pre-flight section 1–6 GREEN; aspect autospawn unresolved
**Date:** 2026-05-11
**Target:** test nexus :18888 against `C:\Users\jacin\AppData\Local\Temp\nexus-diligence` data dir
**Method:** systematic execution of the cutover checklist (`2026-05-09-crossing-cutover-checklist.md` §Pre-flight) against the test deployment.

## Cutover prep work landed inline

- `agents/keel/aspect.json`: `provider: claude-api` → `claude-code`. No API key in our setup; cutover runs claude-code subprocess. This is the production-cutover identity, not a diligence-only knob.

## Pre-flight checklist execution

### Section 1 — Nexus identity initialised — PASS

`nexus_id=9da33057-9871-4e24-aa58-9e713d719b65` already populated. Re-running `nexus identity init` returns "already initialised" cleanly.

### Section 2 — Tailnet TLS cert — PASS

Cert at `C:\src\agent-network\certs\server.crt` (Let's Encrypt, SAN includes `agentnetwork.<tailnet>.ts.net`). Broker accepts. Chrome trust verified earlier.

### Section 3 — Aspect provisioning — PASS (after fix)

**Initial state:** only `test-keel` minted in the test DB. None of the 7 production aspects (forge, wren, verity, keel, harrow, maren, anvil) nor plumb.

**Fix applied:**
```
nexus migrate personality-from-disk --aspect-dir <agents> --data-dir <data>
```
→ 8 personalities inserted (anvil, forge, harrow, keel, maren, plumb, verity, wren). test-keel skipped (already exists).

Then `nexus aspect mint <name>` for each of the 8 to bump them from placeholder pubkeys to real keyfiles. All 8 now show `version=2, status=active` in `nexus aspect list`.

**Cutover note:** the checklist mentions `nexus migrate personality-from-disk` only as a recovery step ("If an aspect isn't there, run …"). For a fresh production DB this is a REQUIRED step, not optional. Cutover doc should promote it from recovery to standard pre-flight.

### Section 4 — Operator passkey registered — DEFERRED

Skipped here because `NEXUS_AUTH_BYPASS=1` is set on the test deployment. Production cutover MUST run this step (passkey registration via SPA's first-boot wizard). No test evidence collected for it in this pass; the SPA WebAuthn ceremony has its own coverage in 5e.

### Section 5 — Operator login round-trip — DEFERRED (same reason as §4)

### Section 6 — Pre-cutover smoke (cutover-smoke script) — PASS

```
$ NEXUS_URL=wss://agentnetwork.<tailnet>.ts.net:18888 \
  NEXUS_DATA_DIR=$NEXUS_DATA_DIR \
  NEXUS_INSECURE=1 \
  go run ./scripts/cutover-smoke

[1/8] reading nexus identity
[2/8] minting operator JWT
[3/8] dialing wss://...:18888
[4/8] roster.list → 1 aspects online: keel
[5/8] subscribe.chat
[6/8] chat.send → chat.deliver
[7/8] knowledge.store → knowledge.list
[8/8] aspect.say → @keel msg_id=162
PASS — every operator-WS frame round-tripped clean.
```

## Real keel behaviour — DELIBERATION VERIFIED

The cutover-smoke ran two chat probes through the Frame harness. Broker log captured both turns:

```
funnel: turn complete aspect=keel steps=0 tool_calls=0
  input_tokens=5 output_tokens=30 cache_read=0 cache_create=23809
  cumulative=35 stop_reason=model_done
funnel: turn complete aspect=keel steps=0 tool_calls=0
  input_tokens=5 output_tokens=16 cache_read=0 cache_create=23809
  cumulative=56 stop_reason=model_done
```

Both turns fired claude-code subprocess, cached 23809-token prefix, produced real output, ended `model_done`. Frame harness alive.

Keel's actual replies in chat:

- `msg 163 (keel → reply_to 161)`: "Acknowledged — cutover-smoke probe 1778476627280527 received."
- `msg 164 (keel → reply_to 162)`: "Acknowledged, ignoring."

Both threaded correctly (reply_to set). Tone-appropriate for admin probes (terse, factual, no chatter). End-to-end: WS frame → broker chat.send → recipient policy → Frame inbox → funnel → bridle → claude-code subprocess → send_chat tool → broker chat.send-back → operator's chat.deliver subscription. Every link works.

## Outstanding gaps

### Gap 1 — Aspect autospawn disabled

Broker log: `auto-spawn dir set but no harness path; skipping dir=C:\src\nexus-cw\nexus\agents`

Effect: the 7 non-Frame aspects (forge, wren, verity, harrow, maren, anvil, plumb) won't auto-spawn at cutover step C3. Operator will see `roster.list` returning only `keel` after cutover.

Required: harness path config. Need to identify what value to set, which env var or flag, and verify each aspect's `agentfunnel.exe` (or equivalent) is buildable + on PATH. Pre-cutover blocker.

### Gap 2 — Cutover checklist drift

`nexus migrate personality-from-disk` is presented in the cutover checklist as a recovery step ("If an aspect isn't there…"). On a fresh production DB it is the **only** way to populate the aspect_personalities table. Should be promoted to a top-line pre-flight step.

### Gap 3 — Section 4 & 5 — RESOLVED (real bug found and fixed)

**First attempt:** WebAuthn registration completed (Windows Hello prompt accepted), but assertion failed silently. operator_passkeys table got a row but subsequent login asserted invalid.

**Root cause:** Default WebAuthn origin derivation in `nexus/cmd/nexus/main.go` `buildOperatorLogin` was `"https://" + rpID` with **no port**. The browser sends `https://agentnetwork.<tailnet>.ts.net:18888` and the broker checked against `https://agentnetwork.<tailnet>.ts.net` (no `:18888`). WebAuthn matches origins as exact strings — mismatch → silent reject. Production cutover would hit this on :7888 too unless operator knew to set `NEXUS_OPERATOR_ORIGINS` explicitly.

**Fix:** Added `deriveDefaultOrigin(rpID, addr)` helper that extracts the port from the listen addr and produces `"https://" + rpID + ":" + port` for non-standard ports. `:443` and unparseable addrs fall back to the original `"https://" + rpID`. `NEXUS_OPERATOR_ORIGINS` override still wins for front-proxy setups.

**Verification:**
- Pre-fix: required `NEXUS_OPERATOR_ORIGINS=https://...:18888` for any non-standard port. Diligence run hit silent assertion failure.
- Post-fix: launched without the env var; broker logs `origins=[https://agentnetwork.<tailnet>.ts.net:18888]`. Real Chrome registration + assertion + dashboard login round-tripped clean. `nexus operator list` shows `dMon` row with `last_used_at` populated.

### Gap 1 — Autospawn — DEFERRED

Per operator: aspects will be manually started post-cutover, so the autospawn harness path gap is non-blocking.

## Final status

Pre-flight 1–6 all GREEN. Real Frame harness deliberation verified end-to-end (chat → broker → funnel → bridle → claude-code subprocess → send_chat → broker → operator chat.deliver).

Two real bugs found and fixed inline:
1. WebAuthn origin default missing port (this doc, §Gap 3) — code fix in `main.go::deriveDefaultOrigin`.
2. `nexus migrate personality-from-disk` was documented as recovery but is required on fresh DB (this doc, §Gap 2) — doc fix in cutover checklist §3.

The cutover checklist now reflects reality. Ready to ship.
