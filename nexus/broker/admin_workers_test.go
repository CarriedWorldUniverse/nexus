package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/workerstatus"
)

// adminWorkersTestRig wires the admin surface with a WorkerStatusStore
// so GET /api/admin/workers registers. Mirrors modelConfigTestRig
// (admin_model_config_test.go).
type adminWorkersTestRig struct {
	url        string
	adminToken string
	peerToken  string
	store      workerstatus.Store
}

func newAdminWorkersTestRig(t *testing.T) *adminWorkersTestRig {
	t.Helper()
	store := &memWorkerStatus{}

	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}

	r := roster.New()
	b := New(Config{
		Tokens:            tokens,
		Admin:             &AdminCallbacks{},
		WorkerStatusStore: store,
	}, r)

	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &adminWorkersTestRig{
		url:        srv.URL,
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
		store:      store,
	}
}

type adminWorkersBody struct {
	Workers []workerstatus.Status `json:"workers"`
}

func TestAdminWorkers_ReturnsConsolidatedRows(t *testing.T) {
	rig := newAdminWorkersTestRig(t)
	ctx := context.Background()
	if err := rig.store.Upsert(ctx, workerstatus.Status{
		Agent: "anvil", Role: "builder", State: "running",
		LastHeartbeat: time.UnixMilli(2000),
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.store.Upsert(ctx, workerstatus.Status{
		Agent: "plumb", Role: "tester", State: "done",
		LastHeartbeat: time.UnixMilli(1000),
	}); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/workers", nil)
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got adminWorkersBody
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Workers) != 2 {
		t.Fatalf("workers = %+v, want 2 rows", got.Workers)
	}
}

func TestAdminWorkers_RejectsNonAdmin(t *testing.T) {
	rig := newAdminWorkersTestRig(t)

	req, _ := http.NewRequest("GET", rig.url+"/api/admin/workers", nil)
	req.Header.Set("Authorization", "Bearer "+rig.peerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin GET status = %d; want 403", resp.StatusCode)
	}
}

// TestAdminWorkers_RouteAbsentWithoutStore confirms the endpoint isn't
// registered at all when no WorkerStatusStore is configured — 404, not
// 501 — matching the other config-gated admin routes' convention.
func TestAdminWorkers_RouteAbsentWithoutStore(t *testing.T) {
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	b := New(Config{Tokens: tokens, Admin: &AdminCallbacks{}}, roster.New())
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/workers", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("frame"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status without store configured = %d; want 404", resp.StatusCode)
	}
}
