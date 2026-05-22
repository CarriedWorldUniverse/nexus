# agentfunnel Connection Stability — Implementation Plan

> **For agentic workers:** Execute task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. TDD throughout — failing test, then minimal code, then verify.

**Goal:** Eliminate two root causes of "agentfunnel disconnects and doesn't maintain connections": (A) client read deadline so dead peers are detected within 45s instead of hours, (B) register-on-Events so reconnects re-handshake cleanly without racing chat sends.

**Spec:** `docs/2026-05-23-agentfunnel-connection-stability-spec.md`

**Tech:** Go, `github.com/coder/websocket`, no new deps.

---

## File Structure

- Modify: `runtime/wsclient/wsclient.go` — add `Config.ReadIdleTimeout`, change `readLoop` to use a per-read deadline
- Modify: `runtime/wsclient/wsclient_test.go` — add silent-peer test + healthy-ping test
- Modify: `runtime/aspect/wsasp/wsasp.go` — replace `registerOnReady` polling with `Events()` subscription; add a register barrier consumed by `drainPendingLoop`
- Modify: `runtime/aspect/wsasp/wsasp_test.go` — add reconnect-ordering test

No new files; this is two surgical changes.

---

## Task 1 — Fix A: client read deadline

**Files:**
- Modify: `runtime/wsclient/wsclient.go` (Config struct, New, readLoop)
- Test: `runtime/wsclient/wsclient_test.go`

### Step 1.1 — Add failing test: silent peer triggers reconnect

- [ ] Append to `runtime/wsclient/wsclient_test.go`:

```go
// silentServer accepts the WS upgrade then stops responding — no
// pings, no close. Simulates broker death with half-open socket.
func silentServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		// Hold the connection open, never read, never write.
		<-r.Context().Done()
		_ = wsc.Close(websocket.StatusNormalClosure, "shutdown")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReadIdleTimeoutReconnects(t *testing.T) {
	srv := silentServer(t, "tok")

	var reconnects atomic.Int32
	c, err := New(Config{
		URL:               wsURL(srv),
		AuthToken:         "tok",
		Handler:           HandlerFunc(func(frames.Envelope) {}),
		ReadIdleTimeout:   200 * time.Millisecond,
		MinReconnectDelay: 10 * time.Millisecond,
		MaxReconnectDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for ev := range c.Events() {
			if ev.Connected {
				reconnects.Add(1)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Run(ctx)

	// Within 2s with a 200ms idle timeout, we should have multiple
	// connect cycles. >=2 means at least one reconnect happened.
	if got := reconnects.Load(); got < 2 {
		t.Fatalf("expected >=2 connect events from idle-timeout reconnects, got %d", got)
	}
}
```

- [ ] Run: `go test ./runtime/wsclient/ -run TestReadIdleTimeoutReconnects -race -timeout 30s`
- [ ] Expected: FAIL — `Config{}.ReadIdleTimeout` doesn't compile, or test hangs because no timeout exists.

### Step 1.2 — Add the config field + plumb through readLoop

- [ ] Edit `runtime/wsclient/wsclient.go` Config struct (after `MaxReconnectDelay`):

```go
	// ReadIdleTimeout is the per-read deadline applied inside readLoop.
	// If no frame arrives within this window the read returns a timeout
	// error, readLoop returns, and Run reconnects. Default 45s — server
	// pings every 30s (broker/ws.go), so 45s gives 1.5x jitter headroom.
	// 0 disables (back-compat / disable for tests that explicitly want
	// the unbounded behaviour).
	ReadIdleTimeout time.Duration
```

- [ ] In `New`, default it (right after the MaxReconnectDelay default at line 138-140):

```go
	if cfg.ReadIdleTimeout == 0 {
		cfg.ReadIdleTimeout = 45 * time.Second
	}
```

- [ ] Replace `readLoop`'s `conn.Read` call (currently line 269) so each read carries a deadline:

```go
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, c.cfg.ReadIdleTimeout)
		msgType, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		// ... rest unchanged
```

(Keep the rest of `readLoop` exactly as-is — only the Read call and its ctx change.)

- [ ] Run: `go test ./runtime/wsclient/ -run TestReadIdleTimeoutReconnects -race -timeout 30s`
- [ ] Expected: PASS.

### Step 1.3 — Add healthy-ping survival test

- [ ] Append to `wsclient_test.go`:

```go
// pingingServer accepts a WS and sends a text frame every interval.
// Simulates the broker's keepalive: while frames flow, the client
// must not reconnect.
func pingingServer(t *testing.T, token string, interval time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tk.C:
				env, _ := frames.New(frames.KindRegister, nil)
				raw, _ := frames.Encode(env)
				if err := wsc.Write(r.Context(), websocket.MessageText, raw); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReadIdleTimeoutDoesNotReconnectWhenHealthy(t *testing.T) {
	srv := pingingServer(t, "tok", 50*time.Millisecond)

	var connects atomic.Int32
	c, err := New(Config{
		URL:             wsURL(srv),
		AuthToken:       "tok",
		Handler:         HandlerFunc(func(frames.Envelope) {}),
		ReadIdleTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for ev := range c.Events() {
			if ev.Connected {
				connects.Add(1)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = c.Run(ctx)

	if got := connects.Load(); got != 1 {
		t.Fatalf("expected exactly 1 connect event (no reconnects on healthy line), got %d", got)
	}
}
```

- [ ] Run: `go test ./runtime/wsclient/ -race -timeout 30s`
- [ ] Expected: both new tests PASS, full suite green.

### Step 1.4 — Commit

- [ ] Commit:

```bash
git add runtime/wsclient/wsclient.go runtime/wsclient/wsclient_test.go
git commit -m "wsclient: per-read deadline to detect dead peers within 45s

Without a deadline conn.Read blocks for hours when the broker dies
without closing the socket (nginx idle close, container OOM, NAT
drop). Server pings every 30s, so 45s gives 1.5x jitter headroom.
Triggers existing reconnect path. ReadIdleTimeout=0 preserves old
behaviour for tests that want it."
```

---

## Task 2 — Fix B: register on Events, drain after register

**Files:**
- Modify: `runtime/aspect/wsasp/wsasp.go` (Run, registerOnReady → reconnectLoop, drainPendingLoop)
- Test: `runtime/aspect/wsasp/wsasp_test.go`

### Step 2.1 — Add failing test: register precedes drained sends

- [ ] Append to `runtime/aspect/wsasp/wsasp_test.go`:

```go
func TestRegisterPrecedesDrainedSendsAfterReconnect(t *testing.T) {
	// Recorder for the order of inbound frames on the server side.
	var (
		mu    sync.Mutex
		order []frames.Kind
	)
	record := func(env frames.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, env.Kind)
	}

	// Server that records every frame it receives and stays up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		for {
			_, data, err := wsc.Read(r.Context())
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil {
				continue
			}
			record(env)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(Config{
		URL:        "ws" + strings.TrimPrefix(srv.URL, "http"),
		AspectName: "test",
		OnDeliver:  func(DeliveredMessage) {},
		Register:   schemas.RegisterRequest{Aspect: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Queue three chat sends BEFORE Run starts → they go into
	// pending immediately (ws not connected yet).
	bg := context.Background()
	_, _ = c.SendChat(bg, "first",  0, "")
	_, _ = c.SendChat(bg, "second", 0, "")
	_, _ = c.SendChat(bg, "third",  0, "")

	ctx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	go c.Run(ctx)

	// Wait for the server to see 4 frames (register + 3 chats).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 4 {
		t.Fatalf("server only received %d frames: %v", len(order), order)
	}
	if order[0] != frames.KindRegister {
		t.Fatalf("first frame should be register, got %v; full order: %v", order[0], order)
	}
	for i := 1; i < 4; i++ {
		if order[i] != frames.KindChatSend {
			t.Fatalf("frame %d should be chat.send, got %v", i, order[i])
		}
	}
}
```

- [ ] Add imports if missing: `"net/http"`, `"net/http/httptest"`, `"strings"`, `"sync"`, `"github.com/coder/websocket"`, `"github.com/CarriedWorldUniverse/nexus/shared/schemas"`.
- [ ] Run: `go test ./runtime/aspect/wsasp/ -run TestRegisterPrecedesDrainedSendsAfterReconnect -race -timeout 30s -count 20`
- [ ] Expected: FAIL intermittently — current code races register against drain. `-count 20` exposes the race.

### Step 2.2 — Replace polling with Events subscription + register barrier

- [ ] In `runtime/aspect/wsasp/wsasp.go`, replace `registerOnReady` (lines 142-164) and modify `Run` (lines 133-140) and `drainPendingLoop` (lines 186-213):

```go
// Run drives the WS connection lifecycle. Blocks until ctx done.
// On each (re)connect a register frame is sent before any buffered
// chat sends are flushed — the broker must see register first or
// it can't attribute follow-up frames to this aspect on this conn.
func (c *Client) Run(ctx context.Context) error {
	registered := make(chan struct{})   // closed after register succeeds
	c.setRegisteredBarrier(registered)

	go c.handleConnectEvents(ctx)
	go c.drainPendingLoop(ctx)
	return c.ws.Run(ctx)
}

// handleConnectEvents subscribes to wsclient connect/disconnect
// transitions. On connect: send register, then close the barrier
// so drainPendingLoop is allowed to flush. On disconnect: install
// a fresh barrier so the next connect cycle re-gates the drain.
func (c *Client) handleConnectEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-c.ws.Events():
			if !ok {
				return
			}
			if ev.Connected {
				c.sendRegister(ctx)
				close(c.registeredBarrier())
			} else {
				c.setRegisteredBarrier(make(chan struct{}))
			}
		}
	}
}
```

- [ ] Add the barrier fields and helpers to the Client struct + a new section just above `Run`:

```go
// (in the Client struct, alongside cursor and pending)
	registered chan struct{} // closed when register has been sent on the current connection

// setRegisteredBarrier swaps in a fresh barrier under c.mu.
func (c *Client) setRegisteredBarrier(ch chan struct{}) {
	c.mu.Lock()
	c.registered = ch
	c.mu.Unlock()
}

// registeredBarrier returns the current barrier under c.mu.
func (c *Client) registeredBarrier() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registered
}
```

- [ ] Replace `drainPendingLoop` so it waits on the barrier each cycle:

```go
// drainPendingLoop flushes the outbound buffer once register has
// completed on the current connection. On every (re)connect cycle
// it re-reads the barrier and waits for it to close before draining.
func (c *Client) drainPendingLoop(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !c.ws.Connected() {
				continue
			}
			barrier := c.registeredBarrier()
			select {
			case <-ctx.Done():
				return
			case <-barrier:
				// Register has been sent on this connection.
			}

			c.mu.Lock()
			pending := c.pending
			c.pending = nil
			c.mu.Unlock()
			for _, env := range pending {
				if err := c.ws.Send(ctx, env); err != nil {
					c.mu.Lock()
					c.pending = append([]frames.Envelope{env}, c.pending...)
					c.mu.Unlock()
					return
				}
			}
		}
	}
}
```

- [ ] Delete the old `registerOnReady` function entirely.
- [ ] Run: `go test ./runtime/aspect/wsasp/ -race -timeout 30s -count 20`
- [ ] Expected: all tests PASS, including the new ordering test 20×.

### Step 2.3 — Sanity check the full nexus build

- [ ] Run: `cd ~/Source/nexus && go build ./... && go test -race -timeout 60s ./runtime/...`
- [ ] Expected: build clean, runtime test suite green.

### Step 2.4 — Commit

- [ ] Commit:

```bash
git add runtime/aspect/wsasp/wsasp.go runtime/aspect/wsasp/wsasp_test.go
git commit -m "wsasp: register on ConnectEvent, gate drain on register

registerOnReady polled at 250ms and drainPendingLoop polled at
500ms — buffered chat.send could ship before register on reconnect,
so the broker had no aspect identity for the new connection.
Switch register to react to wsclient.Events(), and gate the drain
loop on a per-connection barrier that closes only after register
sent. The Events channel exists for exactly this; the old polling
shape's TODO ('F2.6 may rev wsclient to add a connect callback')
is now resolved."
```

---

## Self-review checklist

- [ ] No `TODO` / `TBD` / placeholders left in either file.
- [ ] All new code uses existing types — `wsclient.ConnectEvent`, `frames.Envelope`, no new abstractions.
- [ ] `ReadIdleTimeout=0` path still works (existing tests don't set it; they use the new 45s default — which is fine because they ping or close quickly).
- [ ] No protocol change; broker is untouched.
- [ ] Spec sections "Behaviour after the fix" and "Testing strategy" each have at least one corresponding step above.
