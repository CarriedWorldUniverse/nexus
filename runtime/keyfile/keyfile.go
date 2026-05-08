// Package keyfile is the agentfunnel-side reader for nexus keyfiles +
// the HTTP client for the spec §5 startup validation handshake.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §4–§5:
//
//   1. Read the on-disk keyfile (JSON: envelope + encrypted_payload).
//   2. GET <nexus_url>/api/nexus_id, compare to envelope.nexus_id.
//      Mismatch → abort, "wrong nexus" — keyfile was sealed for a
//      different Nexus instance.
//   3. POST <nexus_url>/api/aspect/validate with {encrypted_payload}.
//      On 200, the response carries the session JWT + personality
//      bundle + provider/model.
//
// This package is the *client* of broker/validate_endpoint.go. Wire
// shapes match exactly; deliberate duplication of types so the runtime
// doesn't take a build-time dep on the broker package (different
// release cadences, separate test surfaces).
//
// Out of scope (deferred to agentfunnel composition in cmd/agentfunnel):
//   - WebSocket dial + register frame
//   - Personality-to-SystemPrompt wiring
//   - JWT refresh on expiry
//
// This package is HTTP-only.

package keyfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Format / Version match the on-disk keyfile envelope.
const (
	expectedFormat  = "nexus-keyfile-v1"
	expectedVersion = 1
)

// Sentinels — agentfunnel surfaces specific shapes to the operator
// (e.g. "wrong nexus" should print a hint pointing at the right URL).
var (
	// ErrBadKeyfile: on-disk file isn't parseable / wrong format /
	// wrong version. Pre-network failure.
	ErrBadKeyfile = errors.New("keyfile: malformed or unsupported keyfile")

	// ErrNexusMismatch: the server's nexus_id doesn't match the
	// keyfile envelope's. Possible causes: keyfile is stale (old
	// Nexus regenerated identity), Nexus URL points at the wrong
	// host, or DNS poisoning. Treat as fatal — do NOT send the
	// encrypted payload to a Nexus that can't decrypt it.
	ErrNexusMismatch = errors.New("keyfile: server nexus_id does not match envelope nexus_id")

	// ErrValidationRejected: server returned a non-200. Wrapped
	// errors carry the body for log surfacing.
	ErrValidationRejected = errors.New("keyfile: server rejected validation")

	// ErrBadServerResponse: server returned a 200 with a malformed
	// body (e.g. ok=false, missing JWT). Distinct from
	// ErrValidationRejected because it indicates a server bug, not a
	// keyfile issue — agentfunnel should surface this differently
	// (don't suggest re-minting; complain about Nexus).
	ErrBadServerResponse = errors.New("keyfile: server returned 200 with bad response shape")
)

// Envelope is the on-disk plaintext routing layer. Mirrors
// nexus/aspects.Envelope — kept in sync by hand because we don't want
// the runtime taking a build-time dependency on the nexus package.
type Envelope struct {
	NexusURL string `json:"nexus_url"`
	NexusID  string `json:"nexus_id"`
	IssuedAt string `json:"issued_at"`
}

// Keyfile is the on-disk JSON document.
type Keyfile struct {
	Version          int      `json:"version"`
	Format           string   `json:"format"`
	Envelope         Envelope `json:"envelope"`
	EncryptedPayload string   `json:"encrypted_payload"`
}

// PersonalityBundle is what the validation response delivers. Wire
// shape: see broker/validate_endpoint.go's personalityWire — must
// stay in sync field-for-field with that struct.
type PersonalityBundle struct {
	NexusMD   string `json:"nexus_md"`
	SoulMD    string `json:"soul_md"`
	PrimerMD  string `json:"primer_md"`
	Composed  string `json:"composed"`
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

// ValidationResult is the digested output of a successful handshake.
// agentfunnel uses these fields directly: JWT for the WS bearer,
// SystemPrompt for funnel.Config, Provider+Model for bridle setup,
// AspectName for register frame and logging.
type ValidationResult struct {
	// AspectName is the aspect_name from the decrypted payload (returned
	// by Nexus in the JWT sub claim — but agentfunnel doesn't decode
	// the JWT itself; it trusts the Nexus that issued it).
	//
	// Populated from the keyfile envelope's nexus_id flow indirectly:
	// the Nexus echoes the aspect_name through the validation logic,
	// but the success response shape doesn't carry it (the JWT does).
	// We pull it from the Aspect field below, which is set client-side
	// from the JWT's sub claim — see Validate.
	AspectName string

	// SessionJWT is the bearer agentfunnel uses for /connect and
	// subsequent requests.
	SessionJWT string

	// SessionExpiresAt is when the JWT becomes invalid. agentfunnel
	// re-validates before this point to refresh.
	SessionExpiresAt time.Time

	// Personality is the per-aspect bundle straight from the response.
	// Composed is the canonical per-aspect prompt; agentfunnel's
	// caller layers CentralNexusMD ABOVE it (per Part 9 decomposition
	// spec) — the per-aspect Composed must NOT include central
	// content.
	Personality PersonalityBundle

	// CentralNexusMD is nexus_settings.nexus_md from the Nexus —
	// network-wide operational scope shared by every aspect (Part 9).
	// Empty when the Nexus isn't running Part 9 (legacy validators).
	// agentfunnel layers it above Personality.NexusMD in the composed
	// prompt; see runtime/cmd/agentfunnel for the concat logic.
	CentralNexusMD string

	// CentralVersion lets agentfunnel detect when central content
	// changes between re-validations, independent of personality.Version.
	CentralVersion int64

	// Provider/Model identify the bridle backend.
	Provider string
	Model    string

	// NexusURL is the WS endpoint agentfunnel should dial. Drawn from
	// the keyfile envelope, surfaced here for caller convenience.
	NexusURL string

	// NexusID is the verified Nexus instance ID (envelope == server).
	// Useful for log correlation.
	NexusID string
}

// Load reads and parses an on-disk keyfile. Validates format + version
// + that all required envelope fields are non-empty. Does NOT touch
// the network.
func Load(path string) (*Keyfile, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrBadKeyfile, path, err)
	}
	var kf Keyfile
	if err := json.Unmarshal(raw, &kf); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", ErrBadKeyfile, path, err)
	}
	if kf.Format != expectedFormat {
		return nil, fmt.Errorf("%w: format=%q (want %q)", ErrBadKeyfile, kf.Format, expectedFormat)
	}
	if kf.Version != expectedVersion {
		return nil, fmt.Errorf("%w: version=%d (want %d)", ErrBadKeyfile, kf.Version, expectedVersion)
	}
	if kf.Envelope.NexusURL == "" || kf.Envelope.NexusID == "" {
		return nil, fmt.Errorf("%w: envelope missing nexus_url or nexus_id", ErrBadKeyfile)
	}
	if kf.EncryptedPayload == "" {
		return nil, fmt.Errorf("%w: encrypted_payload empty", ErrBadKeyfile)
	}
	return &kf, nil
}

// Client performs the spec §5 handshake against a Nexus. Configurable
// HTTP client lets callers inject a TLS-trust-store-aware transport
// (e.g. when dialing a self-signed dev cert).
type Client struct {
	HTTP *http.Client
}

// NewClient returns a Client with a sensible default HTTP client
// (10-second timeout, default transport — system CAs apply, including
// any tailscale-issued certs already in the trust store).
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}

// Validate runs the spec §5 startup handshake:
//
//   1. GET <nexus_url>/api/nexus_id, compare to envelope.
//   2. POST <nexus_url>/api/aspect/validate with the encrypted_payload.
//   3. Decode response, extract aspect_name from the JWT sub claim
//      (parse-only, no signature check — we trust the JWT because we
//      trust the TLS cert + the nexus_id match).
//
// Returns ValidationResult on success; sentinel-wrapped errors on
// failure so the caller can render hints (ErrNexusMismatch suggests
// re-minting, ErrValidationRejected with body lets the operator
// see "revoked, current=N").
func (c *Client) Validate(ctx context.Context, kf *Keyfile) (*ValidationResult, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second}
	}

	// Translate envelope.nexus_url (wss://) to the HTTPS base for
	// REST calls. The broker serves WS and HTTP on the same listener
	// post PR-A2, so wss://host/connect → https://host.
	httpsBase, err := wsToHTTPS(kf.Envelope.NexusURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKeyfile, err)
	}

	// Step 1: identity check.
	if err := c.checkNexusID(ctx, httpsBase, kf.Envelope.NexusID); err != nil {
		return nil, err
	}

	// Step 2: validate.
	resp, err := c.postValidate(ctx, httpsBase, kf.EncryptedPayload)
	if err != nil {
		return nil, err
	}

	// Step 3: extract aspect_name from JWT sub claim.
	aspectName, err := jwtSub(resp.SessionJWT)
	if err != nil {
		return nil, fmt.Errorf("keyfile.Validate: extract aspect from JWT: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, resp.SessionExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("keyfile.Validate: parse session_expires_at %q: %w",
			resp.SessionExpiresAt, err)
	}

	return &ValidationResult{
		AspectName:       aspectName,
		SessionJWT:       resp.SessionJWT,
		SessionExpiresAt: expiresAt,
		Personality:      resp.Personality,
		CentralNexusMD:   resp.CentralNexusMD,
		CentralVersion:   resp.CentralVersion,
		Provider:         resp.Provider,
		Model:            resp.Model,
		NexusURL:         kf.Envelope.NexusURL,
		NexusID:          kf.Envelope.NexusID,
	}, nil
}

// checkNexusID dials GET /api/nexus_id and compares the response
// against the envelope's nexus_id. Done before sending the encrypted
// payload so a wrong-Nexus dial doesn't leak the sealed bytes (which
// can't be decrypted but might be logged elsewhere).
func (c *Client) checkNexusID(ctx context.Context, base, expected string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/nexus_id", nil)
	if err != nil {
		return fmt.Errorf("keyfile.checkNexusID: build request: %w", err)
	}
	httpResp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("keyfile.checkNexusID: GET %s/api/nexus_id: %w", base, err)
	}
	defer httpResp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("keyfile.checkNexusID: status %d: %s", httpResp.StatusCode, string(body))
	}
	var nid struct {
		NexusID string `json:"nexus_id"`
	}
	if err := json.Unmarshal(body, &nid); err != nil {
		return fmt.Errorf("keyfile.checkNexusID: decode: %w", err)
	}
	if nid.NexusID == "" {
		return fmt.Errorf("keyfile.checkNexusID: server returned empty nexus_id")
	}
	if nid.NexusID != expected {
		return fmt.Errorf("%w: server=%s envelope=%s", ErrNexusMismatch, nid.NexusID, expected)
	}
	return nil
}

// validateResponse is the wire shape POST /api/aspect/validate returns
// on success. Mirrors broker.validateResponse — must stay in sync.
type validateResponse struct {
	OK               bool              `json:"ok"`
	SessionJWT       string            `json:"session_jwt"`
	SessionExpiresAt string            `json:"session_expires_at"`
	Personality      PersonalityBundle `json:"personality"`
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`

	// Part 9 fields. Older Nexus instances (pre-Part-9) won't emit
	// these; JSON decoding leaves them zero-valued, agentfunnel
	// composes from per-aspect content alone (legacy shape).
	CentralNexusMD string `json:"central_nexus_md"`
	CentralVersion int64  `json:"central_version"`
}

// postValidate POSTs the encrypted_payload and decodes the response.
func (c *Client) postValidate(ctx context.Context, base, encryptedPayload string) (*validateResponse, error) {
	body, err := json.Marshal(map[string]string{"encrypted_payload": encryptedPayload})
	if err != nil {
		return nil, fmt.Errorf("keyfile.postValidate: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/aspect/validate", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("keyfile.postValidate: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("keyfile.postValidate: POST: %w", err)
	}
	defer httpResp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 64*1024))
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d: %s", ErrValidationRejected, httpResp.StatusCode, string(respBody))
	}
	var r validateResponse
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, fmt.Errorf("keyfile.postValidate: decode: %w", err)
	}
	if !r.OK || r.SessionJWT == "" {
		// 200 with bad shape = server bug, not keyfile rejection.
		// Distinct sentinel keeps callers from suggesting "re-mint
		// your keyfile" for what's actually a Nexus-side problem.
		return nil, fmt.Errorf("%w: ok=%v jwt_empty=%v", ErrBadServerResponse, r.OK, r.SessionJWT == "")
	}
	return &r, nil
}

// wsToHTTPS rewrites a wss:// or ws:// URL to https:// or http://, and
// strips the /connect suffix (the WS path lives at /connect; the HTTP
// API lives at /api/*). Per spec §4 the canonical shape is
// `wss://host:port/connect`; URLs with any other path are rejected
// rather than silently corrupted.
func wsToHTTPS(wsURL string) (string, error) {
	switch {
	case strings.HasPrefix(wsURL, "wss://"):
		wsURL = "https://" + strings.TrimPrefix(wsURL, "wss://")
	case strings.HasPrefix(wsURL, "ws://"):
		wsURL = "http://" + strings.TrimPrefix(wsURL, "ws://")
	case strings.HasPrefix(wsURL, "https://"), strings.HasPrefix(wsURL, "http://"):
		// Caller passed the HTTPS form already; honor it.
	default:
		return "", fmt.Errorf("nexus_url scheme not ws/wss/http/https: %q", wsURL)
	}

	// Split scheme://authority and the path. We need the path to be
	// either empty, "/", or "/connect" — anything else means the
	// operator put a stray path component in nexus_url and we'd
	// silently produce a wrong base URL.
	schemeEnd := strings.Index(wsURL, "://")
	authStart := schemeEnd + 3
	rest := wsURL[authStart:]
	pathStart := strings.Index(rest, "/")
	var path string
	if pathStart >= 0 {
		path = rest[pathStart:]
		rest = rest[:pathStart]
	}
	switch path {
	case "", "/", "/connect", "/connect/":
		// Acceptable; strip path entirely.
	default:
		return "", fmt.Errorf("nexus_url has unexpected path %q (expected /connect or empty): %q", path, wsURL)
	}
	return wsURL[:authStart] + rest, nil
}
