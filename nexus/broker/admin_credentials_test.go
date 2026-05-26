package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"net/http/httptest"
)

// newCredentialAdminTestServer is a focused harness for the
// admin-credential REST routes. Spins up a broker with a real
// credentials.Store (encrypted bundles + identity key) and an
// operator JWT for authed admin calls.
func newCredentialAdminTestServer(t *testing.T) (*httptest.Server, *credentials.Store, string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	secret := []byte("test-secret-32-bytes-padding-vvvv")
	credStore, err := credentials.NewStore(db, secret)
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}

	opLogin := &OperatorLogin{
		SessionSigningSecret: secret,
		JWTTTL:               time.Hour,
		NexusID:              "test-nexus",
	}

	b := New(Config{
		Tokens:        NewTokenStore(),
		Credentials:   credStore,
		OperatorLogin: opLogin,
		// Non-nil AdminCallbacks triggers admin-route registration.
		// All fields can be nil — we don't call shutdown/compact in
		// these tests, only credential endpoints.
		Admin: &AdminCallbacks{},
	}, roster.New())

	// Real ServeMux so registerAdmin can install the /api/admin/*
	// routes — the lightweight newMux in ws_test.go only handles
	// /connect, /api/aspects, /health.
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tok, err := jwt.Sign(secret, jwt.Claims{
		Iss: "nexus://test-nexus",
		Sub: "operator",
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
		Ses: "test-session",
	})
	if err != nil {
		t.Fatalf("jwt sign: %v", err)
	}
	return srv, credStore, tok
}

// adminPutCredential issues a PUT /api/admin/credentials/{name} and
// returns the response. Operator JWT in Authorization header.
func adminPutCredential(t *testing.T, srv *httptest.Server, tok, name string, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut,
		srv.URL+"/api/admin/credentials/"+name,
		bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT credentials: %v", err)
	}
	return resp
}

// Operator-reported 2026-05-27: default mode was proxy, which silently
// produces a non-functional credential because the broker-side proxy
// path doesn't exist for any provider yet. Default flipped to fetch —
// the only mode that actually works today. Tested by omitting mode
// from the upsert body and reading the stored row back.
func TestAdminCredentialUpsert_DefaultModeIsFetch(t *testing.T) {
	srv, store, tok := newCredentialAdminTestServer(t)

	resp := adminPutCredential(t, srv, tok, "test-cred", map[string]any{
		"kind": "provider",
		"bundle": map[string]any{
			"api_shape": "anthropic",
			"base_url":  "https://api.anthropic.com",
			"key":       "sk-test-123",
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT got %d (want 200); body=%s", resp.StatusCode, body)
	}

	c, err := store.Get(context.Background(), "test-cred")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if c.Mode != credentials.ModeFetch {
		t.Errorf("Mode = %q, want %q (default flipped to fetch — proxy doesn't work today)",
			c.Mode, credentials.ModeFetch)
	}
}

// Operator-reported 2026-05-27: editing a credential to flip mode (or
// description / allowed-aspects) required re-typing the API key — the
// backend rejected PUT without a bundle. Now: omit bundle on a
// pre-existing credential and the stored bundle is preserved while
// mode/description/allowed updates take effect.
func TestAdminCredentialUpsert_OmittedBundlePreservesExisting(t *testing.T) {
	srv, store, tok := newCredentialAdminTestServer(t)

	// 1. Create the credential with a real bundle + mode=proxy.
	resp := adminPutCredential(t, srv, tok, "rotate-me", map[string]any{
		"kind": "provider",
		"mode": "proxy",
		"bundle": map[string]any{
			"api_shape": "anthropic",
			"base_url":  "https://api.anthropic.com",
			"key":       "sk-original-key-aaaa",
		},
		"description":     "before",
		"allowed_aspects": []string{"*"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status %d", resp.StatusCode)
	}

	// 2. PUT again with NO bundle — just flip mode + description.
	resp2 := adminPutCredential(t, srv, tok, "rotate-me", map[string]any{
		"kind":            "provider",
		"mode":            "fetch",
		"description":     "after",
		"allowed_aspects": []string{"harrow"},
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("update-no-bundle got %d (want 200); body=%s", resp2.StatusCode, body)
	}

	// 3. Read back — verify metadata changed AND bundle preserved.
	c, err := store.Get(context.Background(), "rotate-me")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if c.Mode != credentials.ModeFetch {
		t.Errorf("Mode = %q, want fetch (update took effect)", c.Mode)
	}
	if c.Description != "after" {
		t.Errorf("Description = %q, want \"after\"", c.Description)
	}
	if len(c.AllowedAspects) != 1 || c.AllowedAspects[0] != "harrow" {
		t.Errorf("AllowedAspects = %v, want [harrow]", c.AllowedAspects)
	}
	bundle, err := store.Bundle(c)
	if err != nil {
		t.Fatalf("decrypt preserved bundle: %v", err)
	}
	if bundle["key"] != "sk-original-key-aaaa" {
		t.Errorf("preserved bundle key = %v, want sk-original-key-aaaa (bundle must survive a bundle-less update)",
			bundle["key"])
	}
}

// Brand-new credential created without a bundle is still rejected —
// bundle-omit only preserves on EXISTING records. Otherwise we'd
// silently create an empty/garbage row.
func TestAdminCredentialUpsert_OmittedBundleOnCreateRejected(t *testing.T) {
	srv, _, tok := newCredentialAdminTestServer(t)

	resp := adminPutCredential(t, srv, tok, "brand-new", map[string]any{
		"kind": "provider",
		"mode": "fetch",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (bundle required on create)", resp.StatusCode)
	}
}

// Bundle-omit with a kind that differs from the stored credential is
// rejected — operator must supply a new bundle to change the kind
// (the existing bundle's shape is kind-specific and wouldn't validate
// against the new kind).
func TestAdminCredentialUpsert_KindMismatchOnBundleOmitRejected(t *testing.T) {
	srv, _, tok := newCredentialAdminTestServer(t)

	// Seed a provider credential.
	resp := adminPutCredential(t, srv, tok, "kind-shift", map[string]any{
		"kind": "provider",
		"bundle": map[string]any{
			"api_shape": "openai",
			"base_url":  "https://api.openai.com/v1",
			"key":       "sk-openai-test",
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed: %d", resp.StatusCode)
	}

	// Try to update with kind=jira but no bundle — must reject.
	resp2 := adminPutCredential(t, srv, tok, "kind-shift", map[string]any{
		"kind": "jira",
		"mode": "fetch",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (kind mismatch must reject)", resp2.StatusCode)
	}
}
