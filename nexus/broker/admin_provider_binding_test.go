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

// providerBindingTestRig parallels modelConfigTestRig but wires the
// provider-binding routes (NEX-335) registered alongside model-config
// behind the same Credentials+KeyfileValidator config guard.
type providerBindingTestRig struct {
	url        string
	adminToken string
	aspects    *aspects.SQLStore
}

func newProviderBindingTestRig(t *testing.T) *providerBindingTestRig {
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

	return &providerBindingTestRig{
		url:        srv.URL,
		adminToken: tokens.TokenForAgent("frame"),
		aspects:    astore,
	}
}

func (r *providerBindingTestRig) seed(t *testing.T, name, provider, model string) {
	t.Helper()
	if err := r.aspects.Insert(context.Background(), aspects.Aspect{
		Name: name, AspectPubkey: fakePubkeyBytes(),
		Provider: provider, Model: model,
	}); err != nil {
		t.Fatalf("seed aspect %q: %v", name, err)
	}
}

func TestAdminProviderBinding_Get(t *testing.T) {
	rig := newProviderBindingTestRig(t)
	rig.seed(t, "plumb", "claude-code", "claude-opus-4-7")

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/plumb/provider-binding", nil)
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got providerBindingResponse
	mustDecode(t, resp, &got)
	if got.Aspect != "plumb" || got.Provider != "claude-code" || got.Model != "claude-opus-4-7" {
		t.Errorf("got %+v", got)
	}
}

func TestAdminProviderBinding_PutFlipsClaudeCodeToOpenAI(t *testing.T) {
	rig := newProviderBindingTestRig(t)
	rig.seed(t, "plumb", "claude-code", "claude-opus-4-7")

	body := `{"provider":"openai","model":"deepseek-chat"}`
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/plumb/provider-binding", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
	var got providerBindingResponse
	mustDecode(t, resp, &got)
	if got.Provider != "openai" || got.Model != "deepseek-chat" {
		t.Errorf("PUT response: got %+v", got)
	}

	// Read-back via store confirms the column actually changed (i.e.
	// the PUT response isn't echoing the request without writing).
	row, err := rig.aspects.Get(context.Background(), "plumb")
	if err != nil {
		t.Fatalf("readback Get: %v", err)
	}
	if row.Provider != "openai" || row.Model != "deepseek-chat" {
		t.Errorf("store readback: provider=%q model=%q", row.Provider, row.Model)
	}
}

func TestAdminProviderBinding_PutRejectsUnknownProvider(t *testing.T) {
	rig := newProviderBindingTestRig(t)
	rig.seed(t, "plumb", "claude-code", "claude-opus-4-7")

	body := `{"provider":"not-a-real-provider","model":"x"}`
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/plumb/provider-binding", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}

	// Aspect row must not have changed (validation rejected pre-write).
	row, err := rig.aspects.Get(context.Background(), "plumb")
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if row.Provider != "claude-code" {
		t.Errorf("provider mutated to %q after rejected PUT", row.Provider)
	}
}

func TestAdminProviderBinding_PutRejectsEmptyFields(t *testing.T) {
	rig := newProviderBindingTestRig(t)
	rig.seed(t, "plumb", "claude-code", "claude-opus-4-7")

	for _, body := range []string{
		`{"provider":"","model":"x"}`,
		`{"provider":"openai","model":""}`,
	} {
		req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/plumb/provider-binding", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rig.adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %q: %v", body, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %q: status = %d; want 400", body, resp.StatusCode)
		}
	}
}

func TestAdminProviderBinding_GetUnknownAspect(t *testing.T) {
	rig := newProviderBindingTestRig(t)
	// No seed.

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/ghost/provider-binding", nil)
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
}
