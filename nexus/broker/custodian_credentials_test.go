package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// fakeCustodianGit records calls and returns a canned bundle (or error).
type fakeCustodianGit struct {
	calls    []string // "identity|org|host"
	username string
	password string
	host     string
	err      error
}

func (f *fakeCustodianGit) FetchGit(_ context.Context, identity, org, host string) (string, string, string, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%s", identity, org, host))
	if f.err != nil {
		return "", "", "", f.err
	}
	h := f.host
	if h == "" {
		h = host
	}
	return f.username, f.password, h, nil
}

// newRoutingRig builds the agent-cred endpoint with an optional custodian git
// source + org, sharing the same JWT secret + local store as the base rig.
func newRoutingRig(t *testing.T, custodian GitCredentialSource, org string) *agentCredTestRig {
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
		Tokens:       NewTokenStore(),
		Credentials:  credStore,
		CustodianGit: custodian,
		CustodianOrg: org,
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

// TestGitRoutesToCustodian — with a custodian git source configured, a kind=git
// fetch is served by custodian (NOT the local store), and the broker presents
// the agent identity + configured org.
func TestGitRoutesToCustodian(t *testing.T) {
	fake := &fakeCustodianGit{username: "nexus-cw", password: "ghp_from_custodian", host: "github.com"}
	rig := newRoutingRig(t, fake, "cwb-admin")

	// Note: NO local git credential is seeded — proving the bundle came from
	// custodian, not the local store.
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"git","host":"github.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("git via custodian status=%d want 200", resp.StatusCode)
	}
	var got agentCredFetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bundle["password"] != "ghp_from_custodian" || got.Bundle["username"] != "nexus-cw" {
		t.Fatalf("bundle not from custodian: %v", got.Bundle)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "worker-1|cwb-admin|github.com" {
		t.Fatalf("custodian not called with (identity|org|host): %v", fake.calls)
	}
}

// TestGitStaysLocalWhenCustodianNil — NO regression: with no custodian
// configured, kind=git is served from the local store exactly as before.
func TestGitStaysLocalWhenCustodianNil(t *testing.T) {
	rig := newRoutingRig(t, nil, "")
	if err := rig.creds.Set(context.Background(), credentials.UpsertParams{
		Name:           "worker-git",
		Kind:           credentials.KindGit,
		Bundle:         map[string]any{"username": "local", "password": "ghp_local", "host": "github.com"},
		AllowedAspects: []string{"worker-1"},
		Mode:           credentials.ModeFetch,
	}); err != nil {
		t.Fatalf("seed local git: %v", err)
	}
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"git","host":"github.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("local git status=%d want 200", resp.StatusCode)
	}
	var got agentCredFetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bundle["password"] != "ghp_local" {
		t.Fatalf("expected local bundle, got %v", got.Bundle)
	}
}

// TestNonGitStaysLocalWhenCustodianConfigured — NO regression for other kinds:
// provider/jira/imap always use the local store even with custodian wired.
func TestNonGitStaysLocalWhenCustodianConfigured(t *testing.T) {
	fake := &fakeCustodianGit{username: "x", password: "should-not-be-used", host: "github.com"}
	rig := newRoutingRig(t, fake, "cwb-admin")
	if err := rig.creds.Set(context.Background(), credentials.UpsertParams{
		Name:           "worker-prov",
		Kind:           credentials.KindProvider,
		Bundle:         map[string]any{"api_shape": "openai", "base_url": "https://api", "key": "sk-local", "default_model": "m"},
		AllowedAspects: []string{"worker-1"},
		Mode:           credentials.ModeFetch,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"provider","name":"worker-prov"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("provider status=%d want 200", resp.StatusCode)
	}
	var got agentCredFetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bundle["key"] != "sk-local" {
		t.Fatalf("provider must come from local store: %v", got.Bundle)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("custodian must NOT be called for non-git kinds: %v", fake.calls)
	}
}

// TestGitCustodianNotFoundMaps404 — a custodian NotFound surfaces as 404,
// matching the local-store miss semantics.
func TestGitCustodianNotFoundMaps404(t *testing.T) {
	fake := &fakeCustodianGit{err: grpcstatus.Error(grpccodes.NotFound, "no such credential")}
	rig := newRoutingRig(t, fake, "cwb-admin")
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"git","host":"github.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("custodian NotFound status=%d want 404", resp.StatusCode)
	}
}

// TestGitCustodianPermissionDeniedMaps403 — a custodian PermissionDenied
// surfaces as 403.
func TestGitCustodianPermissionDeniedMaps403(t *testing.T) {
	fake := &fakeCustodianGit{err: grpcstatus.Error(grpccodes.PermissionDenied, "missing scope")}
	rig := newRoutingRig(t, fake, "cwb-admin")
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"git","host":"github.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("custodian PermissionDenied status=%d want 403", resp.StatusCode)
	}
}

// TestGitCustodianMissingHost400 — git fetch with no host is a 400 (custodian
// has no credential coordinate without it).
func TestGitCustodianMissingHost400(t *testing.T) {
	fake := &fakeCustodianGit{username: "u", password: "p", host: "github.com"}
	rig := newRoutingRig(t, fake, "cwb-admin")
	resp := rig.fetch(t, rig.mintJWT(t, "worker-1"), `{"kind":"git"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("git-no-host status=%d want 400", resp.StatusCode)
	}
}
