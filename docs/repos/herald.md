<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/herald/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`herald`](https://github.com/CarriedWorldUniverse/herald)'s live `README.md`.
    Edit the README in the repo, not this page.

# herald

**The CWU identity service.** herald attests *who you are* (humans **and** agents) and proclaims *what authority you hold*. Every other CWU service — nexus, cairn, ledger, porter, knowledge, comms — is a **consumer** of herald: they receive a herald-issued token and gate on it. herald is the one canonical identity authority for the stack.

Heraldry was the original identity system: arms encode who you are, your lineage, and which house you belong to — identity, accountability, and org membership in one mark. A herald both **attests** identity and **proclaims** authority between houses. That's the job.

## What makes herald different

Most identity products model **humans**, and bolt machines on as second-class "service accounts." herald is built for a world where **a human owns many autonomous agents**, each needing its own accountable identity:

- **Agents are first-class identities**, not service-accounts — own keypair, own audit trail, own scopes — **linked to** a responsible human, never **equal to** them.
- **Accountability and capability are orthogonal.** An agent's permissions are not a subset of its human's: a read-only human can own a write-everything builder agent. Accountability flows *up* to the human; capability is granted *independently* within the org ceiling.
- **Crypto-rooted.** Agents authenticate by proving possession of a [casket](https://github.com/CarriedWorldUniverse/casket) Ed25519 key — no shared secrets in the hot path.
- **Own it, don't rent it.** Built on the permissive [`zitadel/oidc`](https://github.com/zitadel/oidc) (Apache-2.0) library for the OAuth2/OIDC protocol; the differentiated identity *model* is ours. No US-SaaS holding our identity data.

## Status

**MVP built.** herald is a single Go binary (`cmd/herald`) serving the OIDC token/JWKS endpoints, human password-grant login, casket-keyed agent auth, rotating refresh tokens, and a gRPC AdminService for org/human/agent administration. Admin authority is **identity-derived** — derived from a herald JWT carrying `herald:platform-admin` / `herald:org-admin` scopes, not a static admin token. k3s deployment manifests live under [`deploy/k3s`](https://github.com/CarriedWorldUniverse/herald/blob/HEAD/deploy/k3s); see the [smoke-test runbook](https://github.com/CarriedWorldUniverse/herald/blob/HEAD/docs/dmon-smoketest.md).

Design spec: [`docs/2026-05-30-herald-mvp-spec.md`](https://github.com/CarriedWorldUniverse/herald/blob/HEAD/docs/2026-05-30-herald-mvp-spec.md). Tracked in NEX-376 (epic) + NEX-377–381.

## Stack position

`nexus` (broker) · `cairn` (forge) · `ledger` (tracker) · `casket` (crypto) · `porter` (storage) · `interchange` (gateway) · **`herald` (identity)**
