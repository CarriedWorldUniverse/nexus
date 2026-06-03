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
