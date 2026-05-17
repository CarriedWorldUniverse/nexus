package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// keyfileEndpointFixture builds an in-memory broker with both keyfile
// endpoints registered and a minted keyfile ready to validate. Returns
// a test server and the encrypted_payload for "plumb" v1.
type keyfileEndpointFixture struct {
	srv          *httptest.Server
	encrypted    string
	nexusID      string
	signingSec   []byte
	store        *aspects.SQLStore
	creds        *credentials.Store
	serverPubKey ed25519.PublicKey
}

func newKeyfileEndpointFixture(t *testing.T) *keyfileEndpointFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)

	serverPub, serverPriv, _ := ed25519.GenerateKey(rand.Reader)
	aspectPub, aspectPriv, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Insert(context.Background(), aspects.Aspect{
		Name: "plumb", AspectPubkey: aspectPub,
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	kf, _, err := aspects.Mint(aspects.MintInput{
		AspectName: "plumb", KeyfileVersion: 1,
		AspectPrivkey: aspectPriv, ServerPubkey: serverPub,
		NexusID: "fixture-nexus", NexusURL: "wss://x", MintedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	signingSec := []byte("fixture-secret-32-bytes-padding-x")

	// NEX-169: wire a real credentials.Store so tests can exercise the
	// validate-response mcp_profile path end-to-end (Set credential,
	// SetMCPProfile, POST validate, assert resolved profile on the wire).
	cstore, err := credentials.NewStore(db, signingSec)
	if err != nil {
		t.Fatalf("credentials.NewStore: %v", err)
	}

	cfg := Config{
		KeyfileValidator: &KeyfileValidator{
			NexusID:              "fixture-nexus",
			ServerEd25519Pubkey:  serverPub,
			ServerEd25519Privkey: serverPriv,
			SessionSigningSecret: signingSec,
			Store:                store,
			Credentials:          cstore,
			JWTTTL:               time.Hour,
		},
	}
	b := &Broker{cfg: cfg, log: discardLogger()}

	mux := http.NewServeMux()
	b.registerKeyfileEndpoints(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &keyfileEndpointFixture{
		srv: srv, encrypted: kf.EncryptedPayload,
		nexusID: "fixture-nexus", signingSec: signingSec,
		store: store, creds: cstore, serverPubKey: serverPub,
	}
}

// TestEndpoint_NexusID — serves the configured nexus_id at GET
// /api/nexus_id with no auth required. agentfunnel hits this before
// sending the encrypted payload.
func TestEndpoint_NexusID(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/nexus_id")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	var got nexusIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NexusID != f.nexusID {
		t.Errorf("nexus_id = %q; want %q", got.NexusID, f.nexusID)
	}
}

// TestEndpoint_Validate_HappyPath — POST a real encrypted_payload, get
// a 200 with JWT + personality bundle. Verifies the JWT against the
// fixture's secret to confirm the wire output is internally consistent.
func TestEndpoint_Validate_HappyPath(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	body, _ := json.Marshal(validateRequest{EncryptedPayload: f.encrypted})
	resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, raw)
	}
	var got validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.SessionJWT == "" {
		t.Errorf("ok=%v jwt=%q", got.OK, got.SessionJWT)
	}
	if got.Provider != "claude-api" || got.Model != "claude-opus-4-7" {
		t.Errorf("provider/model wrong: %q / %q", got.Provider, got.Model)
	}
	if got.SessionExpiresAt == "" {
		t.Error("session_expires_at empty")
	}
	// Verify the issued JWT round-trips.
	claims, err := jwt.Verify(f.signingSec, got.SessionJWT, time.Now())
	if err != nil {
		t.Fatalf("issued JWT failed verify: %v", err)
	}
	if claims.Sub != "plumb" || claims.Kfv != 1 || claims.Iss != "nexus://fixture-nexus" {
		t.Errorf("claims wrong: %+v", claims)
	}
}

// TestEndpoint_Validate_StatusCodeMapping — every sentinel in
// aspects.Validate maps to the spec §5 status code on the wire.
func TestEndpoint_Validate_StatusCodeMapping(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*keyfileEndpointFixture, *validateRequest)
		wantStatus int
		wantSub    string
	}{
		{
			name: "decryption failed (cross-Nexus seal)",
			mutate: func(f *keyfileEndpointFixture, req *validateRequest) {
				otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
				_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
				kf, _, _ := aspects.Mint(aspects.MintInput{
					AspectName: "plumb", KeyfileVersion: 1,
					AspectPrivkey: otherPriv, ServerPubkey: otherPub,
					NexusID: "x", NexusURL: "wss://x", MintedAt: time.Now(),
				})
				req.EncryptedPayload = kf.EncryptedPayload
			},
			wantStatus: http.StatusUnauthorized,
			wantSub:    "decryption failed",
		},
		{
			name: "unknown aspect (row deleted)",
			mutate: func(f *keyfileEndpointFixture, req *validateRequest) {
				_, _ = f.store, req // payload unchanged; mutate the DB instead
				// Delete via raw SQL through the store's helper exec path.
				// The fixture exposes SQLStore so we can reach for its db.
				// Easiest: SetStatus to a junk value won't fly (CHECK).
				// Cleanest: re-derive a payload for an unknown aspect_name.
				otherPub := f.serverPubKey
				_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
				kf, _, _ := aspects.Mint(aspects.MintInput{
					AspectName: "nobody", KeyfileVersion: 1,
					AspectPrivkey: otherPriv, ServerPubkey: otherPub,
					NexusID: f.nexusID, NexusURL: "wss://x", MintedAt: time.Now(),
				})
				req.EncryptedPayload = kf.EncryptedPayload
			},
			wantStatus: http.StatusNotFound,
			wantSub:    "unknown aspect",
		},
		{
			name: "retired",
			mutate: func(f *keyfileEndpointFixture, req *validateRequest) {
				_ = f.store.SetStatus(context.Background(), "plumb", aspects.StatusRetired)
			},
			wantStatus: http.StatusForbidden,
			wantSub:    "retired",
		},
		{
			name: "revoked (version too low after re-mint)",
			mutate: func(f *keyfileEndpointFixture, req *validateRequest) {
				newPub, _, _ := ed25519.GenerateKey(rand.Reader)
				_, _ = f.store.BumpKeyfileVersion(context.Background(), "plumb", newPub)
			},
			wantStatus: http.StatusForbidden,
			wantSub:    "revoked",
		},
		{
			name:       "malformed payload (not base64)",
			mutate:     func(f *keyfileEndpointFixture, req *validateRequest) { req.EncryptedPayload = "###not-base64###" },
			wantStatus: http.StatusUnauthorized, // bad b64 → ErrDecryptionFailed → 401
			wantSub:    "decryption failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeyfileEndpointFixture(t)
			req := validateRequest{EncryptedPayload: f.encrypted}
			tc.mutate(f, &req)
			body, _ := json.Marshal(req)
			resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d; want %d (body=%s)", resp.StatusCode, tc.wantStatus, raw)
			}
			var got errorResponse
			_ = json.NewDecoder(resp.Body).Decode(&got)
			if !strings.Contains(got.Error, tc.wantSub) {
				t.Errorf("error = %q; want substring %q", got.Error, tc.wantSub)
			}
		})
	}
}

// TestEndpoint_Validate_RevokedIncludesCurrentVersion — spec §5
// requires the revoked response to surface the current version so
// agentfunnel can log "your keyfile is stale; current is N".
func TestEndpoint_Validate_RevokedIncludesCurrentVersion(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, _ = f.store.BumpKeyfileVersion(context.Background(), "plumb", newPub)

	body, _ := json.Marshal(validateRequest{EncryptedPayload: f.encrypted})
	resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403", resp.StatusCode)
	}
	var got errorResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.CurrentVersion != 2 {
		t.Errorf("current_version = %d; want 2", got.CurrentVersion)
	}
}

// TestEndpoint_Validate_BadRequestBody — empty body, bad JSON, missing
// field. All map to 400.
func TestEndpoint_Validate_BadRequestBody(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	cases := []struct{ name, body string }{
		{"empty", ""},
		{"bad json", "not json"},
		{"missing field", `{}`},
		{"wrong field", `{"foo":"bar"}`}, // DisallowUnknownFields catches this
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: status = %d; want 400", tc.name, resp.StatusCode)
			}
		})
	}
}

// TestEndpoint_Validate_MCPProfile_Empty — no mcp_profile row for the
// aspect → response.mcp_profile is "" but everything else is normal.
// Matches the "operator hasn't configured a profile yet" case (NEX-169).
func TestEndpoint_Validate_MCPProfile_Empty(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	body, _ := json.Marshal(validateRequest{EncryptedPayload: f.encrypted})
	resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, raw)
	}
	var got validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MCPProfile != "" {
		t.Errorf("mcp_profile = %q; want empty (no profile row seeded)", got.MCPProfile)
	}
}

// TestEndpoint_Validate_MCPProfile_Resolved — seed a credential + a
// profile referencing it. The validate response carries the rendered
// JSON with the placeholder substituted in (NEX-169).
func TestEndpoint_Validate_MCPProfile_Resolved(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	ctx := context.Background()

	// Seed a provider credential. Bundle.key is what the placeholder
	// resolves to.
	if err := f.creds.Set(ctx, credentials.UpsertParams{
		Name: "gh-pat", Kind: credentials.KindProvider,
		Bundle: map[string]any{
			"api_shape":     "anthropic",
			"base_url":      "https://example.invalid",
			"key":           "ghp-secret-token",
			"default_model": "claude-opus-4-7",
		},
		AllowedAspects: []string{"*"},
		Mode:           credentials.ModeFetch,
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	const profile = `{"mcpServers":{"github":{"command":"node","env":{"TOKEN":"${credential:gh-pat.key}"}}}}`
	if err := f.creds.SetMCPProfile(ctx, "plumb", profile); err != nil {
		t.Fatalf("SetMCPProfile: %v", err)
	}

	body, _ := json.Marshal(validateRequest{EncryptedPayload: f.encrypted})
	resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, raw)
	}
	var got validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	const want = `{"mcpServers":{"github":{"command":"node","env":{"TOKEN":"ghp-secret-token"}}}}`
	if got.MCPProfile != want {
		t.Errorf("mcp_profile mismatch:\n got  %q\n want %q", got.MCPProfile, want)
	}
}

// TestEndpoint_Validate_MCPProfile_SubstituteFailure — a profile that
// references an unknown credential → 500. Identity passed but the
// response is unusable; we fail loud rather than emit a half-resolved
// or empty profile that would silently boot the aspect without MCP
// (NEX-169).
func TestEndpoint_Validate_MCPProfile_SubstituteFailure(t *testing.T) {
	f := newKeyfileEndpointFixture(t)
	const profile = `{"mcpServers":{"github":{"env":{"TOKEN":"${credential:does-not-exist.key}"}}}}`
	if err := f.creds.SetMCPProfile(context.Background(), "plumb", profile); err != nil {
		t.Fatalf("SetMCPProfile: %v", err)
	}

	body, _ := json.Marshal(validateRequest{EncryptedPayload: f.encrypted})
	resp, err := http.Post(f.srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 500 (body=%s)", resp.StatusCode, raw)
	}
}

// TestEndpoint_Validate_MCPProfile_NoCredentialsStore — when the
// validator has no credentials store wired (legacy boot), the
// mcp_profile field is empty but validation still succeeds.
func TestEndpoint_Validate_MCPProfile_NoCredentialsStore(t *testing.T) {
	// Build a minimal broker with KeyfileValidator.Credentials nil. The
	// shared fixture wires a real credentials.Store, so re-using it here
	// would mask the nil-tolerance path. Mirror its setup directly.
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)
	serverPub, serverPriv, _ := ed25519.GenerateKey(rand.Reader)
	aspectPub, aspectPriv, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Insert(context.Background(), aspects.Aspect{
		Name: "legacy", AspectPubkey: aspectPub,
		Provider: "claude-api", Model: "claude-opus-4-7",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	kf, _, err := aspects.Mint(aspects.MintInput{
		AspectName: "legacy", KeyfileVersion: 1,
		AspectPrivkey: aspectPriv, ServerPubkey: serverPub,
		NexusID: "fixture-nexus", NexusURL: "wss://x", MintedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	signingSec := []byte("fixture-secret-32-bytes-padding-x")

	cfg := Config{
		KeyfileValidator: &KeyfileValidator{
			NexusID:              "fixture-nexus",
			ServerEd25519Pubkey:  serverPub,
			ServerEd25519Privkey: serverPriv,
			SessionSigningSecret: signingSec,
			Store:                store,
			// Credentials intentionally nil.
			JWTTTL: time.Hour,
		},
	}
	b := &Broker{cfg: cfg, log: discardLogger()}
	mux := http.NewServeMux()
	b.registerKeyfileEndpoints(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(validateRequest{EncryptedPayload: kf.EncryptedPayload})
	resp, err := http.Post(srv.URL+"/api/aspect/validate", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, raw)
	}
	var got validateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MCPProfile != "" {
		t.Errorf("mcp_profile = %q; want empty (no credentials store wired)", got.MCPProfile)
	}
}

// TestEndpoint_NotRegistered_WhenValidatorNil — confirms the endpoints
// are skipped when no KeyfileValidator is configured. Older deployments
// that don't enable keyfile auth still get a clean 404 from the mux.
func TestEndpoint_NotRegistered_WhenValidatorNil(t *testing.T) {
	b := &Broker{cfg: Config{}, log: discardLogger()}
	mux := http.NewServeMux()
	b.registerKeyfileEndpoints(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/nexus_id")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (route not registered)", resp.StatusCode)
	}
}
