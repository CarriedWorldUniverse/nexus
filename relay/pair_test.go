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
		t.Errorf("missing fields: %+v", half)
	}
	expectedPub := base64.RawURLEncoding.EncodeToString(ch.PublicKeyBytes())
	if half.Pubkey != expectedPub {
		t.Errorf("pubkey mismatch: got %q", half.Pubkey)
	}

	canonical := CanonicalHalfBytes(half.NexusID, half.SigAlg, half.Pubkey,
		half.Endpoint, half.Nonce, half.Ts)
	sig, err := base64.RawURLEncoding.DecodeString(half.SelfSig)
	if err != nil {
		t.Fatalf("decode self_sig: %v", err)
	}
	if !ed25519.Verify(ch.PublicKeyBytes(), canonical, sig) {
		t.Errorf("self-sig did not verify against channel pubkey")
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
// Verifies the happy path returns an approved PairResult with pathId.
func TestPairFullFlow(t *testing.T) {
	// Need a fake with an approver; setupFixture's fake has none.
	// Build a dedicated fake here.
	reqCh, _ := casket.Load(context.Background(), "keel", newInMemStorage(), casket.P256)
	ownCh, _ := casket.Load(context.Background(), "keel-nexus", newInMemStorage(), casket.P256)
	fake := newFakeInterchange(reqCh.PublicKeyBytes(), ownCh.PublicKeyBytes())
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	client := &Client{
		BaseURL:      srv.URL,
		HTTP:         srv.Client(),
		PollInterval: 5 * time.Millisecond,
	}

	// Approver: every 10ms, flip any pending requests to approved.
	stopApprover := make(chan struct{})
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopApprover:
				return
			case <-tick.C:
				fake.approveAllPending(ownCh.PublicKeyBytes())
			}
		}
	}()
	defer close(stopApprover)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.Pair(ctx, reqCh, "keel", "keel-nexus", "https://keel.local")
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
