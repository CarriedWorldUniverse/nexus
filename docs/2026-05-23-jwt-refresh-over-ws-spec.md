# JWT Refresh Over WebSocket Spec

**Status:** Draft — 2026-05-23
**Scope:** Add a `session.refresh` request/response frame pair so agentfunnel can rotate its session JWT in-place without dropping the WebSocket or exiting for a supervisor restart.
**Non-goals:** Changing JWT TTL defaults. Changing keyfile validation. Changing the broker's JWT signing scheme. Outpost session lifecycle (Outpost uses the same wsclient but a different identity model; out of scope here).

## Motivation

Today's lifecycle (`runtime/cmd/agentfunnel/main.go:305-318`):

1. Aspect validates keyfile → gets `SessionJWT` + `SessionExpiresAt` (default 24h, `nexus/cmd/nexus/main.go:217`).
2. `jwtExpiryMonitor` schedules a wakeup at `expiry - 1min`.
3. At wakeup, calls `stop()` which cancels root ctx.
4. `wsClient.Run` returns; main exits.
5. Supervisor restarts agentfunnel — full re-validate, fresh WS, fresh register, cursor catch-up.

Each restart cleanly preserves Lock-6 semantics (cursor → no message loss; compaction → funnel state). But:

- The WebSocket drops for the dial gap (~10-100ms typical).
- In-memory turn state (mid-flight LLM stream, in-progress deliberation) is discarded.
- Operators see a process-restart event every JWT TTL.
- Any race between supervisor restart and pending broker pushes shifts those pushes to the cursor-replay path.

For 24h TTL this is once per day and largely invisible. But it's also avoidable: the session is already established and authenticated, we just need a fresh bearer for *future* reconnects (and the broker's own session record extended).

## Design

### Frame protocol

Two new frame kinds in `nexus/frames/frames.go`:

```go
KindSessionRefresh       Kind = "session.refresh"
KindSessionRefreshResult Kind = "session.refresh.result"
```

Aspect → broker (request, correlated via `Envelope.ID`):

```go
type SessionRefreshPayload struct {
	// Reason is a short tag for telemetry: "lead_time" | "manual"
	// | "post_reconnect". Not load-bearing for the broker.
	Reason string `json:"reason"`
}
```

Broker → aspect (response, `InReplyTo` set to request ID):

```go
type SessionRefreshResultPayload struct {
	// SessionJWT is the new bearer the aspect should use on the next
	// reconnect. Same identity, fresh expiry.
	SessionJWT string `json:"session_jwt"`

	// SessionExpiresAt is the new expiry; mirrors validate-endpoint
	// shape so consumers can reuse parsing.
	SessionExpiresAt string `json:"session_expires_at"` // RFC3339 UTC
}
```

Failure is signalled by an envelope-level error frame (the existing convention used by `chat.read` etc.), not a typed result with an `Error` field — keeps the success path clean.

### Broker side

`nexus/broker/ws.go` adds a handler for `KindSessionRefresh`. Steps:

1. Look up the aspect identity from the connection's bound session (already established at handshake; no re-validation against the keyfile required — the WS is authenticated).
2. Call `aspects.MintSessionFor(aspectName, cfg)` — a new function that issues a fresh JWT for an existing aspect record using the same signing path as `aspects.Validate` but skipping the encrypted-payload decode (the identity is already proven by the WS).
3. Optionally extend any broker-side session row's expiry to match the new JWT.
4. Emit `KindSessionRefreshResult` with the fresh JWT + expiry.

Rate-limit: at most one refresh per aspect per 60s (silently rejected with error frame on excess). Prevents a misbehaving aspect from chewing signing CPU.

The fresh JWT must use the same `sub` claim as the existing one (aspect identity invariant), which `MintSessionFor` enforces by taking the aspect record as input.

### Aspect side

In `runtime/cmd/agentfunnel/main.go`:

1. Add a `sessionRefreshLoop` goroutine that schedules a refresh at `expiry - 1h` (fixed lead time).
2. `sessionRefreshLoop` sends a `KindSessionRefresh` via `wsClient.Request` (request/response correlation already exists).
3. On success: update the in-memory `currentJWT` and `currentExpiry` (shared atomic, see below) and reschedule the next refresh for the new `expiry - 1h`. Log at Info: "session refreshed".
4. On failure: log Warn, retry after 5min. Up to 3 retries total before giving up *for this refresh window*.
5. `jwtExpiryMonitor` stays as-is — it's now a safety net: if `expiry - 1min` arrives with no successful refresh having advanced `currentExpiry`, the process exits and the supervisor restarts (existing path).

### Shared JWT state

A single `sessionState` struct under `sync.Mutex` (or `atomic.Pointer`) holds `{JWT string, ExpiresAt time.Time}`. It's read by:
- `TokenProvider` (each WS dial) — picks up the latest JWT for the *next* reconnect.
- `jwtExpiryMonitor` — its `expiry` reference is updated on each successful refresh so it reschedules its safety-net wakeup.

`sessionRefreshLoop` is the sole writer.

Migration from current code: `tokenProvider` currently captures the *initial* `kf` and re-validates from scratch on every dial. We change it to first check `sessionState`; if a non-expired JWT exists there, use it. If not, fall back to the keyfile re-validate path (preserves first-dial and post-supervisor-restart behaviour).

### Compatibility

- A new aspect against an old broker: aspect sends `session.refresh`, broker doesn't know the kind, responds with the standard "unknown frame kind" handling (logs and ignores per `broker/ws.go:400` — *"unknown frame kind, dropping"*). The aspect sees no response, retries, gives up after 3 tries, falls back to the safety-net restart path. Operational degradation is exactly today's behaviour. No version negotiation needed.
- An old aspect against a new broker: never sends the frame; broker never serves it. No-op.
- During rolling broker upgrade: same as above — graceful degradation.

The frame kind is additive; no envelope or schema changes.

### Operational impact

- Healthy deployment: 1× refresh log line per hour-before-expiry per aspect (at 24h TTL: 1 per day). No restart per JWT cycle.
- On refresh failure: 3× warn log lines per failure window, then the safety-net restart still kicks in 1 minute before expiry. Equivalent to today's worst case.
- Broker CPU: one HS256 sign per refresh. Negligible.

## Testing strategy

`nexus/broker/ws_test.go`:
1. Aspect sends `session.refresh`, broker responds with new JWT, `sub` matches, expiry is in the future.
2. Rate-limit: two refreshes within 60s — second returns an error frame.

`runtime/cmd/agentfunnel/*_test.go` (new file or extend existing):
1. Mock broker that replies to `session.refresh` with a fresh JWT — assert `sessionState.JWT` is updated.
2. Mock broker that ignores `session.refresh` — assert 3 retries then fallback (timer fires).
3. JWT TTL of 5s, refresh lead of 3s → assert refresh fires, expiry advances, no restart.

No protocol-level e2e change needed; existing transport tests cover correlation.

## Risk

- **Identity drift.** If `MintSessionFor` accidentally issues a JWT with a different `sub`, the aspect would silently start authenticating as a different identity. Mitigation: `MintSessionFor` takes the aspect record directly (not a name string) and asserts the resulting JWT's `sub` matches; broker unit test pins this.
- **Refresh storms.** Many aspects refreshing simultaneously (e.g., after a coordinated startup) hammer the broker's signing path. Mitigation: aspect schedules refresh with `+/- 10%` jitter around the lead time.
- **Stale JWT used during a flap.** A reconnect that beats the refresh schedule will use the *old* JWT via TokenProvider's keyfile re-validate path; that's no worse than today, since today every reconnect re-validates anyway.

## Out of scope

- Pushed refresh from broker side (broker-driven rotation). Pull model is enough.
- Cross-aspect refresh batching.
- Cryptographic improvements (signing scheme, key rotation).
- Outpost JWT refresh — separate identity model.

## Open questions

- Should the broker's session row's `expires_at` extend automatically on refresh, or stay pinned to the original session start? Recommend: extend, to match the JWT, otherwise admin views show misleading "expired" sessions.
