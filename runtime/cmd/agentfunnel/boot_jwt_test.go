package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
)

// seamHTTPToWS maps the broker's CW_SEAM_URL into the WS dial URL.
func TestSeamHTTPToWS(t *testing.T) {
	cases := map[string]string{
		"https://nexus.internal:7888":          "wss://nexus.internal:7888/connect",
		"https://nexus.internal:7888/":         "wss://nexus.internal:7888/connect",
		"https://nexus.internal:7888/connect":  "wss://nexus.internal:7888/connect",
		"http://localhost:7888":                "ws://localhost:7888/connect",
		"https://broker.tail.ts.net:7888/conn": "wss://broker.tail.ts.net:7888/conn/connect",
	}
	for in, want := range cases {
		if got := seamHTTPToWS(in); got != want {
			t.Errorf("seamHTTPToWS(%q) = %q, want %q", in, got, want)
		}
	}
}

// The hand boot path: with CW_SESSION_JWT + CW_SEAM_URL set and no
// keyfile, the funnel resolves persona/provider from the broker keyed
// on the JWT. This drives the same keyfile.Client.ResolveByJWT call the
// boot branch makes, against a stub broker that mimics /api/aspect/resolve.
func TestHandBootResolvesViaJWT(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/aspect/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hand-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                 true,
			"session_jwt":        fakeSub(t, "shadow.umbra"),
			"session_expires_at": "2026-06-11T11:00:00Z",
			"personality":        map[string]any{"soul_md": "shadow soul"},
			"provider":           "deepseek",
			"model":              "deepseek-chat",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := seamHTTPToWS(srv.URL) // http://… → ws://…/connect
	client := &keyfile.Client{HTTP: srv.Client()}
	res, err := client.ResolveByJWT(context.Background(), wsURL, "nexus-id", "hand-token")
	if err != nil {
		t.Fatalf("hand boot resolve: %v", err)
	}
	if res.AspectName != "shadow.umbra" {
		t.Errorf("AspectName = %q, want the derived hand name", res.AspectName)
	}
	if res.Provider != "deepseek" {
		t.Errorf("hand must inherit the parent provider, got %q", res.Provider)
	}
	if res.Personality.SoulMD != "shadow soul" {
		t.Errorf("persona = %+v", res.Personality)
	}
}

// fakeSub builds a JWT whose sub the client parses for AspectName. The
// signature is not verified client-side (the TLS + nexus_id checks
// vouch for the broker), so an opaque sig is fine here.
func fakeSub(t *testing.T, sub string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	cls, _ := json.Marshal(map[string]any{"sub": sub})
	return enc(hdr) + "." + enc(cls) + "." + enc([]byte("sig"))
}
