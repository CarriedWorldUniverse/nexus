package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// nexusMDTestRig wires the admin endpoint with a SettingsStore + an
// optional onChange capture so tests can verify the callback fires.
type nexusMDTestRig struct {
	srv        *httptest.Server
	adminToken string
	peerToken  string
	settings   *aspects.SQLSettingsStore
	calls      *atomic.Int32
	lastVer    *atomic.Int64
}

func newNexusMDTestRig(t *testing.T) *nexusMDTestRig {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	settings := aspects.NewSQLSettingsStore(db)

	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}

	rig := &nexusMDTestRig{
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
		settings:   settings,
		calls:      &atomic.Int32{},
		lastVer:    &atomic.Int64{},
	}

	r := roster.New()
	b := New(Config{
		Tokens:           tokens,
		Admin:            &AdminCallbacks{},
		KeyfileValidator: &KeyfileValidator{Store: aspects.NewSQLStore(db), Settings: settings},
		OnNexusMDChange: func(v int64) {
			rig.calls.Add(1)
			rig.lastVer.Store(v)
		},
	}, r)
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rig.srv = srv
	return rig
}

// TestAdminNexusMDEdit_HappyPath — admin sets central content; row
// is written; OnNexusMDChange fires with the new version.
func TestAdminNexusMDEdit_HappyPath(t *testing.T) {
	rig := newNexusMDTestRig(t)

	body := `{"nexus_md":"## central network scope"}`
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/nexus-md", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got adminNexusMDResponse
	mustDecode(t, resp, &got)
	if got.OldVersion != 0 || got.NewVersion != 1 {
		t.Errorf("versions: %d → %d; want 0 → 1", got.OldVersion, got.NewVersion)
	}

	ns, _ := rig.settings.Get(context.Background())
	if ns.NexusMD != "## central network scope" {
		t.Errorf("DB content wrong: %q", ns.NexusMD)
	}
	if rig.calls.Load() != 1 {
		t.Errorf("callback fired %d times; want 1", rig.calls.Load())
	}
	if rig.lastVer.Load() != 1 {
		t.Errorf("callback version = %d; want 1", rig.lastVer.Load())
	}
}

// TestAdminNexusMDEdit_RejectsNonAdmin — peer token gets 403.
func TestAdminNexusMDEdit_RejectsNonAdmin(t *testing.T) {
	rig := newNexusMDTestRig(t)
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/nexus-md", bytesReader(`{"nexus_md":"x"}`))
	req.Header.Set("Authorization", "Bearer "+rig.peerToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403", resp.StatusCode)
	}
	if rig.calls.Load() != 0 {
		t.Errorf("callback fired despite 403; calls=%d", rig.calls.Load())
	}
}

// TestAdminNexusMDEdit_MalformedBody — bad JSON / unknown fields → 400.
func TestAdminNexusMDEdit_MalformedBody(t *testing.T) {
	rig := newNexusMDTestRig(t)
	cases := []string{
		"",
		"not json",
		`{"unknown":"field"}`,
	}
	for _, body := range cases {
		req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/nexus-md", bytesReader(body))
		req.Header.Set("Authorization", "Bearer "+rig.adminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do(%q): %v", body, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body=%q status=%d; want 400", body, resp.StatusCode)
		}
	}
}

// TestAdminNexusMDEdit_NotRegisteredWithoutSettings — when no
// SettingsStore is configured, the route returns 404 from the mux.
func TestAdminNexusMDEdit_NotRegisteredWithoutSettings(t *testing.T) {
	tokens := NewTokenStore()
	_ = tokens.mintInMemory("frame", true)
	r := roster.New()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	b := New(Config{
		Tokens: tokens,
		Admin:  &AdminCallbacks{},
		// KeyfileValidator with Store but NO Settings.
		KeyfileValidator: &KeyfileValidator{Store: aspects.NewSQLStore(db)},
	}, r)
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("PUT", srv.URL+"/api/admin/nexus-md", bytesReader(`{"nexus_md":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("frame"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (route not registered)", resp.StatusCode)
	}
}
