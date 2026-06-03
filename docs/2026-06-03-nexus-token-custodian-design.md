# nexus token custodian — design

**Date:** 2026-06-03
**Status:** design (approved in operator/shadow brainstorming)
**Scope:** the first nexus-side CWB integration — **bootstrap step 2** of `docs/2026-06-03-herald-rooted-agent-bootstrap-design.md`. A nexus component that redeems an agent's casket assertion at herald, holds the resulting per-aspect token, and hands out an authenticated `cwb-client` for that aspect so nexus can call CWB pillars **as the aspect**. Single nexus cycle; the WS-handshake wiring that *feeds* it an assertion is step 3 (out of scope here).

## Goal

nexus holds the connection to CWB and mediates for all aspects. To preserve herald's identity-derived authz/ownership/attribution, nexus must act **as each aspect** at the pillars — i.e. be a per-aspect **token custodian**, not a single "nexus" identity. The custodian is that: given an agent's assertion (however obtained), it mints + holds + refreshes that aspect's herald token and yields a `cwb-client` bound to it.

## Grounding (verified)

- **herald's jwt-bearer grant returns a `refresh_token`** (+ `expires_in`, 10-min access TTL) — `herald/internal/oidc/agent_grant.go`. The `refresh_token` grant works for agent-minted tokens (rebuilds claims from the record, so scope/block changes take effect) — `herald/internal/oidc/refresh.go`. **⇒ the custodian refreshes via the refresh token; it never needs the agent's key after Redeem.** Chain exhaustion → the agent must re-assert.
- The access token's `sub` claim is the **agent's herald UUID** (`herald/internal/oidc/claims.go`). The custodian keys tokens by `sub`.
- **cwb-client API** (`github.com/CarriedWorldUniverse/cwb-client/{oidc,client,identity}`):
  - `oidc.New(edge) *Client`; `(*Client).JWTBearerGrant(ctx, assertion) (Token, error)`; `(*Client).RefreshGrant(ctx, refreshToken) (Token, error)`; `(*Client).TokenEndpoint(ctx) (string, error)`. `Token{AccessToken, RefreshToken, TokenType, ExpiresIn}`.
  - `client.New(edge string, src TokenSource) *Client`; `TokenSource{ Token(ctx)(string,error); Refresh(ctx)(string,error) }`; `client.ErrReauth`; `client.WithStaticToken`.
  - `identity.DecodeAccessClaims(token) (map[string]any, error)` (read `sub`); `identity.AgentAssertion(seed, slug, agentID, tokenURL) (string, error)` (tests mint an assertion).
- nexus: module `github.com/CarriedWorldUniverse/nexus`, **go 1.25.5 → bump to 1.26** (cwb-client requires it); first package to import cwb-client. Idiomatic home `nexus/cwb/custodian` (mirrors `nexus/credentials`, `nexus/identity`). Concurrency idiom (`nexus/nexus/outpost`): a `sync` mutex guarding an in-memory map, lock released before network I/O.

## Component — `nexus/cwb/custodian`

```go
// Custodian mints, holds, and refreshes per-aspect herald tokens, and yields a
// cwb-client authed AS an aspect. In-memory; tokens are ephemeral derived state.
type Custodian struct {
	edge string
	oc   *oidc.Client
	mu   sync.RWMutex
	by   map[string]*entry // keyed by herald subject (agent UUID)
}

type entry struct {
	mu      sync.Mutex // serialises this subject's refresh
	access  string
	refresh string
	exp     time.Time
}

func New(edge string) *Custodian // { edge, oidc.New(edge), by: map{} }

// Redeem exchanges an agent's casket assertion for a herald token, custodies it,
// and returns the subject (agent UUID).
func (c *Custodian) Redeem(ctx context.Context, assertion string) (subject string, err error)

// Client returns a cwb-client authed AS subject (a refreshing TokenSource).
// Error if subject was never Redeem'd.
func (c *Custodian) Client(subject string) (*client.Client, error)

// Forget drops a subject's custodied token (on disconnect).
func (c *Custodian) Forget(subject string)
```

- **`Redeem`** — `oc.JWTBearerGrant(ctx, assertion)` → `Token`; `sub := DecodeAccessClaims(tok.AccessToken)["sub"].(string)` (error if missing); store `entry{access, refresh, exp: now+ExpiresIn}` under `sub`; return `sub`. A grant failure (bad/expired/forged assertion) propagates herald's error.
- **`Client(subject)`** — under RLock, confirm `subject` is custodied (else error); return `client.New(c.edge, &source{c, subject})`.
- **`source`** implements `client.TokenSource`:
  - `Token(ctx)` — RLock → read the entry; if `exp` is more than a small skew (60s) ahead, return `access`; else `Refresh`.
  - `Refresh(ctx)` — take the entry's per-subject `mu`, re-check freshness (another goroutine may have refreshed), then `oc.RefreshGrant(ctx, entry.refresh)` → update `access`/`refresh`/`exp` → return the new access. On grant failure → `client.ErrReauth` (chain exhausted; the agent must re-assert). The herald HTTP call happens **outside** the map RWMutex (only the per-entry `mu` is held).
- **Concurrency:** the map RWMutex guards membership/reads; the per-`entry` `mu` serialises that subject's refresh so concurrent pillar calls for one aspect don't stampede herald. No lock is held across the herald request except the per-entry refresh mutex.

## Data flow

`assertion → Redeem (JWTBearerGrant) → custody {access, refresh, exp} keyed by sub → Client(sub) → pillar call as the aspect → on staleness/401, RefreshGrant(refresh) updates the entry → chain exhausted → ErrReauth → aspect re-asserts (step 3).`

## Error handling

- bad/expired/forged assertion → `Redeem` returns herald's grant error.
- `sub` missing from the redeemed token → `Redeem` errors (shouldn't happen with herald).
- `Client(subject)` for an un-redeemed subject → error.
- refresh chain exhausted → `client.ErrReauth` surfaced from a pillar call (the caller re-drives Redeem).

## Testing

- **Unit** (httptest herald stub mirroring `cwb-client/oidc/oidc_test.go` — discovery + `/herald/token` switching on `grant_type`): a real assertion via `identity.AgentAssertion(seed, slug, agentID, tokenURL)` →
  - `Redeem` returns the expected `sub` + custodies `{access, refresh}`; `Client(sub).Token` returns the access token.
  - a custodied entry whose `exp` is in the past triggers a `refresh_token` grant on the next `Token` (assert the stub saw `grant_type=refresh_token` and the access token rotated).
  - `Client(unknown)` errors; `Forget(sub)` then `Client(sub)` errors.
  - a refresh-grant 4xx → `Refresh`/`Token` surfaces `client.ErrReauth`.
- **Gated live** (dMon, `CWB_IT_*`-style env, skips offline): provision an agent (org + responsible human + agent, as the other live tests do) → mint its assertion (`AgentAssertion` with the owner seed + slug + agent id, `aud` = `oc.TokenEndpoint`) → `Redeem` → `Client(sub)` → call a pillar (`herald.Me` and/or `commonplace.List`) and assert it succeeds **as that agent** (the returned record/identity is the agent) → force a refresh and confirm a subsequent call still works.

## Build order

Single nexus cycle: (1) `go get cwb-client` + bump the `go` directive, add the `custodian` package + `source` + unit tests; (2) the gated live test against dMon. Then PR + merge.

## Out of scope

- The WS register-handshake that feeds `Redeem` an assertion (**step 3**).
- nexus config/secrets for the herald edge (the constructor takes `edge`; the broker wires it in step 3).
- The broker invoking the custodian / binding sessions (step 3).
- Persisting tokens (in-memory by design).

## References

`nexus/docs/2026-06-03-herald-rooted-agent-bootstrap-design.md` (step 2); `cwb-client/{oidc,client,identity}`; herald `internal/oidc/{agent_grant,refresh,claims}.go`; `nexus/nexus/outpost/outpost.go` (concurrency idiom).
