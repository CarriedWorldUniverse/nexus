package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	casket "github.com/nexus-cw/casket-go"
)

// TestBuildSignedPairHalf verifies the assembled half's self-sig
// verifies against the channel's own pubkey — if this passes, the
// interchange will accept the half at /pair/request.
// Also asserts v2 fields (dh_alg, dh_pubkey) are populated and covered
// by the v2 preimage signature.
func TestBuildSignedPairHalf(t *testing.T) {
	ch, err := casket.Load(context.Background(), "keel", newInMemStorage(), casket.P256)
	if err != nil {
		t.Fatal(err)
	}

	half, err := BuildSignedPairHalf(ch, "keel", "https://keel.local")
	if err != nil {
		t.Fatalf("BuildSignedPairHalf: %v", err)
	}

	if half.NexusID != "keel" || half.SigAlg != "ed25519" {
		t.Errorf("wrong identity fields: %+v", half)
	}
	if half.Pubkey == "" || half.Nonce == "" || half.Ts == "" || half.SelfSig == "" {
		t.Errorf("missing base fields: %+v", half)
	}
	// v2: dh fields must be populated
	if half.DhAlg == "" || half.DhPubkey == "" {
		t.Errorf("v2 dh fields missing: dh_alg=%q dh_pubkey=%q", half.DhAlg, half.DhPubkey)
	}
	expectedPub := base64.RawURLEncoding.EncodeToString(ch.PublicKeyBytes())
	if half.Pubkey != expectedPub {
		t.Errorf("pubkey mismatch: got %q", half.Pubkey)
	}
	expectedDhPub := base64.RawURLEncoding.EncodeToString(ch.DHPublicKeyBytes())
	if half.DhPubkey != expectedDhPub {
		t.Errorf("dh_pubkey mismatch: got %q", half.DhPubkey)
	}

	// Verify against v2 preimage
	canonical := CanonicalHalfBytes(half.NexusID, half.SigAlg, half.Pubkey,
		half.DhAlg, half.DhPubkey, half.Endpoint, half.Nonce, half.Ts)
	sig, err := base64.RawURLEncoding.DecodeString(half.SelfSig)
	if err != nil {
		t.Fatalf("decode self_sig: %v", err)
	}
	if !ed25519.Verify(ch.PublicKeyBytes(), canonical, sig) {
		t.Errorf("self-sig did not verify against channel pubkey (v2 preimage)")
	}

	// Must NOT verify against v1 preimage (proves dh fields are load-bearing)
	canonicalV1 := canonicalHalfBytesV1(half.NexusID, half.SigAlg, half.Pubkey,
		half.Endpoint, half.Nonce, half.Ts)
	if ed25519.Verify(ch.PublicKeyBytes(), canonicalV1, sig) {
		t.Errorf("self-sig verified against v1 preimage — dh fields not covered by signature")
	}
}

// TestBuildSignedPairHalfFreshNonce — two calls produce distinct
// nonces. Replay protection requires a fresh nonce per half.
func TestBuildSignedPairHalfFreshNonce(t *testing.T) {
	ch, _ := casket.Load(context.Background(), "keel", newInMemStorage(), casket.P256)
	a, _ := BuildSignedPairHalf(ch, "keel", "")
	b, _ := BuildSignedPairHalf(ch, "keel", "")
	if a.Nonce == b.Nonce {
		t.Errorf("nonce reused across halves: %s", a.Nonce)
	}
}

// TestPairFullFlow drives Client.Pair end-to-end against a fake that
// auto-approves after a short delay — simulating an operator click.
// Verifies the happy path returns an approved PairResult with pathId and
// owner_half (v2 protocol — no sneakernet token exchange required).
func TestPairFullFlow(t *testing.T) {
	ctx := context.Background()
	reqCh, _ := casket.Load(ctx, "keel", newInMemStorage(), casket.P256)
	ownCh, _ := casket.Load(ctx, "keel-nexus", newInMemStorage(), casket.P256)
	fake := newFakeInterchange(reqCh.PublicKeyBytes(), ownCh.PublicKeyBytes())
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := &Client{
		BaseURL:      srv.URL,
		HTTP:         srv.Client(),
		PollInterval: 5 * time.Millisecond,
	}

	// Pre-build the owner half so the approver goroutine can pass it to
	// approveAllPending (v2: full signed half, not just pubkey bytes).
	ownerHalf, err := BuildSignedPairHalf(ownCh, "keel-nexus", "https://keel-nexus.local")
	if err != nil {
		t.Fatalf("BuildSignedPairHalf (owner): %v", err)
	}

	stopApprover := make(chan struct{})
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopApprover:
				return
			case <-tick.C:
				fake.approveAllPending(ownerHalf)
			}
		}
	}()
	defer close(stopApprover)

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.Pair(timeoutCtx, reqCh, "keel", "keel-nexus", "https://keel.local")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if result.Status != "approved" {
		t.Errorf("status = %q, want approved", result.Status)
	}
	if !strings.HasPrefix(result.PathID, "nxc_") {
		t.Errorf("path_id = %q, want nxc_...", result.PathID)
	}
	if result.PathID != fake.pathID {
		t.Errorf("result path_id = %q, fake.pathID = %q", result.PathID, fake.pathID)
	}
	// v2 protocol: poll response includes owner_half so requester can pair locally.
	if result.OwnerHalf == nil {
		t.Errorf("owner_half missing from poll response — v2 protocol requires it on approval")
	} else {
		if result.OwnerHalf.DhAlg == "" || result.OwnerHalf.DhPubkey == "" {
			t.Errorf("owner_half missing dh fields: %+v", result.OwnerHalf)
		}
		if result.OwnerHalf.NexusID != "keel-nexus" {
			t.Errorf("owner_half nexus_id = %q, want keel-nexus", result.OwnerHalf.NexusID)
		}
	}
}

func TestPairRejectsNilChannel(t *testing.T) {
	client, _, _, teardown := setupFixture(t)
	defer teardown()
	_, err := client.Pair(context.Background(), nil, "keel", "alice", "")
	if err == nil {
		t.Errorf("Pair with nil channel should error")
	}
}

func TestPairRejectsEmptyTarget(t *testing.T) {
	client, _, _, teardown := setupFixture(t)
	defer teardown()
	ch, _ := casket.Load(context.Background(), "keel", newInMemStorage(), casket.P256)
	_, err := client.Pair(context.Background(), ch, "keel", "", "")
	if err == nil {
		t.Errorf("empty target should error")
	}
}

// TestPairPropagatesSubmitFailure — unreachable interchange surfaces
// the error rather than silently polling forever.
func TestPairPropagatesSubmitFailure(t *testing.T) {
	client, _, _, teardown := setupFixture(t)
	defer teardown()
	client.BaseURL = "http://127.0.0.1:1"
	ch, _ := casket.Load(context.Background(), "keel", newInMemStorage(), casket.P256)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.Pair(ctx, ch, "keel", "alice", "")
	if err == nil {
		t.Errorf("unreachable interchange should surface error")
	}
}

// TestPairBlocksUntilDecision — no approver means Pair blocks until
// ctx deadline. Confirms polling actually loops.
func TestPairBlocksUntilDecision(t *testing.T) {
	reqCh, _ := casket.Load(context.Background(), "keel", newInMemStorage(), casket.P256)
	ownCh, _ := casket.Load(context.Background(), "keel-nexus", newInMemStorage(), casket.P256)
	fake := newFakeInterchange(reqCh.PublicKeyBytes(), ownCh.PublicKeyBytes())
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTP: srv.Client(), PollInterval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Pair(ctx, reqCh, "keel", "keel-nexus", "")
	if err == nil {
		t.Errorf("expected ctx deadline error, got nil")
	}
}

// TestPairFromHalf — happy path: a half built by BuildSignedPairHalf round-trips
// through PairFromHalf and produces a PairedChannel with the correct pathID.
func TestPairFromHalf(t *testing.T) {
	ctx := context.Background()
	aCh, _ := casket.Load(ctx, "alice", newInMemStorage(), casket.P256)
	bCh, _ := casket.Load(ctx, "bob", newInMemStorage(), casket.P256)

	// Build halves for both sides (simulating what the relay delivers).
	aHalf, err := BuildSignedPairHalf(aCh, "alice", "https://alice.local")
	if err != nil {
		t.Fatalf("BuildSignedPairHalf alice: %v", err)
	}
	bHalf, err := BuildSignedPairHalf(bCh, "bob", "https://bob.local")
	if err != nil {
		t.Fatalf("BuildSignedPairHalf bob: %v", err)
	}

	// Each side pairs using the other's half — no OOB PairingToken exchange.
	aPaired, err := PairFromHalf(ctx, aCh, bHalf, 3600)
	if err != nil {
		t.Fatalf("PairFromHalf (alice consumes bob's half): %v", err)
	}
	bPaired, err := PairFromHalf(ctx, bCh, aHalf, 3600)
	if err != nil {
		t.Fatalf("PairFromHalf (bob consumes alice's half): %v", err)
	}

	// Both sides must derive the same path_id.
	if aPaired.PathID() != bPaired.PathID() {
		t.Errorf("path_id mismatch: alice=%q bob=%q", aPaired.PathID(), bPaired.PathID())
	}
	if !strings.HasPrefix(aPaired.PathID(), "nxc_") {
		t.Errorf("path_id = %q, want nxc_ prefix", aPaired.PathID())
	}
}

// TestPairFromHalfMissingFields — rejects a half with blank required fields.
func TestPairFromHalfMissingFields(t *testing.T) {
	ctx := context.Background()
	ch, _ := casket.Load(ctx, "alice", newInMemStorage(), casket.P256)

	cases := []struct {
		name string
		h    PairHalfPayload
	}{
		{"missing pubkey", PairHalfPayload{NexusID: "bob", DhAlg: "P-256", DhPubkey: "x", Ts: "2026-04-30T00:00:00Z"}},
		{"missing dh_pubkey", PairHalfPayload{NexusID: "bob", DhAlg: "P-256", Pubkey: "x", Ts: "2026-04-30T00:00:00Z"}},
		{"missing dh_alg", PairHalfPayload{NexusID: "bob", Pubkey: "x", DhPubkey: "x", Ts: "2026-04-30T00:00:00Z"}},
		{"missing nexus_id", PairHalfPayload{DhAlg: "P-256", Pubkey: "x", DhPubkey: "x", Ts: "2026-04-30T00:00:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PairFromHalf(ctx, ch, tc.h, 3600)
			if err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

// TestPairFromHalfBadTs — rejects a half with an unparseable timestamp.
func TestPairFromHalfBadTs(t *testing.T) {
	ctx := context.Background()
	ch, _ := casket.Load(ctx, "alice", newInMemStorage(), casket.P256)
	h := PairHalfPayload{NexusID: "bob", DhAlg: "P-256", Pubkey: "x", DhPubkey: "x", Ts: "not-a-timestamp"}
	_, err := PairFromHalf(ctx, ch, h, 3600)
	if err == nil {
		t.Errorf("expected error for bad ts, got nil")
	}
}
