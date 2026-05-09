// Package relay implements the Nexus-side client for the Frame-to-Frame
// Relay protocol (v3 spec). The client wraps casket-go's crypto with
// the HTTP shape the interchange exposes, so a Frame can:
//
//   - PUT a signed outer envelope
//   - GET envelopes addressed to it (with since-cursor)
//   - Ack consumed envelopes
//   - Pair with another Frame via the staged-approval flow
//
// The interchange is content-blind: the inner envelope is AEAD-sealed
// by casket-go's PairedChannel.EncryptBody before this client sees it.
// This client constructs the outer envelope, signs its canonical form,
// and transports the bytes; it does not handle the inner layer.
//
// All calls propagate context for cancellation and timeout.
package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/casket-go"
)

// Errors surfaced to callers. These map to interchange HTTP statuses.
var (
	ErrPairNotFound     = errors.New("relay: pair not found (404)")
	ErrBadRequest       = errors.New("relay: bad request (400)")
	ErrUnauthorized     = errors.New("relay: unauthorized (401)")
	ErrDuplicate        = errors.New("relay: duplicate msg_id (409)")
	ErrRequestNotFound  = errors.New("relay: request not found (404)")
	ErrRequestConflict  = errors.New("relay: request conflict (409)")
	ErrInterchangeError = errors.New("relay: interchange server error (5xx)")
)

// Client talks to one interchange. Reuse across calls.
type Client struct {
	// BaseURL is the interchange's public URL, e.g.
	// "https://dmon.tailnet.ts.net:8443". No trailing slash.
	BaseURL string
	// HTTP is the underlying transport. Defaults to http.DefaultClient
	// if nil; inject a custom client for tests or timeout control.
	HTTP *http.Client
	// PollInterval is the sleep between /pair/requests/:id polls
	// during Pair(). Defaults to 2s. Tests override to nanoseconds.
	PollInterval time.Duration
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) pollInterval() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return 2 * time.Second
}

// OuterEnvelope is the cleartext routing layer. Construct with
// BuildOuter, then Put sends it over the wire.
type OuterEnvelope struct {
	Version          string `json:"version"`
	MsgID            string `json:"msg_id"`
	Ts               string `json:"ts"`
	PathID           string `json:"path_id"`
	CiphertextSHA256 string `json:"ciphertext_sha256"`
	Ciphertext       string `json:"ciphertext"`
}

// BuildOuter wraps an already-AEAD-sealed ciphertext in an outer envelope,
// computing sha256 + base64url on the way. The caller supplies a fresh
// UUIDv7 msg_id and the current time.
//
// Use casket's PairedChannel.EncryptBody(plaintext, aad) to produce
// ciphertextRaw with aad = sha256(ciphertextRaw) — but that's circular,
// so the typical flow is:
//
//	ct, _ := paired.EncryptBody(inner, digestOf(ct))
//
// which isn't possible. In practice, Encrypt first with AAD = the
// digest of the ciphertext-minus-AAD — the spec binds the hash of the
// final ciphertext bytes into the outer envelope, NOT into AEAD AAD.
// The AEAD's own integrity guarantee covers inner tampering; the outer
// hash guarantee covers relay tampering. This function builds the outer
// envelope AROUND a pre-encrypted ciphertext.
func BuildOuter(pathID, msgID, ts string, ciphertextRaw []byte) OuterEnvelope {
	digest := sha256.Sum256(ciphertextRaw)
	return OuterEnvelope{
		Version:          "1",
		MsgID:            msgID,
		Ts:               ts,
		PathID:           pathID,
		CiphertextSHA256: hex.EncodeToString(digest[:]),
		Ciphertext:       base64.RawURLEncoding.EncodeToString(ciphertextRaw),
	}
}

// canonicalJSON produces RFC 8785 canonical JSON of the outer envelope
// matching the interchange's server-side re-canonicalization byte-for-
// byte. The field set is fixed (6 ASCII-only strings), so:
//   - lexicographic key order via struct field order
//   - SetEscapeHTML(false) to match TS JSON.stringify
//   - no trailing newline
func canonicalJSON(e OuterEnvelope) ([]byte, error) {
	canonical := struct {
		Ciphertext       string `json:"ciphertext"`
		CiphertextSHA256 string `json:"ciphertext_sha256"`
		MsgID            string `json:"msg_id"`
		PathID           string `json:"path_id"`
		Ts               string `json:"ts"`
		Version          string `json:"version"`
	}{e.Ciphertext, e.CiphertextSHA256, e.MsgID, e.PathID, e.Ts, e.Version}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonical); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Put submits a signed outer envelope to the interchange.
// The paired channel signs the canonical form with its Ed25519 key.
func (c *Client) Put(ctx context.Context, paired *casket.PairedChannel, env OuterEnvelope) error {
	canon, err := canonicalJSON(env)
	if err != nil {
		return fmt.Errorf("relay: canonicalize: %w", err)
	}
	sig, err := paired.Sign(canon)
	if err != nil {
		return fmt.Errorf("relay: sign: %w", err)
	}

	// Transport body is the canonical JSON. Some servers use non-
	// canonical bodies and rely on re-canonicalization — we're stricter
	// and send canonical, guaranteeing interop even if a peer's
	// verifier doesn't re-canonicalize.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.BaseURL+"/mailbox/"+env.PathID, bytes.NewReader(canon))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Signature", base64.RawURLEncoding.EncodeToString(sig))

	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("relay: put: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusBadRequest:
		return wrapErr(ErrBadRequest, resp.Body)
	case http.StatusUnauthorized:
		return wrapErr(ErrUnauthorized, resp.Body)
	case http.StatusNotFound:
		return wrapErr(ErrPairNotFound, resp.Body)
	case http.StatusConflict:
		return wrapErr(ErrDuplicate, resp.Body)
	default:
		return wrapErr(ErrInterchangeError, resp.Body)
	}
}

// GetResponse is the envelope list returned by Get.
type GetResponse struct {
	Envelopes []json.RawMessage `json:"envelopes"`
	Cursor    *string           `json:"cursor"`
}

// Get lists envelopes addressed to the caller newer than sinceMsgID.
// An empty sinceMsgID starts from the oldest retained envelope. The
// caller signs path+query (not a body) per v3 spec.
func (c *Client) Get(ctx context.Context, paired *casket.PairedChannel, pathID, sinceMsgID string) (GetResponse, error) {
	u := "/mailbox/" + pathID
	if sinceMsgID != "" {
		u += "?since=" + url.QueryEscape(sinceMsgID)
	}

	sig, err := paired.Sign([]byte(u))
	if err != nil {
		return GetResponse{}, fmt.Errorf("relay: sign path: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+u, nil)
	if err != nil {
		return GetResponse{}, err
	}
	req.Header.Set("X-Nexus-Signature", base64.RawURLEncoding.EncodeToString(sig))

	resp, err := c.http().Do(req)
	if err != nil {
		return GetResponse{}, fmt.Errorf("relay: get: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out GetResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return GetResponse{}, fmt.Errorf("relay: decode get response: %w", err)
		}
		return out, nil
	case http.StatusBadRequest:
		return GetResponse{}, wrapErr(ErrBadRequest, resp.Body)
	case http.StatusUnauthorized:
		return GetResponse{}, wrapErr(ErrUnauthorized, resp.Body)
	case http.StatusNotFound:
		return GetResponse{}, wrapErr(ErrPairNotFound, resp.Body)
	default:
		return GetResponse{}, wrapErr(ErrInterchangeError, resp.Body)
	}
}

// Ack tells the interchange the caller has consumed these msg_ids so
// they can be evicted from the mailbox. Advisory — local seen() state
// is still the source of truth for dedupe.
//
// Ack signs the path only — no query, no body (per v3 spec §Ack).
// Do NOT add query parameters to this endpoint without updating the
// signing preimage on both client and interchange; silent drift here
// produces 401 signature_invalid with no useful diagnostic.
func (c *Client) Ack(ctx context.Context, paired *casket.PairedChannel, pathID string, msgIDs []string) (int, error) {
	u := "/mailbox/" + pathID + "/ack"
	sig, err := paired.Sign([]byte(u))
	if err != nil {
		return 0, fmt.Errorf("relay: sign path: %w", err)
	}

	body, err := json.Marshal(map[string][]string{"ids": msgIDs})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Signature", base64.RawURLEncoding.EncodeToString(sig))

	resp, err := c.http().Do(req)
	if err != nil {
		return 0, fmt.Errorf("relay: ack: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Evicted int `json:"evicted"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Evicted, nil
	case http.StatusBadRequest:
		return 0, wrapErr(ErrBadRequest, resp.Body)
	case http.StatusUnauthorized:
		return 0, wrapErr(ErrUnauthorized, resp.Body)
	case http.StatusNotFound:
		return 0, wrapErr(ErrPairNotFound, resp.Body)
	default:
		return 0, wrapErr(ErrInterchangeError, resp.Body)
	}
}

// PairResult is the outcome of a completed pair flow.
type PairResult struct {
	RequestID     string
	Status        string           // "approved" | "denied" | "expired"
	PathID        string           // populated when Status == "approved"
	OwnerHalf     *PairHalfPayload // populated when Status == "approved" (requester-side poll)
	RequesterHalf *PairHalfPayload // populated when Status == "approved" (owner-side approve response)
}

// Discover fetches the interchange's /.well-known/nexus-interchange
// document. Useful for bootstrapping when only the URL is known.
// Returns raw JSON so callers can parse only the fields they need.
func (c *Client) Discover(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/.well-known/nexus-interchange", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("relay: discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay: discover: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

// CanonicalHalfBytes builds the v2 self-sig preimage for a pair-flow half.
// v2 adds dh_alg and dh_pubkey to the signed payload so an attacker who can
// mutate stored halves cannot swap the ECDH key without breaking the
// signature. The first line "v2" distinguishes this from the v1 preimage.
//
// Line-oriented UTF-8, 9 fields joined by "\n" (LF), no trailing newline:
//
//	v2 \n nexus_id \n sig_alg \n pubkey \n dh_alg \n dh_pubkey \n endpoint \n nonce \n ts
func CanonicalHalfBytes(nexusID, sigAlg, pubkey, dhAlg, dhPubkey, endpoint, nonce, ts string) []byte {
	return []byte(strings.Join([]string{
		"v2", nexusID, sigAlg, pubkey, dhAlg, dhPubkey, endpoint, nonce, ts,
	}, "\n"))
}

// canonicalHalfBytesV1 builds the legacy v1 preimage (no ECDH fields).
// Used by the verifier to accept v1 halves during the transition period.
// New halves MUST use CanonicalHalfBytes (v2). v1 leaves dh_pubkey
// unsigned and is vulnerable to substitution at storage.
func canonicalHalfBytesV1(nexusID, sigAlg, pubkey, endpoint, nonce, ts string) []byte {
	return []byte(strings.Join([]string{
		"v1", nexusID, sigAlg, pubkey, endpoint, nonce, ts,
	}, "\n"))
}

// IsoTs formats a time.Time as the protocol's canonical wire timestamp
// (ISO 8601 UTC, second precision, "Z" suffix).
func IsoTs(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// SubmitPairRequestBody is the shape POSTed to /pair/request. Built
// here (rather than by the caller) so the half's JSON matches what the
// interchange expects; caller supplies the target, the half, and the
// self-sig bytes (which Part 3.3 will produce via casket.Channel.Sign).
type SubmitPairRequestBody struct {
	TargetNexusID string          `json:"target_nexus_id"`
	Requester     PairHalfPayload `json:"requester"`
}

// PairHalfPayload is either side's submission to the interchange. Same
// shape for requester and owner halves.
type PairHalfPayload struct {
	NexusID  string `json:"nexus_id"`
	SigAlg   string `json:"sig_alg"`   // "ed25519" at v1
	DhAlg    string `json:"dh_alg"`    // "P-256" or "X25519"; covered by v2 self-sig
	Pubkey   string `json:"pubkey"`    // base64url Ed25519 32 bytes
	DhPubkey string `json:"dh_pubkey"` // base64url ECDH public key; covered by v2 self-sig
	Endpoint string `json:"endpoint"`  // optional
	Nonce    string `json:"nonce"`     // base64url 16 bytes
	Ts       string `json:"ts"`        // ISO 8601
	SelfSig  string `json:"self_sig"`  // base64url detached Ed25519 sig
}

// SubmitPairRequest POSTs a pre-signed pair request half and returns
// the request_id. The caller is responsible for assembling + signing
// the half — either via casket-go (once Channel.Sign is exposed) or a
// higher-level helper in Part 3.3. Exposing this at the client level
// keeps the relay package crypto-free; key material stays in casket.
func (c *Client) SubmitPairRequest(ctx context.Context, body SubmitPairRequestBody) (PairResult, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return PairResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/pair/request", bytes.NewReader(raw))
	if err != nil {
		return PairResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http().Do(req)
	if err != nil {
		return PairResult{}, fmt.Errorf("relay: pair_request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		var out struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return PairResult{}, err
		}
		return PairResult{RequestID: out.RequestID, Status: out.Status}, nil
	case http.StatusBadRequest:
		return PairResult{}, wrapErr(ErrBadRequest, resp.Body)
	default:
		return PairResult{}, wrapErr(ErrInterchangeError, resp.Body)
	}
}

// PollRequest polls GET /pair/requests/:id until status != pending,
// ctx cancels, or deadline passes. Used by the requester to learn the
// owner's decision after POSTing /pair/request.
func (c *Client) PollRequest(ctx context.Context, requestID string) (PairResult, error) {
	interval := c.pollInterval()
	for {
		res, err := c.getRequestStatus(ctx, requestID)
		if err != nil {
			return PairResult{}, err
		}
		if res.Status != "pending" {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// getRequestStatus fetches /pair/requests/:id (unauthenticated poll).
// When status is "approved", the response includes owner_half so the requester
// can immediately call channel.Pair without any out-of-band token exchange.
func (c *Client) getRequestStatus(ctx context.Context, requestID string) (PairResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/pair/requests/"+requestID, nil)
	if err != nil {
		return PairResult{}, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return PairResult{}, fmt.Errorf("relay: poll: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			RequestID string           `json:"request_id"`
			Status    string           `json:"status"`
			PathID    string           `json:"path_id"`
			OwnerHalf *PairHalfPayload `json:"owner_half"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return PairResult{}, err
		}
		return PairResult{
			RequestID: out.RequestID,
			Status:    out.Status,
			PathID:    out.PathID,
			OwnerHalf: out.OwnerHalf,
		}, nil
	case http.StatusNotFound:
		return PairResult{}, wrapErr(ErrRequestNotFound, resp.Body)
	case http.StatusBadRequest:
		return PairResult{}, wrapErr(ErrBadRequest, resp.Body)
	default:
		return PairResult{}, wrapErr(ErrInterchangeError, resp.Body)
	}
}

// wrapErr attaches the response body to a sentinel error for caller
// debugging. Body is bounded to 4KB to avoid unbounded allocation on
// pathological servers.
func wrapErr(sentinel error, body io.Reader) error {
	raw, _ := io.ReadAll(io.LimitReader(body, 4096))
	if len(raw) == 0 {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, strings.TrimSpace(string(raw)))
}
