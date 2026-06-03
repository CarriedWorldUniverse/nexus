# Herald-rooted agent bootstrap — design

**Date:** 2026-06-03
**Status:** canonical design (approved in operator/shadow brainstorming)
**Scope:** how an aspect (agent) starts up, authenticates, and provisions itself off a single herald-rooted identity. Spans nexus (the connection holder + custodian), herald (the identity root), and the cw/`cwb-client` suite (the proven client library). Foundational — the data-plane migration (issues/knowledge/git → CWB) rides on this.

## Principle

**One herald authentication is the keystone that unlocks an aspect's entire runtime** — session, config, keys, and per-pillar access. This is the aspect-runtime realization of the cw-suite founding principle: *"herald is the one auth point that enables the rest."* The cw CLI suite (#0–#9) proved every primitive here bottom-up against the live platform; this design wires those primitives into the agent-startup path.

## Trust model (the root of trust)

- **The root secret is the human's casket owner-seed** — one master secret per human, held by the human, never shipped to an agent box.
- **An agent's identity is a deterministic, named derivation off it:** `key = casket.DeriveAgentKey(owner_seed, slug)`, where the **slug is the agent name** (`plumb` → `DeriveAgentKey(owner_seed, "plumb")`). Same (seed, name) → same key, anywhere.
- **An agent ships with nothing but its keyfile** (its *own* derived key — a single, revocable, per-agent credential), never the owner-seed.
- **The owner-seed is used exactly once, at enrollment, on the human's machine** (via `cw`). After that the agent self-authenticates from its keyfile; the owner-seed never travels and never sits on an agent box or a server.

This resolves the owner-seed custody question: **human-carried, one-time, at enroll.** The keyfile is what decouples runtime auth from the owner-seed.

## The two planes (context)

- **Comms plane** — `frames` over WebSocket to the nexus broker; **outposts** are per-host relays that fan N aspect WS connections into one upstream WS to nexus (`nexus/nexus/outpost`; stateless, "nexus owns all persistent state"; per-aspect register frames preserve identity). The bootstrap auth travels on this plane.
- **Data/pillar plane** — herald/cairn/ledger/commonplace, HTTP through interchange. **nexus holds the connection to CWB**; aspects do not call CWB directly. nexus is the per-aspect token custodian.

The agent's only network dependency is its **local relay** (its outpost, or nexus directly). It never talks to the CWB edge.

---

## Enrollment — `cw agent enroll` (human-run, once per agent)

The human (who holds the owner-seed) runs this once to bring an agent into existence and write its bootstrap keyfile.

```
CW_OWNER_SEED=<human owner seed> cw agent enroll --slug plumb \
    --url <local outpost/nexus ws> --out plumb.keyfile.json \
    [--org O --responsible-human H --scope repo:read ...]   # required only on a new mint
```

**Logic — derive → check-exists → attach | confirm+mint → write keyfile:**

1. **Derive** `key, pub = DeriveAgentKey(owner_seed, slug)`; `fp = Fingerprint(pub)` (`identity.Fingerprint` = `base64url(sha256(pub)[:16])`, #9).
2. **Existence check (content-addressed by fingerprint):** "does herald have an agent with this fingerprint?"
   - **exists → attach:** verify the registered record matches the derived fingerprint, capture its `key_id` (herald agent-id), write the keyfile. Frictionless — you're attaching a box to a known agent.
   - **not found → new mint:** this is the dangerous path (a misspelled `plubm` derives a *different* fingerprint → no match → lands here). So **require explicit confirmation** (interactive `y/N` or a `--create`/`--yes` flag) — the existence check *is* the misspelling guard — **and** require the create params (`--org`/`--responsible-human`/`--scope`). Then `cw agent create` (derive→register→link to human→scopes), capture the returned `key_id`, write the keyfile.
3. **Write the keyfile** (`0600`) to the agent's folder.

**Existence-check mechanism — two options (OPEN, pick per phase):**
- **(now) cw-only local registry** — on `create`, herald returns the agent UUID; cw records `slug → key_id` in a local registry on the operator's machine (and the `key_id` in the keyfile). "Exists?" = "in my registry?" Unblocks the single-machine fleet bootstrap with zero herald change; the confirm-on-mint still guards typos. Weakness: not shared across operators/machines, not herald-authoritative.
- **(when needed) herald `GetAgentByFingerprint`, exposed authed** — the RPC already exists internally (cairn's SSH path uses it, mTLS/gRPC-only). Exposing an org-/owner-scoped authed HTTP binding (a small cross-repo cycle, the #7a shape) makes the check authoritative and content-addressed: cw computes the deterministic fingerprint locally and asks herald. Reuses #9's `Fingerprint` end-to-end; also serves multi-operator enrollment.

## The bootstrap keyfile

Self-describing: *who I am, how I prove it, which relay to reach* — exactly enough to do step-1 herald auth with no other config.

```json
{
  "key":         "<base64 ed25519 private key>",
  "key_id":      "<herald agent-id (UUID)>",
  "url":         "<local outpost or nexus WS endpoint>",
  "slug":        "plumb",
  "fingerprint": "<base64url sha256(pub)[:16]>"
}
```

- **`key`** — the **raw** derived private key. Raw (not a casket keyid-reference) *because the bootstrap key must self-start the herald auth before nexus is reachable* — it can't depend on nexus holding it (chicken-and-egg). The *downstream* keys plumb pulls from nexus post-auth are casket keyid-referenced; the bootstrap key is the one local, raw credential.
- **`key_id`** — the **herald agent-id**. The assertion's `iss`/`sub`; ties the keyfile to its herald registration.
- **`url`** — the **local relay** (outpost or nexus WS), *not* the CWB edge. The keyfile is therefore **environment-relative**: re-home an agent to a different outpost by reissuing the keyfile with a different `url`.
- **`slug`, `fingerprint`** — reference fields; the fingerprint lets you eyeball-verify the keyfile against `cw agent pubkey --slug plumb`.
- **Not in the keyfile:** the owner-seed (never), the CWB edge (the agent doesn't talk to it), the herald **audience** (supplied at handshake — see below). `0600`.

This mirrors cw's context + token-store split, collapsed into one portable bootstrap file.

## Boot / runtime auth (every start)

1. Read the keyfile.
2. Open the WS to `url` (the local outpost/nexus).
3. **Register handshake:** nexus/outpost supplies the **herald audience** as a challenge (*"sign for herald-issuer X"*); the agent signs the RFC-7523 casket assertion with `key` (`iss`/`sub` = `key_id`, `aud` = the challenged herald issuer) and sends it in the register frame. *(This is exactly `identity.AgentAssertion`.)*
4. The outpost relays the per-aspect register frame to nexus (transparent).
5. **nexus redeems the assertion at herald** (`jwt-bearer` grant — exactly `oidc.JWTBearerGrant`), receives plumb's herald token, **holds it (token custodian)**, and binds the WS session to plumb's herald identity.
6. nexus serves plumb its **config + downstream keys** (keyid-referenced) and acts as plumb at the pillars (or hands the token down if plumb makes its own pillar calls).
7. plumb starts work.

The agent only **signs**; nexus **redeems** and **custodies**. plumb may never hold a herald token directly — it has an authenticated session with nexus, and nexus *is* plumb at CWB (preserving identity-derived authz/ownership/attribution per aspect).

**Audience binding (replay safety):** the connect target (`url` = outpost) is deliberately *not* the assertion audience (herald). An RFC-7523 assertion must be bound to its redeemer so it can't be replayed elsewhere — so the herald audience is supplied by nexus **at handshake time** (a challenge), keeping the keyfile purely about the local connection + identity and letting the trusted relay pin the herald binding. (Alternative: bake the herald issuer into the keyfile as a field — simpler, but bakes the edge into every box and weakens re-homing. Recommended: handshake-supplied.)

---

## How we're located (why this is mostly wiring, not invention)

Every auth-and-access primitive already exists as a tested, live-smoked cw component:

| Bootstrap step | Existing primitive (cw, live-proven) |
|---|---|
| derive agent key by name | `casket.DeriveAgentKey(owner_seed, slug)` (`cw agent create/pubkey --slug`) |
| fingerprint (existence + verify) | `identity.Fingerprint` (#9; matched herald's stored value live) |
| sign the assertion (step 3) | `identity.AgentAssertion(key, slug, key_id, aud)` |
| redeem at herald (step 5) | `oidc.JWTBearerGrant(assertion)` |
| server-authoritative identity | `/api/me` (#7a) / `cw whoami --remote` (#7b) |
| pillar access (config/keys/work) | `cwb-client` (the cw `internal/{herald,cairn,ledger,commonplace}` wrappers) |

**Greenfield (the actual build):** nexus has zero herald/CWB code (go.mod imports only `casket-go`; its data integrations are `nexus-issue-mcp`/`nexus-github-mcp` → Jira/GitHub — the things CWB replaces). The seam = the WS-handshake herald-auth + the custodian + post-auth distribution.

## Build sequence

0. **Prereq — extract a reusable `cwb-client` library.** Lift cw's `internal/{herald,ledger,cairn,commonplace,client,identity,oidc}` out of `internal/` (a different Go module cannot import them) into a shared public package/module. Turns the cw suite into the shared CWB client nexus + outposts + aspects all consume. *The CLI was the proving ground; the library is the deliverable the bootstrap stands on.*
1. **`cw agent enroll`** — derive → check-exists (local registry to start) → attach | confirm+mint → write keyfile. cw-side; reuses `create` + `pubkey` + a keyfile writer.
2. **nexus herald client + token custodian** — consume `cwb-client`; redeem WS-delivered assertions at herald; hold per-aspect tokens; mediate pillar calls as the aspect.
3. **herald-auth in the nexus WS register handshake** — audience-via-challenge; redeem; bind the session to the herald identity. (Through outposts transparently.)
4. **post-auth config/key distribution** — nexus serves the herald-authed aspect its config + downstream keyid-referenced keys.
5. **(when needed) expose herald `GetAgentByFingerprint` (authed)** — authoritative, content-addressed existence check; multi-operator enrollment.
6. **data-plane cutover** — issues→ledger, knowledge→commonplace, git→cairn on herald-authed sessions; provision the real `nexus` org + per-aspect identities (today there is **no real tenant** — every herald org is a `cwb-test-*`/admin fixture); **canary one aspect** (shadow — controller-shaped, easiest to re-point) before big-bang.

## Open decisions (flagged)

- **Existence-check mechanism** — local registry (cw-only, now) vs herald fingerprint-lookup (cross-repo, authoritative). Start local; upgrade when herald-as-source-of-truth / multi-machine matters.
- **Assertion audience** — handshake-challenge (recommended; keeps keyfile env-relative) vs keyfile field.
- **Rotation** — casket derive is deterministic per slug (same seed+slug → same key), so you cannot rotate while keeping the slug. Convention TBD: a key-epoch in the slug (`plumb/v2`) or re-enroll → new registration. (Revocation is separate and available: herald can block a registered agent.)
- **Token locality** — confirmed lean: nexus custodies the per-aspect token and mediates; the agent holds only the WS session (doesn't call CWB directly). Revisit only if an aspect needs direct pillar access.

## Security properties (summary)

- Owner-seed (the master, derives *every* agent) is **human-carried, used once at enroll, never on an agent box or a server**.
- What lands on an agent box is **one per-agent credential** — revocable at herald, rotatable (slug-epoch), `0600`.
- Assertions are **audience-bound** (no cross-endpoint replay).
- Identity is **content-addressed** (fingerprint) — derivation is provably identical on both sides (verified live in #9).
- An agent's **only network dependency is its local relay**; the CWB edge is never exposed to it.

## References

cw CLI suite (`cw/docs/superpowers/`, #0–#9); `nexus/nexus/outpost`; herald `internal/identity/fingerprint.go` + `internal/grpcadmin` (AgentByFingerprint); casket at-rest envelope / keyid-reference; the CWB MVP definition (agent loop = auth+git+issues+knowledge).
