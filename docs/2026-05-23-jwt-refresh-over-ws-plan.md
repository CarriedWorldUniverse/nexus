# JWT Refresh Over WebSocket — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `session.refresh` / `session.refresh.result` frames so agentfunnel can rotate its session JWT in-place without dropping the WebSocket or exiting for a supervisor restart.

**Architecture:** Aspect sends `session.refresh` on a fixed lead before expiry (1h default, ±10% jitter). Broker mints a fresh JWT for the *same* aspect identity (no encrypted-payload re-decode — the WS is already authenticated) and returns it via correlated response. Aspect stores it in a shared `sessionState`, which `TokenProvider` reads on future dials and `jwtExpiryMonitor` reads to re-arm its safety-net wakeup. Refresh failures retry 3× then fall back to today's restart path.

**Spec:** `docs/2026-05-23-jwt-refresh-over-ws-spec.md`

**Tech:** Go. Frame protocol additive. No new deps.

---

## File Structure

- **Create:** `nexus/aspects/refresh.go` — `MintSessionFor` (signs a fresh JWT for an existing aspect record)
- **Modify:** `nexus/frames/frames.go` — add `KindSessionRefresh`, `KindSessionRefreshResult`
- **Modify:** `nexus/frames/payloads.go` — add `SessionRefreshPayload`, `SessionRefreshResultPayload`
- **Modify:** `nexus/broker/ws.go` — add handler + 60s rate-limit per aspect
- **Create:** `nexus/aspects/refresh_test.go` — `MintSessionFor` unit tests
- **Create:** `nexus/broker/ws_session_refresh_test.go` — broker handler + rate-limit tests
- **Create:** `runtime/cmd/agentfunnel/session_state.go` — shared `sessionState` (atomic JWT + expiry)
- **Create:** `runtime/cmd/agentfunnel/refresh_loop.go` — `sessionRefreshLoop` goroutine
- **Modify:** `runtime/cmd/agentfunnel/main.go` — wire `sessionState`, refactor `tokenProvider`, start refresh loop, update `jwtExpiryMonitor` to read live expiry
- **Create:** `runtime/cmd/agentfunnel/refresh_loop_test.go` — refresh loop behaviour with mock broker

---

## Task 1 — Frame kinds + payload types

**Files:**
- Modify: `nexus/frames/frames.go`
- Modify: `nexus/frames/payloads.go`

### Step 1.1 — Add the two new Kind constants

- [ ] In `nexus/frames/frames.go`, add to the Kind block (alphabetical or grouped with other session-like kinds — match the existing pattern around `KindRegister`):

```go
KindSessionRefresh       Kind = "session.refresh"
KindSessionRefreshResult Kind = "session.refresh.result"
```

### Step 1.2 — Add payload types

- [ ] In `nexus/frames/payloads.go`, add:

```go
// SessionRefreshPayload is sent by an aspect to request a fresh
// session JWT over the existing authenticated WebSocket. The broker
// identifies the aspect from the connection's bound session — no
// keyfile material is required.
//
// Reason is a free-form tag for telemetry. Common values: "lead_time"
// (scheduled refresh), "manual" (operator-triggered), "post_reconnect"
// (defensive refresh after a reconnect cycle). Not load-bearing.
type SessionRefreshPayload struct {
	Reason string `json:"reason"`
}

// SessionRefreshResultPayload carries the fresh session JWT and its
// expiry back to the aspect. Same identity (sub claim unchanged).
// On the wire the expiry uses the validate-endpoint shape (RFC3339
// UTC) so existing parsing helpers apply.
type SessionRefreshResultPayload struct {
	SessionJWT       string `json:"session_jwt"`
	SessionExpiresAt string `json:"session_expires_at"`
}
```

### Step 1.3 — Tests

- [ ] In `nexus/frames/payloads_test.go` (or wherever the round-trip tests live — match existing convention), add encode/decode round-trip tests for both payloads. Mirror the shape of existing round-trip tests for `RegisterPayload`.

### Step 1.4 — Verify + commit

- [ ] Run: `go test ./nexus/frames/... -race -timeout 30s`
- [ ] Expected: PASS
- [ ] Commit:

```bash
git add nexus/frames/frames.go nexus/frames/payloads.go nexus/frames/payloads_test.go
git commit -m "frames: add session.refresh request/response payloads

New frame kinds for in-protocol JWT rotation: aspect requests a
fresh JWT, broker responds with new SessionJWT + expiry. Identity
unchanged; sub claim invariant. See docs/2026-05-23-jwt-refresh-
over-ws-spec.md."
```

---

## Task 2 — Broker-side: `MintSessionFor` + handler

**Files:**
- Create: `nexus/aspects/refresh.go`
- Create: `nexus/aspects/refresh_test.go`
- Modify: `nexus/broker/ws.go`
- Create: `nexus/broker/ws_session_refresh_test.go`

### Step 2.1 — Write `MintSessionFor` (failing test first)

- [ ] Create `nexus/aspects/refresh_test.go`:

```go
package aspects

import (
	"testing"
	"time"
)

func TestMintSessionFor_ReusesAspectIdentity(t *testing.T) {
	cfg := RefreshConfig{
		NexusID:              "nx-test",
		SessionSigningSecret: []byte("test-secret-must-be-32-bytes-min"),
		NewSessionID:         func() string { return "ses-new" },
		Now:                  func() time.Time { return time.Unix(1_700_000_000, 0) },
		JWTTTL:               24 * time.Hour,
	}
	aspect := &Aspect{Name: "alpha", KeyfileVersion: 3}

	out, err := MintSessionFor(cfg, aspect)
	if err != nil {
		t.Fatalf("MintSessionFor: %v", err)
	}
	if out.SessionJWT == "" {
		t.Fatal("empty JWT")
	}
	if out.Claims.Sub != "alpha" {
		t.Fatalf("sub = %q, want alpha", out.Claims.Sub)
	}
	if out.Claims.Kfv != 3 {
		t.Fatalf("kfv = %d, want 3", out.Claims.Kfv)
	}
	if got := out.ExpiresAt.Unix(); got != 1_700_000_000+86400 {
		t.Fatalf("expiry = %d, want %d", got, 1_700_000_000+86400)
	}
}

func TestMintSessionFor_RejectsInvalidConfig(t *testing.T) {
	_, err := MintSessionFor(RefreshConfig{}, &Aspect{Name: "x"})
	if err == nil {
		t.Fatal("expected error on empty config")
	}
}
```

- [ ] Run: `go test ./nexus/aspects/ -run MintSessionFor -timeout 30s`
- [ ] Expected: FAIL (no `MintSessionFor`, no `RefreshConfig`).

### Step 2.2 — Implement `MintSessionFor`

- [ ] Create `nexus/aspects/refresh.go`:

```go
package aspects

import (
	"errors"
	"fmt"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
)

// RefreshConfig is the subset of ValidateConfig used by MintSessionFor.
// Carries the signing material + clock; no Store or decryption needed
// because the aspect identity is already established by the caller.
type RefreshConfig struct {
	NexusID              string
	SessionSigningSecret []byte
	NewSessionID         func() string
	Now                  func() time.Time
	JWTTTL               time.Duration
}

// MintSessionFor issues a fresh session JWT for an already-identified
// aspect. Used by the broker's session.refresh handler — the WebSocket
// is authenticated, so we skip the keyfile decode/validate dance and
// re-enter the same signing path used at validate time.
//
// The sub claim is the aspect Name verbatim; kfv mirrors the aspect's
// current KeyfileVersion. Caller must supply RefreshConfig with all
// fields populated and a non-nil aspect.
func MintSessionFor(cfg RefreshConfig, a *Aspect) (*ValidatedSession, error) {
	if a == nil || a.Name == "" {
		return nil, errors.New("aspects.MintSessionFor: aspect required")
	}
	if cfg.NexusID == "" || len(cfg.SessionSigningSecret) == 0 ||
		cfg.NewSessionID == nil || cfg.Now == nil || cfg.JWTTTL <= 0 {
		return nil, errors.New("aspects.MintSessionFor: config incomplete")
	}

	now := cfg.Now()
	exp := now.Add(cfg.JWTTTL)
	claims := jwt.Claims{
		Iss: "nexus://" + cfg.NexusID,
		Sub: a.Name,
		Iat: now.Unix(),
		Exp: exp.Unix(),
		Kfv: a.KeyfileVersion,
		Ses: cfg.NewSessionID(),
	}
	tok, err := jwt.Sign(cfg.SessionSigningSecret, claims)
	if err != nil {
		return nil, fmt.Errorf("aspects.MintSessionFor: jwt sign: %w", err)
	}
	return &ValidatedSession{
		AspectName:     a.Name,
		KeyfileVersion: a.KeyfileVersion,
		SessionJWT:     tok,
		Claims:         claims,
		ExpiresAt:      exp,
	}, nil
}
```

- [ ] Run: `go test ./nexus/aspects/ -run MintSessionFor -timeout 30s`
- [ ] Expected: PASS.

### Step 2.3 — Broker handler (failing test first)

- [ ] Create `nexus/broker/ws_session_refresh_test.go` with a test that:
  1. Stands up a broker with an authenticated aspect connection
  2. Aspect sends `session.refresh` with `Reason="manual"`
  3. Asserts response is `session.refresh.result` correlated by `InReplyTo`
  4. Asserts the returned JWT decodes with the same `sub` as the original
  5. Asserts a *second* refresh within 60s gets an error frame (rate limit)
  6. Asserts a refresh after the rate-limit window succeeds

Match the existing test scaffolding in `nexus/broker/ws_test.go` (look for how it sets up a test broker and authenticated WS connection — reuse those helpers).

- [ ] Run: `go test ./nexus/broker/ -run SessionRefresh -race -timeout 60s`
- [ ] Expected: FAIL (no handler).

### Step 2.4 — Implement the broker handler

- [ ] In `nexus/broker/ws.go`'s frame-dispatch switch (find where `KindChatSend` is handled — same pattern), add a case for `KindSessionRefresh`. The handler:
  1. Reads the aspect identity from the bound connection state.
  2. Checks `lastRefreshAt[aspectName]` — reject with error frame if within 60s.
  3. Calls `aspects.MintSessionFor` using the broker's existing config (signing secret + nexus ID + clock + NewSessionID factory + JWTTTL — same values used at validate time; they live somewhere in `BrokerConfig` or similar — locate them and reuse).
  4. Updates `lastRefreshAt[aspectName] = now`.
  5. Writes a `KindSessionRefreshResult` envelope correlated via `InReplyTo`.
  6. Optionally: updates the broker's session row's `expires_at` to the new value (matches answer to "Open question" in spec). Implement this; admin views shouldn't show stale "expired" rows for live aspects.

- [ ] The `lastRefreshAt` map should be on the broker struct (`map[string]time.Time` guarded by a mutex). Initialise it where other broker state is initialised.

- [ ] Run: `go test ./nexus/broker/ -run SessionRefresh -race -timeout 60s`
- [ ] Expected: PASS.

### Step 2.5 — Verify + commit

- [ ] Run: `go test ./nexus/... -race -timeout 180s` — full broker/aspects suite green.
- [ ] Commit:

```bash
git add nexus/aspects/refresh.go nexus/aspects/refresh_test.go \
        nexus/broker/ws.go nexus/broker/ws_session_refresh_test.go
git commit -m "broker+aspects: session.refresh handler with MintSessionFor

aspects.MintSessionFor reuses the validate-time signing path for an
already-identified aspect (skips encrypted-payload decode — the WS
is already authenticated). Broker handler dispatches session.refresh,
rate-limits to 1/aspect/60s, returns session.refresh.result with the
fresh JWT and extends the session row's expires_at to match."
```

---

## Task 3 — Aspect-side: `sessionState` + refresh loop

**Files:**
- Create: `runtime/cmd/agentfunnel/session_state.go`
- Create: `runtime/cmd/agentfunnel/refresh_loop.go`
- Modify: `runtime/cmd/agentfunnel/main.go`
- Create: `runtime/cmd/agentfunnel/refresh_loop_test.go`

### Step 3.1 — `sessionState` (no tests; trivial)

- [ ] Create `runtime/cmd/agentfunnel/session_state.go`:

```go
package main

import (
	"sync/atomic"
	"time"
)

// sessionState holds the current session JWT + expiry. Updated by
// the refresh loop on successful refresh; read by tokenProvider on
// each WS dial and by jwtExpiryMonitor for its safety-net wakeup.
//
// Backed by atomic.Pointer so reads are wait-free and writes are
// single-publisher (only refreshLoop and the boot path write).
type sessionState struct {
	v atomic.Pointer[sessionSnapshot]
}

type sessionSnapshot struct {
	JWT     string
	Expires time.Time
}

func newSessionState(initial sessionSnapshot) *sessionState {
	s := &sessionState{}
	s.v.Store(&initial)
	return s
}

func (s *sessionState) Snapshot() sessionSnapshot {
	return *s.v.Load()
}

func (s *sessionState) Set(snap sessionSnapshot) {
	s.v.Store(&snap)
}
```

### Step 3.2 — Refresh loop (failing test first)

- [ ] Create `runtime/cmd/agentfunnel/refresh_loop_test.go`. It needs a fake `refreshSender` interface that the loop calls to send the frame and await the response. Test cases:
  1. Lead-time fires → loop sends a refresh request → fake broker returns new JWT → sessionState.Snapshot reflects new JWT + expiry → next refresh is scheduled relative to the new expiry.
  2. Lead-time fires → fake broker returns error 3× → loop logs, gives up *for this window*, does NOT update sessionState → jwtExpiryMonitor would handle the safety-net path (not directly tested here).
  3. ctx cancel during sleep → loop returns cleanly.

Use a small fake clock so tests run fast (e.g., TTL=200ms, lead=100ms; loop fires after ~100ms).

### Step 3.3 — Implement refresh loop

- [ ] Create `runtime/cmd/agentfunnel/refresh_loop.go`:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// refreshSender abstracts the wsclient request path so the loop can
// be unit-tested with a fake. Production wiring passes wsClient.Request.
type refreshSender interface {
	Request(ctx context.Context, env frames.Envelope) (frames.Envelope, error)
}

// sessionRefreshLoop schedules an in-protocol JWT refresh some lead
// time before the current expiry. On success it updates sessionState;
// on repeated failure it gives up for the current window and lets
// jwtExpiryMonitor's safety-net restart kick in 1 minute before
// expiry. Each successful refresh reschedules off the *new* expiry.
//
// Jitter (±10% of lead) spreads refresh load across aspects so a
// coordinated startup doesn't hammer the broker.
func sessionRefreshLoop(
	ctx context.Context,
	state *sessionState,
	sender refreshSender,
	lead time.Duration,
	log *slog.Logger,
) {
	const maxAttempts = 3
	const retryDelay = 5 * time.Minute

	for {
		snap := state.Snapshot()
		wakeAt := snap.Expires.Add(-lead).Add(jitter(lead))
		d := time.Until(wakeAt)
		if d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}

		ok := false
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if ctx.Err() != nil {
				return
			}
			fresh, err := requestRefresh(ctx, sender)
			if err == nil {
				state.Set(fresh)
				log.Info("agentfunnel: session refreshed",
					"expires", fresh.Expires.Format(time.RFC3339),
					"attempt", attempt)
				ok = true
				break
			}
			log.Warn("agentfunnel: session refresh failed",
				"attempt", attempt, "err", err)
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryDelay):
				}
			}
		}
		if !ok {
			log.Warn("agentfunnel: session refresh giving up; jwtExpiryMonitor will restart",
				"current_expires", state.Snapshot().Expires.Format(time.RFC3339))
			// Don't tight-loop — sleep until close to expiry so the
			// safety-net monitor can do its job.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(state.Snapshot().Expires)):
			}
		}
	}
}

func requestRefresh(ctx context.Context, sender refreshSender) (sessionSnapshot, error) {
	env, err := frames.New(frames.KindSessionRefresh, frames.SessionRefreshPayload{
		Reason: "lead_time",
	})
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("compose: %w", err)
	}
	resp, err := sender.Request(ctx, env)
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("request: %w", err)
	}
	var p frames.SessionRefreshResultPayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		return sessionSnapshot{}, fmt.Errorf("decode result: %w", err)
	}
	exp, err := time.Parse(time.RFC3339, p.SessionExpiresAt)
	if err != nil {
		return sessionSnapshot{}, fmt.Errorf("parse expiry %q: %w", p.SessionExpiresAt, err)
	}
	return sessionSnapshot{JWT: p.SessionJWT, Expires: exp}, nil
}

// jitter returns a random duration in [-10%, +10%] of d.
func jitter(d time.Duration) time.Duration {
	const pct = 0.1
	span := float64(d) * pct
	return time.Duration((rand.Float64()*2 - 1) * span)
}
```

### Step 3.4 — Wire into main.go

- [ ] In `runtime/cmd/agentfunnel/main.go`, after `res, err := client.Validate(bootCtx, kf)` succeeds:

```go
state := newSessionState(sessionSnapshot{
	JWT:     res.SessionJWT,
	Expires: res.SessionExpiresAt,
})
```

- [ ] Replace the existing `tokenProvider` closure (lines 166-177) with a version that reads from `state` first:

```go
tokenProvider := func(ctx context.Context) (string, error) {
	snap := state.Snapshot()
	if snap.JWT != "" && time.Until(snap.Expires) > 1*time.Minute {
		return snap.JWT, nil
	}
	// sessionState is empty or near-expired — fall back to keyfile
	// re-validate (post-restart cold-start, or refresh loop has been
	// failing for so long the JWT is about to expire).
	client := keyfile.NewClient()
	fresh, ferr := client.Validate(ctx, kf)
	if ferr != nil {
		log.Warn("agentfunnel: TokenProvider re-validate failed, using cached token",
			"err", ferr)
		return "", ferr
	}
	state.Set(sessionSnapshot{JWT: fresh.SessionJWT, Expires: fresh.SessionExpiresAt})
	log.Info("agentfunnel: TokenProvider re-validated via keyfile",
		"expires", fresh.SessionExpiresAt.Format(time.RFC3339))
	return fresh.SessionJWT, nil
}
```

- [ ] Update `jwtExpiryMonitor` call to read live expiry from `state` instead of the boot-time `res.SessionExpiresAt`. This requires either passing `state` in or restructuring the monitor to take a `func() time.Time`. Pick the cleaner of the two for the existing code shape. The monitor must re-check the expiry after each sleep (in case the refresh loop pushed it out):

```go
go jwtExpiryMonitor(ctx, func() time.Time { return state.Snapshot().Expires },
	1*time.Minute, stop, log)
```

And update `jwtExpiryMonitor` signature to:

```go
func jwtExpiryMonitor(ctx context.Context, expiryFn func() time.Time, lead time.Duration,
	stop context.CancelFunc, log *slog.Logger) {
	for {
		wakeAt := expiryFn().Add(-lead)
		d := time.Until(wakeAt)
		if d <= 0 {
			break
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
		// Re-check: did refresh push expiry out? If yes, sleep again.
		if time.Until(expiryFn().Add(-lead)) > 0 {
			continue
		}
		break
	}
	log.Info("agentfunnel: JWT nearing expiry — exiting for supervisor restart",
		"jwt_expires", expiryFn().Format(time.RFC3339), "lead", lead.String())
	stop()
}
```

- [ ] Start the refresh loop after `wsClient` is wired but before `wsClient.Run`:

```go
go sessionRefreshLoop(ctx, state, wsClient.ws /* or a Request-exposing accessor */,
	1*time.Hour, log)
```

(If `wsClient` is `*wsasp.Client` not `*wsclient.Client`, you'll need to expose a `Request` method on the bridge — match the existing accessor patterns; don't add a public field.)

### Step 3.5 — Verify + commit

- [ ] Run: `go test ./runtime/cmd/agentfunnel/... -race -timeout 60s` — refresh loop tests green.
- [ ] Run: `go test -race -timeout 180s ./runtime/... ./nexus/...` — full suite green.
- [ ] Commit:

```bash
git add runtime/cmd/agentfunnel/session_state.go \
        runtime/cmd/agentfunnel/refresh_loop.go \
        runtime/cmd/agentfunnel/refresh_loop_test.go \
        runtime/cmd/agentfunnel/main.go
git commit -m "agentfunnel: in-protocol JWT refresh via session.refresh

Scheduled refresh at expiry-1h (±10% jitter). On success the new
JWT lands in sessionState; TokenProvider reads from there on future
dials and jwtExpiryMonitor re-checks expiry after each sleep so a
successful refresh push the safety-net wakeup forward. On 3 failed
attempts the loop gives up for this window and the existing monitor
still triggers the supervisor restart 1 minute before expiry.

In healthy steady state: 1 refresh log per hour-before-expiry per
aspect, no process restart per JWT TTL."
```

---

## Self-review checklist

- [ ] Spec coverage: every section in the spec maps to a task.
- [ ] No `TODO` / placeholders in any task.
- [ ] Type consistency: `sessionSnapshot` field names match across `session_state.go`, `refresh_loop.go`, and the `tokenProvider` rewrite.
- [ ] Identity invariant: a broker-side test asserts `sub` is unchanged across refresh.
- [ ] Failure path covered: refresh-3×-fails test asserts sessionState is unchanged so safety-net monitor still runs.
- [ ] Old broker compatibility: refresh request to a broker that doesn't know the kind silently drops; the aspect's 3-retry-then-give-up path falls back to the existing restart behaviour. Verified by inspection (no test needed since it relies on a broker version we'd have to mock).
- [ ] No protocol negotiation; additive frame kinds only.

## Execution

Once approved, hand off to subagent-driven-development.
