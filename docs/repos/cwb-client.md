<!-- GENERATED FILE — do not edit.
     Sourced from https://github.com/CarriedWorldUniverse/cwb-client/blob/HEAD/README.md
     by scripts/sync-repo-readmes.sh at docs build time.
     Edit that README, not this file. -->

!!! info "Sourced from the repo README"
    This page mirrors [`cwb-client`](https://github.com/CarriedWorldUniverse/cwb-client)'s live `README.md`.
    Edit the README in the repo, not this page.

# cwb-client

A reusable Go client for the [Carried World Builder](https://github.com/CarriedWorldUniverse) (CWB) platform. It wraps the platform pillars — **herald** (identity / OIDC), **cairn** (agent-native git), **ledger** (issue tracker), and **commonplace** (knowledge store) — as typed Go over a single edge-anchored HTTP seam, with the auth helpers needed to obtain and present bearer tokens.

The library was extracted from the `cw` CLI so that other CWB clients can share one accurate, transport-agnostic client surface.

## Layout

| Package | What it wraps |
|---|---|
| `client` | The edge-anchored HTTP `Doer` + `TokenSource` (bearer presentation, 401-refresh-retry). |
| `oidc` | Herald's OIDC token endpoint — discovery + password / JWT-bearer / refresh grants, revoke. |
| `identity` | Agent JWT assertions (from a seed via `casket.DeriveAgentKey`, or from an ed25519 key), access-claim decoding, key fingerprints. |
| `herald` | Orgs, humans, agents, products, `Me` (whoami), agent-by-fingerprint lookup. |
| `cairn` | Repos and pull requests (create/list repos, open/list/get/merge pulls). |
| `ledger` | Issues — create, get, claim, transition, comment, list-mine / list-ready / search-by-project. |
| `commonplace` | Knowledge entries — store, search, list, update, delete. |

## Install

```
go get github.com/CarriedWorldUniverse/cwb-client
```

Requires Go 1.26+.

```go
import (
	"github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
	"github.com/CarriedWorldUniverse/cwb-client/ledger"
	"github.com/CarriedWorldUniverse/cwb-client/cairn"
	"github.com/CarriedWorldUniverse/cwb-client/commonplace"
	"github.com/CarriedWorldUniverse/cwb-client/herald"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
)
```

## Quick start

A `*client.Client` targets one interchange edge as one identity. It is built
from the edge URL plus a `client.TokenSource` that supplies (and refreshes) the
bearer the client presents. On a `401` the client asks the source to refresh
and retries the request once.

### Constructing a client

For stateless, per-invocation use (an already-issued bearer), use
`client.WithStaticToken`:

```go
c := client.WithStaticToken("https://edge.example.com", accessToken)
```

To obtain a token first, talk to herald's OIDC bootstrap endpoints through the
edge via the `oidc` package:

```go
ctx := context.Background()

oc := oidc.New("https://edge.example.com")

// Human login (grant_type=password):
tok, err := oc.PasswordGrant(ctx, "alice@example.com", "secret")

// Agent login (grant_type=jwt-bearer) with a signed casket assertion:
endpoint, _ := oc.TokenEndpoint(ctx)
assertion, _ := identity.AgentAssertion(ownerSeed, slug, agentID, endpoint)
tok, err = oc.JWTBearerGrant(ctx, assertion)

c := client.WithStaticToken("https://edge.example.com", tok.AccessToken)
```

A long-lived caller can instead implement `client.TokenSource` (backed by
`oidc.RefreshGrant`) and pass it to `client.New(edge, src)` so the client
refreshes silently on `401`.

### Calling a pillar

Every pillar operation is a free function taking a `context.Context` and a
`client.Doer` (which `*client.Client` satisfies):

```go
// ledger: file an issue
iss, err := ledger.CreateIssue(ctx, c, ledger.CreateInput{
	Project: "NEX",
	Type:    "task",
	Summary: "Wire up the new edge route",
})

// commonplace: store and search knowledge
e, err := commonplace.Store(ctx, c, commonplace.StoreInput{
	Topic:   "edge-routing",
	Content: "Pillar paths are <edge>/<pillar>/<path>.",
})
hits, err := commonplace.Search(ctx, c, "edge routing", 5)
```

## Pillars and auth

### `client` — the edge seam

`client.New(edge, src)` and `client.WithStaticToken(edge, token)` build a
`*client.Client` targeting one edge as one identity. The client owns the pillar
prefix, the `Authorization: Bearer` header, and the silent single-retry refresh
on `401`. `client.TokenSource` (with `Token` / `Refresh`) abstracts where the
bearer comes from; `client.AccessToken` returns a currently-valid token. The
sentinel errors `client.ErrReauth` and `client.ErrNoRefresh` describe the
refresh outcome.

### `oidc` — token bootstrap

`oidc.New(edge)` reaches herald's tokenless OIDC routes through the edge.
`Discover` reads the discovery document; `PasswordGrant`, `JWTBearerGrant`, and
`RefreshGrant` perform the password, jwt-bearer, and refresh-token grants;
`TokenEndpoint` exposes the discovered token endpoint (an agent needs it as its
assertion audience); `Revoke` revokes a refresh token (RFC 7009).

### `identity` — casket credentials

`identity.AgentAssertion(seed, slug, agentID, tokenURL)` derives an agent's
casket key from `(seed, slug)` and signs an RFC 7523 jwt-bearer assertion;
`AgentAssertionFromKey` does the same from an already-derived key (the bootstrap
path). `Fingerprint(pub)` produces the stable casket fingerprint herald stores,
and `DecodeAccessClaims` reads (without verifying) an access token's claims for
display.

### `herald` — identity and org provisioning

Wraps herald's admin REST surface: `CreateOrg`, `ListOrgs`, `DeleteOrg`,
`GetProducts`, `EnableProduct`, `DisableProduct`, `CreateHuman`, `CreateAgent`,
`GetAgentByFingerprint`, `SetHumanPassword`, and `Me` (the caller's own
authoritative identity from `GET /api/me`).

### `cairn` — agent-native git

Wraps cairn's repo and pull-request surface: `CreateRepo`, `ListRepos`,
`OpenPull`, `ListPulls`, `GetPull`, and `MergePull`. A pull is opened with
`cairn.OpenPullInput` (source/target branches, title, ledger project key, and an
optional definition of done).

### `ledger` — issue tracker

Issue routes are token-org-scoped (no `{org}` in the path; reporter/actor are
server-derived). Operations: `CreateIssue`, `GetIssue`, `ListMine`, `ListReady`,
`SearchByProject`, `Claim`, `Transition`, and `Comment`.

### `commonplace` — knowledge store

Token-org-scoped knowledge entries (owner is server-derived). Operations:
`Store`, `Search`, `List`, `Update`, and `Delete`.

## Transport-agnostic: `client.Doer`

Each pillar wrapper depends only on the `client.Doer` interface, not on the
concrete HTTP client:

```go
type Doer interface {
	Do(ctx context.Context, method, pillar, path string, body []byte) (*http.Response, []byte, error)
}
```

`*client.Client` is the HTTP implementation. Because the pillar functions accept
any `Doer`, an alternate transport — for example a WebSocket relay inside nexus —
can implement the same interface and the pillar wrappers will route through it
unchanged.

## Where this fits

cwb-client is consumed by the `cw` CLI and other CWB clients. It talks to the
platform through the interchange edge: every request is addressed as
`<edge>/<pillar>/<path>`, with herald's OIDC bootstrap routes being the only
tokenless paths.
