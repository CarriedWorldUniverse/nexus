# Almanac — the configuration & internal-secrets pillar

> **Status:** design (operator-directed 2026-06-12). A new CWB pillar — sibling to herald/ledger/commonplace/cairn/custodian on the shared crypto+identity substrate. Working name **almanac** (a reference compendium of settings, consulted constantly — operator to confirm).

## 1. The problem

Configuration is the platform's worst-distributed concern. Tonight's failures were *all* config-distribution failures: codex-auth not reaching pods (NEX-576), `OLLAMA_BASE_URL` not propagating to hands (NEX-610), `CW_SEAM_CA` for self-signed TLS, the napping manifests, keyfile distribution, the broker env secret, provider/model bindings living in a hand-written sqld column, wake-policy as a hand-edited env string. Config lives scattered across **env vars + kubectl secrets + values files + sqld columns + init-containers**, with no single plane that owns *"what does this consumer need to know, and which parts are secret."*

Almanac is that plane: the **configuration engine every Carried World consumer reads to be itself** — modeled on AWS SSM Parameter Store + Secrets Manager, with casket as the KMS-equivalent and herald as the IAM-equivalent.

## 2. Decisions locked (operator, 2026-06-12)

1. **Sibling pillar**, not a face of custodian. Shares the substrate (casket + herald + the seed-derivation tree) but is its own service — because the access patterns differ fundamentally:
   - **custodian** = *external* credentials (passwords/OAuth/API-keys for someone else's service), brokered per-use, human-unlock sessions. The platform acting *as a user elsewhere*.
   - **almanac** = the platform's *own* internal config + secrets that services read to *be themselves* — high-read, boot-time + live, no human in the loop.
   Merging them would muddy custodian's careful 2-min-session human-unlock model with high-frequency machine config reads. Clear lanes: custodian = external creds · satchel = human on-ramp · **almanac = internal config+secrets**.
2. **Live-reload**, not pull-at-boot only. A consumer subscribes to its config subtree and receives changes without restart — turning today's hand-edited env strings (wake policy, provider bindings) into a real, instant control surface.

## 3. The model (Parameter Store shape)

- **Hierarchical paths**: `/<org>/<service>/<env>/<key>` (e.g. `/cw/keel/prod/provider`, `/cw/_global/gemma_base_url`). The path *is* the namespace.
- **Two value tiers**:
  - **Parameter** — cleartext config (URLs, flags, model names, policy strings). Versioned, readable by scope.
  - **SecureParameter** — casket-sealed secret value (keyfiles, tokens, CA material). Same path model; the value is encrypted at rest under a seed-derived key (§4).
- **Layered resolution** — the killer feature. A consumer asks *"resolve my config"* and almanac merges a precedence stack, most-specific wins:
  ```
  /_global  ←  /<org>  ←  /<org>/<service>  ←  /<org>/<service>/<env>  ←  …/<instance>
  ```
  Each resolved key is tagged with the layer it came from (debuggability: *why is this value what it is?*). Define once at the right level; override only where needed.
- **Versioning + history** — every write is a new version; pin/rollback supported. Writes (and secure-reads) emit **ledger events** (the platform's audit substrate) — almanac's own store is config; its audit lives in ledger.

## 4. Crypto — casket + the herald-org seed (the "base seed")

The operator's "base seed applied to secret values" is exactly the custody model locked for custodian ([[custodian-key-model]]):

- **Org base seed**: derived from the org's **herald** base key at org creation (same single derivation tree — no second root). Almanac shares it with custodian via the substrate; it is not a new secret.
- **Per-secret key**: `KDF(org_seed, path)` — each SecureParameter sealed under a key derived from the seed **and its own path**, with casket's **path-bound AAD** (`(store-id, path)`). Consequence: a sealed value decrypts only at its own path in the right org — moving/replaying a secret to another path fails (the satchel entry-swap protection, for free).
- **Read path (v1)**: server-side decrypt over mTLS. Almanac holds/derives the org seed (under its own key-handling, like custodian's vault key), **herald authorizes the reader's scope**, and almanac returns plaintext over the mutually-authenticated channel. Consumers stay simple — they don't each carry the seed.
  - *Hardening option (noted, not v1):* envelope SecureParameters to the consumer's identity (satchel multi-recipient style) so almanac never holds plaintext. Heavier; revisit if the threat model demands the pillar never see secrets.

## 5. Access control — herald-scoped subtrees

- A reader (service/aspect identity, via herald) is granted **read/write scopes on path subtrees** — IAM-for-config. `keel` reads `/cw/keel/**` + `/cw/_global/**`; it cannot read `/cw/maren/**`.
- SecureParameter reads require a **secret-read scope** distinct from plain-config read — so a consumer can see its config without being trusted with adjacent secrets.
- Writes are scoped + audited (ledger event per write, with the writer identity).

## 6. Live-reload

- **`Watch(pathPrefix)`** — gRPC server-streaming RPC. The consumer subscribes to its subtree; almanac pushes **change events** (`{path, newVersion, op}`). To avoid streaming secrets, a SecureParameter change pushes a *version bump only*; the consumer re-`Get`s (which runs the authorized decrypt). Plain parameters can carry the value inline.
- Consumers keep a **local cache**; a change event invalidates the affected keys and re-resolves the layer stack. A dropped Watch reconnects and re-syncs from a version cursor (the chat-replay pattern).
- This makes `nexus aspect set provider` (the known missing CLI) trivially real: `cw config set /cw/keel/prod/provider ollama` → keel's funnel reloads its provider binding live. Wake policy, base URLs, idle timeouts — all become live knobs.

## 7. Faces (like the other pillars)

1. **gRPC** (`AlmanacService`) — the native interface: `Get`, `Resolve`, `Set`, `Delete`, `History`, `Watch`. Over mTLS behind interchange.
2. **`cw config`** — the CLI: `cw config get|set|resolve|ls|history|watch <path>`. Agent-first; the CLI is the substrate every other client wraps.
3. **MCP** — aspects read/manage their own config tree from within a turn (scoped to their subtree).
4. **Sidecar / init-container materializer** — for pods that can't speak gRPC live: an init-container resolves the config tree → env file / mounted files at boot (replaces the hand-written kubectl-secret/env pattern), and an optional sidecar holds the Watch and rewrites the file + signals the process on change. This is how the broker, gemma-ollama, and non-aspect pods consume it.

## 8. What it absorbs on *our* platform (the migration payoff)

Almanac directly retires the config sprawl that bit us tonight:
- **provider/model bindings** (sqld column → `/cw/<aspect>/provider`,`/model`) — live-reloadable, fixes the missing `nexus aspect set` CLI.
- **wake policy + idle timeout** (broker env string → `/cw/_broker/wake_policy`) — live, no broker restart.
- **`OLLAMA_BASE_URL` / `CW_SEAM_URL` / `CW_SEAM_CA`** (per-pod env/secret → `/cw/_global/*` + per-service) — and *propagated to hands*, killing NEX-610's class.
- **codex-auth, keyfiles** (kubectl secrets → SecureParameters, materialized by the sidecar) — killing NEX-576's class.
Migrating these is the proof case; live-reload makes them better-than-before, not just relocated.

## 9. Bootstrapping (chicken-and-egg)

Almanac needs minimal config to start (its own endpoint, the org-seed access). That minimum comes from **herald genesis / the deploy secret** (the same place the org owner + signing secret come from); everything else self-hosts in almanac. A consumer needs only: the almanac endpoint + its own identity (keyfile/derived) — both of which it already has to reach any pillar.

## 10. Non-goals (v1)

- Rotation automation (custodian's concern for external creds; internal secrets rotate by `Set` + the consumer reloading — a v2 scheduler if needed).
- The pillar-never-sees-plaintext envelope mode (§4 hardening option).
- Cross-org config sharing (isolation stays hard; `_global` is per-org-global, not cross-org).
- A config web UI (the human web layer is post-MVP; `cw config` + MCP cover it).

## 11. Open questions

- **Name** — `almanac`? (alternatives: register, rubric, lexicon). Operator to confirm.
- **Repo** — new `almanac` repo (sibling), or incubate in nexus until it stands alone? (Lean: new repo, mirrors the other pillars.)
- **Server-side decrypt vs envelope-to-consumer** (§4) — v1 server-side; confirm the threat model accepts almanac holding plaintext transiently.
- **Resolution determinism with Watch** — when a higher layer changes, every consumer whose resolved view depends on it must re-resolve; confirm the change-event fan-out keys on *resolved dependency*, not just exact path.
- **Relationship to herald's own bootstrap config** — herald can't read its config from a pillar that needs herald; herald stays minimally self-configured (deploy secret), joins almanac for *non-identity* settings only.

## 12. Build plan

- **M1 — store + crypto + gRPC:** path tree, Parameter/SecureParameter, herald-org-seed derivation + casket path-bound seal, scope checks, `Get`/`Set`/`Resolve`/`History`, ledger audit events. Backend: sqld (or the partitioned store, NEX-616). *Usable as a config store from M1.*
- **M2 — live-reload:** `Watch`, consumer cache lib in cw, reconnect/cursor.
- **M3 — faces:** `cw config` CLI, MCP, the init-container/sidecar materializer.
- **M4 — migration:** move provider bindings, wake policy, base URLs, codex-auth, keyfiles into almanac; retire the scattered env/secret/sqld-column sources. The migration *is* the validation.

## 13. Relationships

- **herald** — identity for scope + the org base key the seed derives from.
- **casket** — seals SecureParameters (path-bound AAD).
- **custodian** — sibling; external creds vs almanac's internal config+secrets. Shared substrate, distinct lanes. (custodian *itself* reads its config from almanac.)
- **ledger** — almanac's write/secret-read audit log lives here.
- **cw** — `cw config` lives in the cw suite.
- **NEX-616** — almanac's store is a candidate for the partitioned-store taxonomy (config = low-write, its own store).

---
*Banked from the 2026-06-12 design discussion. Supersede with a proper plan when almanac enters its spec → plan → build cycle.*
