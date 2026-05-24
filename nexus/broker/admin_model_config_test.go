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

// modelConfigTestRig wires the admin surface with credentials + aspects
// stores so the model-config routes (NEX-263) register.
type modelConfigTestRig struct {
	url        string
	adminToken string
	peerToken  string
	creds      *credentials.Store
	aspects    *aspects.SQLStore
}

func newModelConfigTestRig(t *testing.T) *modelConfigTestRig {
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

	return &modelConfigTestRig{
		url:        srv.URL,
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
		creds:      cstore,
		aspects:    astore,
	}
}

func (r *modelConfigTestRig) seedAspect(t *testing.T, name string) {
	t.Helper()
	if err := r.aspects.Insert(context.Background(), aspects.Aspect{
		Name: name, AspectPubkey: fakePubkeyBytes(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("seed aspect %q: %v", name, err)
	}
}

func TestAdminModelConfig_GetEmpty(t *testing.T) {
	rig := newModelConfigTestRig(t)
	rig.seedAspect(t, "anvil")

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/anvil/model-config", nil)
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got credentials.AspectModelConfig
	mustDecode(t, resp, &got)
	if got.Aspect != "anvil" {
		t.Errorf("aspect = %q; want anvil", got.Aspect)
	}
	if got.PrimaryModel != nil || got.JudgeModel != nil || got.CompactModel != nil ||
		got.PrimaryCredential != nil || got.JudgeCredential != nil || got.CompactCredential != nil {
		t.Errorf("expected all-nil model config, got %+v", got)
	}
}

func TestAdminModelConfig_PutThenGet(t *testing.T) {
	rig := newModelConfigTestRig(t)
	rig.seedAspect(t, "anvil")

	body := `{"primary_model": "claude-opus-4-7", "judge_model": "deepseek-v4-flash"}`
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/anvil/model-config", strings.NewReader(body))
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

	req2, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/anvil/model-config", nil)
	req2.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp2.Body.Close()
	var got credentials.AspectModelConfig
	mustDecode(t, resp2, &got)
	if got.PrimaryModel == nil || *got.PrimaryModel != "claude-opus-4-7" {
		t.Errorf("primary_model: got %v", got.PrimaryModel)
	}
	if got.JudgeModel == nil || *got.JudgeModel != "deepseek-v4-flash" {
		t.Errorf("judge_model: got %v", got.JudgeModel)
	}
	if got.CompactModel != nil {
		t.Errorf("compact_model should be unset, got %v", *got.CompactModel)
	}
}

func TestAdminModelConfig_PutClearsField(t *testing.T) {
	rig := newModelConfigTestRig(t)
	rig.seedAspect(t, "anvil")

	// Set primary_model.
	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/anvil/model-config",
		strings.NewReader(`{"primary_model": "x"}`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Clear primary_model via empty string.
	req2, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/anvil/model-config",
		strings.NewReader(`{"primary_model": ""}`))
	req2.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()

	req3, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/anvil/model-config", nil)
	req3.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp3, _ := http.DefaultClient.Do(req3)
	defer resp3.Body.Close()
	var got credentials.AspectModelConfig
	mustDecode(t, resp3, &got)
	if got.PrimaryModel != nil {
		t.Errorf("primary_model should be cleared, got %v", *got.PrimaryModel)
	}
}

func TestAdminModelConfig_RejectsNonAdmin(t *testing.T) {
	rig := newModelConfigTestRig(t)
	rig.seedAspect(t, "anvil")

	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/anvil/model-config",
		strings.NewReader(`{"primary_model":"x"}`))
	req.Header.Set("Authorization", "Bearer "+rig.peerToken)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin PUT status = %d; want 403", resp.StatusCode)
	}

	req2, _ := http.NewRequest("GET", rig.url+"/api/admin/aspects/anvil/model-config", nil)
	req2.Header.Set("Authorization", "Bearer "+rig.peerToken)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin GET status = %d; want 403", resp2.StatusCode)
	}
}

func TestAdminModelConfig_MalformedBody(t *testing.T) {
	rig := newModelConfigTestRig(t)
	rig.seedAspect(t, "anvil")

	req, _ := http.NewRequest("PUT", rig.url+"/api/admin/aspects/anvil/model-config",
		strings.NewReader(`{ not valid json`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body status = %d; want 400", resp.StatusCode)
	}
}
