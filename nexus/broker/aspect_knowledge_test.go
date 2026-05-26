package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/jwt"
	"github.com/CarriedWorldUniverse/nexus/nexus/knowledge"
)

// Operator-reported 2026-05-27: harrow tried to store_knowledge via
// nexus-comms-mcp and the broker rejected with "connection not
// registered". Root cause: nexus-comms-mcp's WS connection
// authenticated as harrow (aspect JWT) but its register frame failed
// because harrow's agentfunnel already held the registered slot, so
// c.registeredAs stayed empty even though the auth middleware had
// proven the connection IS harrow.
//
// Fix: handleAspectKnowledge{Search,Store} fall back to c.auth.AgentID
// (the JWT-verified identity) when c.registeredAs is empty. The
// regression tests below cover the unregistered-but-auth'd shape.

func mintAspectJWT(t *testing.T, secret []byte, aspect string, exp time.Time) string {
	t.Helper()
	tok, err := jwt.Sign(secret, jwt.Claims{
		Iss: "nexus://test-nexus",
		Sub: aspect, // non-operator sub → tryVerifyAspectJWT path
		Iat: time.Now().Unix(),
		Exp: exp.Unix(),
		Ses: "test-aspect-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestAspectKnowledge_StoreFallsBackToAuthIdentity_WhenUnregistered(t *testing.T) {
	srv, _, _, kstore, _ := newOperatorTestServerFull(t)
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	aspectTok := mintAspectJWT(t, secret, "harrow", time.Now().Add(time.Hour))

	// Dial as harrow (aspect JWT) and DON'T send a register frame —
	// simulates nexus-comms-mcp connecting alongside an already-
	// registered agentfunnel.
	c := dialWS(t, srv, aspectTok)

	resp := mustResponse(t, c, frames.KindKnowledgeStore, frames.KnowledgeStorePayload{
		Topic:   "research-note",
		Content: "harrow's research finding",
	})
	if resp.Kind != frames.KindKnowledgeStoreResult {
		t.Fatalf("expected %s, got %s (body=%s)",
			frames.KindKnowledgeStoreResult, resp.Kind, string(resp.Payload))
	}
	p := payloadAs[frames.KnowledgeStoreResultPayload](t, resp)
	if p.ID == 0 {
		t.Error("store must return non-zero id")
	}

	// Verify the row was attributed to harrow (NOT operator or empty)
	// via direct store inspection. Without the auth-fallback fix, the
	// frame would have errored before reaching the store at all.
	entries, err := kstore.List(context.Background(), "harrow", 10)
	if err != nil {
		t.Fatalf("kstore.List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry under harrow, got %d", len(entries))
	}
	if entries[0].Topic != "research-note" {
		t.Errorf("topic: got %q want research-note", entries[0].Topic)
	}
	if entries[0].FromAgent != "harrow" {
		t.Errorf("from_agent: got %q want harrow (auth-identity must stamp store row)", entries[0].FromAgent)
	}
}

func TestAspectKnowledge_SearchFallsBackToAuthIdentity_WhenUnregistered(t *testing.T) {
	srv, _, _, kstore, _ := newOperatorTestServerFull(t)
	secret := []byte("test-secret-32-bytes-padding-vvvv")
	aspectTok := mintAspectJWT(t, secret, "harrow", time.Now().Add(time.Hour))

	// Seed harrow's own entry directly via the store.
	ctx := context.Background()
	if _, err := kstore.Put(ctx, "harrow", "alpha", "harrow alpha content", knowledge.PutOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := dialWS(t, srv, aspectTok)

	resp := mustResponse(t, c, frames.KindKnowledgeSearch, frames.KnowledgeSearchPayload{
		Text:     "alpha",
		OwnAgent: true,
		TopK:     5,
	})
	if resp.Kind != frames.KindKnowledgeSearchResult {
		t.Fatalf("expected %s, got %s (body=%s)",
			frames.KindKnowledgeSearchResult, resp.Kind, string(resp.Payload))
	}
	p := payloadAs[frames.KnowledgeSearchResultPayload](t, resp)
	if len(p.Hits) == 0 {
		t.Error("expected at least one hit for harrow's own entry")
	}
	if len(p.Hits) > 0 && p.Hits[0].FromAgent != "harrow" {
		t.Errorf("hit.from_agent: got %q want harrow", p.Hits[0].FromAgent)
	}
}

// Connection with NO auth identity at all (shouldn't reach the
// handler in production — auth middleware would 401 first — but
// guard anyway so a future code path that bypasses auth can't
// silently store under empty from_agent).
func TestAspectKnowledge_NoIdentity_StillRejected(t *testing.T) {
	// We can't easily construct a TokenInfo-less wsConn in test without
	// bypassing the auth middleware, so we directly assert the helper
	// behaviour. The handler's error path is exercised when
	// aspectIdentity() returns "" — which it does for a zero TokenInfo.
	c := &wsConn{}
	if got := c.aspectIdentity(); got != "" {
		t.Errorf("aspectIdentity on empty conn = %q, want \"\"", got)
	}
}

// Helper for the operator-side knowledge-store path is already
// covered by TestOperatorFrames_KnowledgeStoreAndList in
// operator_frames_test.go (operator dispatch intercepts before the
// aspect handler runs); this file only covers the aspect path.

// Match the operator-frames test conventions: keep a single-line
// strings import even when only used by future tests, so unused-
// import errors don't surface on partial edits.
var _ = strings.Contains