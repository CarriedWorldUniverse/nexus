# herald-auth register handshake Implementation Plan (step 3a)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Single nexus cycle, 2 tasks (the wiring + unit tests, then the gated live test). Touches the live broker — **additive/opt-in**, gated by `HeraldEdge`; default off = zero behavior change. CI-gated merge (nexus).

**Goal:** An aspect may carry a casket assertion in its `register` frame; the broker redeems it via `nexus/cwb/custodian` and binds the herald identity + per-aspect CWB client to the connection.

**Architecture:** optional `assertion` field on the register payload → `handleRegisterFrame` calls a broker-held `Custodian` (interface) → bind `heraldSubject`/`heraldClient` on the `wsConn` → ack the subject → `Forget` on cleanup. Spec: `nexus/docs/2026-06-03-herald-auth-register-handshake-design.md`.

**Tech:** Go 1.26. Imports `github.com/CarriedWorldUniverse/nexus/nexus/cwb/custodian` + `github.com/CarriedWorldUniverse/cwb-client/client`.

## Verified landmarks

- `nexus/frames/payloads.go`: `RegisterPayload` + `ForwardedRegisterPayload` (both embed `schemas.RegisterRequest`, have `omitempty` optional fields); `RegisterAckPayload{HeartbeatIntervalS, StaleAfterS}`.
- `nexus/broker/ws.go`: `wsConn` struct (line 28); `handleRegisterFrame` (line 935) — decodes `ForwardedRegisterPayload` (939), validates (944), `roster.Register` (973), binds `c.registeredAs`/`c.sessionID`/`dispatcher.bind` (986-988), builds `RegisterAckPayload` ack (1020), `c.send(ack)` (1024). `cleanup()` (line 1171) — runs on disconnect (`defer c.cleanup()` at 312), unbinds the dispatcher (1179).
- `nexus/broker/server.go`: `type Config struct` (60); `type Broker struct` (332); `func New(cfg Config, r *roster.Roster) *Broker` (390).
- `nexus/cwb/custodian`: `New(edge) *Custodian`, `Redeem(ctx, assertion)(string,error)`, `Client(subject)(*client.Client,error)`, `Forget(subject)`.
- Test harness (`nexus/broker/ws_test.go`): `newTestServer(t) (*httptest.Server, *roster.Roster, *Broker)`, `dialWS(t, srv, token)`, `sendFrame`, `recvFrame`; pattern in `TestRegisterFrameAddsRoster` (133).

---

## Task 1: the wiring + unit tests

**Files:** `nexus/frames/payloads.go`, `nexus/broker/custodian.go` (new), `nexus/broker/server.go`, `nexus/broker/ws.go`, `nexus/broker/ws_test.go`, `go.mod` (add cwb-client + custodian is already in-module).

- [ ] **Step 1: Add the optional frames fields** — `nexus/frames/payloads.go`:
  - In `RegisterPayload`, add: `Assertion string \`json:"assertion,omitempty"\`  // casket assertion for herald-auth (bootstrap step 3a)`
  - In `ForwardedRegisterPayload`, add the same `Assertion string \`json:"assertion,omitempty"\`` (so it survives the outpost relay).
  - In `RegisterAckPayload`, add: `HeraldSubject string \`json:"herald_subject,omitempty"\`  // set when the register's assertion was redeemed`

- [ ] **Step 2: The `Custodian` interface + dep** — `cd /Users/jacinta/Source/nexus && go get github.com/CarriedWorldUniverse/cwb-client@1db44b1 && go mod tidy` (already a dep via the custodian package; this ensures it). Create `nexus/broker/custodian.go`:

```go
package broker

import (
	"context"

	"github.com/CarriedWorldUniverse/cwb-client/client"
)

// Custodian is the broker's view of the per-aspect herald token custodian
// (nexus/cwb/custodian). An interface so tests inject a fake. When the broker
// has no HeraldEdge configured, the field is nil and herald-auth is skipped.
type Custodian interface {
	Redeem(ctx context.Context, assertion string) (subject string, err error)
	Client(subject string) (*client.Client, error)
	Forget(subject string)
}
```

- [ ] **Step 3: Broker config + construction** — `nexus/broker/server.go`:
  - In `Config`, add: `// HeraldEdge, when set (NEXUS_HERALD_EDGE), enables herald-auth on register:\n\t// an aspect's assertion is redeemed via the custodian. Empty = disabled.\n\tHeraldEdge string`
  - In `Broker`, add a field: `custodian Custodian // nil unless HeraldEdge is configured`
  - In `New`, after the broker `b` is constructed (before `return b`), add:
    ```go
    if cfg.HeraldEdge != "" {
        b.custodian = custodian.New(cfg.HeraldEdge)
    }
    ```
    Import `"github.com/CarriedWorldUniverse/nexus/nexus/cwb/custodian"`. (`*custodian.Custodian` satisfies the `Custodian` interface.)

- [ ] **Step 4: wsConn fields** — `nexus/broker/ws.go`, in `type wsConn struct` add:
```go
	// heraldSubject/heraldClient are set when the register frame carried a
	// casket assertion the custodian redeemed (bootstrap step 3a). Empty/nil
	// otherwise. heraldClient calls CWB pillars AS this aspect.
	heraldSubject string
	heraldClient  *client.Client
```
Add the import `"github.com/CarriedWorldUniverse/cwb-client/client"` to ws.go.

- [ ] **Step 5: The register seam** — `nexus/broker/ws.go` `handleRegisterFrame`, insert AFTER `c.broker.dispatcher.bind(state.Name, c)` (line ~988) and BEFORE the ack construction (line ~1020):
```go
	// Bootstrap step 3a: if the aspect presented a casket assertion and herald-
	// auth is enabled, redeem it and bind the herald identity + per-aspect CWB
	// client to this connection. Additive: absent assertion / no custodian =
	// unchanged. A present-but-failing assertion fails the register (surfaced).
	if payload.Assertion != "" && c.broker.custodian != nil {
		subject, err := c.broker.custodian.Redeem(c.broker.ctx, payload.Assertion)
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
Then set the ack's new field — change the ack construction to include `HeraldSubject: c.heraldSubject` (empty when unbound):
```go
	ack, _ := frames.NewResponse(frames.KindRegisterAck, env.ID, frames.RegisterAckPayload{
		HeartbeatIntervalS: c.broker.cfg.HeartbeatIntervalS,
		StaleAfterS:        int(c.broker.cfg.StaleAfter.Seconds()),
		HeraldSubject:      c.heraldSubject,
	})
```
(Use `c.broker.ctx` — the broker's WS-lifetime context — for `Redeem`.)

- [ ] **Step 6: Forget on cleanup** — `nexus/broker/ws.go` `cleanup()` (line ~1171), where it unbinds the dispatcher, add:
```go
	if c.heraldSubject != "" && c.broker.custodian != nil {
		c.broker.custodian.Forget(c.heraldSubject)
		c.heraldSubject = ""
	}
```

- [ ] **Step 7: Unit tests** — `nexus/broker/ws_test.go`. Add a fake custodian + tests (the interface is what makes this injectable; the real redemption is covered by the custodian's own + live tests). Read `TestRegisterFrameAddsRoster` for the register-send + ack-recv pattern; decode the ack via `frames.PayloadAs`.

```go
type fakeCustodian struct {
	redeem func(ctx context.Context, assertion string) (string, error)
	forgot []string
}

func (f *fakeCustodian) Redeem(ctx context.Context, a string) (string, error) { return f.redeem(ctx, a) }
func (f *fakeCustodian) Client(subject string) (*client.Client, error) {
	return client.WithStaticToken("http://x", "tok"), nil
}
func (f *fakeCustodian) Forget(subject string) { f.forgot = append(f.forgot, subject) }

func registerWith(t *testing.T, c *websocket.Conn, name, assertion string) frames.Envelope {
	t.Helper()
	env, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name: name, ContextMode: schemas.ContextGlobal, Provider: "claude-api",
			SessionID: "sess-1", Home: "/tmp/x", StartedAt: time.Now().UTC(),
		},
		Assertion: assertion,
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	return recvFrame(t, c)
}

func ackSubject(t *testing.T, env frames.Envelope) string {
	t.Helper()
	var p frames.RegisterAckPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		t.Fatalf("ack payload: %v", err)
	}
	return p.HeraldSubject
}

func TestRegisterHeraldAssertionBinds(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = &fakeCustodian{redeem: func(_ context.Context, a string) (string, error) {
		if a == "" {
			return "", fmt.Errorf("empty")
		}
		return "agent-1", nil
	}}
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "ha", "a-real-looking-assertion")
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("kind=%s", ack.Kind)
	}
	if got := ackSubject(t, ack); got != "agent-1" {
		t.Fatalf("herald_subject=%q want agent-1", got)
	}
}

func TestRegisterNoAssertion(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = &fakeCustodian{redeem: func(context.Context, string) (string, error) { return "x", nil }}
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "na", "")
	if ack.Kind != frames.KindRegisterAck || ackSubject(t, ack) != "" {
		t.Fatalf("no-assertion register should not bind; ack=%s subj=%q", ack.Kind, ackSubject(t, ack))
	}
}

func TestRegisterBadAssertion(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = &fakeCustodian{redeem: func(context.Context, string) (string, error) {
		return "", fmt.Errorf("herald rejected")
	}}
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "bad", "nope")
	if ack.Kind != frames.KindError {
		t.Fatalf("bad assertion should error, got %s", ack.Kind)
	}
}

func TestRegisterCustodianNil(t *testing.T) {
	srv, _, b := newTestServer(t)
	b.custodian = nil // herald-auth disabled
	c := dialWS(t, srv, "testtoken")
	ack := registerWith(t, c, "off", "ignored-assertion")
	if ack.Kind != frames.KindRegisterAck || ackSubject(t, ack) != "" {
		t.Fatalf("custodian-nil should ignore the assertion; ack=%s subj=%q", ack.Kind, ackSubject(t, ack))
	}
}

func TestRegisterForgetOnClose(t *testing.T) {
	srv, _, b := newTestServer(t)
	fc := &fakeCustodian{redeem: func(context.Context, string) (string, error) { return "agent-1", nil }}
	b.custodian = fc
	c := dialWS(t, srv, "testtoken")
	registerWith(t, c, "fc", "sig")
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	// cleanup runs async on the serve goroutine; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.connMu.Lock() // any broker lock to avoid a race read; or just sleep
		done := len(fc.forgot) > 0
		b.connMu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(fc.forgot) == 0 || fc.forgot[0] != "agent-1" {
		t.Fatalf("Forget not called on close: %v", fc.forgot)
	}
}
```
> **Implementer note:** match the exact `schemas.RegisterRequest` required fields against `TestRegisterFrameAddsRoster` (Capabilities, Port, etc. may be required by `validateRegister`) — copy that test's payload shape and just add `Assertion`. The `KindError` constant + `respondError` produce the error frame — confirm the kind a failed register returns and assert that. For `TestRegisterForgetOnClose`, if reading `fc.forgot` races, guard the fake with its own mutex; the goal is to confirm `Forget("agent-1")` fires on disconnect.

- [ ] **Step 8: Build + test** — `cd /Users/jacinta/Source/nexus && go build ./... && go test ./nexus/frames/ ./nexus/broker/ -race -v && go vet ./nexus/broker/`
Expected: all green (the 5 new tests + the existing register/broker suite — the additive change must not regress them).

- [ ] **Step 9: Commit**

```bash
cd /Users/jacinta/Source/nexus && git add nexus/frames/payloads.go nexus/broker/ go.mod go.sum
git commit -m "broker: herald-auth on register (assertion -> custodian -> bind), opt-in via HeraldEdge"
```

---

## Task 2: gated live test

**Files:** `nexus/broker/herald_register_live_test.go`

- [ ] **Step 1: Gated live test** — bring up a test broker with a REAL custodian pointed at dMon herald, connect as an aspect, sign a real assertion for a provisioned agent, register, assert the ack's `herald_subject` == the agent id:

```go
package broker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
	"github.com/CarriedWorldUniverse/nexus/nexus/cwb/custodian"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
	"github.com/coder/websocket"
)

// TestLiveHeraldRegister proves the broker redeems a real casket assertion
// presented in a register frame against the live dMon herald.
// Gated on CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG.
func TestLiveHeraldRegister(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG to run the live herald register test")
	}
	srv, _, b := newTestServer(t)
	b.custodian = custodian.New(edge)

	ctx := context.Background()
	tu, err := oidc.New(edge).TokenEndpoint(ctx)
	if err != nil {
		t.Fatalf("token endpoint: %v", err)
	}
	assertion, err := identity.AgentAssertion([]byte(seed), slug, agentID, tu)
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}

	c := dialWS(t, srv, "testtoken")
	env, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name: "live-ha", ContextMode: schemas.ContextGlobal, Provider: "claude-api",
			SessionID: "sess-live", Home: "/tmp/x", StartedAt: time.Now().UTC(),
		},
		Assertion: assertion,
	})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, env)
	ack := recvFrame(t, c)
	if ack.Kind != frames.KindRegisterAck {
		t.Fatalf("kind=%s", ack.Kind)
	}
	if got := ackSubject(t, ack); got != agentID {
		t.Fatalf("herald_subject=%q want %q", got, agentID)
	}
}
```
> **Implementer note:** match the `schemas.RegisterRequest` shape to the unit tests / `TestRegisterFrameAddsRoster` (required fields). Reuse the `ackSubject` helper from Task 1.

- [ ] **Step 2: Offline suite** — `cd /Users/jacinta/Source/nexus && go build ./... && go vet ./nexus/broker/ && go test ./nexus/broker/`
Expected: green; `TestLiveHeraldRegister` SKIPs without `CW_IT_*`.

- [ ] **Step 3: Commit**

```bash
cd /Users/jacinta/Source/nexus && git add nexus/broker/herald_register_live_test.go
git commit -m "broker: gated live test — register assertion redeemed by real herald"
```

- [ ] **Step 4: Controller — live smoke + merge.** Provision an agent via cw (as cwadmin), export `CW_IT_*`, run `TestLiveHeraldRegister` against dMon (assert the ack's herald_subject == the agent id). Then PR + wait for nexus CI (required checks) + merge.

---

## Self-review

**Spec coverage:** optional `assertion` on register payloads + `herald_subject` on the ack → Task 1 Step 1; `Custodian` interface + `HeraldEdge` config + construction → Steps 2-3; `wsConn` fields + the `handleRegisterFrame` redeem/bind seam + ack subject → Steps 4-5; `Forget` on cleanup → Step 6; unit tests (binds / no-assertion / bad / custodian-nil / forget-on-close) → Step 7; gated live (real assertion redeemed by real herald) → Task 2. ✔
**Placeholder scan:** the implementer notes flag the two spots needing the real codebase (the exact `schemas.RegisterRequest` required fields + the `KindError` constant) — both self-correcting via the existing `TestRegisterFrameAddsRoster` + `go build`. No TBD.
**Type consistency:** `broker.Custodian` interface (Redeem/Client/Forget) satisfied by `*custodian.Custodian`; `wsConn.heraldClient *client.Client`; `RegisterAckPayload.HeraldSubject`; `RegisterPayload.Assertion`/`ForwardedRegisterPayload.Assertion`. Additive everywhere; opt-in via `HeraldEdge`.
