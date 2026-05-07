package keyfile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleKeyfileJSON = `{
  "version": 1,
  "format": "nexus-keyfile-v1",
  "envelope": {
    "nexus_url": "wss://test.example/connect",
    "nexus_id": "test-nexus-id",
    "issued_at": "2026-05-08T10:00:00Z"
  },
  "encrypted_payload": "AAAA"
}`

// fakeJWT builds a parseable HS256-shaped token whose sub claim is the
// supplied aspect name. Signature segment is bogus (we don't verify it
// in this package), but the structure must be three base64url segments.
func fakeJWT(t *testing.T, sub string) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	cls, _ := json.Marshal(map[string]any{"sub": sub, "exp": time.Now().Add(time.Hour).Unix()})
	enc := base64.RawURLEncoding.EncodeToString
	return enc(hdr) + "." + enc(cls) + "." + enc([]byte("not-a-real-sig"))
}

// writeFile is a test helper for keyfile-on-disk fixtures.
func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.key")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestLoad_HappyPath — round-trip parse of a valid keyfile.
func TestLoad_HappyPath(t *testing.T) {
	path := writeFile(t, sampleKeyfileJSON)
	kf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if kf.Format != "nexus-keyfile-v1" || kf.Version != 1 {
		t.Errorf("format/version: %q %d", kf.Format, kf.Version)
	}
	if kf.Envelope.NexusURL != "wss://test.example/connect" {
		t.Errorf("nexus_url wrong: %q", kf.Envelope.NexusURL)
	}
}

// TestLoad_BadShape covers the parse-side rejections — every gate in
// Load() is its own branch.
func TestLoad_BadShape(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"not json", "this isn't json"},
		{"wrong format", `{"version":1,"format":"other","envelope":{"nexus_url":"wss://x","nexus_id":"y"},"encrypted_payload":"z"}`},
		{"wrong version", `{"version":99,"format":"nexus-keyfile-v1","envelope":{"nexus_url":"wss://x","nexus_id":"y"},"encrypted_payload":"z"}`},
		{"missing url", `{"version":1,"format":"nexus-keyfile-v1","envelope":{"nexus_id":"y"},"encrypted_payload":"z"}`},
		{"missing id", `{"version":1,"format":"nexus-keyfile-v1","envelope":{"nexus_url":"wss://x"},"encrypted_payload":"z"}`},
		{"empty payload", `{"version":1,"format":"nexus-keyfile-v1","envelope":{"nexus_url":"wss://x","nexus_id":"y"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, tc.body)
			_, err := Load(path)
			if !errors.Is(err, ErrBadKeyfile) {
				t.Errorf("Load %q = %v; want ErrBadKeyfile", tc.name, err)
			}
		})
	}
}

// TestLoad_FileMissing — bad path → ErrBadKeyfile too.
func TestLoad_FileMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "no-such-file.key"))
	if !errors.Is(err, ErrBadKeyfile) {
		t.Errorf("Load missing = %v; want ErrBadKeyfile", err)
	}
}

// fakeNexusServer returns an httptest.Server that mimics the spec §5
// endpoints. The behaviour is configurable via the closures so tests
// can exercise specific failure modes without spinning up a real Nexus.
func fakeNexusServer(t *testing.T, opts fakeServerOpts) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nexus_id", func(w http.ResponseWriter, r *http.Request) {
		if opts.nexusIDStatus != 0 {
			w.WriteHeader(opts.nexusIDStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"nexus_id": opts.nexusID})
	})
	mux.HandleFunc("/api/aspect/validate", func(w http.ResponseWriter, r *http.Request) {
		if opts.validateStatus != 0 {
			w.WriteHeader(opts.validateStatus)
			_, _ = w.Write([]byte(opts.validateBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                 true,
			"session_jwt":        opts.jwt,
			"session_expires_at": opts.expiresAt,
			"personality": map[string]any{
				"nexus_md": "## plumb", "soul_md": "soul", "primer_md": "primer",
				"composed": "## plumb\n\nsoul\n\nprimer", "version": 1, "updated_at": "2026-05-08T10:00:00Z",
			},
			"provider": "claude-api", "model": "claude-opus-4-7",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type fakeServerOpts struct {
	nexusID        string // returned by /api/nexus_id when status==0
	nexusIDStatus  int    // override; 0 → 200 with body
	validateStatus int    // override; 0 → 200 with body
	validateBody   string // override body for non-2xx
	jwt            string // session_jwt to return
	expiresAt      string // session_expires_at to return (RFC3339)
}

// TestValidate_HappyPath — end-to-end against a fake Nexus that mirrors
// the spec §5 success shape. Confirms ValidationResult is populated
// from both the GET and POST responses + the JWT sub claim.
func TestValidate_HappyPath(t *testing.T) {
	srv := fakeNexusServer(t, fakeServerOpts{
		nexusID:   "matching-id",
		jwt:       fakeJWT(t, "plumb"),
		expiresAt: "2026-05-08T11:00:00Z",
	})

	kf := &Keyfile{
		Version: 1, Format: expectedFormat,
		Envelope: Envelope{
			NexusURL: srv.URL + "/connect",
			NexusID:  "matching-id",
		},
		EncryptedPayload: "AAAA",
	}
	c := &Client{HTTP: srv.Client()}
	res, err := c.Validate(context.Background(), kf)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.AspectName != "plumb" {
		t.Errorf("AspectName = %q; want plumb", res.AspectName)
	}
	if res.Provider != "claude-api" || res.Model != "claude-opus-4-7" {
		t.Errorf("provider/model: %q / %q", res.Provider, res.Model)
	}
	if res.Personality.Composed == "" {
		t.Error("personality.composed empty")
	}
	if res.SessionExpiresAt.IsZero() {
		t.Error("SessionExpiresAt zero")
	}
}

// TestValidate_NexusMismatch — server's nexus_id ≠ envelope's. Must
// abort before sending the encrypted_payload (we can't see the request
// went out, but the error sentinel guarantees Validate returned without
// reaching POST).
func TestValidate_NexusMismatch(t *testing.T) {
	srv := fakeNexusServer(t, fakeServerOpts{nexusID: "wrong-id"})
	kf := &Keyfile{
		Version: 1, Format: expectedFormat,
		Envelope:         Envelope{NexusURL: srv.URL + "/connect", NexusID: "expected-id"},
		EncryptedPayload: "AAAA",
	}
	c := &Client{HTTP: srv.Client()}
	_, err := c.Validate(context.Background(), kf)
	if !errors.Is(err, ErrNexusMismatch) {
		t.Errorf("Validate mismatch = %v; want ErrNexusMismatch", err)
	}
}

// TestValidate_NexusIDEndpointFails — the nexus_id check itself
// errors (e.g. 503 because validator not configured).
func TestValidate_NexusIDEndpointFails(t *testing.T) {
	srv := fakeNexusServer(t, fakeServerOpts{nexusIDStatus: http.StatusServiceUnavailable})
	kf := &Keyfile{
		Version: 1, Format: expectedFormat,
		Envelope:         Envelope{NexusURL: srv.URL + "/connect", NexusID: "x"},
		EncryptedPayload: "AAAA",
	}
	c := &Client{HTTP: srv.Client()}
	_, err := c.Validate(context.Background(), kf)
	if err == nil {
		t.Error("expected error from 503; got nil")
	}
}

// TestValidate_ValidationRejected — 403 revoked. Body is bubbled up
// verbatim so the operator/aspect can log "current_version=N".
func TestValidate_ValidationRejected(t *testing.T) {
	srv := fakeNexusServer(t, fakeServerOpts{
		nexusID:        "match",
		validateStatus: http.StatusForbidden,
		validateBody:   `{"error":"revoked","current_version":3}`,
	})
	kf := &Keyfile{
		Version: 1, Format: expectedFormat,
		Envelope:         Envelope{NexusURL: srv.URL + "/connect", NexusID: "match"},
		EncryptedPayload: "AAAA",
	}
	c := &Client{HTTP: srv.Client()}
	_, err := c.Validate(context.Background(), kf)
	if !errors.Is(err, ErrValidationRejected) {
		t.Fatalf("Validate revoked = %v; want ErrValidationRejected", err)
	}
	if !strings.Contains(err.Error(), "current_version") {
		t.Errorf("error %q does not include body — operator can't see the version hint", err)
	}
}

// TestValidate_BadServerResponse_OkFalse — server returns 200 with
// ok=false (a server bug — spec says non-2xx for failures). Must
// surface ErrBadServerResponse, not ErrValidationRejected, so
// agentfunnel doesn't suggest re-minting for a Nexus-side bug.
func TestValidate_BadServerResponse_OkFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/nexus_id":
			_ = json.NewEncoder(w).Encode(map[string]string{"nexus_id": "match"})
		case "/api/aspect/validate":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "session_jwt": ""})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	kf := &Keyfile{
		Version: 1, Format: expectedFormat,
		Envelope:         Envelope{NexusURL: srv.URL + "/connect", NexusID: "match"},
		EncryptedPayload: "AAAA",
	}
	c := &Client{HTTP: srv.Client()}
	_, err := c.Validate(context.Background(), kf)
	if !errors.Is(err, ErrBadServerResponse) {
		t.Errorf("Validate ok=false on 200 = %v; want ErrBadServerResponse", err)
	}
	if errors.Is(err, ErrValidationRejected) {
		t.Errorf("Validate ok=false on 200 also matched ErrValidationRejected — sentinels not distinct")
	}
}

// TestValidate_BadJWT — server returns a 200 with a bogus JWT shape.
// Validate must surface a parse error rather than silently returning
// an empty AspectName.
func TestValidate_BadJWT(t *testing.T) {
	srv := fakeNexusServer(t, fakeServerOpts{
		nexusID: "match", jwt: "not.a.valid.jwt.shape",
		expiresAt: "2026-05-08T11:00:00Z",
	})
	kf := &Keyfile{
		Version: 1, Format: expectedFormat,
		Envelope:         Envelope{NexusURL: srv.URL + "/connect", NexusID: "match"},
		EncryptedPayload: "AAAA",
	}
	c := &Client{HTTP: srv.Client()}
	_, err := c.Validate(context.Background(), kf)
	if err == nil {
		t.Error("Validate with bogus JWT shape: expected error; got nil")
	}
}

// TestWsToHTTPS exhaustively checks the URL rewriting since
// agentfunnel's correctness hinges on hitting the right base URL.
func TestWsToHTTPS(t *testing.T) {
	cases := []struct {
		in, want, errStr string
	}{
		{"wss://x.example/connect", "https://x.example", ""},
		{"wss://x.example:7888/connect", "https://x.example:7888", ""},
		{"ws://localhost/connect", "http://localhost", ""},
		{"https://x.example", "https://x.example", ""},
		{"https://x.example/", "https://x.example", ""},
		{"http://localhost:7888/connect", "http://localhost:7888", ""},
		{"ftp://wat", "", "scheme not"},
		{"", "", "scheme not"},
		// Non-/connect paths must error rather than silently produce a
		// wrong base URL — operator typos surface clearly.
		{"wss://x.example/some/other/path", "", "unexpected path"},
		{"wss://x.example/api/aspect/validate", "", "unexpected path"},
	}
	for _, tc := range cases {
		got, err := wsToHTTPS(tc.in)
		if tc.errStr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("wsToHTTPS(%q) = err=%v; want error containing %q", tc.in, err, tc.errStr)
			}
			continue
		}
		if err != nil {
			t.Errorf("wsToHTTPS(%q) error = %v; want nil", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("wsToHTTPS(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestJwtSub — the sub-only parser. No signature verification; just
// confirms the claim extraction works against a well-formed token and
// rejects malformed ones.
func TestJwtSub(t *testing.T) {
	// Happy path.
	tok := fakeJWT(t, "plumb")
	sub, err := jwtSub(tok)
	if err != nil || sub != "plumb" {
		t.Errorf("jwtSub good = %q,%v; want plumb,nil", sub, err)
	}

	// Wrong shape.
	if _, err := jwtSub("not.enough"); err == nil {
		t.Error("jwtSub two-parts: want error; got nil")
	}

	// Bad b64 in claims.
	if _, err := jwtSub("hdr.!!!.sig"); err == nil {
		t.Error("jwtSub bad b64: want error")
	}

	// Empty sub.
	enc := base64.RawURLEncoding.EncodeToString
	emptySub := enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(`{"sub":""}`)) + "." + enc([]byte("sig"))
	if _, err := jwtSub(emptySub); err == nil {
		t.Error("jwtSub empty sub: want error")
	}
}
