package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// newAspectsAllTestRig spins up a broker with the aspects.Store wired
// + an admin in-memory token + seeded aspects, then returns the test
// server and a few helpers.
func newAspectsAllTestRig(t *testing.T) (srv *httptest.Server, ros *roster.Roster, store aspects.Store, adminTok string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	aspectStore := aspects.NewSQLStore(db)
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("admin", true); err != nil {
		t.Fatal(err)
	}
	ros = roster.New()
	b := New(Config{
		Tokens:           tokens,
		Admin:            &AdminCallbacks{},
		KeyfileValidator: &KeyfileValidator{Store: aspectStore},
	}, ros)

	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, ros, aspectStore, tokens.TokenForAgent("admin")
}

// Operator-reported 2026-05-27: "settings doesn't show aspects who
// arent connected". Pre-fix, the Settings UI used the roster-only
// list and offline aspects had no row. The new
// /api/admin/aspects/all endpoint walks the aspects table directly
// and cross-references the roster for the live flag.
func TestAdminAspectsAll_IncludesOfflineAspects(t *testing.T) {
	srv, ros, store, tok := newAspectsAllTestRig(t)
	ctx := context.Background()

	// Seed three aspects: active+live, active+offline, retired.
	for _, a := range []aspects.Aspect{
		{Name: "harrow", Status: aspects.StatusActive, AspectPubkey: make([]byte, 32), Provider: "claude-api", Model: "claude-opus-4-7"},
		{Name: "anvil", Status: aspects.StatusActive, AspectPubkey: make([]byte, 32), Provider: "openai", Model: "deepseek-chat"},
		{Name: "old-aspect", Status: aspects.StatusRetired, AspectPubkey: make([]byte, 32), Provider: "claude-api", Model: "claude-opus-4"},
	} {
		if err := store.Insert(ctx, a); err != nil {
			t.Fatalf("seed %s: %v", a.Name, err)
		}
	}

	// Register only harrow in the roster — anvil and old-aspect stay offline.
	if _, _, err := ros.Register(&schemas.RegisterRequest{
		Name: "harrow", SessionID: "s1", Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("roster.Register harrow: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/aspects/all", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	var out struct {
		Aspects []adminAspectAll `json:"aspects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Aspects) != 3 {
		t.Fatalf("got %d aspects, want 3 (harrow + anvil + old-aspect)", len(out.Aspects))
	}

	byName := map[string]adminAspectAll{}
	for _, a := range out.Aspects {
		byName[a.Name] = a
	}
	harrow, ok := byName["harrow"]
	if !ok || !harrow.Live || harrow.Status != "active" {
		t.Errorf("harrow row wrong: %+v (want live=true, status=active)", harrow)
	}
	anvil, ok := byName["anvil"]
	if !ok || anvil.Live || anvil.Status != "active" {
		t.Errorf("anvil row wrong: %+v (want live=false, status=active)", anvil)
	}
	retired, ok := byName["old-aspect"]
	if !ok || retired.Live || retired.Status != "retired" {
		t.Errorf("old-aspect row wrong: %+v (want live=false, status=retired)", retired)
	}
}

// Endpoint requires the aspects store — when KeyfileValidator.Store
// is nil, the route is not registered (returns 404 from the mux).
// This guards against a future broker boot path silently dropping
// the route while admin remains usable.
func TestAdminAspectsAll_NotRegisteredWithoutStore(t *testing.T) {
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("admin", true); err != nil {
		t.Fatal(err)
	}
	b := New(Config{
		Tokens: tokens,
		Admin:  &AdminCallbacks{},
		// KeyfileValidator intentionally nil
	}, roster.New())

	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/aspects/all", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("admin"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route gated on aspects store)", resp.StatusCode)
	}
}
