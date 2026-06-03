# herald-auth in the register handshake (bootstrap step 3a) — design

**Date:** 2026-06-03
**Status:** design (approved in operator/shadow brainstorming)
**Scope:** **step 3a** of `docs/2026-06-03-herald-rooted-agent-bootstrap-design.md` — feed the token custodian (step 2) real assertions over the websocket. An aspect may carry a casket assertion in its `register` frame; the broker redeems it via `nexus/cwb/custodian`, binds the herald identity to the connection, and holds the per-aspect CWB client. **Additive + opt-in** — no change to existing aspects or the current transport auth. 3b (config/key distribution) and the aspect-side wsclient signing are deferred.

## Goal

The custodian holds per-aspect herald tokens and yields a CWB client *as the aspect* — but until now nothing feeds it assertions from real connections. This wires the broker's `register` handshake to the custodian: present an assertion → `Redeem` → bind. After this, nexus holds a per-aspect CWB client per herald-authed connection.

## Grounding (verified)

- WS accept + register: `nexus/broker/ws.go` — `handleConnect` authenticates the upgrade with the existing bearer (TokenStore / aspect-JWT from the keyfile-validate flow), then `handleRegisterFrame(env)` decodes a `frames.ForwardedRegisterPayload`, validates, calls `roster.Register`, binds `c.registeredAs`/`c.sessionID`/`dispatcher.bind`, and sends a `KindRegisterAck` with `RegisterAckPayload`.
- Register payloads (`nexus/frames/payloads.go`): `RegisterPayload`/`ForwardedRegisterPayload` embed `schemas.RegisterRequest` and already carry optional fields (`SinceMsgID`/`RequestReplay`, `omitempty`). `ForwardedRegisterPayload` adds `ViaOutpostStamp`. The outpost stores + replays the full payload verbatim, so an added optional field rides through unchanged.
- The custodian (`nexus/cwb/custodian`): `New(edge)`, `Redeem(ctx, assertion) (subject, error)`, `Client(subject) (*client.Client, error)`, `Forget(subject)`. `client` = `github.com/CarriedWorldUniverse/cwb-client/client`.
- Aspect-side assertion: `cwb-client/identity.AgentAssertion(seed, slug, agentID, tokenURL)` (the tests mint one).

## Mechanism (additive, opt-in)

1. **frames** — add an optional field to the register payloads + the ack:
   - `RegisterPayload.Assertion string json:"assertion,omitempty"`
   - `ForwardedRegisterPayload.Assertion string json:"assertion,omitempty"`
   - `RegisterAckPayload.HeraldSubject string json:"herald_subject,omitempty"`
   Backward-compatible: existing aspects omit `assertion`; the broker omits `herald_subject` when no binding happened.

2. **broker holds a custodian behind an interface** — in `broker` (server.go), define a small interface so production wires the concrete custodian and tests inject a fake/stub-backed one:
   ```go
   type Custodian interface {
       Redeem(ctx context.Context, assertion string) (string, error)
       Client(subject string) (*client.Client, error)
       Forget(subject string)
   }
   ```
   `broker.Config` gains `HeraldEdge string` (sourced from `NEXUS_HERALD_EDGE`). In `broker.New`, `if cfg.HeraldEdge != "" { b.custodian = custodian.New(cfg.HeraldEdge) }`. **`HeraldEdge` empty (default) → `b.custodian == nil` → the assertion block is skipped → zero behavior change.**

3. **wsConn** gains `heraldSubject string` + `heraldClient *client.Client`.

4. **`handleRegisterFrame` seam** — after the existing `roster.Register` + bind, before the ack:
   ```go
   if payload.Assertion != "" && c.broker.custodian != nil {
       subject, err := c.broker.custodian.Redeem(ctx, payload.Assertion)
       if err != nil {
           c.respondError(env, "herald assertion redemption failed: "+err.Error())
           return
       }
       cl, err := c.broker.custodian.Client(subject)
       if err != nil {
           c.respondError(env, "custodian client: "+err.Error())
           return
       }
       c.heraldSubject = subject
       c.heraldClient = cl
   }
   ```
   Then set `ack.HeraldSubject = c.heraldSubject` (empty when unbound). Use the connection/request context for `Redeem` (with the http client's own 30s timeout inside cwb-client).

5. **disconnect cleanup** — where the connection unregisters (the existing close path): `if c.heraldSubject != "" && c.broker.custodian != nil { c.broker.custodian.Forget(c.heraldSubject) }`.

## Safety

- **Opt-in + dark by default:** gated on `HeraldEdge`; absent → the whole path is skipped.
- **No impersonation:** an aspect can only present an assertion it can *sign* (needs the agent's casket key, derived from the owner seed), so it can only herald-bind to identities it already holds. No need to enforce `register.Name == herald_subject` in 3a (the roster/transport identity and the herald identity are independent here; they converge when herald replaces the keyfile transport auth, a later step).
- **Strict on explicit failure:** an aspect that *presents* an assertion and fails redemption gets a `respondError` (register fails) rather than a silent half-auth — the problem surfaces.
- **Existing path untouched:** no assertion, or `custodian == nil` → the register proceeds exactly as today.
- **Outpost-transparent:** the assertion rides in the forwarded register payload; the outpost relays it unchanged (no outpost change).

## Error handling

- assertion present + redeem fails (bad/expired/forged/herald down) → `respondError` (register fails; the aspect may retry).
- `custodian == nil` (HeraldEdge unset) → assertion ignored, normal register.
- no assertion → normal register, `herald_subject` omitted from the ack.

## Testing

- **Unit (`broker/ws_test.go`):** the broker wired with a `Custodian` backed by an httptest **stub herald** (the custodian's own test pattern — discovery + `/herald/token` handling jwt-bearer + returning a token whose `sub` is the agent id). Cases:
  - register *with* a real `identity.AgentAssertion` → the ack carries `herald_subject` == the agent id; the conn is bound (`c.heraldSubject` set).
  - register *without* an assertion → existing path; ack has no `herald_subject`; not bound.
  - register with a bad assertion (stub rejects empty / returns 4xx) → `respondError`.
  - broker with `custodian == nil` → assertion present is ignored, normal register.
  - disconnect → `Forget` called (assert via a counting fake custodian).
- **Gated live (skips offline):** bring up a broker with `HeraldEdge`=dMon, connect as an aspect, sign a real assertion for a provisioned agent, register → assert the ack's `herald_subject` == the provisioned agent id (the broker redeemed a real assertion over the WS end-to-end). (The custodian's own live test already proved the redeemed client calls pillars.)

## Build order

Single nexus cycle: (1) frames fields + the `Custodian` interface + `broker.Config.HeraldEdge` + construction in `broker.New` + the `wsConn` fields + the `handleRegisterFrame` seam + the disconnect `Forget` + unit tests; (2) the gated live test. CI-gated merge.

## Out of scope (deferred)

- **3b** — post-auth config/key distribution (serve the aspect its config + downstream keys); needs its own design.
- The **aspect-side wsclient** reading its keyfile + signing the assertion into the register frame (agent-runtime wiring; the tests play the aspect here).
- Replacing the transport (bearer) auth with herald — convergence is a later step.
- Enforcing `register.Name == herald_subject`.

## References

`nexus/docs/2026-06-03-herald-rooted-agent-bootstrap-design.md` (step 3); `nexus/cwb/custodian`; `nexus/broker/ws.go` (`handleRegisterFrame`); `nexus/frames/payloads.go`; `cwb-client/{identity,client}`.
