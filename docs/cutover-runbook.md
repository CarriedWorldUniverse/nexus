# Cutover Runbook — agent-network → nexus

**Pre-flight done:** 2026-05-11. Production data dir is `C:\src\nexus-cw\data` with `nexus.db` initialised, 9 aspects minted (8 production + test-keel), 46 knowledge rows migrated (canon + research from agent-network).

**Approach:** two-port swap. Stand up nexus on `:18888` while agent-network is still alive on `:7888`, validate, then cut over to `:7888`.

**Production token:** `<set NEXUS_TOKEN before launch — operator picks value>`.

**Origins allow-list spans both ports** so a single passkey works on `:18888` and `:7888`. The cutover command sets `NEXUS_OPERATOR_ORIGINS=https://agentnetwork.<tailnet>.ts.net:18888,https://agentnetwork.<tailnet>.ts.net:7888`.

---

## Phase A — Pre-flight (DONE 2026-05-11)

For the record, completed steps:

```
nexus identity init --data-dir C:\src\nexus-cw\data
# → nexus_id: 7a5f2d56-de16-40e8-8505-3360cd982d1d

nexus migrate personality-from-disk --aspect-dir C:\src\nexus-cw\nexus\agents --data-dir C:\src\nexus-cw\data
# → 9 aspects inserted

for aspect in forge wren verity keel harrow maren anvil plumb:
  nexus aspect mint $aspect --out C:\src\nexus-cw\data\keyfiles\$aspect.keyfile.json --nexus-url wss://agentnetwork.<tailnet>.ts.net:7888 --data-dir C:\src\nexus-cw\data
# → 8 keyfiles, all version=2 status=active

go run ./scripts/migrate-knowledge --source C:\src\agent-network\runtime\comms.db --data-dir C:\src\nexus-cw\data --from-agent canon
go run ./scripts/migrate-knowledge --source C:\src\agent-network\runtime\comms.db --data-dir C:\src\nexus-cw\data --from-agent research
# → 22 canon + 24 research = 46 rows migrated, 0 skipped, 0 failed
```

---

## Phase B — Validation on :18888

Run alongside the live agent-network. agent-network keeps :7888.

### B1. Start nexus on :18888

```powershell
$env:NEXUS_TOKEN = "<production-token>"
$env:NEXUS_OPERATOR_RPID = "agentnetwork.<tailnet>.ts.net"
$env:NEXUS_OPERATOR_ORIGINS = "https://agentnetwork.<tailnet>.ts.net:18888,https://agentnetwork.<tailnet>.ts.net:7888"
$env:NEXUS_ALLOW_LEGACY_MASTER = "1"
# DO NOT set NEXUS_AUTH_BYPASS — production needs passkey

Start-Process -FilePath "C:\src\nexus-cw\nexus\nexus.exe" `
  -ArgumentList @(
    "-data-dir", "C:\src\nexus-cw\data",
    "-addr", ":18888",
    "-aspect-dir", "C:\src\nexus-cw\nexus\agents",
    "--tls-cert", "C:\src\agent-network\certs\server.crt",
    "--tls-key",  "C:\src\agent-network\certs\server.key"
  ) `
  -WorkingDirectory "C:\src\nexus-cw\nexus" `
  -RedirectStandardOutput "C:\src\nexus-cw\data\stdout.log" `
  -RedirectStandardError  "C:\src\nexus-cw\data\stderr.log" `
  -WindowStyle Hidden
```

### B2. Verify startup log

```powershell
Get-Content C:\src\nexus-cw\data\stderr.log -Tail 20
```

Look for ALL of:

- `frame: detected name=keel path=C:\src\nexus-cw\nexus\agents\keel`
- `frame: embedded as in-process aspect ... provider=claude-code personality_loaded=true personality_version=1`
- `frame funnel: rewriter enabled aspect=keel distiller_provider=claude-code`
- `frame funnel: deliberation loop ready frame=keel provider=claude-code model=claude-opus-4-7`
- `operator login enabled rp_id=agentnetwork.<tailnet>.ts.net origins=[https://agentnetwork.<tailnet>.ts.net:18888 https://agentnetwork.<tailnet>.ts.net:7888]`
- `broker listening addr=:18888`

If ANY are missing or there's an `ERROR` line — STOP, fix, retry. Don't proceed with a half-broken nexus.

### B3. Passkey registration

Open in browser: `https://agentnetwork.<tailnet>.ts.net:18888/dashboard/`

You should see "Register this device" overlay (operator_passkeys table is empty).

Complete the WebAuthn ceremony — label it `dMon-prod` or similar.

Refresh; dashboard mounts, Status view shows keel as only live aspect.

Verify:
```powershell
C:\src\nexus-cw\nexus\nexus.exe operator list --data-dir C:\src\nexus-cw\data
# → one row, last_used_at set
```

### B4. Cutover-smoke against :18888

```powershell
Push-Location C:\src\nexus-cw\nexus
$env:NEXUS_URL = "wss://agentnetwork.<tailnet>.ts.net:18888"
$env:NEXUS_DATA_DIR = "C:\src\nexus-cw\data"
$env:NEXUS_INSECURE = "0"
go run ./scripts/cutover-smoke
Pop-Location
```

All 8 steps PASS, aspect.say to @keel returns msg_id, keel deliberates and replies in chat.

### B5. Operator chat round-trip

In the dashboard, type `@keel are you alive on the new substrate?`. Expect keel to reply within ~10s.

Verify the dashboard chat surface: day separators readable, reply badges appear, own messages visible, timestamps render correctly.

### B6. Verify migrated knowledge

```powershell
# Via dashboard Knowledge view, or:
Push-Location C:\src\nexus-cw\nexus
$env:NEXUS_URL = "wss://agentnetwork.<tailnet>.ts.net:18888"
$env:NEXUS_DATA_DIR = "C:\src\nexus-cw\data"
$env:NEXUS_INSECURE = "0"
.\nexus-wstest.exe -url wss://agentnetwork.<tailnet>.ts.net:18888 -data-dir C:\src\nexus-cw\data -insecure -surface operator -filter knowledge.list 2>&1
Pop-Location
```

Should return 46 entries.

### B7. Decision point

If B2–B6 all clean → proceed to Phase C.

If any fail → leave agent-network alive on :7888, stop nexus on :18888, debug, retry. No cutover commitment yet.

---

## Phase C — Port swap (the real cutover)

### C1. Announce on agent-network chat (5-min warning)

Operator posts to keel:
> "🚪 cutover starting. agent-network shutting down in 5 min. nexus comes up on :7888 next."

### C2. Stop nexus on :18888

```powershell
$pid = (Get-NetTCPConnection -LocalPort 18888 -State Listen -ErrorAction SilentlyContinue).OwningProcess
if ($pid) { Stop-Process -Id $pid -Force }
```

### C3. Stop agent-network

```
# however it's started — pm2 stop / Stop-Process / kill the node
```

Verify :7888 free:
```powershell
Get-NetTCPConnection -LocalPort 7888 -State Listen -ErrorAction SilentlyContinue
# (no output expected)
```

### C4. Start nexus on :7888 (same data dir)

Same env as B1 (origins-allow-list spans both ports already). Just change `-addr`:

```powershell
$env:NEXUS_TOKEN = "<production-token>"
$env:NEXUS_OPERATOR_RPID = "agentnetwork.<tailnet>.ts.net"
$env:NEXUS_OPERATOR_ORIGINS = "https://agentnetwork.<tailnet>.ts.net:18888,https://agentnetwork.<tailnet>.ts.net:7888"
$env:NEXUS_ALLOW_LEGACY_MASTER = "1"

Start-Process -FilePath "C:\src\nexus-cw\nexus\nexus.exe" `
  -ArgumentList @(
    "-data-dir", "C:\src\nexus-cw\data",
    "-addr", ":7888",
    "-aspect-dir", "C:\src\nexus-cw\nexus\agents",
    "--tls-cert", "C:\src\agent-network\certs\server.crt",
    "--tls-key",  "C:\src\agent-network\certs\server.key"
  ) `
  -WorkingDirectory "C:\src\nexus-cw\nexus" `
  -RedirectStandardOutput "C:\src\nexus-cw\data\stdout.log" `
  -RedirectStandardError  "C:\src\nexus-cw\data\stderr.log" `
  -WindowStyle Hidden
```

Verify same log lines as B2, except `broker listening addr=:7888`.

### C5. Login on :7888

Open `https://agentnetwork.<tailnet>.ts.net:7888/dashboard/`. **Same passkey** (the origins allow-list covers both ports).

Dashboard should mount; keel as only live aspect.

### C6. Final smoke

```powershell
Push-Location C:\src\nexus-cw\nexus
$env:NEXUS_URL = "wss://agentnetwork.<tailnet>.ts.net:7888"
$env:NEXUS_DATA_DIR = "C:\src\nexus-cw\data"
$env:NEXUS_INSECURE = "0"
go run ./scripts/cutover-smoke
Pop-Location
```

All 8 PASS. `@keel` ping returns a reply.

### C7. Announce completion

> "✅ cutover complete. nexus is live on :7888. aspects come online as you restart them."

---

## Phase D — Aspect restart (post-cutover, operator-driven)

Each non-Frame aspect needs:
1. Their keyfile at `C:\src\nexus-cw\data\keyfiles\<name>.keyfile.json` (already minted, all 8)
2. Their runtime (agentfunnel.exe / aspect.exe / whatever) restarted with the keyfile path env

Suggested order: forge, wren (most likely needed), then verity, harrow, maren, anvil, plumb as required.

After each: `nexus aspect list --data-dir C:\src\nexus-cw\data` shows them active and ping with `@<aspect>`.

---

## Rollback (if needed)

Triggered when keel-on-nexus is inaccessible or unreliable.

```powershell
# Stop nexus
$pid = (Get-NetTCPConnection -LocalPort 7888 -State Listen -ErrorAction SilentlyContinue).OwningProcess
if ($pid) { Stop-Process -Id $pid -Force }

# Confirm :7888 free
Get-NetTCPConnection -LocalPort 7888 -State Listen -ErrorAction SilentlyContinue

# Start agent-network back up — whatever its normal start command was
```

State preserved:
- nexus.db keeps everything that happened during the failed attempt (chat rows, knowledge, triage). Not lost; just only visible from nexus.
- agent-network's DB is untouched (we never wrote to it).

Cutover can be retried later from Phase B with the same data dir; no re-init needed.

---

## What's NOT in this runbook

- Aspect autospawn (operator manual start per Phase D)
- Mobile dashboard fixes
- Tickets view (placeholder per Crossing Part 4)
- Topics view (deferred — chat_messages.topic column not yet plumbed)
- Second-device passkey (CLI-only path; not blocking)
- Legacy-master token retirement (kept one more iteration for back-compat)
