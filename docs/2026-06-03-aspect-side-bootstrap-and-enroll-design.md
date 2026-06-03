# Aspect-side bootstrap + `cw agent enroll` — design

**Date:** 2026-06-03
**Status:** design (operator/shadow brainstorming)
**Scope:** the **agent-runtime half** of the herald-rooted bootstrap (`docs/2026-06-03-herald-rooted-agent-bootstrap-design.md`). A real aspect reads a bootstrap keyfile, signs a casket assertion, and presents it in its `register` frame — so the broker (step 3a) redeems it end-to-end. Plus `cw agent enroll`, the human CLI that *writes* that keyfile (derive → create-or-attach at herald → write). Steps 0/2/3a are done; this is the producer (`enroll`) + the consumer (aspect runtime) that make 3a fire from a real agent instead of a test.

## Goal

After step 3a the broker can redeem an assertion carried in a `register` frame, but only tests produce one. This cycle makes it real on both ends:

- **`cw agent enroll`** (human, once per agent) writes a bootstrap keyfile `{ key, key_id, url, slug, fingerprint }`.
- **the aspect runtime** (every start) reads that keyfile, signs a fresh assertion, and attaches it to its `register` frame.

One human enrollment, then the named agent boots and herald-binds on its own — the aspect-runtime realization of "the agent works off its name once the human has authenticated."

## The edge abstraction (load-bearing)

**Agents only ever talk to nexus.** The production topology is firewalled: nexus holds the *single* allowed egress to CWB, so neither aspects nor `enroll` can reach herald directly — nexus is the boundary (a reverse proxy, the "interchange gateway"). The aspect and `enroll` are therefore written **edge-agnostic**: they take a configurable CWB edge URL and never name herald. Today that edge resolves to herald directly on the tailnet (reachable, used for the live test); once the gateway lands it resolves to the nexus gateway url — **same binaries, config only, no code change.** "Agents don't need to know it's a proxy — they talk to nexus" is exactly this seam.

**Explicitly out of scope (next cycle):** the nexus CWB gateway itself — the reverse proxy + OIDC discovery/endpoint rewrite + the dMon herald issuer reconfig. It is its own subsystem and gets its own brainstorm. This cycle delivers the runtime + `enroll` against the edge abstraction; flipping the edge to the gateway is a later deployment step.

## The bootstrap keyfile

A plaintext JSON file, distinct from the existing sealed aspect keyfile (`nexus/aspects/keyfile.go`) and from the transport bearer. It carries only what the runtime needs to sign + connect:

```json
{
  "key":         "<base64 raw ed25519 private key (64-byte expanded form)>",
  "key_id":      "<herald agent UUID>",
  "url":         "<nexus relay base url the aspect connects + discovers through>",
  "slug":        "<agent name, e.g. plumb>",
  "fingerprint": "<base64url sha256(pub)[:16] — herald's Fingerprint(pub)>"
}
```

- **`key`** is the *derived per-agent* private key (`casket.DeriveAgentKey(owner_seed, slug)`), **not** the owner seed. The owner seed derives *every* one of the human's agents, so it must never sit in an aspect keyfile. The runtime signs directly from `key`; it never needs the seed.
- **`url`** is the nexus relay — the only host the aspect ever talks to. The aspect derives both the WS endpoint (connect) and the OIDC discovery base (to learn the assertion audience) from it. It never names herald.
- **`slug`** / **`fingerprint`** are carried for display, logging, and self-check (the runtime can recompute the fingerprint from `key`'s public half and assert it matches — a cheap corruption guard).

## Mechanism

### Piece 1 — `cwb-client/identity.AgentAssertionFromKey`

`AgentAssertion` takes `(seed, slug)` and *derives* the key. The keyfile stores the derived key, so the runtime needs to sign from a raw key. Add a sibling that takes the private key directly; refactor `AgentAssertion` to derive-then-delegate so the signing path is shared and single-sourced.

```go
// AgentAssertionFromKey signs an RFC 7523 jwt-bearer assertion
// (iss=sub=agentID, aud=tokenURL, 2-minute exp) from an already-derived key.
func AgentAssertionFromKey(priv ed25519.PrivateKey, agentID, tokenURL string) (string, error)

// AgentAssertionFromKeyAt is AgentAssertionFromKey with an explicit clock.
func AgentAssertionFromKeyAt(priv ed25519.PrivateKey, agentID, tokenURL string, now time.Time) (string, error)
```

`AgentAssertionAt(seed, slug, …)` becomes: `priv, _, err := casket.DeriveAgentKey(seed, slug)` then `AgentAssertionFromKeyAt(priv, agentID, tokenURL, now)`. The existing assertion JWT is byte-identical (same claims, same EdDSA signer). Validation: `priv` non-nil/correct length, `agentID`/`tokenURL` non-empty. Merge cwb-client → pin into nexus + cw (`go get @merged-hash`).

### Piece 2 — the runtime keyfile loader (`runtime/heraldkeyfile`)

A small package: a `Keyfile` struct for the five fields, `Load(path) (*Keyfile, error)`, and a `PrivateKey() (ed25519.PrivateKey, error)` that base64-decodes `key`. `Load` validates presence of all fields and that `key` decodes to a valid ed25519 private key; optionally recomputes `Fingerprint(priv.Public())` and errors if it disagrees with the stored `fingerprint` (corruption guard). No herald or network dependency — pure file + decode.

### Piece 3 — wire it into the aspect runtime (`runtime/cmd/agent` + `runtime/agent`)

- **Config:** `agent.Config` gains a `HeraldKeyfile *heraldkeyfile.Keyfile` (nil when not enrolled). `runtime/cmd/agent/main.go` reads a path from `NEXUS_HERALD_KEYFILE` (env, optional); if set, `heraldkeyfile.Load` it and put it on the config. Absent → no assertion, existing behavior unchanged (additive + dark, mirroring 3a's opt-in).
- **Discover the audience:** at register time, if `HeraldKeyfile != nil`, the runtime computes the assertion `aud` by OIDC discovery against the keyfile `url`: `oidc.New(httpBase(url)).TokenEndpoint(ctx)`. `httpBase` maps a `ws(s)://` relay url to its `http(s)://` origin. The aspect makes exactly one HTTP call here — to **nexus** (the relay), which is the only egress it has; nexus serves/forwards discovery (gateway, next cycle) so the returned token endpoint is the one herald validates against. For the tailnet live test the edge resolves to herald directly, so discovery returns herald's real token endpoint.
- **Sign + attach:** `runtime/agent/agent.go` `sendRegister` — if `HeraldKeyfile != nil`, `priv, _ := kf.PrivateKey()`, `assertion, err := identity.AgentAssertionFromKey(priv, kf.KeyID, tokenURL)`, set `RegisterPayload.Assertion = assertion`. On any signing/discovery error: log + register **without** the assertion (the connection still comes up on the existing transport bearer; herald-binding is best-effort this cycle, not a hard gate — 3a only hard-fails when an assertion *is presented* and redemption fails, which never happens if we omit it). A fresh assertion is signed on every (re)connect — the 2-minute expiry needs no refresh loop.
- **Transport auth untouched:** the existing bearer for the WS upgrade is unchanged; herald/transport convergence stays a later step (per 3a).

### Piece 4 — `cw agent enroll` (attach-only)

A new subcommand under `cw agent` (sibling of `create`/`pubkey`/`keygen`). **Attach-only this cycle:** it writes the bootstrap keyfile for an agent that *already exists* at herald; it does not mint. Inputs: `--slug` (required), `--url` (required, the nexus relay for the keyfile), `--out` (keyfile path, default `./<slug>.keyfile.json`), `CW_OWNER_SEED` (env, the derivation root). Edge via the existing `--edge`/`CW_EDGE` global (the gateway url in production; herald directly for the test).

Flow:
1. **Derive:** `priv, pub, _ := casket.DeriveAgentKey([]byte(seed), slug)`; `fp := identity.Fingerprint(pub)`.
2. **Attach (resolve the UUID):** look up the agent by fingerprint at the edge — `herald.GetAgentByFingerprint(ctx, c, fp)`.
   - **Found** → take its `id` as `key_id`.
   - **Not found (404)** → abort with a message naming the slug + fingerprint and pointing the human to provision the agent first. Because the key is deterministic, a *misspelled* slug yields a valid-but-unregistered fingerprint, so a not-found is also the typo signal — surfacing it (rather than minting) is the guard this cycle.
3. **Write keyfile:** marshal `{ key: base64(priv), key_id, url, slug, fingerprint }` to `--out` with `0600` perms. Print the path + a one-line "start the aspect with `NEXUS_HERALD_KEYFILE=<path>`" hint to stderr.

**Herald-side dependency — already satisfied.** herald already serves `GET /api/agents/by-fingerprint/{fp}` (NEX-412; returns `{id, kind, display_name, org, responsible_human, fingerprint, status, active, scopes}`, 404 when no agent matches). **No herald change** — the cycle only adds a `herald.GetAgentByFingerprint` wrapper in cwb-client. (Reachability note: that route is not a gateway public-path, so an external caller goes through the gateway's bearer-auth — `enroll` carries the human token via the existing `cmdutil.Session`. On the tailnet test the edge is herald directly; confirm the route is reachable in that deployment at test time.)

## Data flow (end to end)

```
human, once (agent already provisioned at herald):
  cw agent enroll --slug plumb --url <nexus>
    derive(owner_seed, "plumb") -> priv,pub,fp
    GET by-fingerprint @edge -> found? key_id=id : abort (provision first / typo guard)
    write plumb.keyfile.json { key, key_id, url=<nexus>, slug, fingerprint }

aspect, every start:
  NEXUS_HERALD_KEYFILE=plumb.keyfile.json  ->  heraldkeyfile.Load
  connect WS to url (transport bearer, unchanged)
  tokenURL = oidc.New(httpBase(url)).TokenEndpoint()        # via nexus
  assertion = AgentAssertionFromKey(priv, key_id, tokenURL) # aud=tokenURL
  sendRegister{ ..., Assertion: assertion }
    -> broker (HeraldEdge set): custodian.Redeem -> herald -> bind heraldSubject
    -> ack.herald_subject == key_id
```

## Error handling

- **No keyfile / `NEXUS_HERALD_KEYFILE` unset** → register with no assertion; existing behavior. (Dark by default.)
- **Keyfile load/decode/fingerprint-mismatch** → fail aspect startup with a clear error (a malformed credential is operator error, surface it — unlike a runtime discovery hiccup).
- **Discovery or signing failure at register** → log + register without the assertion (transport still works; herald-bind retried next connect). Best-effort, not a hard gate, because omitting the assertion is the safe degrade and 3a only fails-closed on a *presented* bad assertion.
- **`enroll` herald down / lookup error** → abort with the herald error; never write a keyfile on an uncertain lookup.
- **`enroll` not-found (404)** → abort naming slug + fingerprint, pointing to provisioning (the typo / not-yet-provisioned guard).

## Testing

- **cwb-client (Piece 1):** `AgentAssertionFromKey` produces a JWT that decodes to the expected claims; `AgentAssertion(seed,slug,…)` and `AgentAssertionFromKey(DeriveAgentKey(seed,slug),…)` produce byte-identical assertions at a fixed clock (proves the refactor is lossless).
- **heraldkeyfile (Piece 2):** load a good keyfile → fields + `PrivateKey()` round-trip; missing field → error; bad base64 / wrong key length → error; fingerprint-mismatch → error.
- **runtime (Piece 3):** with a fake broker (the `wsclient` `fakeServer` harness) + a stub discovery endpoint, assert the `register` frame carries a non-empty `Assertion` whose decoded `aud` == the stub token endpoint and `iss/sub` == `key_id`; with no keyfile, assert `Assertion` is empty. (Decode-only, no signature verify — mirrors `identity.DecodeAccessClaims`.)
- **enroll (Piece 4):** unit with a stub herald — found-fingerprint → keyfile written `0600` with all five fields and `key_id` == the looked-up `id`; not-found (404) → abort, no file written; herald error → abort, no file written.
- **Gated live (skips offline):** against dMon herald on the tailnet (edge = herald directly), using a pre-provisioned throwaway `cwb-test-*` agent. `enroll --slug … --url …` → keyfile written; bring up a `HeraldEdge` broker; start a minimal aspect with `NEXUS_HERALD_KEYFILE` → it discovers, signs, registers; assert the ack `herald_subject` == the enrolled `key_id`. Proves producer→consumer→broker→herald end-to-end (everything except the not-yet-built gateway, which only changes the edge url).

## Build order

1. **cwb-client** `AgentAssertionFromKey` + refactor + `herald.GetAgentByFingerprint` wrapper + tests → merge → pin into nexus & cw.
2. **cw** `agent enroll` (attach-only) + tests (depends on 1).
3. **nexus** `runtime/heraldkeyfile` + `agent.Config`/`main.go` wiring + `sendRegister` signing + tests.
4. **Gated live test** end-to-end. CI-gated merges (nexus + cw + cwb-client each green before merge; no `--admin` bypass).

## Out of scope (deferred)

- **The nexus CWB gateway** — reverse proxy + OIDC discovery/endpoint rewrite + dMon herald issuer reconfig (the firewalled "everything via nexus" topology). Its own brainstorm; this cycle's edge abstraction is the seam it plugs into.
- **3b** — post-auth config/key distribution (the broker serving the herald-bound aspect its config + downstream keys).
- Replacing the transport bearer with herald (convergence).
- Re-enrolling / key rotation / revocation flows for an already-enrolled agent.
- **Mint-if-missing in `enroll`** — deferred until herald's human-facing create surface is settled (current HTTP create is the self-provision→pending→validate handshake or gRPC-admin-via-interchange). Related pre-existing bug: **`cw agent create` posts to `POST /api/orgs/{org}/agents`, a route current herald no longer serves** — flagged for a separate fix, not this cycle.

## References

`docs/2026-06-03-herald-rooted-agent-bootstrap-design.md` (the parent); `docs/2026-06-03-herald-auth-register-handshake-design.md` (step 3a, the broker side this feeds); `nexus/cwb/custodian`; `nexus/broker/ws.go` (`handleRegisterFrame`); `nexus/frames/payloads.go` (`RegisterPayload.Assertion`); `runtime/agent/agent.go` (`sendRegister`); `cwb-client/{identity,herald,oidc}`; `cw/internal/cli/agent` (`create`/`pubkey`).
