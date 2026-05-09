package broker

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
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

// Regression for issue #24: ResolveToken must do constant-time
// compares against every registered token, not a Go map lookup. Map
// ops branch on hash bucket layout, so a hit vs miss has measurably
// different timing.
//
// We assert behavior, not timing (timing tests are flaky on CI).
// Specifically: every candidate path (registered hit, legacy hit,
// total miss with various lengths) returns the right outcome, with
// the same TokenInfo a hit-by-hit-on-second-call returns. Plus a
// short-circuit-detector: after a "successful" earlier match in the
// loop body, a later wrong candidate must NOT cause the result to
// reset. The current implementation captures via if eq == 1 { hit = ... },
// so this confirms the body sees stable hit assignment.
func TestResolveToken_HandlesAllPaths(t *testing.T) {
	store := NewTokenStore()
	store.SetTokenForTest("wren", "wrentoken", false)
	store.SetTokenForTest("anvil", "anviltoken", false)
	store.SetLegacyMaster("legacymaster")

	cases := []struct {
		name   string
		token  string
		wantOK bool
		wantID string
		wantAd bool
	}{
		{"registered aspect", "wrentoken", true, "wren", false},
		{"registered other aspect", "anviltoken", true, "anvil", false},
		{"legacy master", "legacymaster", true, FrameAgentID, true},
		{"miss same length as wren", "wrongtoken", false, "", false},
		{"miss longer", "wrongtoken-extra-bytes-padding", false, "", false},
		{"miss shorter", "wt", false, "", false},
		{"miss empty (early return)", "", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, ok := store.ResolveToken(c.token)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if info.AgentID != c.wantID {
				t.Errorf("AgentID = %q, want %q", info.AgentID, c.wantID)
			}
			if info.Admin != c.wantAd {
				t.Errorf("Admin = %v, want %v", info.Admin, c.wantAd)
			}
		})
	}
}

// ExtractBearer must return empty for any non-bearer header without
// branching on header bytes. Pre-fix used == compare which leaked
// "is this a bearer header" via early-return timing.
func TestExtractBearer_ConstantTime(t *testing.T) {
	cases := map[string]string{
		"Bearer abc123": "abc123",
		"bearer abc123": "", // case-sensitive; the lib expects exactly "Bearer "
		"Basic abc123":  "",
		"":              "",
		"Bearer":        "", // no space, too short
		"Bearer ":       "", // no token after space
		"Token abc":     "",
		"   ":           "",
	}
	for in, want := range cases {
		if got := ExtractBearer(in); got != want {
			t.Errorf("ExtractBearer(%q) = %q, want %q", in, got, want)
		}
	}
}

// Regression for issue #31 / PR-A2.3: when AllowLegacyMaster is false
// (default), an AuthToken passed to broker.New must NOT be promoted
// to a legacy-master fallback — presenting it must fail to resolve.
// When AllowLegacyMaster is true, it resolves with ViaLegacy=true so
// connect handlers can WARN.
func TestLegacyMaster_GatedByAllowFlag(t *testing.T) {
	// Default off: AuthToken set, AllowLegacyMaster false → no legacy
	// fallback; presenting the token doesn't resolve.
	bOff := New(Config{AuthToken: "legacytok", HeartbeatIntervalS: 15}, nil)
	if _, ok := bOff.cfg.Tokens.ResolveToken("legacytok"); ok {
		t.Error("legacy master resolved despite AllowLegacyMaster=false")
	}

	// Opt-in: AllowLegacyMaster true → token resolves with ViaLegacy=true.
	bOn := New(Config{
		AuthToken:          "legacytok",
		AllowLegacyMaster:  true,
		HeartbeatIntervalS: 15,
	}, nil)
	info, ok := bOn.cfg.Tokens.ResolveToken("legacytok")
	if !ok {
		t.Fatal("legacy master not resolved despite AllowLegacyMaster=true")
	}
	if info.AgentID != FrameAgentID {
		t.Errorf("AgentID = %q, want %q", info.AgentID, FrameAgentID)
	}
	if !info.Admin {
		t.Error("Admin = false, want true")
	}
	if !info.ViaLegacy {
		t.Error("ViaLegacy = false; connect handler won't WARN")
	}
}

// Regression: if a per-aspect token's bytes happen to match the
// legacy master string, the per-aspect identity wins — never silently
// elevate to FrameAgentID/admin/ViaLegacy.
func TestPerAspectWinsOnTokenCollision(t *testing.T) {
	store := NewTokenStore()
	store.SetTokenForTest("wren", "shared-bytes", false)
	store.SetLegacyMaster("shared-bytes")

	info, ok := store.ResolveToken("shared-bytes")
	if !ok {
		t.Fatal("expected resolve")
	}
	if info.AgentID != "wren" {
		t.Errorf("AgentID = %q, want wren (per-aspect must win on collision)", info.AgentID)
	}
	if info.Admin {
		t.Error("Admin = true (legacy master overwrote per-aspect identity)")
	}
	if info.ViaLegacy {
		t.Error("ViaLegacy = true (legacy branch ran despite per-aspect match)")
	}
}

// Per-aspect tokens never carry ViaLegacy=true.
func TestPerAspectTokensNotMarkedViaLegacy(t *testing.T) {
	store := NewTokenStore()
	store.SetTokenForTest("wren", "wrentok", false)
	info, ok := store.ResolveToken("wrentok")
	if !ok {
		t.Fatal("expected resolve")
	}
	if info.ViaLegacy {
		t.Error("per-aspect token resolved with ViaLegacy=true")
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
