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

func TestUnlistedPrefixNotProxied(t *testing.T) {
	var edgeHit bool
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		edgeHit = true
		_, _ = w.Write([]byte("ok"))
	}))
	defer edge.Close()

	mux := http.NewServeMux()
	if err := Register(mux, edge.URL); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/other/api/thing") // not in Prefixes
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unlisted prefix: status = %d, want 404", resp.StatusCode)
	}
	if edgeHit {
		t.Fatal("unlisted prefix was forwarded to the edge — allowlist breached")
	}
}

func TestRegisterEmptyEdge(t *testing.T) {
	if err := Register(http.NewServeMux(), ""); err == nil {
		t.Fatal("empty edge should error")
	}
}
