package relay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
)

// Test-only msg_ids with valid UUIDv7 shape. Sequence-ordered so GET
// cursor tests exercise the lexicographic ordering the interchange
// relies on.
const (
	validMsgID1 = "0194a81e-73c4-7001-8aaa-000000000001"
	validMsgID2 = "0194a81e-73c4-7002-8aaa-000000000002"
)

// sampleEnvelope builds a real-AEAD-encrypted envelope ready to Put.
// Empty AAD — matches v3 spec decision (outer hash is the integrity
// binding, AEAD tag covers inner; AAD=sha256(ct) would be circular).
func sampleEnvelope(t *testing.T, paired *casket.PairedChannel, pathID, msgID string) OuterEnvelope {
	t.Helper()
	ct, err := paired.EncryptBody([]byte(`{"kind":"announce"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return BuildOuter(pathID, msgID, IsoTs(time.Now().UTC()), ct)
}

// --- PUT tests ---

func TestPutHappyPath(t *testing.T) {
	client, keel, _, teardown := setupFixture(t)
	defer teardown()
	env := sampleEnvelope(t, keel, keel.PathID(), validMsgID1)
	if err := client.Put(context.Background(), keel, env); err != nil {
		t.Errorf("Put: %v", err)
	}
}

func TestPutDuplicateReturns409(t *testing.T) {
	client, keel, _, teardown := setupFixture(t)
	defer teardown()
	env := sampleEnvelope(t, keel, keel.PathID(), validMsgID1)
	_ = client.Put(context.Background(), keel, env)
	err := client.Put(context.Background(), keel, env)
	if err == nil || !errorsIs(err, ErrDuplicate) {
		t.Errorf("err = %v, want ErrDuplicate", err)
	}
}

func TestPutUnknownPathReturns404(t *testing.T) {
	client, keel, _, teardown := setupFixture(t)
	defer teardown()
	env := sampleEnvelope(t, keel, "nxc_"+strings.Repeat("z", 43), validMsgID1)
	err := client.Put(context.Background(), keel, env)
	if err == nil || !errorsIs(err, ErrPairNotFound) {
		t.Errorf("err = %v, want ErrPairNotFound", err)
	}
}

// --- GET / Ack tests ---

func TestGetReturnsEnvelopesForReader(t *testing.T) {
	client, keel, nexus, teardown := setupFixture(t)
	defer teardown()
	for _, id := range []string{validMsgID1, validMsgID2} {
		env := sampleEnvelope(t, keel, keel.PathID(), id)
		if err := client.Put(context.Background(), keel, env); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := client.Get(context.Background(), nexus, nexus.PathID(), "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(resp.Envelopes) != 2 {
		t.Errorf("envelopes = %d, want 2", len(resp.Envelopes))
	}
}

// TestGetDirectionIsolation pins the invariant that a side CAN NOT read
// its own outbox. Interchange filters by direction; client-side this
// means keel's Get on its own pathId returns nothing it sent.
func TestGetDirectionIsolation(t *testing.T) {
	client, keel, _, teardown := setupFixture(t)
	defer teardown()
	_ = client.Put(context.Background(), keel, sampleEnvelope(t, keel, keel.PathID(), validMsgID1))
	resp, _ := client.Get(context.Background(), keel, keel.PathID(), "")
	if len(resp.Envelopes) != 0 {
		t.Errorf("keel read own outbox: %d", len(resp.Envelopes))
	}
}

func TestGetSinceCursor(t *testing.T) {
	client, keel, nexus, teardown := setupFixture(t)
	defer teardown()
	for _, id := range []string{validMsgID1, validMsgID2} {
		_ = client.Put(context.Background(), keel, sampleEnvelope(t, keel, keel.PathID(), id))
	}
	resp, err := client.Get(context.Background(), nexus, nexus.PathID(), validMsgID1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(resp.Envelopes) != 1 {
		t.Errorf("after cursor: %d, want 1", len(resp.Envelopes))
	}
}

func TestAckEvicts(t *testing.T) {
	client, keel, nexus, teardown := setupFixture(t)
	defer teardown()
	_ = client.Put(context.Background(), keel, sampleEnvelope(t, keel, keel.PathID(), validMsgID1))

	n, err := client.Ack(context.Background(), nexus, nexus.PathID(), []string{validMsgID1})
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n != 1 {
		t.Errorf("evicted = %d", n)
	}
	resp, _ := client.Get(context.Background(), nexus, nexus.PathID(), "")
	if len(resp.Envelopes) != 0 {
		t.Errorf("envelopes after ack: %d", len(resp.Envelopes))
	}
}

// --- End-to-end encrypt / PUT / GET / decrypt ---

// TestEndToEndDecryptRoundTrip is the load-bearing proof that the
// client + fake + casket pairs actually produce a valid round trip:
// keel encrypts, PUTs, nexus GETs, verifies ciphertext_sha256, decrypts
// via its own PairedChannel. If any single layer (canonical JSON,
// signing preimage, AEAD framing) drifts, this fails.
func TestEndToEndDecryptRoundTrip(t *testing.T) {
	client, keel, nexus, teardown := setupFixture(t)
	defer teardown()
	plaintext := []byte("the hidden message")
	ct, err := keel.EncryptBody(plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := BuildOuter(keel.PathID(), validMsgID1, IsoTs(time.Now().UTC()), ct)
	if err := client.Put(context.Background(), keel, env); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(context.Background(), nexus, nexus.PathID(), "")
	if err != nil || len(resp.Envelopes) != 1 {
		t.Fatalf("Get: %v, %d", err, len(resp.Envelopes))
	}
	var got OuterEnvelope
	_ = json.Unmarshal(resp.Envelopes[0], &got)
	ctBytes, _ := base64.RawURLEncoding.DecodeString(got.Ciphertext)
	digest := sha256.Sum256(ctBytes)
	if hex.EncodeToString(digest[:]) != got.CiphertextSHA256 {
		t.Errorf("ciphertext hash drift")
	}
	recovered, err := nexus.DecryptBody(ctBytes, nil)
	if err != nil {
		t.Fatalf("DecryptBody: %v", err)
	}
	if string(recovered) != string(plaintext) {
		t.Errorf("decrypt = %q", recovered)
	}
}

// --- Discover / Pair-request tests ---

func TestDiscover(t *testing.T) {
	client, _, _, teardown := setupFixture(t)
	defer teardown()
	doc, err := client.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !strings.Contains(string(doc), `"protocol":"nexus-frame-relay/1"`) {
		t.Errorf("discovery doc missing protocol: %s", doc)
	}
}

// TestSubmitPairRequest exercises POST /pair/request with a hand-built
// v2 self-signature (dh_alg + dh_pubkey covered by preimage).
func TestSubmitPairRequest(t *testing.T) {
	client, _, _, teardown := setupFixture(t)
	defer teardown()
	ch, _ := casket.Load(context.Background(), "bob", newInMemStorage(), casket.P256)
	half, err := BuildSignedPairHalf(ch, "bob", "https://bob")
	if err != nil {
		t.Fatalf("BuildSignedPairHalf: %v", err)
	}

	res, err := client.SubmitPairRequest(context.Background(), SubmitPairRequestBody{
		TargetNexusID: "alice",
		Requester:     half,
	})
	if err != nil {
		t.Fatalf("SubmitPairRequest: %v", err)
	}
	if res.RequestID == "" || res.Status != "pending" {
		t.Errorf("res = %+v", res)
	}
}

func TestPollRequestContextCancel(t *testing.T) {
	client, _, _, teardown := setupFixture(t)
	defer teardown()
	ch, _ := casket.Load(context.Background(), "bob", newInMemStorage(), casket.P256)
	half, _ := BuildSignedPairHalf(ch, "bob", "")
	res, _ := client.SubmitPairRequest(context.Background(), SubmitPairRequestBody{
		TargetNexusID: "alice",
		Requester:     half,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := client.PollRequest(ctx, res.RequestID)
	if err == nil {
		t.Errorf("expected ctx error, got nil")
	}
}

// --- canonical helper format tests ---

func TestCanonicalHalfBytesFormat(t *testing.T) {
	// v2 preimage: 9 fields, first field is "v2", dh_alg and dh_pubkey after pubkey.
	got := string(CanonicalHalfBytes("bob", "ed25519", "pub", "P-256", "dhpub", "ep", "nonce", "2026-04-25T12:00:00Z"))
	want := "v2\nbob\ned25519\npub\nP-256\ndhpub\nep\nnonce\n2026-04-25T12:00:00Z"
	if got != want {
		t.Errorf("\ngot  %q\nwant %q", got, want)
	}
	// v1 preimage: 7 fields, first field is "v1", no dh fields — accepted during transition.
	gotV1 := string(canonicalHalfBytesV1("bob", "ed25519", "pub", "ep", "nonce", "2026-04-25T12:00:00Z"))
	wantV1 := "v1\nbob\ned25519\npub\nep\nnonce\n2026-04-25T12:00:00Z"
	if gotV1 != wantV1 {
		t.Errorf("v1:\ngot  %q\nwant %q", gotV1, wantV1)
	}
}

// TestCanonicalJSONNoHTMLEscape pins the HTML-escape regression guard
// that Part 2.3 review caught on the interchange side. The same
// regression could break signature verification here.
func TestCanonicalJSONNoHTMLEscape(t *testing.T) {
	env := OuterEnvelope{
		Version: "1", MsgID: validMsgID1, Ts: "2026-04-25T12:00:00Z",
		PathID: "nxc_x", CiphertextSHA256: "abc", Ciphertext: "<>&",
	}
	out, err := canonicalJSON(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("HTML escape leaked: %s", out)
		}
	}
	if !strings.Contains(string(out), `"<>&"`) {
		t.Errorf("literal <>& missing: %s", out)
	}
}

// errorsIs — tiny shim to avoid importing errors just for Is in tests.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type wrapper interface{ Unwrap() error }
		if w, ok := err.(wrapper); ok {
			err = w.Unwrap()
		} else {
			return false
		}
	}
	return false
}
