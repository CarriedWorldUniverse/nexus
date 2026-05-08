package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/nexus/aspects"
	"github.com/nexus-cw/nexus/nexus/jwt"
	"github.com/nexus-cw/nexus/nexus/roster"
	"github.com/nexus-cw/nexus/nexus/storage"
)

// selfEditTestRig wires the aspect self-edit endpoint with a real
// JWT-signing secret so tests can mint session tokens for any
// aspect_name and verify auth gating.
type selfEditTestRig struct {
	srv          *httptest.Server
	signingSec   []byte
	store        *aspects.SQLStore
}

func newSelfEditTestRig(t *testing.T) *selfEditTestRig {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)

	signingSec := []byte("fixture-secret-32-bytes-padding-x")

	tokens := NewTokenStore()
	r := roster.New()
	b := New(Config{
		Tokens: tokens,
		KeyfileValidator: &KeyfileValidator{
			NexusID:              "test-nexus",
			SessionSigningSecret: signingSec,
			Store:                store,
			JWTTTL:               time.Hour,
		},
	}, r)
	mux := http.NewServeMux()
	b.registerKeyfileEndpoints(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &selfEditTestRig{srv: srv, signingSec: signingSec, store: store}
}

// mintSessionJWT creates a valid HS256 token for the given aspect.
func (r *selfEditTestRig) mintSessionJWT(t *testing.T, aspectName string) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.Sign(r.signingSec, jwt.Claims{
		Iss: "nexus://test-nexus",
		Sub: aspectName,
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
		Kfv: 1,
		Ses: "test-session",
	})
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return tok
}

// TestAspectSelfEdit_HappyPath — aspect mints its own JWT, edits its
// own row, sees the version bump.
func TestAspectSelfEdit_HappyPath(t *testing.T) {
	rig := newSelfEditTestRig(t)
	if err := rig.store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tok := rig.mintSessionJWT(t, "plumb")
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/aspect/personality",
		bytesReader(`{"nexus_md":"my-delta","soul_md":"my-voice","primer_md":"my-primer"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got aspectSelfEditResponse
	mustDecode(t, resp, &got)
	if got.Aspect != "plumb" {
		t.Errorf("aspect = %q; want plumb", got.Aspect)
	}
	if got.NewVersion != 1 {
		t.Errorf("new_version = %d; want 1", got.NewVersion)
	}

	p, _ := rig.store.PersonalityGet(context.Background(), "plumb")
	if p.SoulMD != "my-voice" {
		t.Errorf("DB soul_md wrong: %q", p.SoulMD)
	}
}

// TestAspectSelfEdit_CannotEditOther — the load-bearing authorisation
// invariant: aspect_name comes ONLY from the JWT sub claim. plumb's
// JWT cannot edit wren's row, even with wren in some other field.
// (No other field exists; this test confirms the sub claim is the
// only path.)
func TestAspectSelfEdit_CannotEditOther(t *testing.T) {
	rig := newSelfEditTestRig(t)
	for _, name := range []string{"plumb", "wren"} {
		if err := rig.store.Insert(context.Background(), aspects.Aspect{
			Name: name, AspectPubkey: fakePubkeyBytes(),
			Provider: "p", Model: "m",
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if err := rig.store.PersonalitySet(context.Background(), aspects.Personality{
		AspectName: "wren", NexusMD: "wren-original", SoulMD: "wren-soul",
	}); err != nil {
		t.Fatalf("set wren: %v", err)
	}

	// plumb's JWT, attempting to write content meant for wren.
	tok := rig.mintSessionJWT(t, "plumb")
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/aspect/personality",
		bytesReader(`{"nexus_md":"INJECTED","soul_md":"INJECTED","primer_md":""}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("plumb's edit of plumb returned %d; want 200", resp.StatusCode)
	}

	// Verify wren is unchanged (its content didn't get clobbered) and
	// plumb received the injection (because it's plumb's own row).
	wren, _ := rig.store.PersonalityGet(context.Background(), "wren")
	if wren.NexusMD != "wren-original" {
		t.Errorf("wren clobbered by plumb's edit: %q", wren.NexusMD)
	}
	plumb, _ := rig.store.PersonalityGet(context.Background(), "plumb")
	if plumb.NexusMD != "INJECTED" {
		t.Errorf("plumb's own row not updated: %q", plumb.NexusMD)
	}
}

// TestAspectSelfEdit_RejectsAdminBearer — spec §4.2 invariant:
// "Admin bearers also cannot use this endpoint — they must go
// through /api/admin/aspect/<name>/personality."
//
// In practice this is enforced structurally because the self-edit
// handler only accepts HS256 JWTs (via jwt.Verify), and the admin
// bearer minted by TokenStore is an opaque random token, not a JWT.
// jwt.Verify rejects it because the structure isn't a 3-part JWT.
// This test pins the structural rejection so a future code change
// that introduces a JWT-based admin token path doesn't silently
// admit admin tokens here.
func TestAspectSelfEdit_RejectsAdminBearer(t *testing.T) {
	rig := newSelfEditTestRig(t)
	if err := rig.store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Mint an admin TokenStore bearer (the kind that gates
	// /api/admin/* endpoints). Opaque random string, not a JWT.
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	adminBearer := tokens.TokenForAgent("frame")

	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/aspect/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Authorization", "Bearer "+adminBearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("admin bearer to self-edit endpoint = %d; want 401", resp.StatusCode)
	}
}

// TestAspectSelfEdit_RejectsBadJWT — invalid signature → 401.
func TestAspectSelfEdit_RejectsBadJWT(t *testing.T) {
	rig := newSelfEditTestRig(t)
	if err := rig.store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: fakePubkeyBytes(),
		Provider: "p", Model: "m",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tampered token — sign with a different secret.
	wrongSec := []byte("wrong-secret-32-bytes-padding-xx")
	now := time.Now()
	bad, err := jwt.Sign(wrongSec, jwt.Claims{
		Iss: "nexus://test-nexus", Sub: "plumb",
		Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(), Kfv: 1, Ses: "x",
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/aspect/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Authorization", "Bearer "+bad)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
}

// TestAspectSelfEdit_RejectsMissingBearer — no Authorization header → 401.
func TestAspectSelfEdit_RejectsMissingBearer(t *testing.T) {
	rig := newSelfEditTestRig(t)
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/aspect/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
}

// TestAspectSelfEdit_AspectGone — JWT valid but the aspect row was
// retired/deleted between mint and edit → 404.
func TestAspectSelfEdit_AspectGone(t *testing.T) {
	rig := newSelfEditTestRig(t)
	tok := rig.mintSessionJWT(t, "ghost")
	req, _ := http.NewRequest("PUT", rig.srv.URL+"/api/aspect/personality",
		bytesReader(`{"nexus_md":"x","soul_md":"y","primer_md":"z"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
}
