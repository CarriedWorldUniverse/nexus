---
name: nexus-auth
description: 'Use when working with nexus/CWB authentication or access — operator passkey login to the dashboard, doing a broker admin op, cw config/cw kb mTLS, aspect keyfiles/tokens, the dormant herald IdP, the org-seed/custodian vault, or recovering dashboard/admin access. Read before changing any auth/credential/identity path.'
when_to_use: 'When working with nexus/CWB authentication or access — passkey login, broker admin ops, mTLS config paths, aspect keyfiles/tokens, or recovering dashboard/admin access.'
---

# nexus / CWB auth — who authenticates how

Single-owner personal cloud on dMon (k3s). Identity is **seed-rooted, sovereign** (no external IdP on the load-bearing path). `NEXUS_AUTH_BYPASS` is **retired** (2026-06-30) — the dashboard is passkey-gated.

**Two front doors, and they are not interchangeable** (verified live 2026-07-26):

| you are | use | why |
|---|---|---|
| the **operator**, in a browser | **`https://dashboard.nexus.carriedworld.dev/dashboard/`** | the ONLY origin WebAuthn accepts |
| an **agent / machine** | `wss://nexus.tail41686e.ts.net:7888/connect` | tailnet cert, no WebAuthn involved |

Browsing the tailnet `:7888` URL to log in **cannot work**: rpId `carriedworld.dev` is not a registrable suffix of `nexus.tail41686e.ts.net`, so the BROWSER refuses before sending anything. No server-side error is logged — it just looks like the UI is broken. That mismatch cost real debugging time on 2026-07-26.

## The surfaces (who → what → how)

| Caller | Target | Mechanism |
|---|---|---|
| **Operator (human)** | dashboard `https://dashboard.nexus.carriedworld.dev/dashboard/` (:443) | **WebAuthn passkey** (rpId `carriedworld.dev`, origins `[https://dashboard.nexus.carriedworld.dev]`). No bypass. |
| **shadow/croft** | broker admin ops | **in-pod `nexus` CLI, DB-direct, no network auth** (preferred) — see recipe. HTTP `/api/admin/*` needs an operator JWT; there is **no standing admin token** by design. |
| **shadow/croft** | almanac config / commonplace kb | **in-mesh mTLS** (`CW_APP_TLS_CERT/_KEY/_CA`) + `cwb-subject`/`cwb-org`/`cwb-scopes` metadata. `cw config` → almanac:8083, `cw kb` → commonplace:8101. **From croft** (a pod; reaches ClusterIP/cluster-DNS). NOT herald/edge. |
| **Aspects** (anvil, keel…) | broker | **keyfile validate handshake** → broker mints a session **JWT** (HS256, `SessionSigningSecret`, sub=aspect, ~24h, refreshable over WS). Or a per-aspect `NEXUS_TOKEN` bearer. |
| **CWB services** (almanac/commonplace/custodian/ledger/cairn…) | each other | **mTLS** + `cwb-subject`/`cwb-org`/`cwb-scopes` metadata (`mdident`); trust the metadata (mTLS-gated). |
| **claude-ornith / litellm** | Ornith | **keyless** (local vLLM; `api_key: dummy`). |

## Identity roots
- **One org-seed** roots everything (custodian vault, LIVE: HKDF → per-org DEK → casket seal). Aspect identities are **seed-derived** (`casket.DeriveAgentKey`, `cw agent enroll`).
- **Operator** = WebAuthn passkey for the dashboard; broker verifies operator JWTs it mints at login (HS256, `SessionSigningSecret`, sub="operator", admin+operator).
- **herald** (OIDC IdP): **DORMANT** — kept-but-off the operator-auth path. `cw login` / the interchange "edge" session is herald-bound → expired/unused. Don't rely on it; use the mTLS path.
- **Legacy master token** (`NEXUS_TOKEN` as a shared bearer): **OFF** (`NEXUS_ALLOW_LEGACY_MASTER` unset → 401). Deprecated; left dead.
- **Irreducible external manual auth**: codex + Claude provider-subscription logins. Claude session token is now an **8h TTL** ([[reference_claude_code_token_expiry]]) — claude-ornith needs none of it.

## Recipes

**Change platform config (the one supported path):** from croft, `cw config get|set|list <path>` (almanac, mTLS; `--org 07244ac5-1fbd-4786-9301-a925f4241306` for cloud-config). e.g. flip an aspect brain: `echo '{"provider":"openai","model":"ornith"}' | cw config set cwb/nexus/provider-bindings/<aspect>` → broker reconciles ≤30s (cfgreconcile). Full map: the CONFIG CHEAT-SHEET in `~/nexus-cloud-redesign-2026-06-27.md`.

**Broker admin op (no token needed):** run the `nexus` CLI **in-pod, DB-direct**:
```
ssh jacinta@100.91.185.71 "sudo k3s kubectl exec -n nexus deploy/nexus-control -c broker -- nexus <subcommand>"
```
e.g. `nexus operator list|delete <id>|reset-passkey`, aspect mint, etc. Bypass-independent; the canonical recovery + admin path now.

**Operator passkeys (manage devices):** `nexus operator list` · `delete <id>` · `reset-passkey` (delete ALL → reopens zero-passkey bootstrap registration). Register a new passkey only via the **dashboard** (browser WebAuthn ceremony) — bootstrap path is open only when zero passkeys exist; otherwise registration needs a valid operator JWT (and it does NOT honor bypass).

## Gotchas (hard-won)
- **Passkeys are bound to the rpId** (`NEXUS_OPERATOR_RPID`, currently **`carriedworld.dev`** — it is NOT set in the deployment env, so confirm from the broker's boot log: `msg="operator login enabled" rp_id=... origins=[...]`). A passkey registered under a PREVIOUS name/host is dead against the current one even though it shows in `operator_passkeys`/`allowCredentials`. Fix: `nexus operator delete` the stale row → re-register under the current name.
- **Certs differ per door.** `:7888` serves `CN=nexus.tail41686e.ts.net`; `:443` serves `CN=nexus.carriedworld.dev` with SAN `*.nexus.carriedworld.dev` (expires 2026-10-15). Asking for a carriedworld.dev name on `:7888` gets `tls: unknown certificate` from the client — a real symptom seen 2026-07-26, and a sign someone crossed the two doors.
- **Diagnose without a browser**: `curl -sS -X POST https://dashboard.nexus.carriedworld.dev/api/operator/login/begin -H 'Content-Type: application/json' -d '{}'` returns the challenge with `rpId` and `allowCredentials`. If your passkey's prefix appears there and the rpId matches the host you are browsing, the server side is fine and the remaining step is the authenticator itself.
- **The SPA hides the register flow when bypass is on** (`app.js` probes `/api/auth/mode`; `{bypass:true}` → skips Login, where the register button lives). So registration requires bypass OFF (or zero-passkey bootstrap reached another way).
- **Registration gating:** bootstrap (open) ONLY at zero passkeys; any subsequent register needs an operator JWT in the Authorization header; `registrationGated` ignores bypass.
- **Recovery levers (never get locked out):** the in-pod `nexus operator` CLI works regardless of auth; and `NEXUS_AUTH_BYPASS` can be re-enabled in ~1 min — `kubectl patch secret -n nexus nexus-broker-env` set `NEXUS_AUTH_BYPASS` → base64 `1` (`MQ==`) + `rollout restart deploy/nexus-control`. (`0` = `MA==` to disable; broker checks `== "1"`.)
- **Reachability:** croft (a pod) reaches in-cluster ClusterIPs/cluster-DNS (almanac/commonplace) fine; the **operator box does NOT** (not on the pod net). litellm/Ornith reached via robo-dog tailnet `100.92.111.3`.

## Related
[[project_nexus_sovereign_rebuild]] (the rebuild + INC-4 retire-bypass decision) · [[reference_claude_cli_on_ornith]] · [[reference_claude_code_token_expiry]] · `~/nexus-cloud-redesign-2026-06-27.md` (§2 identity, CONFIG CHEAT-SHEET, INC-4 closed).
