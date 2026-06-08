# nexus token custodian Implementation Plan (bootstrap step 2)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Single nexus cycle, 2 tasks (package + unit tests, then the gated live test). Controller does the live smoke + merge.

**Goal:** Add `nexus/cwb/custodian` — redeem a casket assertion at herald, hold the per-aspect token, hand out a `cwb-client` authed AS the aspect (refreshing via `refresh_token`).

**Architecture:** First nexus package to import `github.com/CarriedWorldUniverse/cwb-client`. In-memory map `subject→entry`; per-entry refresh serialization; herald I/O outside the map lock. Spec: `nexus/docs/2026-06-03-nexus-token-custodian-design.md`.

**Tech:** Go 1.26 (bump from nexus's current 1.25.5 — cwb-client requires it). cwb-client pin: `1db44b1` (main).

## Verified facts

- cwb-client: `oidc.New(edge)`, `(*oidc.Client).JWTBearerGrant(ctx, assertion) (Token, error)`, `RefreshGrant(ctx, refreshToken) (Token, error)`, `TokenEndpoint(ctx) (string, error)`; `Token{AccessToken, RefreshToken, TokenType, ExpiresIn}`. `client.New(edge, TokenSource)`, `TokenSource{Token(ctx),Refresh(ctx)}`, `client.ErrReauth`. `identity.DecodeAccessClaims(token) (map[string]any, error)`, `identity.AgentAssertion(seed, slug, agentID, tokenURL) (string, error)`.
- herald jwt-bearer returns `refresh_token`+`expires_in`; refresh_token grant works for agent tokens; access `sub` = agent UUID.
- nexus module `github.com/CarriedWorldUniverse/nexus`; concurrency idiom = `sync` mutex guarding an in-memory map, lock released before I/O (`nexus/nexus/outpost/outpost.go`).

---

## Task 1: `nexus/cwb/custodian` package + unit tests

**Files:** `go.mod`/`go.sum` (add cwb-client + bump go), `nexus/cwb/custodian/custodian.go`, `nexus/cwb/custodian/custodian_test.go`

- [ ] **Step 1: Add the dep + bump go** — `cd /Users/jacinta/Source/nexus && go get github.com/CarriedWorldUniverse/cwb-client@1db44b1 && go mod tidy`. If the `go` directive is still `1.25.x`, set it to `go 1.26` in `go.mod` (cwb-client requires it; `go get` usually bumps it automatically — verify with `go build ./...`).

- [ ] **Step 2: Write the failing unit test** — `nexus/cwb/custodian/custodian_test.go`:

```go
package custodian

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
)

func jwt(sub string) string { // a fake JWT whose payload carries sub (DecodeAccessClaims reads it)
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `","kind":"agent","exp":9999999999}`))
	return "x." + p + ".y"
}

// stubHerald serves discovery + /herald/token (jwt-bearer + refresh_token).
func stubHerald(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	refreshes := 0
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /herald/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token_endpoint":"` + srv.URL + `/herald/token","jwks_uri":"` + srv.URL + `/herald/jwks","revocation_endpoint":"` + srv.URL + `/herald/revoke"}`))
	})
	mux.HandleFunc("POST /herald/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:jwt-bearer":
			if r.Form.Get("assertion") == "" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + jwt("agent-1") + `","token_type":"Bearer","expires_in":600,"refresh_token":"r1"}`))
		case "refresh_token":
			refreshes++
			_, _ = w.Write([]byte(`{"access_token":"` + jwt("agent-1") + `","token_type":"Bearer","expires_in":600,"refresh_token":"r2"}`))
		default:
			w.WriteHeader(400)
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &refreshes
}

func TestCustodianRedeemAndClient(t *testing.T) {
	srv, refreshes := stubHerald(t)
	c := New(srv.URL)
	ctx := context.Background()

	tu := srv.URL + "/herald/token"
	assertion, err := identity.AgentAssertion([]byte("test-owner-seed"), "shadow", "agent-1", tu)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := c.Redeem(ctx, assertion)
	if err != nil || sub != "agent-1" {
		t.Fatalf("Redeem: %v sub=%q", err, sub)
	}

	// Client(sub) yields a usable client; its source returns the custodied token.
	s := &source{cust: c, subject: sub}
	got, err := s.Token(ctx)
	if err != nil || !strings.HasPrefix(got, "x.") {
		t.Fatalf("Token: %v %q", err, got)
	}
	if *refreshes != 0 {
		t.Fatalf("fresh token should not refresh; refreshes=%d", *refreshes)
	}

	// Expire the entry → next Token triggers a refresh_token grant.
	c.mu.Lock()
	c.by[sub].exp = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	if _, err := s.Token(ctx); err != nil {
		t.Fatalf("Token after expiry: %v", err)
	}
	if *refreshes != 1 {
		t.Fatalf("expired token should refresh once; refreshes=%d", *refreshes)
	}

	// Unknown subject + Forget.
	if _, err := c.Client("nope"); err == nil {
		t.Fatal("Client(unknown) should error")
	}
	c.Forget(sub)
	if _, err := c.Client(sub); err == nil {
		t.Fatal("Client after Forget should error")
	}
}

func TestCustodianRefreshExhausted(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /herald/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token_endpoint":"` + srv.URL + `/herald/token","jwks_uri":"x","revocation_endpoint":"x"}`))
	})
	mux.HandleFunc("POST /herald/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			w.WriteHeader(http.StatusBadRequest) // chain exhausted
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"` + jwt("agent-1") + `","token_type":"Bearer","expires_in":600,"refresh_token":"r1"}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	ctx := context.Background()
	if _, err := c.Redeem(ctx, "assertion"); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	s := &source{cust: c, subject: "agent-1"}
	c.mu.Lock()
	c.by["agent-1"].exp = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	if _, err := s.Token(ctx); err != client.ErrReauth {
		t.Fatalf("exhausted refresh should be ErrReauth, got %v", err)
	}
}
```

> Note: `Redeem` with a literal `"assertion"` works against the stub (it only checks non-empty). The first test uses a real `AgentAssertion` for realism.

- [ ] **Step 3: Run — expect FAIL** — `cd /Users/jacinta/Source/nexus && go test ./nexus/cwb/custodian/`
Expected: build error (package/symbols undefined).

- [ ] **Step 4: Implement** — `nexus/cwb/custodian/custodian.go`:

```go
// Package custodian mints, holds, and refreshes per-aspect herald tokens (casket
// jwt-bearer + refresh_token grants) and yields a cwb-client authed AS an aspect.
// In-memory: tokens are ephemeral derived state (nexus owns the persistent state).
// Bootstrap step 2 — fed assertions by the WS register handshake (step 3).
package custodian

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/cwb-client/client"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
)

// skew refreshes a little before expiry to avoid races.
const skew = 60 * time.Second

// Custodian holds per-aspect herald tokens keyed by subject (agent UUID).
type Custodian struct {
	edge string
	oc   *oidc.Client
	mu   sync.RWMutex
	by   map[string]*entry
}

type entry struct {
	mu      sync.Mutex // serialises this subject's refresh
	access  string
	refresh string
	exp     time.Time
}

// New builds a Custodian targeting one herald edge.
func New(edge string) *Custodian {
	return &Custodian{edge: edge, oc: oidc.New(edge), by: map[string]*entry{}}
}

// Redeem exchanges a casket assertion for a herald token, custodies it, and
// returns the subject (agent UUID).
func (c *Custodian) Redeem(ctx context.Context, assertion string) (string, error) {
	tok, err := c.oc.JWTBearerGrant(ctx, assertion)
	if err != nil {
		return "", fmt.Errorf("custodian: redeem: %w", err)
	}
	claims, err := identity.DecodeAccessClaims(tok.AccessToken)
	if err != nil {
		return "", fmt.Errorf("custodian: decode redeemed token: %w", err)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errors.New("custodian: redeemed token has no subject")
	}
	c.mu.Lock()
	c.by[sub] = &entry{
		access:  tok.AccessToken,
		refresh: tok.RefreshToken,
		exp:     time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
	}
	c.mu.Unlock()
	return sub, nil
}

// Client returns a cwb-client authed AS subject. Error if subject was not Redeem'd.
func (c *Custodian) Client(subject string) (*client.Client, error) {
	if _, ok := c.lookup(subject); !ok {
		return nil, fmt.Errorf("custodian: no token for %q (not redeemed)", subject)
	}
	return client.New(c.edge, &source{cust: c, subject: subject}), nil
}

// Forget drops a subject's custodied token (on disconnect).
func (c *Custodian) Forget(subject string) {
	c.mu.Lock()
	delete(c.by, subject)
	c.mu.Unlock()
}

func (c *Custodian) lookup(subject string) (*entry, bool) {
	c.mu.RLock()
	e, ok := c.by[subject]
	c.mu.RUnlock()
	return e, ok
}

// source is the per-aspect client.TokenSource.
type source struct {
	cust    *Custodian
	subject string
}

func (s *source) Token(ctx context.Context) (string, error) {
	e, ok := s.cust.lookup(s.subject)
	if !ok {
		return "", client.ErrReauth
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Until(e.exp) > skew {
		return e.access, nil
	}
	return s.refreshLocked(ctx, e)
}

func (s *source) Refresh(ctx context.Context) (string, error) {
	e, ok := s.cust.lookup(s.subject)
	if !ok {
		return "", client.ErrReauth
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return s.refreshLocked(ctx, e)
}

// refreshLocked runs the refresh_token grant; the caller holds e.mu. The herald
// HTTP call happens here under the per-entry mutex only (never the map lock).
func (s *source) refreshLocked(ctx context.Context, e *entry) (string, error) {
	if e.refresh == "" {
		return "", client.ErrReauth
	}
	tok, err := s.cust.oc.RefreshGrant(ctx, e.refresh)
	if err != nil {
		return "", client.ErrReauth
	}
	e.access = tok.AccessToken
	if tok.RefreshToken != "" {
		e.refresh = tok.RefreshToken
	}
	e.exp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return e.access, nil
}
```

- [ ] **Step 5: Run — expect PASS** — `cd /Users/jacinta/Source/nexus && go test ./nexus/cwb/custodian/ -v && go build ./... && go vet ./nexus/cwb/custodian/`
Expected: both tests PASS; nexus still builds.

- [ ] **Step 6: Commit**

```bash
cd /Users/jacinta/Source/nexus && git add go.mod go.sum nexus/cwb/custodian/
git commit -m "cwb/custodian: per-aspect herald token custodian (bootstrap step 2)"
```

---

## Task 2: gated live integration

**Files:** `nexus/cwb/custodian/custodian_live_test.go`

- [ ] **Step 1: Gated live test** — `nexus/cwb/custodian/custodian_live_test.go`:

```go
package custodian

import (
	"context"
	"os"
	"testing"

	"github.com/CarriedWorldUniverse/cwb-client/herald"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/cwb-client/oidc"
)

// TestLiveCustodian redeems a provisioned agent's assertion at the live herald,
// then uses the custodian's client to call a pillar AS that agent (identity-
// derived authz proven) and forces a refresh.
//
// Gated on CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG.
func TestLiveCustodian(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG to run the live custodian test")
	}
	ctx := context.Background()
	tu, err := oidc.New(edge).TokenEndpoint(ctx)
	if err != nil {
		t.Fatalf("token endpoint: %v", err)
	}
	assertion, err := identity.AgentAssertion([]byte(seed), slug, agentID, tu)
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}

	c := New(edge)
	sub, err := c.Redeem(ctx, assertion)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if sub != agentID {
		t.Fatalf("subject %q != agentID %q", sub, agentID)
	}
	cl, err := c.Client(sub)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	// Call /api/me AS the agent — proves the custodied token carries the agent's identity.
	ui, err := herald.Me(ctx, cl)
	if err != nil {
		t.Fatalf("herald.Me via custodian: %v", err)
	}
	if ui.ID != agentID || ui.Kind != "agent" {
		t.Fatalf("Me as agent: %+v", ui)
	}
	// Force a refresh, then call again.
	if _, err := (&source{cust: c, subject: sub}).Refresh(ctx); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if _, err := herald.Me(ctx, cl); err != nil {
		t.Fatalf("Me after refresh: %v", err)
	}
}
```

- [ ] **Step 2: Offline suite** — `cd /Users/jacinta/Source/nexus && go build ./... && go vet ./nexus/cwb/custodian/ && go test ./nexus/cwb/custodian/`
Expected: green; `TestLiveCustodian` SKIPs without `CW_IT_*`.

- [ ] **Step 3: Commit**

```bash
cd /Users/jacinta/Source/nexus && git add nexus/cwb/custodian/
git commit -m "cwb/custodian: gated live test (redeem -> call pillar as the agent -> refresh)"
```

- [ ] **Step 4: Controller — live smoke + merge.** Provision an agent via cw (as cwadmin), export `CW_IT_OWNER_SEED`/`CW_IT_AGENT_ID`/`CW_IT_AGENT_SLUG`/`CW_IT_EDGE`, run `TestLiveCustodian` against dMon (redeem → `herald.Me` as the agent → refresh). Then PR + merge nexus.

---

## Self-review

**Spec coverage:** `custodian.New`/`Redeem`/`Client`/`Forget` + the `source` TokenSource (refresh via refresh_token, ErrReauth on exhaustion) → Task 1; per-entry refresh serialization, herald I/O outside the map lock → Task 1 (`refreshLocked` holds only `e.mu`); gated live (Redeem → pillar-call-as-agent → refresh) → Task 2. ✔
**Placeholder scan:** the `jwt()` test helper builds a fake-but-decodable JWT (DecodeAccessClaims only base64-decodes the payload, no signature check) — concrete. The go-directive bump is verified via `go build`.
**Type consistency:** `Custodian`/`entry`/`source`; `oidc.JWTBearerGrant`/`RefreshGrant`; `identity.DecodeAccessClaims`/`AgentAssertion`; `client.New`/`ErrReauth`/`TokenSource`. Keyed by `sub`. Matches cwb-client + the spec.
