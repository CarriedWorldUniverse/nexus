# M1 — Custodian-Brokered Auth Foundation — Implementation Spec

**Status:** Approved design (2026-06-05) · **Story:** NEX-435 · **Epic:** NEX-434 (k3s on-demand work dispatch)
**Repos:** `CarriedWorldUniverse/nexus` (seam + git Kind + provider wiring) · `CarriedWorldUniverse/cw` (credential helper + issue-permission)
**Related:** `docs/2026-06-04-credential-custodian-design.md` (full Custodian service, future), NEX-376 (herald/identity)

## Goal

A k3s worker (and, by extension, any nexus agent) can **`git push`** and **call model providers** without the agent ever holding a raw secret. Credentials are fetched, scoped, and audited through a **custodian seam** — an endpoint contract served by the nexus broker today and by the CWB custodian service later, with consumers unchanged.

This is the load-bearing first milestone of NEX-434: it is what lets an on-demand worker ship code (the structural fix for NEX-433-class blockers, where an aspect could analyse a fix but had no way to push).

## Decisions (from brainstorming, 2026-06-05)

1. **Custodian-first** — no raw secret reaches the agent.
2. **Seam now, migrate backend later** — define the custodian credential *contract* and route all agents through it; back it initially with the existing broker credential store; migrate the backend into the standalone CWB custodian service as a tracked follow-on. No throwaway.
3. **Provider keys go through the same seam.**
4. **The helper lives in `cw`** (the CWB CLI), reusing its existing herald agent-auth.

## What already exists (reuse — do not reinvent)

- **Broker credential store + `credential.fetch` WS frame** (`nexus/broker/aspect_credentials.go`, `nexus/frames/payloads.go`, `nexus/credentials/credentials.go`). Already brokers `jira`/`imap`/`provider`: scoped per-aspect via `AllowedAspects`, mode-gated (`Fetch`/`Proxy`/`Both`), decrypts a bundle, and writes an audit row via `RecordAudit(AuditFetch)`. Request `CredentialFetchPayload{Kind, Name}` → response `CredentialFetchResultPayload{Name, Kind, Bundle map[string]any, ExpiresAt}`.
- **Aspect-side fetch helpers** (`runtime/brokercreds/brokercreds.go`): `Fetch`, `FetchProvider`, `FetchJira`, `FetchIMAP`.
- **`cw` CLI** (`CarriedWorldUniverse/cw`): cobra root with `auth`/`repo`/`pr`/`issue`/`kb`/`org`/`human`/`agent` groups; global flags `--context/--edge/--token/--identity/--json` (env `CW_*`); agent login via `cwb-client` herald jwt-bearer; a token store.
- **GitHub credential today** (`runtime/keyfile/keyfile.go` `GitHubConfig{Username,Email,PAT,DefaultOrg}`, consumed by `nexus-github-mcp` for REST). Aspects do **not** `git push` today — they use the github MCP's REST API. M1 introduces the push path.

## Components

### 1. `git` credential Kind (nexus) 🆕

Add a `git` Kind to the credential store (`nexus/credentials/credentials.go`). Its bundle holds the push credential:

```
bundle = { "username": "<git user>", "password": "<PAT or token>", "host": "github.com" }
```

- Scoped via the existing `AllowedAspects` (the worker identity that may fetch it). Mode `Fetch`.
- Registered/encrypted exactly like the existing Kinds; no new storage mechanism.
- The PAT moves out of the keyfile plaintext `github` block into the store for the push path (the github-MCP REST path can migrate later; out of scope for M1).

### 2. Custodian seam endpoint (nexus) 🆕

The seam is the *contract* `cw` calls. M1 serves it from the broker over the existing `credential.fetch` mechanism, extended so an **agent-authenticated HTTP/edge caller** (not only an aspect WS connection) can fetch:

- Auth: the caller presents its herald identity (token/keyfile); the broker maps it to the credential store's `AllowedAspects`.
- Request: `{ kind: "git" | "provider" | …, name?: string }`.
- Response: `{ name, kind, bundle, expires_at? }` — identical to `CredentialFetchResultPayload`.
- Audit: `RecordAudit(AuditFetch)` on every issuance (including denials).

The contract is intentionally backend-agnostic: today nexus serves it over the existing store; later the CWB custodian service serves the same contract and `cw` retargets via `--edge`. **Consumers do not change.**

### 3. `cw` git-credential-helper 🆕

A new cw subcommand implementing git's credential-helper protocol (`get`/`store`/`erase`, `key=value` stdin/stdout). Registered in the worker's git config as `credential.helper`.

- On `get`: read `host`/`protocol` from stdin → authenticate as the worker's herald identity (existing cw auth) → call the seam (`kind=git`) → write `username`/`password` to stdout for git → exit. The token exists only in this process for that one operation; never in env, shell history, or repo config.
- `store`/`erase`: no-ops (the seam is the source of truth; nothing is cached locally).
- Lives under `internal/cli/<group>/` following cw's command pattern; reuses `auth.GlobalFlags` + the cwb-client herald path.

### 4. `cw issue-git-permission` 🆕

An admin/orchestrator-side cw command to register a worker's least-privilege git credential + scope into the store (the `git` Kind bundle, `AllowedAspects` = the worker identity). This is how a dispatch grants a worker exactly the push right its brief needs.

### 5. Provider-key wiring (nexus) ✅🆕

`FetchProvider` already exists. M1 routes the provider CLIs (claude-code/codex) and the direct-API path to fetch the provider key from the seam **at provider-spawn** and set it on the child process just-in-time, instead of mounting it as a long-lived env/keyfile value. The model never reads its own process env, so spawn-time injection carries no exposure — and the key flows through the audited seam.

## Data flow

**Worker `git push`:** git invokes `cw` (credential helper) → cw authenticates as the worker's herald identity → calls the seam (`kind=git`) → broker checks `AllowedAspects`, decrypts the bundle, `RecordAudit` → returns `username`/`password` to git → push succeeds. Token never touches the agent/model.

**Provider call:** at provider-spawn the funnel/bridle fetches `kind=provider` from the seam → sets the key on the child env for that process → model runs. Audited; not mounted.

## Identity & security model

- **Who** = herald identity (the worker's keyfile/assertion), already how cw authenticates.
- **What it may use now** = the seam's scoped, audited issuance (`AllowedAspects` + Kind + Mode).
- These stay separate (identity ≠ credential scope), matching the CWB herald/custodian split.
- Audit: `RecordAudit` rows in `credentials.credential_audit` for M1; Ledger `credential_events` is the follow-on.

## Repos & file targets

- **nexus**: `nexus/credentials/credentials.go` (git Kind), the broker seam endpoint (extend `aspect_credentials.go` / a new agent-auth credential handler + `frames/payloads.go`), provider-spawn wiring in the funnel/bridle provider path, register the git PAT into the store (admin/`nexus credential` path).
- **cw**: new `internal/cli/<group>` for `git-credential-helper` + `issue-git-permission`, wired into `cmd/cw/main.go` root.

## Testing

- **Unit (nexus):** git Kind scope/mode enforcement; seam returns the bundle for an allowed identity and denies (+ audits) others.
- **Unit (cw):** credential-helper protocol round-trip against a stub seam — `get` returns `username`/`password`, no token leaks to logs; `store`/`erase` are no-ops.
- **Live round-trip (gated):** a process with no secret in its env performs a real `git push` via cw+seam and obtains a provider key from the seam, with the audit row asserted. NEX-433 is the natural first real subject once an M2 worker exists.

## Non-goals (explicit)

M1 does **not** build the standalone CWB custodian service, move the store's data into CWB, add Ledger/approval workflows or delegated short-lived handles, or migrate the github-MCP REST path off the keyfile PAT. Those are tracked follow-ons — chiefly the **backend migration** (store → CWB custodian service), which the seam is explicitly designed to absorb without consumer changes.

## Open questions

- **Seam transport** — extend the WS `credential.fetch` to accept an agent-auth HTTP/edge caller, or add a dedicated broker HTTP credential endpoint for `cw`? (Lean: a small agent-authenticated HTTP endpoint on the broker that reuses the store + `RecordAudit`; cleanest for cw's `--edge`/token model. Resolve at plan time.)
- **PAT scope granularity** — per-identity (`AllowedAspects`) is M1's scoping; fine-grained per-repo push limits depend on GitHub fine-grained PATs and are a refinement, not a blocker.
- **Worker identity provisioning** — how a per-dispatch worker obtains its herald identity to authenticate cw (pool vs ephemeral) is resolved in M2 (worker runtime); M1 assumes the worker can authenticate as itself.
