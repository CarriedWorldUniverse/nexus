package broker

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/nexus-cw/nexus/nexus/storage"
)

// openAuthTestDB returns a fresh nexus.db in t.TempDir with the schema
// applied. Used by tests that need a real *sql.DB to exercise
// agent_tokens persistence.
func openAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestGenerateAgentToken(t *testing.T) {
	a, err := GenerateAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two generated tokens collided — randomness broken")
	}
	if len(a) != 64 { // 32 bytes hex-encoded
		t.Errorf("token length = %d, want 64 (32 bytes hex)", len(a))
	}
}

// TestReconcileAndResolve_RoundTrip pins the core pattern: mint
// per-aspect tokens at reconcile time, then resolve a presented token
// back to its identity. Aspects are NOT admin; frame IS admin.
func TestReconcileAndResolve_RoundTrip(t *testing.T) {
	db := openAuthTestDB(t)
	store := NewTokenStore()

	if err := store.ReconcileAgentTokens(context.Background(), db, []string{"wren", "anvil"}); err != nil {
		t.Fatalf("ReconcileAgentTokens: %v", err)
	}
	frameToken, err := store.ReconcileFrameToken(context.Background(), db)
	if err != nil {
		t.Fatalf("ReconcileFrameToken: %v", err)
	}

	wrenToken := store.TokenForAgent("wren")
	if wrenToken == "" {
		t.Fatal("wren token not set after reconcile")
	}
	anvilToken := store.TokenForAgent("anvil")
	if anvilToken == "" {
		t.Fatal("anvil token not set after reconcile")
	}
	if wrenToken == anvilToken {
		t.Error("wren and anvil got the same token")
	}
	if frameToken == "" || frameToken == wrenToken {
		t.Error("frame token missing or collides with aspect token")
	}

	// Resolve.
	info, ok := store.ResolveToken(wrenToken)
	if !ok || info.AgentID != "wren" || info.Admin {
		t.Errorf("resolve(wren) = %+v ok=%v; want wren, admin=false", info, ok)
	}
	info, ok = store.ResolveToken(frameToken)
	if !ok || info.AgentID != FrameAgentID || !info.Admin {
		t.Errorf("resolve(frame) = %+v ok=%v; want frame, admin=true", info, ok)
	}
	if _, ok := store.ResolveToken("not-a-real-token"); ok {
		t.Error("resolve of bogus token returned ok=true")
	}
	if _, ok := store.ResolveToken(""); ok {
		t.Error("resolve of empty token returned ok=true")
	}
}

// TestReconcile_Idempotent — calling reconcile twice with the same
// ids preserves the same token. This is critical for broker restarts:
// aspects' env-injected NEXUS_TOKEN must survive across nexus daemon
// restarts.
func TestReconcile_Idempotent(t *testing.T) {
	db := openAuthTestDB(t)

	store1 := NewTokenStore()
	if err := store1.ReconcileAgentTokens(context.Background(), db, []string{"wren"}); err != nil {
		t.Fatal(err)
	}
	t1 := store1.TokenForAgent("wren")

	// Fresh store, same DB — should load the existing token, not mint
	// a new one.
	store2 := NewTokenStore()
	if err := store2.ReconcileAgentTokens(context.Background(), db, []string{"wren"}); err != nil {
		t.Fatal(err)
	}
	t2 := store2.TokenForAgent("wren")

	if t1 != t2 {
		t.Errorf("token rotated across reconciles: %q vs %q (broker restart would invalidate aspects)", t1, t2)
	}
}

// TestLegacyMaster_ResolvesAsFrame — the back-compat path: the shared
// AuthToken (Config.AuthToken pre-drift-C) resolves to FrameAgentID +
// admin=true. Existing tests and autospawn keep working until per-
// aspect token injection lands in the autospawn pipeline.
func TestLegacyMaster_ResolvesAsFrame(t *testing.T) {
	store := NewTokenStore()
	store.SetLegacyMaster("legacymaster")

	info, ok := store.ResolveToken("legacymaster")
	if !ok {
		t.Fatal("legacy master token did not resolve")
	}
	if info.AgentID != FrameAgentID {
		t.Errorf("legacy master resolved to %q, want %q", info.AgentID, FrameAgentID)
	}
	if !info.Admin {
		t.Error("legacy master should resolve with admin=true")
	}

	// A wrong token still fails.
	if _, ok := store.ResolveToken("not-the-master"); ok {
		t.Error("non-master token resolved against legacy fallback")
	}
}

// TestRequireAdmin pins the helper Drift D will use on override
// handlers. Admin passes; non-admin returns ErrAdminRequired so the
// caller can map it to a structured 403.
func TestRequireAdmin(t *testing.T) {
	if err := RequireAdmin(TokenInfo{AgentID: "wren", Admin: false}); err == nil {
		t.Error("non-admin should fail RequireAdmin")
	} else if !errors.Is(err, ErrAdminRequired) {
		t.Errorf("err = %v, want ErrAdminRequired", err)
	}
	if err := RequireAdmin(TokenInfo{AgentID: FrameAgentID, Admin: true}); err != nil {
		t.Errorf("frame should pass RequireAdmin, got %v", err)
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc123", "abc123"},
		{"bearer abc123", ""}, // case-sensitive per existing behaviour
		{"abc123", ""},
		{"", ""},
		{"Bearer ", ""}, // empty payload
	}
	for _, c := range cases {
		got := ExtractBearer(c.header)
		if got != c.want {
			t.Errorf("ExtractBearer(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}

// SetTokenForTest hooks the test path so we can install known tokens
// without the DB round-trip.
func TestSetTokenForTest(t *testing.T) {
	store := NewTokenStore()
	store.SetTokenForTest("wren", "wrentok", false)
	store.SetTokenForTest(FrameAgentID, "frametok", true)

	if got := store.TokenForAgent("wren"); got != "wrentok" {
		t.Errorf("TokenForAgent(wren) = %q", got)
	}
	info, ok := store.ResolveToken("wrentok")
	if !ok || info.AgentID != "wren" || info.Admin {
		t.Errorf("resolve(wrentok) = %+v ok=%v", info, ok)
	}
	info, ok = store.ResolveToken("frametok")
	if !ok || !info.Admin {
		t.Errorf("frame resolve = %+v ok=%v", info, ok)
	}
}
