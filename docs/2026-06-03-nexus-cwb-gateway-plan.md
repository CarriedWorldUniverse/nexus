# nexus CWB gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** nexus becomes the single egress to CWB — a WS↔REST translator for aspects (custodied token injected; aspect holds no token, speaks only WS) plus a thin HTTP reverse-proxy for human CLIs — both forwarding to the interchange edge.

**Architecture:** Invert cwb-client's pillar wrappers onto a `client.Doer` interface so they run over HTTP or a WS transport. nexus's broker gains a `cwb.request`→`cwb.response` handler that replays the call through the 3a-bound custodied `heraldClient`; a small `cwbproxy` reverse-proxy (registered via the broker's `HTTPRegistrar`) forwards human-CLI traffic; one env var `NEXUS_CWB_EDGE` wires both. The aspect runtime gets a `wsDoer` so its tools use the ordinary pillar wrappers over WS.

**Tech Stack:** Go 1.26; cwb-client (`client`/pillar packages); `nexus/frames`, `nexus/broker`, `runtime/agent`, `runtime/wsclient`; `net/http/httputil`.

**Spec:** `docs/2026-06-03-nexus-cwb-gateway-design.md`

**Repos / branches:**
- cwb-client: `/Users/jacinta/Source/cwb-client`, branch `nexus-cwb-gateway` (create).
- nexus: `/Users/jacinta/Source/nexus`, branch `nexus-cwb-gateway` (already created, holds the design doc).

**Cross-repo barrier:** Task 1 (cwb-client) merges before Task 2 pins it into nexus; Tasks 3–8 (nexus) depend on the pin. CI-gated merges (nexus); no `--admin` bypass.

---

## File Structure

**cwb-client:**
- `client/client.go` (modify) — add `Doer` interface.
- `herald/herald.go`, `ledger/ledger.go`, `cairn/cairn.go`, `commonplace/commonplace.go` (modify) — repoint `do(...)` + exported wrappers from `*client.Client` to `client.Doer`.
- `client/doer_test.go` (create) — a fake `Doer` drives a wrapper.

**nexus:**
- `nexus/frames/frames.go` (modify) — `KindCWBRequest`/`KindCWBResponse`.
- `nexus/frames/payloads.go` (modify) — `CWBRequestPayload`/`CWBResponsePayload`.
- `nexus/broker/ws_cwb.go` (create) — `handleCWBRequest`.
- `nexus/broker/ws.go` (modify) — dispatch `case frames.KindCWBRequest`.
- `nexus/broker/ws_cwb_test.go` (create) — broker relay test.
- `nexus/cwb/cwbproxy/cwbproxy.go` (create) — reverse-proxy handler.
- `nexus/cwb/cwbproxy/cwbproxy_test.go` (create).
- `nexus/cmd/nexus/main.go` (modify) — read `NEXUS_CWB_EDGE` → `broker.Config.HeraldEdge` + register `cwbproxy` routes in the `HTTPRegistrar`.
- `runtime/agent/wsdoer.go` (create) — `wsDoer` (`client.Doer` over `a.ws`) + Agent accessor.
- `runtime/agent/wsdoer_test.go` (create).
- `runtime/agent/cwb_gateway_live_test.go` (create) — gated live test.

---

## Task 1: cwb-client — `client.Doer` inversion

**Repo:** `/Users/jacinta/Source/cwb-client` (create branch `nexus-cwb-gateway` first: `git checkout main && git pull && git checkout -b nexus-cwb-gateway`).

**Files:** Modify `client/client.go`, `herald/herald.go`, `ledger/ledger.go`, `cairn/cairn.go`, `commonplace/commonplace.go`; create `client/doer_test.go`.

- [ ] **Step 1: Write the failing test**

Create `client/doer_test.go`:

```go
package client

import (
	"context"
	"net/http"
	"testing"
)

// fakeDoer satisfies Doer without being a *Client — proves the seam is an
// interface, so pillar wrappers can run over a non-HTTP transport.
type fakeDoer struct {
	gotMethod, gotPillar, gotPath string
	status                        int
	body                          []byte
}

func (f *fakeDoer) Do(_ context.Context, method, pillar, path string, _ []byte) (*http.Response, []byte, error) {
	f.gotMethod, f.gotPillar, f.gotPath = method, pillar, path
	return &http.Response{StatusCode: f.status}, f.body, nil
}

func TestClientSatisfiesDoer(t *testing.T) {
	var _ Doer = (*Client)(nil)      // *Client implements Doer
	var _ Doer = (*fakeDoer)(nil)    // and so can a fake
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/cwb-client && go test ./client/ -run Doer`
Expected: FAIL — `undefined: Doer`.

- [ ] **Step 3: Add the `Doer` interface (client/client.go)**

After the `TokenSource` interface block, add:

```go
// Doer executes an authenticated CWB request and returns the raw response.
// *Client is the HTTP implementation; other transports (e.g. a WS relay in
// nexus) implement it so the pillar wrappers are transport-agnostic.
type Doer interface {
	Do(ctx context.Context, method, pillar, path string, body []byte) (*http.Response, []byte, error)
}
```

(`*Client` already has this exact `Do` method, so it satisfies `Doer` with no further change.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/jacinta/Source/cwb-client && go test ./client/ -run Doer`
Expected: PASS.

- [ ] **Step 5: Repoint each pillar package's `do` + wrappers to `client.Doer`**

In `herald/herald.go`, `ledger/ledger.go`, `cairn/cairn.go`, `commonplace/commonplace.go`: change the `do` helper's client param and EVERY exported wrapper's client param from `c *client.Client` to `c client.Doer`. The helper signature becomes:

```go
func do(ctx context.Context, c client.Doer, method, path string, body, out any) error {
```

and each wrapper, e.g.:

```go
func CreateAgent(ctx context.Context, c client.Doer, org string, in CreateAgentInput) (Agent, error) {
```

Mechanical type swap only — the bodies are unchanged (`c.Do(...)` is on the interface). Do this for all exported funcs in all four packages. (`client` is already imported in each.)

- [ ] **Step 6: Add a fake-Doer wrapper test (herald)**

Append to `client/doer_test.go` is not possible (different package); instead add to `herald/herald_test.go`:

```go
func TestWrapperOverFakeDoer(t *testing.T) {
	fd := &fakeHeraldDoer{status: 200, body: []byte(`{"id":"a1","fingerprint":"fp1"}`)}
	a, err := GetAgentByFingerprint(context.Background(), fd, "fp1")
	if err != nil {
		t.Fatalf("GetAgentByFingerprint over fake doer: %v", err)
	}
	if a.ID != "a1" || fd.gotPath != "/api/agents/by-fingerprint/fp1" || fd.gotPillar != "herald" {
		t.Fatalf("agent=%+v doer=%+v", a, fd)
	}
}

type fakeHeraldDoer struct {
	gotPillar, gotPath string
	status             int
	body               []byte
}

func (f *fakeHeraldDoer) Do(_ context.Context, _ , pillar, path string, _ []byte) (*http.Response, []byte, error) {
	f.gotPillar, f.gotPath = pillar, path
	return &http.Response{StatusCode: f.status}, f.body, nil
}
```

Fix the `Do` signature param list to match `Doer` exactly (`method, pillar, path string`); ensure `net/http` + `context` are imported in `herald_test.go`.

- [ ] **Step 7: Run the full cwb-client suite**

Run: `cd /Users/jacinta/Source/cwb-client && go build ./... && go test ./... -count=1`
Expected: PASS — all existing pillar tests still pass (they pass `*client.Client`, which satisfies `Doer`), plus the new fake-Doer tests.

- [ ] **Step 8: Commit**

```bash
cd /Users/jacinta/Source/cwb-client
git add client/ herald/ ledger/ cairn/ commonplace/
git commit -m "feat(client): Doer interface — pillar wrappers transport-agnostic

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Merge cwb-client + pin into nexus

- [ ] **Step 1: Push + PR + merge (no required checks on cwb-client)**

```bash
cd /Users/jacinta/Source/cwb-client
git push -u origin nexus-cwb-gateway
gh pr create --fill --title "client.Doer — transport-agnostic pillar wrappers"
gh pr merge --squash --delete-branch     # cwb-client has no CI gate; merge when MERGEABLE
git checkout main && git pull && git rev-parse --short main   # → <CWBHASH>
```

- [ ] **Step 2: Pin into nexus**

```bash
cd /Users/jacinta/Source/nexus   # already on branch nexus-cwb-gateway
go get github.com/CarriedWorldUniverse/cwb-client@<CWBHASH>
go mod tidy && go build ./... && go test ./runtime/... ./nexus/cwb/... -count=1
git add go.mod go.sum && git commit -m "chore: pin cwb-client (client.Doer)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```
Expected: builds + tests pass (the Doer change is backward-compatible; nexus's custodian passes `*client.Client`).

---

## Task 3: nexus frames — `cwb.request` / `cwb.response`

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:** Modify `nexus/frames/frames.go`, `nexus/frames/payloads.go`.

- [ ] **Step 1: Write the failing test**

Add to `nexus/frames/payloads_test.go` (create if absent, package `frames`):

```go
func TestCWBPayloadsRoundTrip(t *testing.T) {
	req, err := NewRequest(KindCWBRequest, CWBRequestPayload{
		Pillar: "herald", Method: "GET", Path: "/api/me", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var rp CWBRequestPayload
	if err := PayloadAs(req, &rp); err != nil {
		t.Fatal(err)
	}
	if rp.Pillar != "herald" || rp.Method != "GET" || rp.Path != "/api/me" {
		t.Fatalf("rp=%+v", rp)
	}
	resp, err := NewResponse(KindCWBResponse, req.ID, CWBResponsePayload{Status: 200, Body: []byte(`{"id":"a1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var sp CWBResponsePayload
	if err := PayloadAs(resp, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Status != 200 || string(sp.Body) != `{"id":"a1"}` {
		t.Fatalf("sp=%+v", sp)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/frames/ -run CWB`
Expected: FAIL — `undefined: KindCWBRequest` / `CWBRequestPayload`.

- [ ] **Step 3: Add the kinds (frames.go)**

In the `const (... Kind = ...)` block (near `KindDispatch`/`KindDispatchResult`), add:

```go
	KindCWBRequest  Kind = "cwb.request"
	KindCWBResponse Kind = "cwb.response"
```

- [ ] **Step 4: Add the payloads (payloads.go)**

```go
// CWBRequestPayload is an aspect's CWB API call relayed over the WS. The broker
// executes it through the connection's custodied herald client (token injected)
// and replies with CWBResponsePayload. Pillar+path (not a URL) so the broker
// pins the destination host to the CWB edge.
type CWBRequestPayload struct {
	Pillar string `json:"pillar"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   []byte `json:"body,omitempty"` // raw JSON request body
}

// CWBResponsePayload is the relayed CWB response (status + raw body); the
// aspect's cwb-client wrapper maps non-2xx to an error as usual.
type CWBResponsePayload struct {
	Status int    `json:"status"`
	Body   []byte `json:"body,omitempty"`
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/frames/ -run CWB`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add nexus/frames/frames.go nexus/frames/payloads.go nexus/frames/payloads_test.go
git commit -m "feat(frames): cwb.request / cwb.response payloads

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: nexus broker — `handleCWBRequest`

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:** Create `nexus/broker/ws_cwb.go`; modify `nexus/broker/ws.go`; create `nexus/broker/ws_cwb_test.go`.

Grounding: `wsConn` has `heraldClient *client.Client` (bound in `handleRegisterFrame`, 3a). Responses are sent with `frames.NewResponse(kind, env.ID, payload)` + `c.send(env)`; errors with `c.respondError(env, msg)`. The broker context is `c.broker.ctx`. The frame dispatch `switch env.Kind` is at `ws.go:440`. The broker test harness (`newTestServer`, `dialWS`, `sendFrame`, `recvFrame`, `registerWith`, `fakeCustodian`) is in `nexus/broker/ws_test.go` (from 3a).

- [ ] **Step 1: Write the failing test**

Add to `nexus/broker/ws_cwb_test.go`. Model on `TestRegisterHeraldAssertionBinds`. The fakeCustodian must return a `heraldClient` pointing at a stub CWB server:

```go
func TestCWBRequestRelays(t *testing.T) {
	// Stub CWB edge: echoes identity for GET /herald/api/me with the bearer.
	var gotAuth, gotPath string
	cwb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"agent-x"}`))
	}))
	defer cwb.Close()

	srv, _, b := newTestServer(t)
	b.custodian = &fakeCustodian{
		redeem: func(string) (string, error) { return "agent-x", nil },
		client: func(string) (*client.Client, error) {
			return client.WithStaticToken(cwb.URL, "tok-agent-x"), nil
		},
	}

	conn := dialWS(t, srv, "optok")
	defer conn.Close(websocket.StatusNormalClosure, "")
	registerWith(t, conn, "aspect-a", "assertion-blob") // binds heraldClient via fakeCustodian

	req, _ := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{
		Pillar: "herald", Method: "GET", Path: "/api/me",
	})
	sendFrame(t, conn, req)
	resp := recvFrame(t, conn, frames.KindCWBResponse)

	var p frames.CWBResponsePayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 200 || string(p.Body) != `{"id":"agent-x"}` {
		t.Fatalf("resp=%+v", p)
	}
	if gotPath != "/herald/api/me" || gotAuth != "Bearer tok-agent-x" {
		t.Fatalf("stub saw path=%q auth=%q", gotPath, gotAuth)
	}
}

func TestCWBRequestUnboundErrors(t *testing.T) {
	srv, _, _ := newTestServer(t) // no custodian → register won't bind heraldClient
	conn := dialWS(t, srv, "optok")
	defer conn.Close(websocket.StatusNormalClosure, "")
	registerWith(t, conn, "aspect-a", "") // no assertion → heraldClient stays nil

	req, _ := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{Pillar: "herald", Method: "GET", Path: "/api/me"})
	sendFrame(t, conn, req)
	resp := recvFrame(t, conn, frames.Kind("cwb.request.error"))
	// recvFrame should accept the error kind; assert it is an error frame.
	if resp.Kind != "cwb.request.error" {
		t.Fatalf("want cwb.request.error, got %s", resp.Kind)
	}
}
```

Read `ws_test.go` first and match the EXACT signatures of `newTestServer`, `dialWS`, `sendFrame`, `recvFrame`, `registerWith`, and `fakeCustodian` (field names `redeem`/`client` are illustrative — use the real ones; if `fakeCustodian` lacks a `client` hook, extend it additively). If `recvFrame` asserts a specific kind, adapt the unbound case to read whatever it returns and assert the `.error` kind.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run CWBRequest`
Expected: FAIL — `handleCWBRequest` not wired / unknown kind.

- [ ] **Step 3: Implement the handler (ws_cwb.go)**

Create `nexus/broker/ws_cwb.go`:

```go
package broker

import "github.com/CarriedWorldUniverse/nexus/nexus/frames"

// handleCWBRequest relays an aspect's CWB API call through the connection's
// custodied herald client (token injected) and returns the response. Requires
// a herald-bound connection; the aspect acts as its own org identity and CWB
// enforces authz. The frame carries pillar+path (not a URL), so the broker
// pins the destination host to the configured CWB edge.
func (c *wsConn) handleCWBRequest(env frames.Envelope) {
	if c.heraldClient == nil {
		c.respondError(env, "cwb.request requires a herald-bound connection")
		return
	}
	var p frames.CWBRequestPayload
	if err := frames.PayloadAs(env, &p); err != nil {
		c.respondError(env, "cwb.request: bad payload: "+err.Error())
		return
	}
	if p.Pillar == "" || p.Method == "" || p.Path == "" {
		c.respondError(env, "cwb.request: pillar, method, path required")
		return
	}
	resp, raw, err := c.heraldClient.Do(c.broker.ctx, p.Method, p.Pillar, p.Path, p.Body)
	if err != nil {
		c.respondError(env, "cwb relay: "+err.Error())
		return
	}
	out, nerr := frames.NewResponse(frames.KindCWBResponse, env.ID, frames.CWBResponsePayload{
		Status: resp.StatusCode,
		Body:   raw,
	})
	if nerr != nil {
		c.respondError(env, "cwb.request: build response: "+nerr.Error())
		return
	}
	c.send(out)
}
```

- [ ] **Step 4: Wire the dispatch case (ws.go)**

In the `switch env.Kind {` at `ws.go:440`, add a case alongside the others:

```go
	case frames.KindCWBRequest:
		c.handleCWBRequest(env)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run CWBRequest -count=1`
Expected: PASS. Then the full broker suite: `go test ./nexus/broker/ -count=1`.

- [ ] **Step 6: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add nexus/broker/ws_cwb.go nexus/broker/ws.go nexus/broker/ws_cwb_test.go
git commit -m "feat(broker): cwb.request relay via custodied herald client

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: nexus `cwbproxy` — HTTP reverse-proxy handler

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:** Create `nexus/cwb/cwbproxy/cwbproxy.go`, `nexus/cwb/cwbproxy/cwbproxy_test.go`.

- [ ] **Step 1: Write the failing test**

Create `nexus/cwb/cwbproxy/cwbproxy_test.go`:

```go
package cwbproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReverseProxyForwardsVerbatim(t *testing.T) {
	var gotPath, gotAuth string
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer edge.Close()

	mux := http.NewServeMux()
	if err := Register(mux, edge.URL); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/herald/api/me", nil)
	req.Header.Set("Authorization", "Bearer human-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" || gotPath != "/herald/api/me" || gotAuth != "Bearer human-tok" {
		t.Fatalf("body=%q path=%q auth=%q", body, gotPath, gotAuth)
	}
}

func TestRegisterEmptyEdge(t *testing.T) {
	if err := Register(http.NewServeMux(), ""); err == nil {
		t.Fatal("empty edge should error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/cwb/cwbproxy/`
Expected: FAIL — package/`Register` undefined.

- [ ] **Step 3: Implement (cwbproxy.go)**

```go
// Package cwbproxy reverse-proxies CWB path prefixes to the CWB edge
// (interchange) over nexus's egress, pass-through: the caller's own bearer is
// forwarded verbatim and interchange authenticates. Serves human CLIs (cw /
// cw agent enroll) and the aspect bootstrap OIDC discovery. The host is pinned
// to the edge (callers cannot retarget).
package cwbproxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Prefixes proxied to the CWB edge.
var Prefixes = []string{"/herald/", "/cairn/", "/ledger/", "/knowledge/"}

// Register attaches the reverse-proxy routes for the CWB edge onto mux.
func Register(mux *http.ServeMux, edge string) error {
	edge = strings.TrimRight(edge, "/")
	if edge == "" {
		return fmt.Errorf("cwbproxy: empty CWB edge")
	}
	target, err := url.Parse(edge)
	if err != nil {
		return fmt.Errorf("cwbproxy: parse edge %q: %w", edge, err)
	}
	rp := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.Host = target.Host
			// path preserved as-is (already includes the /pillar prefix)
		},
	}
	for _, p := range Prefixes {
		mux.Handle(p, rp)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/cwb/cwbproxy/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add nexus/cwb/cwbproxy/
git commit -m "feat(cwbproxy): pass-through reverse-proxy to the CWB edge

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: nexus cmd — wire `NEXUS_CWB_EDGE`

**Repo:** `/Users/jacinta/Source/nexus`. **File:** `nexus/cmd/nexus/main.go`. Config wiring (no new unit test; covered by Tasks 4/5 + the live test).

Grounding: `broker.New(broker.Config{...})` is built at `main.go:516`; `broker.Config.HeraldEdge` constructs the custodian; `HTTPRegistrar func(*http.ServeMux)` is set in the same `Config` literal (around `main.go:585`) and runs inside `ListenAndServe`.

- [ ] **Step 1: Read the env + set HeraldEdge**

Near the other `os.Getenv("NEXUS_*")` reads (before `broker.New`), add:

```go
	cwbEdge := os.Getenv("NEXUS_CWB_EDGE")
```

In the `broker.Config{...}` literal, set:

```go
		HeraldEdge: cwbEdge,
```

- [ ] **Step 2: Register the reverse-proxy in HTTPRegistrar**

Add the import `"github.com/CarriedWorldUniverse/nexus/nexus/cwb/cwbproxy"`. Inside the existing `HTTPRegistrar: func(mux *http.ServeMux) { ... }` body (main.go ~585), append:

```go
			if cwbEdge != "" {
				if err := cwbproxy.Register(mux, cwbEdge); err != nil {
					log.Error("cwb reverse-proxy", "err", err)
					os.Exit(1)
				}
				log.Info("CWB reverse-proxy enabled", "edge", cwbEdge)
			}
```

Match the logger variable name in scope (`log`/`logger`) and the existing closure's capture style. If `HTTPRegistrar` is assigned outside the `Config` literal, add the block there instead — the requirement is that it runs with the same mux the broker serves.

- [ ] **Step 3: Build + vet**

Run: `cd /Users/jacinta/Source/nexus && go build ./... && go vet ./nexus/cmd/nexus/ && go test ./nexus/... -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add nexus/cmd/nexus/main.go
git commit -m "feat(nexus): wire NEXUS_CWB_EDGE — custodian + reverse-proxy

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: nexus runtime — `wsDoer`

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:** Create `runtime/agent/wsdoer.go`, `runtime/agent/wsdoer_test.go`.

Grounding: `wsclient.Client.Request(ctx, env) (frames.Envelope, error)` is the correlated round-trip; the Agent holds `a.ws *wsclient.Client`. `client.Doer` (cwb-client) is the interface to satisfy.

- [ ] **Step 1: Write the failing test**

Create `runtime/agent/wsdoer_test.go`:

```go
package agent

import (
	"context"
	"net/http"
	"testing"

	"github.com/CarriedWorldUniverse/cwb-client/herald"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

func TestWSDoerRelaysAndDecodes(t *testing.T) {
	// fakeNexus that answers a cwb.request with a cwb.response carrying an agent.
	var gotPillar, gotPath, gotMethod string
	nx := newFakeNexus(t, "tok")
	nx.onCWB = func(p frames.CWBRequestPayload) frames.CWBResponsePayload {
		gotPillar, gotPath, gotMethod = p.Pillar, p.Path, p.Method
		return frames.CWBResponsePayload{Status: 200, Body: []byte(`{"id":"a1","fingerprint":"fp1"}`)}
	}
	a := newAgent(t, nx.URL(), "tok", &mockProvider{reply: "ok"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()
	waitRegistered(t, nx)

	d := a.CWBDoer()
	ag, err := herald.GetAgentByFingerprint(context.Background(), d, "fp1")
	if err != nil {
		t.Fatalf("over wsDoer: %v", err)
	}
	if ag.ID != "a1" || gotPillar != "herald" || gotPath != "/api/agents/by-fingerprint/fp1" || gotMethod != http.MethodGet {
		t.Fatalf("ag=%+v pillar=%q path=%q method=%q", ag, gotPillar, gotPath, gotMethod)
	}
}
```

This needs `fakeNexus` to handle `cwb.request`. In `agent_test.go`'s `serveLoop`, add a `case frames.KindCWBRequest:` that, if `f.onCWB != nil`, decodes the payload, calls it, and replies with a `cwb.response`; add an `onCWB func(frames.CWBRequestPayload) frames.CWBResponsePayload` field to `fakeNexus`. Also add a `waitRegistered(t, nx)` helper if not present (poll `nx.registers.Load() > 0`), or inline the existing wait loop.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/agent/ -run WSDoer`
Expected: FAIL — `a.CWBDoer` / `nx.onCWB` undefined.

- [ ] **Step 3: Implement (wsdoer.go)**

```go
package agent

import (
	"context"
	"fmt"
	"net/http"

	cwbclient "github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// wsDoer is a cwb-client client.Doer that relays CWB API calls over the
// aspect's WS to nexus (which executes them as the aspect via its custodied
// herald token). The aspect thus holds no CWB bearer and makes no HTTP call —
// its pillar wrappers run over this transport.
type wsDoer struct{ a *Agent }

func (d *wsDoer) Do(ctx context.Context, method, pillar, path string, body []byte) (*http.Response, []byte, error) {
	req, err := frames.NewRequest(frames.KindCWBRequest, frames.CWBRequestPayload{
		Pillar: pillar, Method: method, Path: path, Body: body,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("wsDoer: build request: %w", err)
	}
	resp, err := d.a.ws.Request(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("wsDoer: relay: %w", err)
	}
	if resp.Kind == frames.Kind("cwb.request.error") {
		var e map[string]string
		_ = frames.PayloadAs(resp, &e)
		return nil, nil, fmt.Errorf("wsDoer: nexus error: %s", e["error"])
	}
	var p frames.CWBResponsePayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		return nil, nil, fmt.Errorf("wsDoer: bad response: %w", err)
	}
	return &http.Response{StatusCode: p.Status}, p.Body, nil
}

// CWBDoer returns a client.Doer that routes the aspect's CWB calls over the WS.
func (a *Agent) CWBDoer() cwbclient.Doer { return &wsDoer{a: a} }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/agent/ -run WSDoer -count=1`
Expected: PASS. Then `go test ./runtime/agent/ -count=1` (whole suite still green).

- [ ] **Step 5: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add runtime/agent/wsdoer.go runtime/agent/wsdoer_test.go runtime/agent/agent_test.go
git commit -m "feat(runtime): wsDoer — aspect CWB calls relayed over WS

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: Gated live test (both surfaces)

**Repo:** `/Users/jacinta/Source/nexus`. **File:** Create `runtime/agent/cwb_gateway_live_test.go`.

Proves the WS surface end to end against dMon: a herald-bound aspect WS sends a `cwb.request` and nexus relays it to herald *as the agent*. Reuse the env-var convention from `nexus/broker/herald_register_live_test.go` (`CW_IT_EDGE`, `CW_IT_OWNER_SEED`, `CW_IT_AGENT_ID`, `CW_IT_AGENT_SLUG`) and its broker-bring-up pattern. Skips offline.

- [ ] **Step 1: Write the gated live test**

Create `runtime/agent/cwb_gateway_live_test.go`. Read `nexus/broker/herald_register_live_test.go` to copy the exact broker construction (a broker with `HeraldEdge=CW_IT_EDGE`, the real custodian, a real assertion in register so `heraldClient` binds). Then, instead of asserting only the ack, send a `cwb.request` and assert the relayed identity:

```go
//go:build !skip_live

package agent

// Gated live: requires CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID +
// CW_IT_AGENT_SLUG (a provisioned agent). Brings up a HeraldEdge broker,
// connects a herald-bound aspect WS, sends cwb.request GET /herald /api/me,
// and asserts the relayed response carries the agent's own id — nexus relayed
// the call to herald AS the agent, end to end.
//
// (Implementation mirrors broker/herald_register_live_test.go's setup; this
// file lives in runtime/agent only if it can reuse an exported broker test
// harness — otherwise add the cwb.request assertion directly in
// broker/herald_register_live_test.go as a second test. The reviewer/
// implementer picks whichever keeps the harness in one package.)
```

Because the broker bring-up harness lives in `package broker` (unexported), the cleanest home is to ADD a `TestLiveCWBRequestRelay` to `nexus/broker/herald_register_live_test.go` (same package, reuses its setup): after binding `heraldClient` via the live assertion, directly invoke the relay path — construct a `cwb.request` envelope and call the bound conn's `handleCWBRequest`, or drive it over the dialed WS — and assert the `cwb.response` body's `id` == `CW_IT_AGENT_ID`. Implement it there if reuse in `runtime/agent` would require exporting harness internals.

- [ ] **Step 2: Verify it skips offline**

Run: `cd /Users/jacinta/Source/nexus && go test ./nexus/broker/ -run TestLiveCWBRequestRelay -v -count=1`
Expected: SKIP (env unset).

- [ ] **Step 3: Provision a test agent + run live (controller does this)**

The controller provisions a throwaway `cwb-test-*` agent through the interchange edge (CreateOrg/CreateHuman/CreateAgent via the cwadmin platform-admin bearer over `POST <edge>/herald/api/orgs[...]`), then:

```bash
cd /Users/jacinta/Source/nexus
CW_IT_EDGE=<dMon edge> CW_IT_OWNER_SEED=<seed> CW_IT_AGENT_ID=<uuid> CW_IT_AGENT_SLUG=<slug> \
  go test ./nexus/broker/ -run TestLiveCWBRequestRelay -v -count=1
```
Expected: PASS — relayed `/api/me` body `id` == the agent id.

- [ ] **Step 4: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add nexus/broker/herald_register_live_test.go   # (or runtime/agent/cwb_gateway_live_test.go)
git commit -m "test: gated live cwb.request relay e2e

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: PRs + CI-gated merge

- [ ] **Step 1: nexus PR**

```bash
cd /Users/jacinta/Source/nexus && git push -u origin nexus-cwb-gateway
gh pr create --fill --title "nexus CWB gateway: WS↔REST translator + reverse-proxy"
gh pr checks --watch && gh pr merge --squash --delete-branch
```
Expected: 3 CI jobs (macos/ubuntu/windows) green, then merged; no `--admin` bypass.

(cwb-client merged in Task 2. cw needs no change — the Doer inversion is backward-compatible; cw still passes `*client.Client`.)

---

## Self-Review

**Spec coverage:**
- Component 1 (`client.Doer` inversion) → Task 1. ✓
- Component 2 (WS translator: frames, broker handler, runtime wsDoer) → Tasks 3, 4, 7. ✓
- Component 3 (HTTP reverse-proxy) → Task 5 (+ wiring Task 6). ✓
- Component 4 (`NEXUS_CWB_EDGE` → custodian + proxy) → Task 6. ✓
- Security (bound-only, host-pinned, pass-through) → Task 4 (unbound errors; pillar+path), Task 5 (Director pins host). ✓
- Live proof (WS surface) → Task 8. ✓
- Cross-repo pin + CI-gated merge → Tasks 2, 9. ✓

**Type consistency:** `client.Doer.Do(ctx, method, pillar, path string, body []byte) (*http.Response, []byte, error)` identical in Task 1 (def), Task 7 (wsDoer impl), Task 4 (broker calls the concrete `*client.Client.Do`). `CWBRequestPayload{Pillar,Method,Path,Body}` / `CWBResponsePayload{Status,Body}` identical across Tasks 3, 4, 7. `KindCWBRequest`/`KindCWBResponse` + the `cwb.request.error` kind (from `respondError`'s `<kind>.error`) consistent across Tasks 4, 7. `cwbproxy.Register(mux, edge)` defined Task 5, called Task 6.

**Placeholder scan:** none — every code step is concrete. `<CWBHASH>`, `<dMon edge>`, etc. are runtime substitutions. Task 8 explicitly notes the harness-location decision (broker package) rather than leaving it vague.

**Scope:** one feature, cwb-client→pin→nexus barrier as in the prior cycle; appropriate for a single plan.
