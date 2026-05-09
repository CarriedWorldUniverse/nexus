package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func bytesReader(s string) io.Reader { return strings.NewReader(s) }

func mustDecode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// personalityTestRig wires the admin surface AND the keyfile validator
// (which carries the aspects.Store). The two share state so tests can
// drive the PUT and read back the row to verify.
type personalityTestRig struct {
	srv        *httptest.Server
	adminToken string
	peerToken  string
	store      *aspects.SQLStore
}

func newPersonalityTestRig(t *testing.T) *personalityTestRig {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)

	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}

	r := roster.New()
	b := New(Config{
		Tokens: tokens,
		Admin:  &AdminCallbacks{}, // empty callbacks; we don't hit the other endpoints
		KeyfileValidator: &KeyfileValidator{
			Store: store,
		},
	}, r)

	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &personalityTestRig{
		srv:        srv,
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
		store:      store,
	}
}

// TestAdminPersonalityEdit_HappyPath — admin PUTs a 3-section update;
// row is written, response carries old/new versions.
func TestAdminPersonalityEdit_HappyPath(t *testing.T) {
	rig := newPersonalityTestRig(t)
	if err := rig.store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("seed aspect: %v", err)
	}

	body := `{"nexus_md":"## plumb","soul_md":"voice","primer_md":"primer"}`
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/aspect/plumb/personality", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got adminPersonalityResponse
	mustDecode(t, resp, &got)
	if got.Aspect != "plumb" {
		t.Errorf("aspect = %q; want plumb", got.Aspect)
	}
	if got.OldVersion != 0 || got.NewVersion != 1 {
		t.Errorf("versions: old=%d new=%d; want 0→1", got.OldVersion, got.NewVersion)
	}

	// Verify it actually landed.
	p, err := rig.store.PersonalityGet(context.Background(), "plumb")
	if err != nil {
		t.Fatalf("PersonalityGet: %v", err)
	}
	if p.NexusMD != "## plumb" {
		t.Errorf("DB content wrong: %+v", p)
	}
}

// TestAdminPersonalityEdit_RejectsNonAdmin — peer token must get 403,
// not 200. The admin gate enforces.
func TestAdminPersonalityEdit_RejectsNonAdmin(t *testing.T) {
	rig := newPersonalityTestRig(t)
	if err := rig.store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/aspect/plumb/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Authorization", "Bearer "+rig.peerToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("peer token status = %d; want 403", resp.StatusCode)
	}
}

// TestAdminPersonalityEdit_UnknownAspect — aspect doesn't exist → 404.
func TestAdminPersonalityEdit_UnknownAspect(t *testing.T) {
	rig := newPersonalityTestRig(t)
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/aspect/ghost/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Authorization", "Bearer "+rig.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
}

// TestAdminPersonalityEdit_MalformedBody — bad JSON / unknown fields
// rejected with 400.
func TestAdminPersonalityEdit_MalformedBody(t *testing.T) {
	rig := newPersonalityTestRig(t)
	if err := rig.store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := []string{
		"",
		"not json",
		`{"unknown_field":"x"}`,
	}
	for _, body := range cases {
		req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/admin/aspect/plumb/personality", bytesReader(body))
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

// TestAdminPersonalityEdit_FiresOnPersonalityChange — Part 7c hook.
// Successful edits invoke the OnPersonalityChange callback so the
// embedded Frame can refresh in-process. Failed edits do NOT fire it.
func TestAdminPersonalityEdit_FiresOnPersonalityChange(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)
	if err := store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tokens := NewTokenStore()
	_ = tokens.mintInMemory("frame", true)

	type fired struct {
		name    string
		version int64
	}
	calls := []fired{}
	r := roster.New()
	b := New(Config{
		Tokens:           tokens,
		Admin:            &AdminCallbacks{},
		KeyfileValidator: &KeyfileValidator{Store: store},
		OnPersonalityChange: func(name string, ver int64) {
			calls = append(calls, fired{name, ver})
		},
	}, r)
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Successful edit: callback fires.
	req, _ := http.NewRequest("PUT", srv.URL+"/api/admin/aspect/plumb/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("frame"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if len(calls) != 1 {
		t.Fatalf("callback fired %d times; want 1", len(calls))
	}
	if calls[0].name != "plumb" || calls[0].version != 1 {
		t.Errorf("callback args = %+v; want {plumb, 1}", calls[0])
	}

	// Failed edit (unknown aspect): callback must NOT fire.
	req2, _ := http.NewRequest("PUT", srv.URL+"/api/admin/aspect/ghost/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req2.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("frame"))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do2: %v", err)
	}
	resp2.Body.Close()
	if len(calls) != 1 {
		t.Errorf("callback fired on failed edit; calls=%d", len(calls))
	}
}

// TestAdminPersonalityEdit_NotRegisteredWithoutValidator — endpoint
// is gated on KeyfileValidator presence. If a broker boots without
// keyfile auth wired, the route returns 404.
func TestAdminPersonalityEdit_NotRegisteredWithoutValidator(t *testing.T) {
	tokens := NewTokenStore()
	_ = tokens.mintInMemory("frame", true)
	r := roster.New()
	b := New(Config{
		Tokens: tokens,
		Admin:  &AdminCallbacks{},
		// KeyfileValidator omitted.
	}, r)
	mux := http.NewServeMux()
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("PUT", srv.URL+"/api/admin/aspect/plumb/personality",
		bytesReader(`{"nexus_md":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("frame"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (route not registered)", resp.StatusCode)
	}
}

// fakePubkeyBytes is a deterministic 32-byte placeholder for tests.
// (broker package can't reach into aspects package tests' fakePubkey.)
func fakePubkeyBytes() []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = 0xCD
	}
	return out
}
