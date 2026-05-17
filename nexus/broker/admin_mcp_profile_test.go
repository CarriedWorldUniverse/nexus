package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// mcpProfileTestRig wires the admin surface with a credentials.Store
// (so the mcp-profile routes get registered) and an aspects.SQLStore
// to seed aspect rows the profile FK references.
type mcpProfileTestRig struct {
	url        string
	adminToken string
	peerToken  string
	creds      *credentials.Store
	aspects    *aspects.SQLStore
}

func newMCPProfileTestRig(t *testing.T) *mcpProfileTestRig {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	astore := aspects.NewSQLStore(db)

	secret := []byte("test-session-signing-secret-32-bytes-padded")
	cstore, err := credentials.NewStore(db, secret)
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}

	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}

	r := roster.New()
	b := New(Config{
		Tokens:      tokens,
		Admin:       &AdminCallbacks{},
		Credentials: cstore,
		KeyfileValidator: &KeyfileValidator{
			Store: astore,
		},
	}, r)

	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &mcpProfileTestRig{
		url:        srv.URL,
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
		creds:      cstore,
		aspects:    astore,
	}
}

func (r *mcpProfileTestRig) seedAspect(t *testing.T, name string) {
	t.Helper()
	if err := r.aspects.Insert(context.Background(), aspects.Aspect{
		Name: name, AspectPubkey: fakePubkeyBytes(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("seed aspect %q: %v", name, err)
	}
}

func TestAdminMCPProfile_PutThenGetRoundTrip(t *testing.T) {
	rig := newMCPProfileTestRig(t)
	rig.seedAspect(t, "forge")

	const profile = `{"mcpServers":{"github":{"command":"node","env":{"TOKEN":"${credential:gh-pat.key}"}}}}`
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/forge/mcp_profile", strings.NewReader(profile))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d; want 200", resp.StatusCode)
	}

	req2, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/forge/mcp_profile", nil)
	req2.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; want 200", resp2.StatusCode)
	}
	var got struct {
		Aspect  string `json:"aspect"`
		Profile string `json:"profile"`
	}
	mustDecode(t, resp2, &got)
	if got.Aspect != "forge" {
		t.Errorf("aspect = %q; want forge", got.Aspect)
	}
	if got.Profile != profile {
		t.Errorf("profile round-trip mismatch:\n got  %q\n want %q", got.Profile, profile)
	}
}

func TestAdminMCPProfile_GetMissingReturnsEmpty(t *testing.T) {
	rig := newMCPProfileTestRig(t)
	rig.seedAspect(t, "wren")

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/wren/mcp_profile", nil)
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got struct {
		Aspect  string `json:"aspect"`
		Profile string `json:"profile"`
	}
	mustDecode(t, resp, &got)
	if got.Profile != "" {
		t.Errorf("profile for empty aspect: got %q want empty", got.Profile)
	}
}

func TestAdminMCPProfile_RejectsNonAdmin(t *testing.T) {
	rig := newMCPProfileTestRig(t)
	rig.seedAspect(t, "forge")

	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/forge/mcp_profile", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+rig.peerToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin PUT status = %d; want 403", resp.StatusCode)
	}
}

func TestAdminMCPProfile_PutRejectsMalformedJSON(t *testing.T) {
	rig := newMCPProfileTestRig(t)
	rig.seedAspect(t, "forge")

	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/forge/mcp_profile",
		strings.NewReader(`{not json`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed PUT status = %d; want 400", resp.StatusCode)
	}
}
