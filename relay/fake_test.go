package relay

// fakeInterchange is a hand-written in-process HTTP server that speaks
// the Frame-to-Frame wire protocol end-to-end: discovery, mailbox
// (PUT/GET/Ack), and pair-request submission/polling. It does NOT
// import the real interchange from nexus-cw/interchange, because Go's
// `internal/` visibility rule forbids cross-module imports of the
// interchange's handler packages.
//
// Decoupling is actually healthier: client tests pin the wire contract
// (headers, paths, signatures, envelope JSON shape) rather than
// server internals. Drift between this fake and the real interchange
// is the primary risk. Mitigations:
//
//   1. canonicalJSONServer calls the CLIENT's canonicalJSON — so tests
//      verify client+fake self-consistency. Byte-identical match with
//      interchange/internal/mailbox.canonicalJSON is not tested here.
//      Integration test against the real interchange binary is Part 2.6
//      — until that lands, a manual inspection of both canonical
//      functions is the only guarantee.
//
//   2. Direction semantics (A_to_B vs B_to_A), pathId derivation,
//      signature preimage selection all mirror the real interchange's
//      logic by convention, not by import. If the real server's logic
//      changes, update here too — commit messages in
//      nexus-cw/interchange/go/internal/mailbox should trigger a look.
//
// Part 2.6 adds a smoke test that runs the real interchange binary and
// drives this client against it, closing the decoupling gap. This fake
// remains as the fast-feedback unit test layer.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
)

// inMemStorage is a casket.ChannelStorage backed by a map — sufficient
// for tests, no file I/O.
type inMemStorage struct {
	mu   sync.Mutex
	data map[string]string
}

func newInMemStorage() *inMemStorage {
	return &inMemStorage{data: map[string]string{}}
}

func (s *inMemStorage) Get(ctx context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key], nil
}

func (s *inMemStorage) Put(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func (s *inMemStorage) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

// fakeEnvelope stores both the parsed outer envelope and its raw bytes
// so GET responses can return byte-for-byte what was PUT (matching real
// interchange behavior where outer_json is persisted verbatim).
type fakeEnvelope struct {
	Direction string
	Outer     OuterEnvelope
	RawBody   []byte
}

type fakePairRequest struct {
	RequestID     string
	Status        string
	Half          json.RawMessage // requester's raw submitted body
	RequesterHalf *PairHalfPayload
	OwnerHalf     *PairHalfPayload
	PathID        string
}

// fakeInterchange is the test-side HTTP handler implementing the wire
// protocol. One instance serves one pair (sideA, sideB).
type fakeInterchange struct {
	mu               sync.Mutex
	pathID           string
	sideAPub         []byte
	sideBPub         []byte
	envelopes        []fakeEnvelope
	pendingRequests  map[string]fakePairRequest
	nextRequestIDSeq int
}

func newFakeInterchange(aPub, bPub []byte) *fakeInterchange {
	return &fakeInterchange{
		pathID:          pathIDFromPubkeys(aPub, bPub),
		sideAPub:        aPub,
		sideBPub:        bPub,
		pendingRequests: map[string]fakePairRequest{},
	}
}

// pathIDFromPubkeys — "nxc_" + base64url(sha256(sort(a, b))). Must
// match casket-go + interchange pairflow logic. Commutative.
func pathIDFromPubkeys(a, b []byte) string {
	first, second := a, b
	if bytes.Compare(a, b) > 0 {
		first, second = b, a
	}
	h := sha256.New()
	h.Write(first)
	h.Write(second)
	return "nxc_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// verifySig tries each half's pubkey; returns the matching one or nil.
func (f *fakeInterchange) verifySig(sigB64 string, msg []byte) []byte {
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil
	}
	for _, pub := range [][]byte{f.sideAPub, f.sideBPub} {
		if ed25519.Verify(pub, msg, sig) {
			return pub
		}
	}
	return nil
}

func (f *fakeInterchange) directionFrom(senderPub []byte) string {
	if bytes.Equal(senderPub, f.sideAPub) {
		return "A_to_B"
	}
	return "B_to_A"
}

func (f *fakeInterchange) readDirectionFor(readerPub []byte) string {
	if bytes.Equal(readerPub, f.sideAPub) {
		return "B_to_A"
	}
	return "A_to_B"
}

// canonicalJSONServer — MUST produce bytes byte-identical to
// `interchange/internal/mailbox.canonicalJSON`. See fake_test.go doc
// comment above: the decoupling between this fake and the real server
// is the main risk in 3.1. Part 2.6 integration test closes the gap.
//
// Currently reuses the client's canonicalJSON — ensures client/fake
// internal consistency but NOT parity with the real interchange.
// Editing one without the other is a foot-gun.
func canonicalJSONServer(env OuterEnvelope) []byte {
	b, _ := canonicalJSON(env)
	return b
}

func (f *fakeInterchange) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/nexus-interchange", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1","protocol":"nexus-frame-relay/1","trust_model":"operator_approval"}`))
	})

	mux.HandleFunc("/mailbox/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/mailbox/")
		parts := strings.Split(trimmed, "/")
		pathID := parts[0]
		if pathID != f.pathID {
			http.Error(w, `{"error":"pair_not_found"}`, http.StatusNotFound)
			return
		}
		if len(parts) == 2 && parts[1] == "ack" && r.Method == http.MethodPost {
			f.handleAck(w, r)
			return
		}
		if len(parts) == 1 {
			switch r.Method {
			case http.MethodPut:
				f.handlePut(w, r, pathID)
			case http.MethodGet:
				f.handleGet(w, r, pathID)
			default:
				w.Header().Set("Allow", "PUT, GET")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/pair/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))

		// Verify the requester's self-sig before accepting. Catches
		// client-side bugs in CanonicalHalfBytes / Channel.Sign that
		// would otherwise silently store garbage and confuse tests
		// downstream. Real interchange rejects bad sigs with 400
		// bad_self_sig; the fake mirrors this.
		var submitted struct {
			Requester PairHalfPayload `json:"requester"`
		}
		if err := json.Unmarshal(body, &submitted); err != nil {
			http.Error(w, `{"error":"invalid_json"}`, http.StatusBadRequest)
			return
		}
		if err := verifyHalfSelfSig(submitted.Requester); err != nil {
			http.Error(w, `{"error":"bad_self_sig","detail":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		f.nextRequestIDSeq++
		reqID := "00000000-0000-4000-8000-" + strings.Repeat("0", 11) + formatHex(f.nextRequestIDSeq)
		f.pendingRequests[reqID] = fakePairRequest{
			RequestID: reqID, Status: "pending", Half: json.RawMessage(body),
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"request_id": reqID, "status": "pending",
		})
	})

	mux.HandleFunc("/pair/requests/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/pair/requests/")
		parts := strings.Split(trimmed, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && len(parts) == 1 {
			f.mu.Lock()
			defer f.mu.Unlock()
			req, ok := f.pendingRequests[parts[0]]
			if !ok {
				http.Error(w, `{"error":"request_not_found"}`, http.StatusNotFound)
				return
			}
			out := struct {
				RequestID string           `json:"request_id"`
				Status    string           `json:"status"`
				PathID    string           `json:"path_id,omitempty"`
				OwnerHalf *PairHalfPayload `json:"owner_half,omitempty"`
			}{
				RequestID: req.RequestID,
				Status:    req.Status,
				PathID:    req.PathID,
				OwnerHalf: req.OwnerHalf,
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		http.NotFound(w, r)
	})

	return mux
}

func (f *fakeInterchange) handlePut(w http.ResponseWriter, r *http.Request, pathID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, `{"error":"body_read_failed"}`, http.StatusBadRequest)
		return
	}
	var env OuterEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, `{"error":"invalid_json"}`, http.StatusBadRequest)
		return
	}
	if env.PathID != pathID {
		http.Error(w, `{"error":"path_id_mismatch"}`, http.StatusBadRequest)
		return
	}
	senderPub := f.verifySig(r.Header.Get("X-Nexus-Signature"), canonicalJSONServer(env))
	if senderPub == nil {
		http.Error(w, `{"error":"signature_invalid"}`, http.StatusUnauthorized)
		return
	}
	direction := f.directionFrom(senderPub)

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.envelopes {
		if existing.Outer.MsgID == env.MsgID {
			http.Error(w, `{"error":"duplicate_msg_id"}`, http.StatusConflict)
			return
		}
	}
	f.envelopes = append(f.envelopes, fakeEnvelope{
		Direction: direction, Outer: env, RawBody: body,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"msg_id": env.MsgID})
}

func (f *fakeInterchange) handleGet(w http.ResponseWriter, r *http.Request, pathID string) {
	signedPath := r.URL.Path
	if r.URL.RawQuery != "" {
		signedPath += "?" + r.URL.RawQuery
	}
	callerPub := f.verifySig(r.Header.Get("X-Nexus-Signature"), []byte(signedPath))
	if callerPub == nil {
		http.Error(w, `{"error":"signature_invalid"}`, http.StatusUnauthorized)
		return
	}
	readDir := f.readDirectionFor(callerPub)
	since := r.URL.Query().Get("since")

	f.mu.Lock()
	defer f.mu.Unlock()
	envelopes := make([]json.RawMessage, 0)
	var cursor string
	for _, env := range f.envelopes {
		if env.Direction != readDir {
			continue
		}
		if since != "" && env.Outer.MsgID <= since {
			continue
		}
		envelopes = append(envelopes, json.RawMessage(env.RawBody))
		cursor = env.Outer.MsgID
	}
	resp := struct {
		Envelopes []json.RawMessage `json:"envelopes"`
		Cursor    *string           `json:"cursor"`
	}{Envelopes: envelopes}
	if cursor != "" {
		resp.Cursor = &cursor
	}
	writeJSON(w, http.StatusOK, resp)
}

func (f *fakeInterchange) handleAck(w http.ResponseWriter, r *http.Request) {
	callerPub := f.verifySig(r.Header.Get("X-Nexus-Signature"), []byte(r.URL.Path))
	if callerPub == nil {
		http.Error(w, `{"error":"signature_invalid"}`, http.StatusUnauthorized)
		return
	}
	readDir := f.readDirectionFor(callerPub)
	var body struct {
		IDs []string `json:"ids"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	defer f.mu.Unlock()
	wanted := map[string]bool{}
	for _, id := range body.IDs {
		wanted[id] = true
	}
	evicted := 0
	kept := f.envelopes[:0]
	for _, env := range f.envelopes {
		if env.Direction == readDir && wanted[env.Outer.MsgID] {
			evicted++
			continue
		}
		kept = append(kept, env)
	}
	f.envelopes = kept
	writeJSON(w, http.StatusOK, map[string]int{"evicted": evicted})
}

// verifyHalfSelfSig rebuilds the canonical bytes from the payload's own fields
// and checks self_sig against the payload's pubkey. Accepts both v2 (preferred)
// and v1 (transition) preimage formats. v2 is detected by presence of DhAlg +
// DhPubkey fields; falls back to v1 if those are absent (legacy half).
func verifyHalfSelfSig(h PairHalfPayload) error {
	if h.SigAlg != "ed25519" {
		return fmt.Errorf("sig_alg %q not supported at v1", h.SigAlg)
	}
	pubBytes, err := base64.RawURLEncoding.DecodeString(h.Pubkey)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid pubkey")
	}
	sig, err := base64.RawURLEncoding.DecodeString(h.SelfSig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid self_sig")
	}

	// Try v2 first (dh_alg + dh_pubkey in preimage). If those fields are
	// present, the half MUST verify against the v2 preimage; falling back
	// to v1 on a v2 half would defeat the purpose of signature coverage.
	if h.DhAlg != "" && h.DhPubkey != "" {
		canonical := CanonicalHalfBytes(h.NexusID, h.SigAlg, h.Pubkey,
			h.DhAlg, h.DhPubkey, h.Endpoint, h.Nonce, h.Ts)
		if !ed25519.Verify(pubBytes, canonical, sig) {
			return fmt.Errorf("self_sig did not verify (v2 preimage)")
		}
		return nil
	}

	// v1 fallback: dh fields absent, use legacy preimage.
	canonical := canonicalHalfBytesV1(h.NexusID, h.SigAlg, h.Pubkey,
		h.Endpoint, h.Nonce, h.Ts)
	if !ed25519.Verify(pubBytes, canonical, sig) {
		return fmt.Errorf("self_sig did not verify (v1 preimage)")
	}
	return nil
}

// approveAllPending flips every currently-pending request to approved.
// ownerHalf is the fully-signed PairHalfPayload representing the owner's
// side — in tests this is built via BuildSignedPairHalf on the owner channel.
// The requester's half is parsed from the stored request body so it can be
// returned in poll responses, completing the no-sneakernet token exchange.
func (f *fakeInterchange) approveAllPending(ownerHalf PairHalfPayload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ownerPub, err := base64.RawURLEncoding.DecodeString(ownerHalf.Pubkey)
	if err != nil {
		return
	}
	for id, req := range f.pendingRequests {
		if req.Status != "pending" {
			continue
		}
		var payload struct {
			Requester PairHalfPayload `json:"requester"`
		}
		if err := json.Unmarshal(req.Half, &payload); err != nil {
			continue
		}
		reqPub, err := base64.RawURLEncoding.DecodeString(payload.Requester.Pubkey)
		if err != nil {
			continue
		}
		pathID := pathIDFromPubkeys(reqPub, ownerPub)
		req.Status = "approved"
		req.PathID = pathID
		req.RequesterHalf = &payload.Requester
		oh := ownerHalf
		req.OwnerHalf = &oh
		f.pendingRequests[id] = req
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func formatHex(n int) string {
	if n == 0 {
		return "0"
	}
	const hex = "0123456789abcdef"
	var out [8]byte
	i := len(out)
	for n > 0 {
		i--
		out[i] = hex[n&0xf]
		n >>= 4
	}
	return string(out[i:])
}

// setupFixture stands up two paired casket channels against a fake
// interchange and returns the client + both PairedChannels + teardown.
func setupFixture(t *testing.T) (*Client, *casket.PairedChannel, *casket.PairedChannel, func()) {
	t.Helper()
	ctx := context.Background()

	keelCh, err := casket.Load(ctx, "keel", newInMemStorage(), casket.P256)
	if err != nil {
		t.Fatalf("keel Load: %v", err)
	}
	nexusCh, err := casket.Load(ctx, "keel-nexus", newInMemStorage(), casket.P256)
	if err != nil {
		t.Fatalf("nexus Load: %v", err)
	}
	keelToken := keelCh.MakePairingToken("https://keel.local")
	nexusToken := nexusCh.MakePairingToken("https://keel-nexus.local")
	keelPaired, err := keelCh.Pair(ctx, nexusToken, 3600)
	if err != nil {
		t.Fatalf("keel.Pair: %v", err)
	}
	nexusPaired, err := nexusCh.Pair(ctx, keelToken, 3600)
	if err != nil {
		t.Fatalf("nexus.Pair: %v", err)
	}
	if keelPaired.PathID() != nexusPaired.PathID() {
		t.Fatalf("pathId mismatch")
	}

	fake := newFakeInterchange(keelCh.PublicKeyBytes(), nexusCh.PublicKeyBytes())
	srv := httptest.NewServer(fake.handler())
	client := &Client{
		BaseURL: srv.URL, HTTP: srv.Client(), PollInterval: time.Millisecond,
	}
	return client, keelPaired, nexusPaired, srv.Close
}
