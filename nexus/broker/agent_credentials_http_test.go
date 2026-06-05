package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// agentCredTestRig wires the agent credential seam endpoint with a real
// JWT-signing secret (for auth) and a real credential store (the source
// of the scoped bundle), so tests can mint agent tokens and assert
// scope + audit behaviour.
type agentCredTestRig struct {
	srv        *httptest.Server
	signingSec []byte
	creds      *credentials.Store
}

func newAgentCredTestRig(t *testing.T) *agentCredTestRig {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	signingSec := []byte("fixture-secret-32-bytes-padding-x")
	credStore, err := credentials.NewStore(db, signingSec)
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}

	r := roster.New()
	b := New(Config{
		Tokens:      NewTokenStore(),
		Credentials: credStore,
		KeyfileValidator: &KeyfileValidator{
			NexusID:              "test-nexus",
			SessionSigningSecret: signingSec,
			Store:                aspects.NewSQLStore(db),
			JWTTTL:               time.Hour,
		},
	}, r)
	mux := http.NewServeMux()
	b.registerKeyfileEndpoints(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &agentCredTestRig{srv: srv, signingSec: signingSec, creds: credStore}
}

func (r *agentCredTestRig) mintJWT(t *testing.T, agent string) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.Sign(r.signingSec, jwt.Claims{
		Iss: "nexus://test-nexus", Sub: agent,
		Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(), Kfv: 1, Ses: "test",
	})
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return tok
}

func (r *agentCredTestRig) fetch(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", r.srv.URL+"/api/agent/credential.fetch", bytesReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestAgentCredentialFetch_Git(t *testing.T) {
	rig := newAgentCredTestRig(t)
	ctx := context.Background()
	if err := rig.creds.Set(ctx, credentials.UpsertParams{
		Name:           "worker-git",
		Kind:           credentials.KindGit,
		Bundle:         map[string]any{"username": "nexus-cw", "password": "ghp_x", "host": "github.com"},
		AllowedAspects: []string{"worker-1"},
		Mode:           credentials.ModeFetch,
	}); err != nil {
		t.Fatalf("seed git cred: %v", err)
	}

	// allowed agent → 200 + bundle
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"git","name":"worker-git"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("worker-1 status=%d want 200", resp.StatusCode)
	}
	var got agentCredFetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bundle["password"] != "ghp_x" || got.Bundle["username"] != "nexus-cw" {
		t.Fatalf("bundle = %v", got.Bundle)
	}

	// disallowed agent → 403 + AuditDenied row
	resp2 := rig.fetch(t, rig.mintJWT(t, "worker-2"), `{"kind":"git","name":"worker-git"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Fatalf("worker-2 status=%d want 403", resp2.StatusCode)
	}
	rows, err := rig.creds.ListAudit(ctx, "worker-git", 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	denied := false
	for _, row := range rows {
		if row.Action == credentials.AuditDenied {
			denied = true
		}
	}
	if !denied {
		t.Fatal("expected AuditDenied row for worker-2")
	}

	// no bearer → 401
	resp3 := rig.fetch(t, "", `{"kind":"git"}`)
	defer resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Fatalf("no-bearer status=%d want 401", resp3.StatusCode)
	}
}
