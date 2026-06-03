# nexus CWB gateway — design

**Date:** 2026-06-03
**Status:** design (operator/shadow brainstorming)
**Scope:** the nexus-side egress to CWB — so aspects and human CLIs reach CWB *through nexus*, which holds the single allowed outbound connection to the CWB edge (interchange). Two surfaces over one egress: a **WS↔REST translator** for aspects (the agent acts as its own org identity; nexus injects its custodied token) and a **thin HTTP reverse-proxy** for human CLIs (`cw`/`enroll`, pass-through). This is the firewalled "everything via nexus" realization the aspect-side bootstrap (`docs/2026-06-03-aspect-side-bootstrap-and-enroll-design.md`) deferred; it plugs into the edge-agnostic seam that work left behind.

## Goal

In the target deployment the firewall opens a single egress port for nexus→CWB; nothing else reaches CWB. nexus is therefore the boundary. The comms plane is already mediated (outpost multiplexes aspect WS → one upstream WS to nexus). This adds the **data plane**: nexus relays agents' CWB REST/gRPC requests across its connection, and reverse-proxies human-CLI CWB traffic — so an aspect speaks *only* WS and a human CLI points at nexus, neither needing direct CWB egress.

The model (operator): **agents are org members having the conversation with CWB; nexus is just a forwarder/translator moving WS → REST/gRPC → WS.** nexus is transport glue, not an authority — CWB enforces authz against each agent's own herald identity.

## Grounding (verified)

- **interchange** is the existing CWB *ingress* edge: verifies the bearer against herald's JWKS, injects `cwb-*`/`X-CWB-*` identity, grpc-gateway-translates the admin/pillar RPCs (HTTP→gRPC) and reverse-proxies cairn git, dials pillars over mTLS; backends are ClusterIP, interchange is the only public CWB surface. herald's issuer is already the interchange edge URL (live discovery confirms `issuer: …:8080/herald/`).
- **3a already binds, per herald-authed WS connection, `c.heraldClient *client.Client`** — a custodied CWB client that injects + refreshes that aspect's herald token (`nexus/cwb/custodian`). The WS translator replays requests through it.
- **`client.Client.Do(ctx, method, pillar, path, body) (*http.Response, []byte, error)`** is the request seam; every pillar package's `do(ctx, c *client.Client, method, path, body, out)` helper calls it (`c.Do(ctx, method, "<pillar>", path, raw)`), and the public wrappers (`herald.CreateAgent`, `ledger.*`, `cairn.*`, `commonplace.*`) take `*client.Client`.
- **`wsclient.Client.Request(ctx, env) (frames.Envelope, error)`** is the correlated WS round-trip (already used by `sendRegister`).
- **`broker.Config.HeraldEdge`** exists and constructs the custodian, but **is not yet read from any env var** in `cmd/nexus` (greenfield wiring). The broker's HTTP routes (`/connect`, `/api/*`, `/dashboard/*`, `/health`) are registered in `ListenAndServe`.

## Architecture — two surfaces, one egress

```
ASPECT (WS only, holds no CWB token)
  cwb-client pillar wrapper (ledger.CreateIssue, …) over a WS-Doer
    └─ cwb.request{pillar,method,path,body} ──WS──▶ broker
                                                     └─ c.heraldClient.Do(...)  [custodied token injected]
                                                          └──HTTPS (single egress)──▶ interchange ──▶ pillar
       cwb.response{status,body} ◀──WS───────────────────┘

HUMAN CLI (cw / enroll, holds its own bearer)
  cwb-client over HTTP ─▶ nexus /herald|/cairn|/ledger|/knowledge  (reverse-proxy, pass-through)
                            └──HTTPS (single egress)──▶ interchange ──▶ pillar
```

## Component 1 — `client.Doer` inversion (cwb-client)

Make the pillar wrappers transport-agnostic so they run over HTTP *or* the WS translator, mirroring the existing `TokenSource` inversion.

```go
// in package client
type Doer interface {
	Do(ctx context.Context, method, pillar, path string, body []byte) (*http.Response, []byte, error)
}
```

`*client.Client` already satisfies `Doer` (its `Do` has that exact signature). Change each pillar package's `do(...)` helper and every exported wrapper to take `client.Doer` instead of `*client.Client`. **Backward-compatible:** all current call sites pass a `*client.Client`, which satisfies `Doer`, so cw / enroll / custodian compile unchanged. (`AccessToken`/`URL` are not used by the wrappers, so `Doer` stays minimal.)

This is the load-bearing change that lets the aspect reuse the real pillar wrappers (path/method/marshal logic) over WS — no duplication.

## Component 2 — WS translator (aspect data plane)

**Frames** (`nexus/frames/payloads.go`):
- `cwb.request` → `CWBRequestPayload { Pillar string; Method string; Path string; Body []byte }` (body = raw JSON, omitempty).
- `cwb.response` → `CWBResponsePayload { Status int; Body []byte }` (carried on the response envelope; the wrappers map non-2xx to errors themselves).

**Broker handler** (`nexus/broker/ws.go`), on the `cwb.request` kind:
```go
if c.heraldClient == nil {
    c.respondError(env, "cwb.request requires a herald-bound connection")
    return
}
var p frames.CWBRequestPayload
if err := frames.PayloadAs(env, &p); err != nil { c.respondError(env, ...); return }
resp, raw, err := c.heraldClient.Do(c.broker.ctx, p.Method, p.Pillar, p.Path, p.Body)
if err != nil { c.respondError(env, "cwb relay: "+err.Error()); return }
c.respond(env, frames.KindCWBResponse, frames.CWBResponsePayload{Status: resp.StatusCode, Body: raw})
```
The custodied `heraldClient` injects + refreshes the token; CWB authorizes the agent's own identity. The frame carries `pillar`+`path` (not a URL), so nexus pins the host to the CWB edge — no SSRF, no host retargeting.

**Runtime WS-Doer** (`runtime/agent`, satisfies `client.Doer`):
```go
type wsDoer struct{ ws *wsclient.Client; edge string }
func (d *wsDoer) Do(ctx, method, pillar, path string, body []byte) (*http.Response, []byte, error) {
	req, _ := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{Pillar: pillar, Method: method, Path: path, Body: body})
	resp, err := d.ws.Request(ctx, req)        // correlated WS round-trip
	if err != nil { return nil, nil, err }
	var p frames.CWBResponsePayload
	if err := frames.PayloadAs(resp, &p); err != nil { return nil, nil, err }
	return &http.Response{StatusCode: p.Status}, p.Body, nil   // wrappers read StatusCode + bytes
}
```
The aspect builds its pillar clients over `wsDoer` and uses the ordinary wrappers (`ledger.CreateIssue`, `commonplace.Search`, …). The Agent exposes the WS-Doer to its tool layer. **The aspect holds no CWB token and makes no HTTP call.** (`edge` is unused at runtime — the broker owns the real edge — but kept for the synthesized response's `Request` if a wrapper ever needs it; omit if not.)

## Component 3 — HTTP reverse-proxy (human CLIs + bootstrap discovery)

A handler on the broker's existing HTTP mux (registered in `ListenAndServe`, not a new process) that reverse-proxies the CWB path prefixes to the edge, pass-through:

- Prefixes `/herald`, `/cairn`, `/ledger`, `/knowledge` → `httputil.ReverseProxy` to `NEXUS_CWB_EDGE`, forwarding the request (incl. `Authorization`) verbatim; the `Director` rewrites scheme/host to the edge and preserves the path.
- Serves: `cw` + `cw agent enroll` (point their `--edge`/`CW_EDGE` at the nexus url), and the aspect's existing `buildAssertion` OIDC discovery (point the bootstrap keyfile `url` at the nexus url; `/herald/.well-known/*` forwards to interchange, which already returns the interchange-issuer doc — so the assertion `aud` stays consistent, no herald reconfig).
- Pass-through auth: nexus does not verify; interchange does (it's the authority). nexus only constrains the destination host (the edge).

## Component 4 — config + custodian wiring

One env var: **`NEXUS_CWB_EDGE`** = the interchange base URL. `cmd/nexus` reads it and sets `broker.Config.HeraldEdge = NEXUS_CWB_EDGE` (wiring the custodian, currently dormant) *and* passes it to the reverse-proxy handler. The WS translator's `heraldClient` (from the custodian) and the reverse-proxy thus share one edge. Empty → both surfaces dark (custodian nil → `cwb.request` errors; reverse-proxy routes not registered) — additive, no behavior change when unset.

## Data flow (end to end)

```
aspect tool: ledger.CreateIssue(ctx, wsDoer, ...)
  -> wsDoer.Do(POST, "ledger", "/api/...", body)
  -> cwb.request frame --WS--> broker (herald-bound conn)
  -> c.heraldClient.Do(POST,"ledger","/api/...",body)   [custodied token]
  -> HTTPS NEXUS_CWB_EDGE/ledger/api/... --> interchange --> ledger gRPC
  <- status+body -> cwb.response frame --WS--> wsDoer -> wrapper unmarshals

cw agent enroll --edge https://nexus ...
  -> GET https://nexus/herald/api/agents/by-fingerprint/{fp}  (human bearer)
  -> reverse-proxy --> interchange/herald composite --> herald
```

## Security

- **No new trust:** nexus injects only the *already-custodied* per-aspect token (WS surface) or forwards the caller's own bearer (HTTP surface); interchange remains the authority and authorizes each agent's own org identity. nexus cannot escalate.
- **No SSRF / host retargeting:** WS `cwb.request` carries `pillar`+`path`; nexus pins the host to `NEXUS_CWB_EDGE`. The reverse-proxy `Director` likewise forces the edge host and only matches the four CWB prefixes.
- **Token confinement:** the aspect WS surface keeps CWB bearers inside nexus (custodian) — the aspect never holds one, matching the bootstrap custody model.
- **Bound-only:** `cwb.request` is refused on a connection with no `heraldClient` (not herald-bound).

## Error handling

- `cwb.request` on an unbound connection → `respondError` ("requires a herald-bound connection").
- `heraldClient.Do` transport error / `ErrReauth` (custodian refresh exhausted) → `respondError` with the cause; the aspect surfaces it to its tool layer (CWB authz failures arrive as a normal `cwb.response` with a 4xx status + body, which the wrapper maps to an error).
- `NEXUS_CWB_EDGE` unset → both surfaces dark (no custodian, no proxy routes).
- Reverse-proxy upstream failure → standard 502 from `httputil.ReverseProxy`.

## Testing

- **cwb-client (Doer):** the inversion compiles and all existing pillar tests pass unchanged (a `*client.Client` is still accepted); add a tiny test that a fake `Doer` drives a wrapper (proves transport-agnosticism).
- **WS translator (broker):** with the `fakeNexus`/stub-herald harness + a fake custodian whose `heraldClient` points at a stub CWB server, send a `cwb.request` on a herald-bound connection → assert the stub received `GET /herald/api/me` with the injected bearer and the `cwb.response` carries the stub's status+body; on an unbound connection → `respondError`.
- **runtime WS-Doer:** with the `fakeNexus` echoing a `cwb.response`, a pillar wrapper over `wsDoer` round-trips and unmarshals; assert the emitted `cwb.request` frame carries the right pillar/method/path/body.
- **reverse-proxy:** an httptest "edge" behind the broker mux; `GET /herald/api/me` with a bearer is forwarded verbatim (path + Authorization preserved) to the edge.
- **Gated live (skips offline):** against dMon, using the provisioned `cwb-test-*` agent. Bring up a broker with `NEXUS_CWB_EDGE`=interchange; open a herald-bound aspect WS (assertion → custodian → `heraldClient`); send `cwb.request GET /herald /api/me`; assert the `cwb.response` body's `id` == the agent id (it called herald *as the agent*, through nexus, end to end). Plus: `cw --edge http://nexus … whoami --remote` through the reverse-proxy returns the same identity.

## Build order

1. **cwb-client** `client.Doer` + repoint pillar `do`/wrappers; tests; merge → pin into nexus.
2. **nexus frames** `cwb.request`/`cwb.response` payloads + kinds.
3. **nexus broker** `cwb.request` handler (uses `c.heraldClient`) + the reverse-proxy handler + `NEXUS_CWB_EDGE` wiring in `cmd/nexus` (→ `broker.Config.HeraldEdge` + proxy); unit tests.
4. **nexus runtime** `wsDoer` (`client.Doer` over `a.ws`) + Agent exposes it to the tool layer; unit test.
5. **Gated live test** end-to-end (WS surface + reverse-proxy surface). CI-gated merges; no `--admin` bypass.

## Out of scope (deferred)

- **cairn git** packfiles (Smart-HTTP/SSH) and **native gRPC** pass-through — the RPC pillars are reached via interchange's HTTP grpc-gateway; git keeps its own transport. The WS translator handles JSON API calls only.
- **Streaming / large responses** — `cwb.response` is a single framed body (cap to the WS read limit); chunked/streaming relay is a later concern.
- **The data-plane cutover itself** (issues→ledger, knowledge→commonplace, git→cairn in the aspects' tools) — this builds the transport; the cutover wires aspect tools onto it.
- **3b** post-auth config/key distribution (beyond the token, which the custodian already holds).
- Replacing the bootstrap-discovery HTTP call with a WS connect-hello — the reverse-proxy serves discovery, so YAGNI for now.

## References

`docs/2026-06-03-aspect-side-bootstrap-and-enroll-design.md` (the seam this plugs into); `docs/2026-06-03-herald-auth-register-handshake-design.md` (3a — binds `c.heraldClient`); `nexus/cwb/custodian`; `nexus/broker/ws.go`/`server.go`; `runtime/agent/agent.go`; `runtime/wsclient`; `cwb-client/client` + the pillar packages; `interchange/cmd/interchange-gateway` (the edge this fronts).
