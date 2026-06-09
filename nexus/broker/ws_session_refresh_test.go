package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/nexus/storage"
)

// sessionRefreshRig stands up a broker with both OperatorLogin (so WS
// JWT auth works and the clock is pinned) and KeyfileValidator (so
// the handler can look up the aspect row).
type sessionRefreshRig struct {
	srv        *httptest.Server
	broker     *Broker
	signingSec []byte
	store      *aspects.SQLStore
	clock      func() time.Time
	now        *atomic.Int64 // unix nanos; mutate to advance the pinned clock
}

func newSessionRefreshRig(t *testing.T) *sessionRefreshRig {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := aspects.NewSQLStore(db)

	signingSec := []byte("fixture-secret-32-bytes-padding-x")

	var now atomic.Int64
	now.Store(time.Unix(1_700_000_000, 0).UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }

	r := roster.New()
	b := New(Config{
		Tokens: NewTokenStore(),
		OperatorLogin: &OperatorLogin{
			SessionSigningSecret: signingSec,
			JWTTTL:               time.Hour,
			NexusID:              "test-nexus",
			Now:                  clock,
			NewSessionID:         func() string { return "ses-fixed" },
		},
		KeyfileValidator: &KeyfileValidator{
			NexusID:              "test-nexus",
			SessionSigningSecret: signingSec,
			Store:                store,
			JWTTTL:               time.Hour,
		},
		HeartbeatIntervalS: 15,
		StaleAfter:         30 * time.Second,
	}, r)

	srv := httptest.NewServer(&testHandler{b: b})
	t.Cleanup(srv.Close)

	return &sessionRefreshRig{
		srv:        srv,
		broker:     b,
		signingSec: signingSec,
		store:      store,
		clock:      clock,
		now:        &now,
	}
}

func (r *sessionRefreshRig) advance(d time.Duration) {
	r.now.Add(int64(d))
}

func (r *sessionRefreshRig) mintAspectJWT(t *testing.T, aspectName string) string {
	t.Helper()
	now := r.clock()
	tok, err := jwt.Sign(r.signingSec, jwt.Claims{
		Iss: "nexus://test-nexus",
		Sub: aspectName,
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
		Kfv: 1,
		Ses: "initial-session",
	})
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return tok
}

func (r *sessionRefreshRig) insertAspect(t *testing.T, name string, kfv int64) {
	t.Helper()
	a := aspects.Aspect{
		Name:                  name,
		AspectPubkey:          fakePubkeyBytes(),
		Provider:              "p",
		Model:                 "m",
		CurrentKeyfileVersion: kfv,
	}
	// Insert defaults CurrentKeyfileVersion to 1 if zero.
	if err := r.store.Insert(context.Background(), a); err != nil {
		t.Fatalf("aspect insert: %v", err)
	}
	// If caller wanted a kfv > 1, bump after insert.
	if kfv > 1 {
		got, err := r.store.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("get after insert: %v", err)
		}
		got.CurrentKeyfileVersion = kfv
		if err := r.store.Update(context.Background(), *got); err != nil {
			t.Fatalf("update kfv: %v", err)
		}
	}
}

func dialAspectWS(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/connect"
	ctx, cancel := context.WithTimeout(context.Background(), brokerAsyncWait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "done") })
	return c
}

func TestSessionRefresh_HappyPath(t *testing.T) {
	rig := newSessionRefreshRig(t)
	rig.insertAspect(t, "plumb", 2)
	tok := rig.mintAspectJWT(t, "plumb")
	c := dialAspectWS(t, rig.srv, tok)

	req, err := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, c, req)

	resp := recvFrame(t, c)
	if resp.Kind != frames.KindSessionRefreshResult {
		t.Fatalf("kind = %q, want %q (raw=%s)", resp.Kind, frames.KindSessionRefreshResult, string(resp.Payload))
	}
	if resp.InReplyTo != req.ID {
		t.Errorf("InReplyTo = %q, want %q", resp.InReplyTo, req.ID)
	}

	var rp frames.SessionRefreshResultPayload
	if err := frames.PayloadAs(resp, &rp); err != nil {
		t.Fatal(err)
	}
	if rp.SessionJWT == "" {
		t.Fatal("empty SessionJWT")
	}
	if rp.SessionExpiresAt == "" {
		t.Fatal("empty SessionExpiresAt")
	}

	// The fresh JWT must verify against the same secret and carry the
	// same sub (identity preserved across refresh).
	claims, err := jwt.Verify(rig.signingSec, rp.SessionJWT, rig.clock())
	if err != nil {
		t.Fatalf("verify refreshed jwt: %v", err)
	}
	if claims.Sub != "plumb" {
		t.Errorf("sub = %q, want plumb (identity invariant)", claims.Sub)
	}
	if claims.Kfv != 2 {
		t.Errorf("kfv = %d, want 2 (mirrors aspect row)", claims.Kfv)
	}
}

func TestSessionRefresh_RateLimited(t *testing.T) {
	rig := newSessionRefreshRig(t)
	rig.insertAspect(t, "plumb", 1)
	tok := rig.mintAspectJWT(t, "plumb")
	c := dialAspectWS(t, rig.srv, tok)

	// First refresh: success.
	req1, _ := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{Reason: "first"})
	sendFrame(t, c, req1)
	resp1 := recvFrame(t, c)
	if resp1.Kind != frames.KindSessionRefreshResult {
		t.Fatalf("first refresh: kind = %q, want %q", resp1.Kind, frames.KindSessionRefreshResult)
	}

	// Second within the 60s window: error frame.
	rig.advance(5 * time.Second)
	req2, _ := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{Reason: "spam"})
	sendFrame(t, c, req2)
	resp2 := recvFrame(t, c)
	wantErr := frames.Kind(string(frames.KindSessionRefresh) + ".error")
	if resp2.Kind != wantErr {
		t.Fatalf("second refresh: kind = %q, want %q (raw=%s)",
			resp2.Kind, wantErr, string(resp2.Payload))
	}
	if resp2.InReplyTo != req2.ID {
		t.Errorf("error InReplyTo = %q, want %q", resp2.InReplyTo, req2.ID)
	}

	// After the window: succeeds again.
	rig.advance(sessionRefreshMinInterval + time.Second)
	req3, _ := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{Reason: "later"})
	sendFrame(t, c, req3)
	resp3 := recvFrame(t, c)
	if resp3.Kind != frames.KindSessionRefreshResult {
		t.Fatalf("third refresh: kind = %q, want %q (raw=%s)",
			resp3.Kind, frames.KindSessionRefreshResult, string(resp3.Payload))
	}
}

func TestSessionRefresh_OperatorConnRejected(t *testing.T) {
	rig := newSessionRefreshRig(t)
	// Operator JWT — must NOT be able to refresh as an aspect.
	now := rig.clock()
	tok, err := jwt.Sign(rig.signingSec, jwt.Claims{
		Iss: "nexus://test-nexus",
		Sub: "operator",
		Iat: now.Unix(),
		Exp: now.Add(time.Hour).Unix(),
		Ses: "op-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := dialAspectWS(t, rig.srv, tok)

	req, _ := frames.NewRequest(frames.KindSessionRefresh, frames.SessionRefreshPayload{Reason: "manual"})
	sendFrame(t, c, req)
	resp := recvFrame(t, c)
	wantErr := frames.Kind(string(frames.KindSessionRefresh) + ".error")
	if resp.Kind != wantErr {
		t.Fatalf("operator refresh: kind = %q, want %q", resp.Kind, wantErr)
	}
}

// http.Handler compile-time assert reused.
var _ http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
