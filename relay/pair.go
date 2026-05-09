package relay

// Pairing UX helpers. Wraps the low-level SubmitPairRequest +
// PollRequest primitives from Part 3.1 with a full convenience flow
// that consumes casket-go's Channel.Sign (landed in casket-go 2cc55ba
// after anvil #7859).
//
// Split rationale (anvil #7857 / operator #7858): casket-go stays
// crypto-only; the canonical-format + endpoint + ISO-timestamp
// assembly lives here in relay, which owns the wire shape. Channel
// doesn't need to know about PairHalfPayload or RFC3339.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
)

// BuildSignedPairHalf assembles a PairHalfPayload for the given
// channel + endpoint. Generates a fresh 16-byte nonce, current UTC
// timestamp, then signs the canonical line-oriented bytes (v3 spec
// §Components.2 Pairing.self_sig_canonical) via ch.Sign.
//
// Exposed (not a method) so Part 3.4 keel-side code can build halves
// under whichever identity the operator is pairing as, without
// reaching through a Client.
//
// SigAlg is hardcoded to "ed25519". In casket-go, signing is ALWAYS
// Ed25519 regardless of the Channel's dh_alg (P-256 vs X25519 is the
// ECDH curve for body encryption, not signing). ch.PublicKeyBytes()
// returns the 32-byte Ed25519 public key; ch.Sign produces a 64-byte
// Ed25519 signature. Spec + interchange verifier only accept ed25519
// at v1 (anvil #7828, #7841). See project_casket_signing_always_ed25519.
func BuildSignedPairHalf(ch *casket.Channel, nexusID, endpoint string) (PairHalfPayload, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return PairHalfPayload{}, fmt.Errorf("relay: nonce: %w", err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(ch.PublicKeyBytes())
	dhPubB64 := base64.RawURLEncoding.EncodeToString(ch.DHPublicKeyBytes())
	dhAlg := string(ch.DHAlg())
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	ts := IsoTs(time.Now().UTC())

	canonical := CanonicalHalfBytes(nexusID, "ed25519", pubB64, dhAlg, dhPubB64, endpoint, nonce, ts)
	sig, err := ch.Sign(canonical)
	if err != nil {
		return PairHalfPayload{}, fmt.Errorf("relay: sign half: %w", err)
	}

	return PairHalfPayload{
		NexusID:  nexusID,
		SigAlg:   "ed25519",
		DhAlg:    dhAlg,
		Pubkey:   pubB64,
		DhPubkey: dhPubB64,
		Endpoint: endpoint,
		Nonce:    nonce,
		Ts:       ts,
		SelfSig:  base64.RawURLEncoding.EncodeToString(sig),
	}, nil
}

// Pair is the requester-side convenience that runs the full staged-approval
// flow:
//
//  1. Build + sign our half (BuildSignedPairHalf).
//  2. POST /pair/request (Client.SubmitPairRequest).
//  3. Poll GET /pair/requests/:id until the owner decides or ctx cancels.
//
// Returns the final PairResult. Under v2 protocol the poll response includes
// the owner's full half (PairResult.OwnerHalf), so callers can immediately
// call PairFromHalf to activate a local PairedChannel without any OOB token
// exchange. v1 peers return OwnerHalf == nil; callers must fall back to
// Channel.Pair with a manually exchanged PairingToken.
func (c *Client) Pair(ctx context.Context, ch *casket.Channel, nexusID, targetNexusID, endpoint string) (PairResult, error) {
	if ch == nil {
		return PairResult{}, errors.New("relay: Pair: casket.Channel is nil")
	}
	if targetNexusID == "" {
		return PairResult{}, errors.New("relay: Pair: targetNexusID empty")
	}

	half, err := BuildSignedPairHalf(ch, nexusID, endpoint)
	if err != nil {
		return PairResult{}, err
	}

	submitted, err := c.SubmitPairRequest(ctx, SubmitPairRequestBody{
		TargetNexusID: targetNexusID,
		Requester:     half,
	})
	if err != nil {
		return PairResult{}, fmt.Errorf("relay: submit: %w", err)
	}

	// Block until owner decides or ctx cancels. PollInterval throttles.
	final, err := c.PollRequest(ctx, submitted.RequestID)
	if err != nil {
		return PairResult{}, fmt.Errorf("relay: poll: %w", err)
	}
	return final, nil
}

// PairFromHalf converts a relay-delivered PairHalfPayload into a PairingToken
// and calls Channel.Pair, returning an active PairedChannel. This is the
// canonical way to activate a local channel after receiving the peer's half
// from the interchange (v2 protocol: approve response carries requester_half,
// poll response carries owner_half).
//
// maxAgeSec is passed through to Channel.Pair. For relay-mediated pairs the
// half was signed at request-submit time and the response may arrive minutes
// later; 3600 (one hour) is a reasonable default. SDK consumers that know
// the expected flow latency can tune tighter.
//
// Returns ErrBadRequest if the half is missing required fields.
func PairFromHalf(ctx context.Context, ch *casket.Channel, h PairHalfPayload, maxAgeSec int64) (*casket.PairedChannel, error) {
	if h.Pubkey == "" || h.DhPubkey == "" || h.DhAlg == "" || h.NexusID == "" {
		return nil, fmt.Errorf("%w: peer half missing required fields (pubkey/dh_pubkey/dh_alg/nexus_id)", ErrBadRequest)
	}

	tsUnix, err := parseIsoTs(h.Ts)
	if err != nil {
		return nil, fmt.Errorf("%w: peer half ts: %v", ErrBadRequest, err)
	}

	token := casket.PairingToken{
		V:        1,
		NexusID:  h.NexusID,
		SigAlg:   h.SigAlg,
		DhAlg:    casket.DhAlgorithm(h.DhAlg),
		Pubkey:   h.Pubkey,
		DhPubkey: h.DhPubkey,
		Endpoint: h.Endpoint,
		Nonce:    h.Nonce,
		Ts:       tsUnix,
	}
	return ch.Pair(ctx, token, maxAgeSec)
}

// parseIsoTs parses the protocol's ISO 8601 UTC timestamp (e.g.
// "2026-04-30T12:00:00Z") into a Unix second value for Channel.Pair.
func parseIsoTs(s string) (int64, error) {
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}
