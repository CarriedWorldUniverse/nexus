# agentfunnel Connection Stability Spec

**Status:** Draft — 2026-05-23
**Scope:** Two targeted fixes to the aspect-side WebSocket transport: a client read deadline (Fix A) and a Connect-event-driven register handshake (Fix B).
**Non-goals:** Pending-buffer cap, deliberateLoop error escalation, observability buffering. Those are real issues (see [Background](#background)) but out of scope here.

## Background

Symptom reported by operator: agentfunnel "disconnects and doesn't maintain connections." A deep read of the comms stack found five culprits, ranked. This spec addresses the top two.

### Fix A — Client read deadline (CRITICAL)

`runtime/wsclient/wsclient.go:267-272`:

```go
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		msgType, data, err := conn.Read(ctx)   // line 269 — blocks forever
		if err != nil {
			return err
		}
		...
```

`conn.Read(ctx)` has no per-read deadline. If the broker dies without closing the TCP socket (broker OOM, container kill, mid-box NAT/firewall idle drop, nginx idle timeout), the read blocks until the OS TCP keepalive eventually fires — minutes to hours. During that window:

- `Connected()` returns true → `wsasp.queueOrSend` keeps trying immediate sends that hang on `c.writeMu`
- `registerOnReady` sees `wasConnected=true` and never re-registers
- `drainPendingLoop` keeps appending to `pending` (unbounded)
- The funnel deliberation loop continues ticking; from the model's view nothing's wrong; from the operator's view the agent has gone silent.

Server pings every 30s with a 10s pong deadline (`broker/ws.go:323-343`) — but **only if the server is alive**. Client has no symmetric guard.

### Fix B — Register race after reconnect (HIGH)

`runtime/aspect/wsasp/wsasp.go:148-164`:

```go
func (c *Client) registerOnReady(ctx context.Context) {
	wasConnected := false
	t := time.NewTicker(250 * time.Millisecond)   // poll
	...
	now := c.ws.Connected()
	if now && !wasConnected {
		c.sendRegister(ctx)
	}
```

Two problems:

1. **Race with drainPendingLoop.** `registerOnReady` polls every 250ms; `drainPendingLoop` polls every 500ms. After a reconnect, the drain loop can flush queued chat frames *before* `registerOnReady` sends the register frame. The broker receives chat.send for an aspect it doesn't recognise yet on this connection — frames are dropped or misattributed.

2. **Up-to-250ms latency on every reconnect**, paid before the first frame can be processed.

`wsclient.Events()` (added under issue #29) exists precisely to replace this polling. The in-code comment at wsasp.go:144-147 even flags it: *"A subscription pattern would be cleaner ... F2.6 may rev wsclient to add a connect callback."* The callback exists; wsasp wasn't updated.

## Design

### Fix A: per-read deadline with idle reconnect

**Approach:** wrap each `conn.Read` call in a context with a deadline. If the read times out without producing a frame, treat it as a dead connection — return the timeout error from `readLoop`, which propagates up to `Run`, which triggers the existing reconnect path.

**Timeout value:** **45 seconds.**

Reasoning: server pings every 30s (`broker/ws.go:323`). On a healthy connection the client receives a pong-driven ping frame from `coder/websocket`'s internals at least every 30s. A 45s deadline gives 1.5× headroom for jitter / GC / network blips, and detects a dead peer within 45s instead of hours.

**Configuration:** expose as `Config.ReadIdleTimeout` with a default of 45s. Tests use 200ms to keep the suite fast. Setting to 0 disables (back-compat for any caller who really wants the old behaviour).

**Why not server-side TCP keepalives:** they exist (`tcp_keepalive_*`), but defaults vary by OS, can't be configured from Go without raw socket access on Darwin/Windows, and offer no upper bound shorter than minutes. An app-layer deadline is portable and tight.

**Why not a write keepalive:** the broker already pings; the client just needs to *notice*. Adding a client→server ping is extra protocol surface for no extra detection — when both peers ping you double the traffic and detect at the same time.

### Fix B: register on ConnectEvent, ordered before drain

**Approach:** replace `registerOnReady`'s polling loop with a subscriber on `c.ws.Events()`. On each `ConnectEvent{Connected: true}`, send the register frame *synchronously* (i.e., `Send` returns before the event handler proceeds). The drain loop is changed to wait on the same event channel — but does nothing until *after* register has been acknowledged.

**Ordering guarantee:** we adopt a simple "register barrier" — a `chan struct{}` closed by the register handler after `sendRegister` returns nil. `drainPendingLoop` waits on this barrier on each new connection cycle before flushing. The barrier is re-created on every disconnect.

This keeps the existing buffer-then-drain semantics intact (Lock 6 "AI never sees connection state"), while ensuring no chat.send frame races register on the wire.

**Why not multiplex into the existing tick loop:** the ticker is the bug. The whole point is to react to the event, not poll for the state-change-after-the-fact.

**Single-consumer constraint:** `wsclient.Events()` is documented as single-consumer (wsclient.go:151-156). wsasp.Run owns it; nothing else may subscribe. Add a runtime assertion via panic on second Subscribe if we ever expose more, but for v1 we just document it.

**JWT refresh interaction:** none. `TokenProvider` runs in `dialAndServe`, which fires before any `ConnectEvent{Connected:true}` is emitted. The register frame sees the fresh token via the connection that's now live.

## Behaviour after the fix

| Scenario | Before | After |
|---|---|---|
| Broker crash, idle connection | Hang minutes-to-hours; pending grows unbounded | Detect within 45s; reconnect; drain after register |
| nginx idle-close at 60s | Same as broker crash | Same — detected within 45s |
| Reconnect with queued chat sends | Some sends lost to register race | All sends arrive after register, in order |
| Healthy long-running connection | Works | Works (server's 30s ping resets the 45s deadline) |
| Ctrl+C during reconnect | Works | Works (ctx cancellation still wins) |

## Testing strategy

Unit tests in `runtime/wsclient/wsclient_test.go` and `runtime/aspect/wsasp/wsasp_test.go`:

1. **Fix A:** stand up a test WS server that accepts the connection then stops responding (no pings, no close). Assert `Run` returns / reconnects within `ReadIdleTimeout + slack`.
2. **Fix A:** healthy server that pings every 100ms with `ReadIdleTimeout: 200ms` — assert connection survives N seconds without reconnect.
3. **Fix B:** test server that records frame arrival order. Client queues 3 chat sends *while disconnected*, then server comes up. Assert: first observed frame is `register`, then the three chat sends in order.
4. **Fix B:** event subscription is exclusive — calling `Events()` twice returns the same channel; or, if we keep it single-take, documented and asserted.

Integration test in `runtime/cmd/agentfunnel/` is out of scope; the unit tests cover the contract.

## Migration / rollout

- Both fixes ship in the same release. They're independent in code but reinforce each other operationally (Fix A triggers more reconnects; Fix B makes those reconnects clean).
- No protocol change. Existing brokers and aspects are wire-compatible.
- No config knobs required for operators; defaults are sensible. `ReadIdleTimeout` is exposed for tests and emergency tuning.
- Outpost (`runtime/wsclient` is shared between aspect and Outpost per the package doc) gets Fix A automatically. Fix B is wsasp-only, doesn't touch Outpost.

## Risk

- **False positives on Fix A.** If something legitimately blocks the broker's ping loop for >45s (long GC, scheduler starvation, kernel pause), the client reconnects unnecessarily. Mitigation: the reconnect is cheap (~1s + register), backoff resets on natural drop, and the symptom of "occasional brief reconnects" is strictly better than the current "silent permanent hang."
- **Single-consumer Events channel.** If future code subscribes elsewhere, register stops firing. Mitigation: add a one-line guard.
