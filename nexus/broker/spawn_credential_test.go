package broker

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func newMintFixture(t *testing.T) (*KeyfileValidator, *aspects.SQLStore) {
	t.Helper()
	db, err := storage.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)
	v := &KeyfileValidator{
		NexusID:              "test-nexus",
		SessionSigningSecret: []byte("fixture-secret-32-bytes-padding-x"),
		Store:                store,
		JWTTTL:               time.Hour,
	}
	return v, store
}

// The mint round-trip: the derived credential is a broker-signed
// session JWT (sub=<parent>.sub-N, kfv=parent's) — the same shape the
// WS upgrade's tryVerifyAspectJWT accepts, so a hand Job boots with it
// and no keyfile.
func TestMintDerivedCredentialRoundTrip(t *testing.T) {
	v, store := newMintFixture(t)
	if err := store.Insert(context.Background(), aspects.Aspect{
		Name:         "plumb",
		AspectPubkey: fakePubkeyBytes(),
	}); err != nil {
		t.Fatal(err)
	}

	tok, err := v.MintDerivedCredential(context.Background(), "plumb", "plumb.sub-1")
	if err != nil {
		t.Fatalf("MintDerivedCredential: %v", err)
	}
	claims, err := jwt.Verify(v.SessionSigningSecret, tok, time.Now())
	if err != nil {
		t.Fatalf("derived credential failed verify: %v", err)
	}
	if claims.Sub != "plumb.sub-1" {
		t.Errorf("sub = %q", claims.Sub)
	}
	if claims.Kfv != 1 {
		t.Errorf("kfv = %d, want parent's 1", claims.Kfv)
	}
	if claims.Iss != "nexus://test-nexus" {
		t.Errorf("iss = %q", claims.Iss)
	}

	// The minted credential must resolve on the WS-upgrade auth path.
	b := New(Config{SessionSigningSecret: v.SessionSigningSecret}, nil)
	info, ok := b.tryVerifyAspectJWT(tok)
	if !ok {
		t.Fatal("tryVerifyAspectJWT rejected the derived credential")
	}
	if info.AgentID != "plumb.sub-1" || info.Admin || info.Operator {
		t.Fatalf("TokenInfo = %+v", info)
	}
}

func TestMintDerivedCredentialRejections(t *testing.T) {
	v, store := newMintFixture(t)
	if err := store.Insert(context.Background(), aspects.Aspect{
		Name:         "plumb",
		AspectPubkey: fakePubkeyBytes(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := v.MintDerivedCredential(context.Background(), "ghost", "ghost.sub-1"); err == nil {
		t.Error("unknown parent must not mint")
	}
	if _, err := v.MintDerivedCredential(context.Background(), "plumb", "anvil.sub-1"); err == nil {
		t.Error("lineage mismatch must not mint")
	}
	if _, err := v.MintDerivedCredential(context.Background(), "plumb", "plumb"); err == nil {
		t.Error("non-derived name must not mint")
	}
	var nilV *KeyfileValidator
	if _, err := nilV.MintDerivedCredential(context.Background(), "plumb", "plumb.sub-1"); err == nil {
		t.Error("nil validator must error, not panic")
	}
	if _, err := v.MintDerivedCredential(context.Background(), "plumb", "plumb.sub-1"); err != nil {
		t.Errorf("happy path after rejections: %v", err)
	}
	// Error text should not leak the signing secret (sanity).
	if _, err := v.MintDerivedCredential(context.Background(), "ghost", "ghost.sub-1"); err != nil &&
		strings.Contains(err.Error(), string(v.SessionSigningSecret)) {
		t.Error("error leaks signing material")
	}
}

// A connected hand can session.refresh: the broker resolves the BASE
// row for kfv/retired state and reissues for the derived sub.
func TestSessionRefreshForDerivedIdentity(t *testing.T) {
	rig := newSessionRefreshRig(t)
	rig.insertAspect(t, "plumb", 2)
	tok := rig.mintAspectJWT(t, "plumb.sub-1")
	c := dialAspectWS(t, rig.srv, tok)

	req, err := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{Reason: "hand refresh"})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, req)
	resp := recvFrame(t, c)
	if resp.Kind != frames.KindSessionRefreshResult {
		t.Fatalf("kind = %q (raw=%s)", resp.Kind, string(resp.Payload))
	}
	var p frames.SessionRefreshResultPayload
	if err := frames.PayloadAs(resp, &p); err != nil {
		t.Fatal(err)
	}
	claims, err := jwt.Verify(rig.signingSec, p.SessionJWT, rig.clock())
	if err != nil {
		t.Fatalf("refreshed hand JWT failed verify: %v", err)
	}
	if claims.Sub != "plumb.sub-1" {
		t.Errorf("sub = %q, want the derived name back", claims.Sub)
	}
	if claims.Kfv != 2 {
		t.Errorf("kfv = %d, want base row's 2", claims.Kfv)
	}
}

// Derived names are never in the discovery map — that's expected, not
// the legacy path: a hand inherits its BASE aspect's discovered home
// (and registers without the discovery-map WARN).
func TestRegisterDerivedNameInheritsBaseHome(t *testing.T) {
	r := roster.New()
	b := New(Config{
		AuthToken:          "testtoken",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
		AspectHomes:        map[string]string{"plumb": "/homes/plumb"},
	}, r)
	b.ctx, b.ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(b.ctxCancel)
	srv := httptest.NewServer(newMux(b))
	t.Cleanup(srv.Close)

	c := dialWS(t, srv, "testtoken")
	registerAspect(t, c, "plumb.sub-1")

	state, ok := r.Get("plumb.sub-1")
	if !ok {
		t.Fatal("hand not in roster after register")
	}
	if state.Home != "/homes/plumb" {
		t.Errorf("Home = %q, want the base aspect's discovered home", state.Home)
	}
	if state.Lineage != "plumb" {
		t.Errorf("Lineage = %q, want plumb", state.Lineage)
	}
}
