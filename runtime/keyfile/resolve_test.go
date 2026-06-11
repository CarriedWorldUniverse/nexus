package keyfile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveByJWT_HappyPath — the keyfile-less hand boot path: present
// a session JWT as a bearer to /api/aspect/resolve, get back a populated
// ValidationResult (persona/provider/model), with the aspect name taken
// from the JWT sub the broker echoes (NOT from any env).
func TestResolveByJWT_HappyPath(t *testing.T) {
	const handJWT = "header." // value is opaque to the client; the broker echoes one back

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/aspect/resolve", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                 true,
			"session_jwt":        fakeJWT(t, "shadow.umbra"),
			"session_expires_at": "2026-06-11T11:00:00Z",
			"personality": map[string]any{
				"soul_md": "shadow soul", "version": 1,
			},
			"provider": "claude-api",
			"model":    "claude-opus",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client()}
	// wsURL form mirrors what seamHTTPToWS produces; ResolveByJWT maps it
	// back to the HTTPS base internally.
	wsURL := wsFromHTTPTest(srv.URL)
	res, err := c.ResolveByJWT(context.Background(), wsURL, "fixture-nexus", handJWT)
	if err != nil {
		t.Fatalf("ResolveByJWT: %v", err)
	}
	if gotAuth != "Bearer "+handJWT {
		t.Errorf("Authorization header = %q, want bearer of the presented JWT", gotAuth)
	}
	if res.AspectName != "shadow.umbra" {
		t.Errorf("AspectName = %q, want shadow.umbra (from JWT sub)", res.AspectName)
	}
	if res.Provider != "claude-api" || res.Model != "claude-opus" {
		t.Errorf("provider/model = %q/%q", res.Provider, res.Model)
	}
	if res.Personality.SoulMD != "shadow soul" {
		t.Errorf("persona = %+v", res.Personality)
	}
	if res.NexusURL != wsURL || res.NexusID != "fixture-nexus" {
		t.Errorf("NexusURL/ID not carried through: %q / %q", res.NexusURL, res.NexusID)
	}
}

func TestResolveByJWT_Rejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/aspect/resolve", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid session token"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client()}
	if _, err := c.ResolveByJWT(context.Background(), wsFromHTTPTest(srv.URL), "x", "bad"); err == nil {
		t.Fatal("expected error on 401")
	}
}

// wsFromHTTPTest turns an httptest http:// URL into the ws://…/connect
// form ResolveByJWT accepts (wsToHTTPS reverses it internally).
func wsFromHTTPTest(httpURL string) string {
	return "ws://" + httpURL[len("http://"):] + "/connect"
}
