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

	casket "github.com/nexus-cw/casket-go"
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
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	ts := IsoTs(time.Now().UTC())

	canonical := CanonicalHalfBytes(nexusID, "ed25519", pubB64, endpoint, nonce, ts)
	sig, err := ch.Sign(canonical)
	if err != nil {
		return PairHalfPayload{}, fmt.Errorf("relay: sign half: %w", err)
	}

	return PairHalfPayload{
		NexusID:  nexusID,
		SigAlg:   "ed25519",
		Pubkey:   pubB64,
		Endpoint: endpoint,
		Nonce:    nonce,
		Ts:       ts,
		SelfSig:  base64.RawURLEncoding.EncodeToString(sig),
	}, nil
}

// Pair is the requester-side convenience that runs the full
// staged-approval flow:
//
//  1. Build + sign our half (BuildSignedPairHalf).
//  2. POST /pair/request (Client.SubmitPairRequest).
//  3. Poll GET /pair/requests/:id until the owner decides or ctx
//     cancels (Client.PollRequest).
//
// Returns the final PairResult. On approval, the owner's half is NOT
// returned by the interchange — the requester must still obtain the
// owner's PairingToken out-of-band to call channel.Pair locally and
// instantiate a PairedChannel. This is intentional per v3 spec: the
// interchange stores pubkeys for signature verification but doesn't
// redistribute pairing tokens.
//
// For the PoC (keel ↔ keel-nexus, one operator both sides), the
// operator's dashboard on the owner side can render the owner's
// PairingToken alongside the approved request so the requester side
// can fetch it — that's dashboard work in Part 4.3.
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
